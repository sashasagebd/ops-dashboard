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
	SourceDir          string    `db:"source_dir"`      // "" is the repo's root
	AutoDeploy         bool      `db:"auto_deploy"`     // pushes to Branch deploy it
	NextCommitSHA      string    `db:"next_commit_sha"` // a pushed commit waiting for the deploy in progress; "" if none
	PublicName         string    `db:"public_name"`     // served publicly at <PublicName>.<PUBLIC_DOMAIN>; "" if tailnet-only
	Stopped            bool      `db:"stopped"`         // stopped on purpose: no container, no routes, no deploys
	CreatedAt          time.Time `db:"created_at"`
}

const appColumns = `id, name, repo_url, branch, hostname, port, health_check_path,
	memory_limit_bytes, cpu_limit_millicores, source_dir, auto_deploy,
	COALESCE(next_commit_sha, '') AS next_commit_sha,
	COALESCE(public_name, '') AS public_name, stopped, created_at`

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
	SourceDir          string
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
	optional("source_dir", a.SourceDir, a.SourceDir != "")

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

// SetPublicName makes an app public at <name>.<PUBLIC_DOMAIN>, or
// tailnet-only again with "", returning the updated app. A name another app
// has is ErrConflict.
func (s *Store) SetPublicName(ctx context.Context, id int64, name string) (App, error) {
	rows, _ := s.pool.Query(ctx, `
		UPDATE apps SET public_name = NULLIF($2, '')
		WHERE id = $1 RETURNING `+appColumns, id, name)
	app, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[App])
	return app, translate(err)
}

// SetAutoDeploy turns push-to-deploy on or off for an app, returning the
// updated app. Turning it off also drops a pushed commit waiting to deploy.
func (s *Store) SetAutoDeploy(ctx context.Context, id int64, on bool) (App, error) {
	rows, _ := s.pool.Query(ctx, `
		UPDATE apps SET auto_deploy = $2,
			next_commit_sha = CASE WHEN $2 THEN next_commit_sha END
		WHERE id = $1 RETURNING `+appColumns, id, on)
	app, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[App])
	return app, translate(err)
}
