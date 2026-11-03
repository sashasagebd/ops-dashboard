package store

import (
	"context"
	"errors"
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
// for an app: the deployment, and the job that runs it, in one statement, so
// there's never a deployment that nothing will run. ErrDeployInProgress if
// the app already has one in progress; ErrNotFound if the app doesn't exist.
func (s *Store) CreateDeployment(ctx context.Context, appID int64, commitSHA string) (Deployment, error) {
	return createDeployment(ctx, s.pool, appID, commitSHA)
}

func createDeployment(ctx context.Context, db interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, appID int64, commitSHA string) (Deployment, error) {
	rows, _ := db.Query(ctx, `
		WITH d AS (
			INSERT INTO deployments (app_id, commit_sha) VALUES ($1, $2) RETURNING *
		), j AS (
			INSERT INTO jobs (kind, deployment_id) SELECT 'deploy', id FROM d
		)
		SELECT `+deploymentColumns+` FROM d`,
		appID, commitSHA)
	d, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Deployment])
	return d, translate(err)
}

// Push-to-deploy can't simply call CreateDeployment: a push that arrives
// while the app is deploying would be refused and lost. So a pushed commit
// either starts a deploy or waits as the app's next commit, and finishing a
// deploy starts the waiting one. Both sides lock the app's row first, so they
// take turns: a push can't see a deploy in progress, then miss it finishing
// and leave its commit waiting with nothing to start it.

// RequestDeployment queues a deploy of commitSHA for an app if it has none in
// progress (started is true), and otherwise makes commitSHA the app's next
// commit, replacing any older one, for StartNextDeployment to deploy once the
// one in progress has finished.
func (s *Store) RequestDeployment(ctx context.Context, appID int64, commitSHA string) (d Deployment, started bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockApp(ctx, tx, appID); err != nil {
			return err
		}
		// In a savepoint (a nested transaction), because a refused insert
		// would otherwise abort the whole transaction.
		err := pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error {
			var err error
			d, err = createDeployment(ctx, sp, appID, commitSHA)
			return err
		})
		switch {
		case err == nil:
			started = true
			return nil
		case errors.Is(err, ErrDeployInProgress):
			_, err := tx.Exec(ctx, "UPDATE apps SET next_commit_sha = $2 WHERE id = $1", appID, commitSHA)
			return err
		default:
			return err
		}
	})
	return d, started, translate(err)
}

// StartNextDeployment queues a deploy of the app's next commit, if it has one
// and no deploy is in progress, and clears it. ok reports whether it queued
// one. The deployer calls it whenever a deploy finishes.
func (s *Store) StartNextDeployment(ctx context.Context, appID int64) (d Deployment, ok bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var next *string
		err := tx.QueryRow(ctx, "SELECT next_commit_sha FROM apps WHERE id = $1 FOR UPDATE", appID).Scan(&next)
		if err != nil || next == nil {
			return err
		}
		err = pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error {
			var err error
			d, err = createDeployment(ctx, sp, appID, *next)
			return err
		})
		if errors.Is(err, ErrDeployInProgress) {
			return nil // still busy: it stays next, for when that one finishes
		}
		if err != nil {
			return err
		}
		ok = true
		_, err = tx.Exec(ctx, "UPDATE apps SET next_commit_sha = NULL WHERE id = $1", appID)
		return err
	})
	return d, ok, translate(err)
}

// lockApp locks an app's row until the transaction ends (ErrNotFound if
// there's no such app).
func lockApp(ctx context.Context, tx pgx.Tx, appID int64) error {
	var one int
	return translate(tx.QueryRow(ctx, "SELECT 1 FROM apps WHERE id = $1 FOR UPDATE", appID).Scan(&one))
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

// MarkBuilding moves an unfinished deployment to building. Usually it's
// queued; building or starting means an earlier attempt was cut off (the
// platform restarted, or Docker was unreachable) and its job is retrying it.
// That's the one move back a deployment can make: once finished, it stays
// finished. started_at keeps the first attempt's time.
func (s *Store) MarkBuilding(ctx context.Context, id int64) error {
	return s.transition(ctx, s.pool, id, "SET status = 'building', started_at = COALESCE(started_at, now())",
		inProgress)
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
