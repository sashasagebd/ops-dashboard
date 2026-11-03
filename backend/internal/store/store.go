// Package store is the platform's access to Postgres, the source of truth for
// apps, deployments and jobs. The schema lives in migrations/, embedded in the
// binary and applied by Migrate on startup (see migrate.go). Rules that must
// always hold (one deploy in progress per app, one live deployment, forward-
// only status changes) are enforced by the database or by single atomic
// statements, not by Go code checking first.
package store

import (
	"context"
	"embed"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sashasagebd/ops-dashboard/backend/internal/secrets"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Store holds the platform's connection pool, and the key that encrypts the
// values it must not keep in plaintext (env vars).
type Store struct {
	pool *pgxpool.Pool
	box  *secrets.Box
}

// Open prepares a pool for databaseURL. It doesn't connect: the first
// connection is made on first use (e.g. Ping), so Open succeeds even while
// Postgres is still starting.
//
// Pool settings can go in the URL (e.g. pool_max_conns=4); pgx's default is
// the greater of 4 and the number of CPUs, which is 4 on the server.
func Open(databaseURL string, box *secrets.Box) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// Deliberately not wrapping err: pgx redacts the password from its
		// parse errors only on a best-effort basis, and this ends up in the
		// log.
		return nil, errors.New("DATABASE_URL: not a valid connection string")
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	return &Store{pool: pool, box: box}, nil
}

// Ping checks that Postgres is reachable.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Migrate brings the schema up to date, returning the versions it applied
// (none if it was already current).
func (s *Store) Migrate(ctx context.Context) ([]int64, error) {
	migrations, err := loadMigrations(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	return migrate(ctx, s.pool, migrations)
}

// Close closes every connection in the pool.
func (s *Store) Close() {
	s.pool.Close()
}
