package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DeploymentStatus is where a deployment is in its life (see the
// 0002_create_deployments migration for the allowed moves).
type DeploymentStatus string

const (
	StatusQueued     DeploymentStatus = "queued"
	StatusBuilding   DeploymentStatus = "building"
	StatusStarting   DeploymentStatus = "starting"
	StatusHealthy    DeploymentStatus = "healthy"
	StatusFailed     DeploymentStatus = "failed"
	StatusSuperseded DeploymentStatus = "superseded"
	StatusRolledBack DeploymentStatus = "rolled_back"
)

// inProgress are the statuses of a deploy that hasn't finished yet; an app
// has at most one deployment in them (a unique index enforces it).
var inProgress = []string{string(StatusQueued), string(StatusBuilding), string(StatusStarting)}

// Deployment is one attempt to deploy a commit of an app.
type Deployment struct {
	ID         int64            `db:"id"`
	AppID      int64            `db:"app_id"`
	CommitSHA  string           `db:"commit_sha"`
	Status     DeploymentStatus `db:"status"`
	ImageTag   string           `db:"image_tag"` // "" until built
	Error      string           `db:"error"`     // "" unless failed
	CreatedAt  time.Time        `db:"created_at"`
	StartedAt  *time.Time       `db:"started_at"`
	FinishedAt *time.Time       `db:"finished_at"`
}

const deploymentColumns = `id, app_id, commit_sha, status,
	COALESCE(image_tag, '') AS image_tag, COALESCE(error, '') AS error,
	created_at, started_at, finished_at`

// CreateDeployment queues a deploy of commitSHA (40 lowercase hex characters)
// for an app. ErrDeployInProgress if the app already has one in progress;
// ErrNotFound if the app doesn't exist.
func (s *Store) CreateDeployment(ctx context.Context, appID int64, commitSHA string) (Deployment, error) {
	rows, _ := s.pool.Query(ctx,
		"INSERT INTO deployments (app_id, commit_sha) VALUES ($1, $2) RETURNING "+deploymentColumns,
		appID, commitSHA)
	d, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Deployment])
	return d, translate(err)
}

// Deployment returns one deployment, or ErrNotFound.
func (s *Store) Deployment(ctx context.Context, id int64) (Deployment, error) {
	rows, _ := s.pool.Query(ctx, "SELECT "+deploymentColumns+" FROM deployments WHERE id = $1", id)
	d, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Deployment])
	return d, translate(err)
}

// Deployments returns an app's most recent deployments, newest first.
func (s *Store) Deployments(ctx context.Context, appID int64, limit int) ([]Deployment, error) {
	rows, _ := s.pool.Query(ctx,
		"SELECT "+deploymentColumns+" FROM deployments WHERE app_id = $1 ORDER BY id DESC LIMIT $2",
		appID, limit)
	ds, err := pgx.CollectRows(rows, pgx.RowToStructByName[Deployment])
	return ds, translate(err)
}

// CurrentDeployment returns the app's healthy (live) deployment, or
// ErrNotFound if it has none.
func (s *Store) CurrentDeployment(ctx context.Context, appID int64) (Deployment, error) {
	rows, _ := s.pool.Query(ctx,
		"SELECT "+deploymentColumns+" FROM deployments WHERE app_id = $1 AND status = 'healthy'",
		appID)
	d, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Deployment])
	return d, translate(err)
}

// LiveApp is an app with a healthy deployment: one entry in what Caddy
// should route.
type LiveApp struct {
	AppName      string `db:"name"`
	Hostname     string `db:"hostname"`
	Port         int    `db:"port"`
	DeploymentID int64  `db:"deployment_id"`
}

// LiveApps returns every app that has a healthy deployment, by name.
func (s *Store) LiveApps(ctx context.Context) ([]LiveApp, error) {
	rows, _ := s.pool.Query(ctx, `
		SELECT a.name, a.hostname, a.port, d.id AS deployment_id
		FROM apps a JOIN deployments d ON d.app_id = a.id AND d.status = 'healthy'
		ORDER BY a.name`)
	live, err := pgx.CollectRows(rows, pgx.RowToStructByName[LiveApp])
	return live, translate(err)
}

// Each change below is one UPDATE that names the statuses it may start from,
// so it's atomic: if the deployment has moved on in the meantime (or never
// got there), nothing changes and the caller gets ErrWrongStatus.

// MarkBuilding moves a queued deployment to building.
func (s *Store) MarkBuilding(ctx context.Context, id int64) error {
	return s.transition(ctx, s.pool, id, "SET status = 'building', started_at = now()",
		[]string{string(StatusQueued)})
}

// MarkStarting records the built image and moves a building deployment to
// starting.
func (s *Store) MarkStarting(ctx context.Context, id int64, imageTag string) error {
	return s.transition(ctx, s.pool, id, "SET status = 'starting', image_tag = $3",
		[]string{string(StatusBuilding)}, imageTag)
}

// MarkHealthy makes a starting deployment the app's live one, and in the same
// transaction marks the previous live one superseded. Either both happen or
// neither does, so the app never has two live deployments, or none because
// of a half-finished switch.
func (s *Store) MarkHealthy(ctx context.Context, id int64) error {
	return translate(pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Supersede first: the one-healthy-per-app index would otherwise
		// reject the second healthy row.
		if _, err := tx.Exec(ctx, `
			UPDATE deployments SET status = 'superseded'
			WHERE status = 'healthy' AND id <> $1
			  AND app_id = (SELECT app_id FROM deployments WHERE id = $1)`, id); err != nil {
			return err
		}
		return s.transition(ctx, tx, id, "SET status = 'healthy', finished_at = now()",
			[]string{string(StatusStarting)})
	}))
}

// MarkFailed moves an unfinished deployment to failed, with reason (shown in
// the dashboard, so it must not contain secrets or internal detail).
func (s *Store) MarkFailed(ctx context.Context, id int64, reason string) error {
	return s.transition(ctx, s.pool, id, "SET status = 'failed', error = $3, finished_at = now()",
		inProgress, reason)
}

// FailInterruptedDeployments marks every unfinished deployment failed, and
// returns how many there were. The platform calls it at startup: in P1 a
// deploy runs in a goroutine, so one that was in progress when the platform
// stopped will never finish, and would otherwise block the app's next deploy
// forever (see deployments_one_in_progress_per_app). P2's job queue replaces
// this with retrying the deploy instead.
func (s *Store) FailInterruptedDeployments(ctx context.Context, reason string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE deployments SET status = 'failed', error = $2, finished_at = now()
		WHERE status = ANY($1)`, inProgress, reason)
	if err != nil {
		return 0, translate(err)
	}
	return tag.RowsAffected(), nil
}

// querier is what transition needs: the pool, or a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// transition applies set ("SET ...", which may use $3 onwards) to deployment
// id if its status is one of from. $1 is the id and $2 the allowed statuses.
func (s *Store) transition(ctx context.Context, db querier, id int64, set string, from []string, args ...any) error {
	tag, err := db.Exec(ctx, "UPDATE deployments "+set+" WHERE id = $1 AND status = ANY($2)",
		append([]any{id, from}, args...)...)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// Nothing changed: either there's no such deployment, or it's in
	// another status. One more query tells the caller which.
	var exists bool
	if err := db.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM deployments WHERE id = $1)", id).Scan(&exists); err != nil {
		return translate(err)
	}
	if !exists {
		return ErrNotFound
	}
	return ErrWrongStatus
}
