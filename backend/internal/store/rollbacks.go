package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// A rollback is a new deployment that runs an earlier one's image, with no
// build (see docs/decisions/platform.md). Which images exist to go back to is
// decided here too: the platform keeps an app's ImagesKept most recent live
// versions and removes the rest (internal/deploy), so only those can be
// rolled back to.

// ImagesKept is how many images the platform keeps per app: the live
// deployment's, and those of the five most recent previously live ones.
const ImagesKept = 6

// KeptImages returns the image tags the platform keeps for an app, newest
// first: the ImagesKept most recent distinct images of deployments that were
// live at some point (healthy now, superseded or rolled back). The first is
// the live deployment's, if the app has one.
func (s *Store) KeptImages(ctx context.Context, appID int64) ([]string, error) {
	return keptImages(ctx, s.pool, appID)
}

func keptImages(ctx context.Context, db interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, appID int64) ([]string, error) {
	// Distinct images, not deployments: redeploying a commit, or rolling
	// back to it, reuses its tag.
	rows, _ := db.Query(ctx, `
		SELECT image_tag FROM deployments
		WHERE app_id = $1 AND image_tag IS NOT NULL
		  AND status IN ('healthy', 'superseded', 'rolled_back')
		GROUP BY image_tag ORDER BY max(id) DESC LIMIT $2`,
		appID, ImagesKept)
	tags, err := pgx.CollectRows(rows, pgx.RowTo[string])
	return tags, translate(err)
}

// CheckRollback says whether an app can be rolled back to deployment d, given
// the app's kept images (KeptImages) and its live deployment's image ("" if
// nothing is live): nil if it can, otherwise ErrNotRollbackable with the
// reason. It doesn't check whether a deploy is in progress.
func CheckRollback(d Deployment, kept []string, liveImage string) error {
	switch {
	case d.Status != StatusSuperseded && d.Status != StatusRolledBack:
		return fmt.Errorf("%w: only a version that was live before can be rolled back to", ErrNotRollbackable)
	case d.ImageTag == "" || !slices.Contains(kept, d.ImageTag):
		return fmt.Errorf("%w: its image is no longer kept (only the last %d live versions are); deploy its commit instead",
			ErrNotRollbackable, ImagesKept-1)
	case d.ImageTag == liveImage:
		return fmt.Errorf("%w: it's the same version as the live one", ErrNotRollbackable)
	}
	return nil
}

// CreateRollback queues a rollback of an app to its deployment targetID: a
// new deployment (and its job) that runs the target's image. In the same
// transaction it turns the app's auto-deploy off and drops a pushed commit
// waiting to deploy, so neither undoes the rollback as soon as it finishes.
//
// ErrNotFound if the app has no such deployment; ErrNotRollbackable (with the
// reason) if it can't be rolled back to; ErrDeployInProgress if a deploy is
// in progress, in which case nothing changes.
func (s *Store) CreateRollback(ctx context.Context, appID, targetID int64) (d Deployment, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// The app's row is locked first, as for pushes: nothing can change
		// its deployments' statuses (StartNextDeployment) or its
		// auto-deploy setting while this decides.
		if err := lockApp(ctx, tx, appID); err != nil {
			return err
		}
		rows, _ := tx.Query(ctx,
			"SELECT "+deploymentColumns+" FROM deployments WHERE id = $1 AND app_id = $2", targetID, appID)
		target, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Deployment])
		if err != nil {
			return translate(err)
		}
		kept, err := keptImages(ctx, tx, appID)
		if err != nil {
			return err
		}
		var live string
		err = tx.QueryRow(ctx, `
			SELECT COALESCE(image_tag, '') FROM deployments WHERE app_id = $1 AND status = 'healthy'`,
			appID).Scan(&live)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return translate(err)
		}
		if err := CheckRollback(target, kept, live); err != nil {
			return err
		}

		// A deploy in progress makes this fail (the one-in-progress index),
		// which rolls the whole transaction back.
		rows, _ = tx.Query(ctx, `
			WITH d AS (
				INSERT INTO deployments (app_id, commit_sha, image_tag, rollback_of)
				VALUES ($1, $2, $3, $4) RETURNING *
			), j AS (
				INSERT INTO jobs (kind, deployment_id) SELECT 'deploy', id FROM d
			)
			SELECT `+deploymentColumns+` FROM d`,
			appID, target.CommitSHA, target.ImageTag, target.ID)
		d, err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Deployment])
		if err != nil {
			return translate(err)
		}
		_, err = tx.Exec(ctx, "UPDATE apps SET auto_deploy = false, next_commit_sha = NULL WHERE id = $1", appID)
		return translate(err)
	})
	return d, translate(err)
}

// StartRollback moves a rollback to starting: it has no building stage, since
// its image already exists. From queued, or from starting when an earlier
// attempt was cut off. started_at keeps the first attempt's time.
func (s *Store) StartRollback(ctx context.Context, id int64) error {
	return s.transition(ctx, s.pool, id,
		"SET status = 'starting', started_at = COALESCE(started_at, now())",
		[]string{string(StatusQueued), string(StatusStarting)})
}
