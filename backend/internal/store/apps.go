package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// App is a registered app: where its code is and how to run it.
type App struct {
	ID                 int64     `db:"id"`
	Name               string    `db:"name"`
	RepoURL            string    `db:"repo_url"`
	Branch             string    `db:"branch"`
	Hostname           string    `db:"hostname"`
	Port               int       `db:"port"`
	HealthCheckPath    string    `db:"health_check_path"`
	MemoryLimitBytes   int64     `db:"memory_limit_bytes"`
	CPULimitMillicores int       `db:"cpu_limit_millicores"`
	CreatedAt          time.Time `db:"created_at"`
}

const appColumns = `id, name, repo_url, branch, hostname, port, health_check_path,
	memory_limit_bytes, cpu_limit_millicores, created_at`

// NewApp is what CreateApp needs. Name, RepoURL, Hostname and Port are
// required; for the rest, the zero value means "use the database's default",
// so the defaults (branch main, path /, 256 MiB, half a core) are defined
// once, in the migration.
type NewApp struct {
	Name     string
	RepoURL  string
	Hostname string
	Port     int

	Branch             string
	HealthCheckPath    string
	MemoryLimitBytes   int64
	CPULimitMillicores int
}

// CreateApp registers an app. A taken name or hostname is ErrConflict.
func (s *Store) CreateApp(ctx context.Context, a NewApp) (App, error) {
	cols := []string{"name", "repo_url", "hostname", "port"}
	args := []any{a.Name, a.RepoURL, a.Hostname, a.Port}
	optional := func(col string, v any, set bool) {
		if set {
			cols = append(cols, col)
			args = append(args, v)
		}
	}
	optional("branch", a.Branch, a.Branch != "")
	optional("health_check_path", a.HealthCheckPath, a.HealthCheckPath != "")
	optional("memory_limit_bytes", a.MemoryLimitBytes, a.MemoryLimitBytes != 0)
	optional("cpu_limit_millicores", a.CPULimitMillicores, a.CPULimitMillicores != 0)

	placeholders := make([]string, len(cols))
	for i := range cols {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	// Only the column names are built into the SQL, and they're the
	// constants above; every value goes in as a parameter.
	q := fmt.Sprintf("INSERT INTO apps (%s) VALUES (%s) RETURNING %s",
		strings.Join(cols, ", "), strings.Join(placeholders, ", "), appColumns)

	rows, _ := s.pool.Query(ctx, q, args...)
	app, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[App])
	return app, translate(err)
}

// App returns one app, or ErrNotFound.
func (s *Store) App(ctx context.Context, id int64) (App, error) {
	rows, _ := s.pool.Query(ctx, "SELECT "+appColumns+" FROM apps WHERE id = $1", id)
	app, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[App])
	return app, translate(err)
}

// Apps returns every app, by name.
func (s *Store) Apps(ctx context.Context) ([]App, error) {
	rows, _ := s.pool.Query(ctx, "SELECT "+appColumns+" FROM apps ORDER BY name")
	apps, err := pgx.CollectRows(rows, pgx.RowToStructByName[App])
	return apps, translate(err)
}
