package store

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLoadMigrations(t *testing.T) {
	file := func(sql string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(sql)} }

	tests := []struct {
		name         string
		files        fstest.MapFS
		wantVersions []int64
		wantErr      string
	}{
		{name: "none", files: fstest.MapFS{}},
		{
			name: "sorted by version, not by name",
			files: fstest.MapFS{
				"m/0002_b.sql": file("B"),
				"m/1_a.sql":    file("A"),
				"m/README.md":  file("not a migration"),
			},
			wantVersions: []int64{1, 2},
		},
		{
			name:    "gap",
			files:   fstest.MapFS{"m/0001_a.sql": file(""), "m/0003_c.sql": file("")},
			wantErr: "expected version 2",
		},
		{
			name:    "duplicate version",
			files:   fstest.MapFS{"m/0001_a.sql": file(""), "m/1_b.sql": file("")},
			wantErr: "expected version 2",
		},
		{
			name:    "not starting at 1",
			files:   fstest.MapFS{"m/0002_b.sql": file("")},
			wantErr: "expected version 1",
		},
		{
			name:    "bad name",
			files:   fstest.MapFS{"m/0001-create-apps.sql": file("")},
			wantErr: "name must look like",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := loadMigrations(tt.files, "m")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("loadMigrations error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadMigrations: %v", err)
			}
			if versions := versionsOf(got); !slices.Equal(versions, tt.wantVersions) {
				t.Errorf("versions = %v, want %v", versions, tt.wantVersions)
			}
		})
	}
}

// The real migrations are checked for naming and numbering even where there's
// no database to apply them to.
func TestEmbeddedMigrationsLoad(t *testing.T) {
	migrations, err := loadMigrations(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) == 0 {
		t.Fatal("no embedded migrations")
	}
}

// The tests below need a real Postgres: what they check (transactional DDL,
// advisory locks) is Postgres's own behaviour, which a fake can't vouch for.
// They skip when DATABASE_URL is unset, except in CI, where they must run.

func TestMigrateAppliesInOrderOnce(t *testing.T) {
	pool := testPool(t)
	migrations := []migration{
		{version: 1, name: "0001_a.sql", sql: "CREATE TABLE a (id int)"},
		// Depends on 1, so it only works if they're applied in order.
		{version: 2, name: "0002_b.sql", sql: "ALTER TABLE a ADD COLUMN b text; INSERT INTO a VALUES (1, 'x')"},
	}

	applied, err := migrate(t.Context(), pool, migrations)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(applied, []int64{1, 2}) {
		t.Errorf("first run applied %v, want [1 2]", applied)
	}

	applied, err = migrate(t.Context(), pool, migrations)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Errorf("second run applied %v, want nothing", applied)
	}
	if got := recordedVersions(t, pool); !slices.Equal(got, []int64{1, 2}) {
		t.Errorf("schema_migrations = %v, want [1 2]", got)
	}
}

func TestMigrateFailureRollsBackThatMigrationOnly(t *testing.T) {
	pool := testPool(t)
	good := migration{version: 1, name: "0001_a.sql", sql: "CREATE TABLE a (id int)"}
	// The table is created, then the second statement fails: the whole file
	// must be rolled back, table included.
	broken := migration{version: 2, name: "0002_b.sql", sql: "CREATE TABLE b (id int); SELECT 1/0"}

	applied, err := migrate(t.Context(), pool, []migration{good, broken})
	if err == nil {
		t.Fatal("migrate succeeded with a broken migration, want error")
	}
	if !slices.Equal(applied, []int64{1}) {
		t.Errorf("applied %v, want [1]", applied)
	}
	if tableExists(t, pool, "b") {
		t.Error("table b exists after its migration failed; want it rolled back")
	}
	if got := recordedVersions(t, pool); !slices.Equal(got, []int64{1}) {
		t.Errorf("schema_migrations = %v, want [1]", got)
	}

	// Fixed, it's applied on the next run, and 1 isn't applied again.
	fixed := migration{version: 2, name: "0002_b.sql", sql: "CREATE TABLE b (id int)"}
	applied, err = migrate(t.Context(), pool, []migration{good, fixed})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(applied, []int64{2}) {
		t.Errorf("after the fix, applied %v, want [2]", applied)
	}
}

