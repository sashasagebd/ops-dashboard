package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Stopping, starting and deleting apps. Each takes the app's row lock first,
// like everything that starts a deploy (createDeployment, RequestDeployment,
// CreateRollback), so "no deploy while stopped" and "don't stop or delete
// mid-deploy" can't race: whichever locks first wins, and the other sees
// what it did.

// StopApp marks an app stopped, returning it: no deploy or rollback can start
// until StartApp, and a pushed commit waiting to deploy is dropped.
// ErrDeployInProgress if a deploy is running (stop it once it's done).
func (s *Store) StopApp(ctx context.Context, id int64) (app App, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockApp(ctx, tx, id); err != nil {
			return err
		}
		if err := noDeployInProgress(ctx, tx, id); err != nil {
			return err
		}
		rows, _ := tx.Query(ctx, `
			UPDATE apps SET stopped = true, next_commit_sha = NULL
			WHERE id = $1 RETURNING `+appColumns, id)
		app, err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[App])
		return err
	})
	return app, translate(err)
}

// StartApp clears an app's stopped mark, returning it.
func (s *Store) StartApp(ctx context.Context, id int64) (App, error) {
	rows, _ := s.pool.Query(ctx, "UPDATE apps SET stopped = false WHERE id = $1 RETURNING "+appColumns, id)
	app, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[App])
	return app, translate(err)
}

// DeletedApp is what DeleteApp removed: the app as it was, and whether it had
// a database (whose volume, with the data, the platform leaves behind).
type DeletedApp struct {
	App         App
	HadDatabase bool
}

// DeleteApp deletes an app and, by cascade, everything the database holds for
// it: deployments and their logs, jobs, env vars, events, its database's
// record and its backups' records. Docker's side (containers, images, the
// database's network) is the deployer's to remove. ErrDeployInProgress or
// ErrBackupInProgress while either runs: their jobs would lose their rows
// halfway.
func (s *Store) DeleteApp(ctx context.Context, id int64) (deleted DeletedApp, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockApp(ctx, tx, id); err != nil {
			return err
		}
		if err := noDeployInProgress(ctx, tx, id); err != nil {
			return err
		}
		var backingUp bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM backups WHERE app_id = $1 AND status IN ('queued', 'running'))`,
			id).Scan(&backingUp); err != nil {
			return err
		}
		if backingUp {
			return ErrBackupInProgress
		}
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM app_databases WHERE app_id = $1)", id).
			Scan(&deleted.HadDatabase); err != nil {
			return err
		}
		rows, _ := tx.Query(ctx, "DELETE FROM apps WHERE id = $1 RETURNING "+appColumns, id)
		deleted.App, err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[App])
		return err
	})
	return deleted, translate(err)
}

// noDeployInProgress is ErrDeployInProgress if the app has a deployment
// queued, building or starting. Called under the app's row lock, so none can
// start before the transaction ends.
func noDeployInProgress(ctx context.Context, tx pgx.Tx, appID int64) error {
	var busy bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM deployments
		               WHERE app_id = $1 AND status IN ('queued', 'building', 'starting'))`,
		appID).Scan(&busy); err != nil {
		return err
	}
	if busy {
		return ErrDeployInProgress
	}
	return nil
}

// appStopped is ErrAppStopped if the app is stopped, ErrNotFound if it
// doesn't exist.
func appStopped(ctx context.Context, db interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, appID int64) error {
	var stopped bool
	if err := db.QueryRow(ctx, "SELECT stopped FROM apps WHERE id = $1", appID).Scan(&stopped); err != nil {
		return translate(err)
	}
	if stopped {
		return ErrAppStopped
	}
	return nil
}
