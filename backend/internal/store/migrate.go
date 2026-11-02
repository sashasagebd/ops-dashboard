package store

import (
	"cmp"
	"context"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationLockKey identifies the platform's migration lock among Postgres
// advisory locks. The value is arbitrary; it only has to differ from any
// other advisory lock taken in the same database, and the platform is the
// only user of this one.
const migrationLockKey int64 = 7_206_431_001

// A migration is one schema change, read from a file named
// <version>_<description>.sql (e.g. 0001_create_apps.sql).
type migration struct {
	version int64
	name    string // the file name, recorded alongside the version
	sql     string
}

var migrationName = regexp.MustCompile(`^([0-9]+)_[a-z0-9_]+\.sql$`)

// loadMigrations reads the .sql files in dir, sorted by version. Versions
// must run 1, 2, 3... with no gaps or duplicates: migrations are applied in
// order, so a gap almost always means a file was renamed or lost, and it's
// better to refuse to start than to skip a schema change.
func loadMigrations(fsys fs.FS, dir string) ([]migration, error) {
	paths, err := fs.Glob(fsys, path.Join(dir, "*.sql"))
	if err != nil {
		return nil, err
	}

	var migrations []migration
	for _, p := range paths {
		name := path.Base(p)
		m := migrationName.FindStringSubmatch(name)
		if m == nil {
			return nil, fmt.Errorf("migration %s: name must look like 0001_description.sql", name)
		}
		version, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migration %s: %w", name, err)
		}
		sql, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, err
		}
		migrations = append(migrations, migration{version: version, name: name, sql: string(sql)})
	}

	slices.SortFunc(migrations, func(a, b migration) int { return cmp.Compare(a.version, b.version) })
	for i, m := range migrations {
		if want := int64(i + 1); m.version != want {
			return nil, fmt.Errorf("migration %s: expected version %d next (versions must run 1, 2, 3... with no gaps or duplicates)", m.name, want)
		}
	}
	return migrations, nil
}

// migrate applies the migrations that the database doesn't have yet, in
// order, and returns the versions it applied.
//
// It holds an advisory lock for the whole run, so if two platform processes
// start at once, the second waits and then finds nothing left to do, instead
// of both applying the same migration. Each migration runs in its own
// transaction together with its schema_migrations row: Postgres runs DDL
// transactionally, so a failing migration leaves no trace and is retried in
// full on the next start, while the ones before it stay applied.
//
// Migration files must not contain their own BEGIN/COMMIT (that would end
// the transaction early), or statements Postgres can't run inside a
// transaction, such as CREATE INDEX CONCURRENTLY.
func migrate(ctx context.Context, pool *pgxpool.Pool, migrations []migration) (applied []int64, err error) {
	// Advisory locks belong to a session, so the lock, and everything done
	// under it, must use one connection rather than whichever the pool hands
	// out per query.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		conn.Release()
		return nil, fmt.Errorf("taking the migration lock: %w", err)
	}
	defer unlockAndRelease(ctx, conn)

	// Created under the lock, so concurrent first runs can't race on it.
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    bigint PRIMARY KEY,
		name       text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return nil, fmt.Errorf("creating schema_migrations: %w", err)
	}

	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("reading schema_migrations: %w", err)
	}
	done, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("reading schema_migrations: %w", err)
	}
	// A database ahead of the binary means an older platform image is running
	// against a newer schema (e.g. after rolling the platform back). Its code
	// may write rows the newer schema's assumptions don't allow, so refuse to
	// start rather than guess. Schema changes only go forward.
	if len(done) > 0 && done[len(done)-1] > int64(len(migrations)) {
		return nil, fmt.Errorf("database schema is at version %d, but this binary only knows up to %d", done[len(done)-1], len(migrations))
	}

	for _, m := range migrations {
		if slices.Contains(done, m.version) {
			continue
		}
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			// No arguments, so pgx uses Postgres's simple query protocol,
			// which allows several statements in one file.
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, name) VALUES ($1, $2)", m.version, m.name)
			return err
		})
		if err != nil {
			return applied, fmt.Errorf("migration %s: %w", m.name, err)
		}
		applied = append(applied, m.version)
	}
	return applied, nil
}

// unlockAndRelease releases the migration lock and returns conn to the pool.
func unlockAndRelease(ctx context.Context, conn *pgxpool.Conn) {
	// Not ctx itself: if it was cancelled mid-migration (e.g. shutdown), the
	// lock still has to be released.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", migrationLockKey); err != nil {
		// The session still holds the lock, so it must not go back into the
		// pool, where it would keep the lock and block the next migration.
		// Closing the connection ends the session, which releases it.
		_ = conn.Hijack().Close(ctx)
		return
	}
	conn.Release()
}