func TestMigrateConcurrentRunsApplyEachMigrationOnce(t *testing.T) {
	pool := testPool(t)
	migrations := []migration{
		// No IF NOT EXISTS, so applying it twice would fail. The sleep widens
		// the window in which a second run would race without the lock.
		{version: 1, name: "0001_a.sql", sql: "SELECT pg_sleep(0.2); CREATE TABLE a (id int)"},
		{version: 2, name: "0002_b.sql", sql: "CREATE TABLE b (id int)"},
	}

	// One fewer than pgx's minimum pool size, so every run gets its own
	// connection (its own session) at once rather than queueing for one.
	const runs = 3
	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		allApplied []int64
		errs       []error
	)
	for range runs {
		wg.Go(func() {
			applied, err := migrate(t.Context(), pool, migrations)
			mu.Lock()
			defer mu.Unlock()
			allApplied = append(allApplied, applied...)
			if err != nil {
				errs = append(errs, err)
			}
		})
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("concurrent runs failed: %v", errs)
	}
	slices.Sort(allApplied)
	if !slices.Equal(allApplied, []int64{1, 2}) {
		t.Errorf("runs applied %v between them, want each version exactly once: [1 2]", allApplied)
	}
}

func TestMigrateRefusesDatabaseAheadOfBinary(t *testing.T) {
	pool := testPool(t)
	newer := []migration{
		{version: 1, name: "0001_a.sql", sql: "CREATE TABLE a (id int)"},
		{version: 2, name: "0002_b.sql", sql: "CREATE TABLE b (id int)"},
	}
	if _, err := migrate(t.Context(), pool, newer); err != nil {
		t.Fatal(err)
	}

	_, err := migrate(t.Context(), pool, newer[:1])
	if err == nil || !strings.Contains(err.Error(), "only knows up to 1") {
		t.Errorf("migrate with an older binary: error = %v, want it to refuse", err)
	}
}

func TestEmbeddedMigrationsApply(t *testing.T) {
	pool := testPool(t)
	s := &Store{pool: pool}

	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}

	// A spot check that the apps constraints work as intended.
	insert := func(name, hostname string) error {
		_, err := pool.Exec(t.Context(),
			"INSERT INTO apps (name, repo_url, hostname, port) VALUES ($1, 'https://github.com/x/y', $2, 8080)",
			name, hostname)
		return err
	}
	if err := insert("pokewordle-api", "api.example.com"); err != nil {
		t.Fatalf("inserting a valid app: %v", err)
	}
	for _, bad := range []struct{ name, hostname string }{
		{"Bad_Name", "bad.example.com"},           // name shape
		{"other", "API.example.com"},              // hostname not lowercase
		{"pokewordle-api", "other.example.com"},   // duplicate name
		{"another", "api.example.com"},            // duplicate hostname
		{"-leading-hyphen", "lead.example.com"},   // name shape
		{"trailing-hyphen-", "trail.example.com"}, // name shape
	} {
		if err := insert(bad.name, bad.hostname); err == nil {
			t.Errorf("inserting app (%q, %q) succeeded, want a constraint error", bad.name, bad.hostname)
		}
	}
}

// testPool returns a pool whose connections work in a fresh, empty schema
// that's dropped when the test ends, so tests can't see each other's tables.
// It skips the test if DATABASE_URL is unset.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		// CI is the only place these run, so a skip there would mean they
		// never run at all, while the job still shows green.
		if os.Getenv("CI") != "" {
			t.Fatal("DATABASE_URL must be set in CI")
		}
		t.Skip("DATABASE_URL not set; database tests run in CI")
	}

	admin, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatalf("connecting to DATABASE_URL: %v", err)
	}
	schema := fmt.Sprintf("test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		pool.Close()
		// t.Context() is already cancelled by the time cleanups run.
		ctx := context.Background()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("dropping test schema: %v", err)
		}
		admin.Close(ctx)
	})
	return pool
}

func recordedVersions(t *testing.T, pool *pgxpool.Pool) []int64 {
	t.Helper()
	rows, err := pool.Query(t.Context(), "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatal(err)
	}
	return versions
}

func tableExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	// to_regclass resolves the name through search_path, i.e. the test schema.
	var exists bool
	if err := pool.QueryRow(t.Context(), "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

func versionsOf(migrations []migration) []int64 {
	var versions []int64
	for _, m := range migrations {
		versions = append(versions, m.version)
	}
	return versions
}
