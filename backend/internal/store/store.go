// Package store is the platform's access to Postgres, the source of truth for
// apps, deployments and jobs. The schema lives in migrations/, embedded in the
// binary and applied by Migrate on startup (see migrate.go).
package store

import (
	"context"
	"embed"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Store holds the platform's connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// Open prepares a pool for databaseURL. It doesn't connect: the first
// connection is made on first use (e.g. Ping), so Open succeeds even while
// Postgres is still starting.
//
// Pool settings can go in the URL (e.g. pool_max_conns=4); pgx's default is
// the greater of 4 and the number of CPUs, which is 4 on the server.
func Open(databaseURL string) (*Store, error) {
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
	return &Store{pool: pool}, nil
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
