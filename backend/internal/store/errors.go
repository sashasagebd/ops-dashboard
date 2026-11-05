package store

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Errors the store's callers can act on (e.g. the API turning ErrNotFound
// into a 404). Many come from database constraints, so the rules hold even if
// a caller forgets to check first. Messages are safe to show to the
// dashboard's user: they never include SQL or internal details.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrInvalid  = errors.New("invalid")
	// ErrDeployInProgress: the app already has a deployment queued, building
	// or starting (the deployments_one_in_progress_per_app index).
	ErrDeployInProgress = errors.New("a deploy of this app is already in progress")
	// ErrAppStopped: the app is stopped, so nothing can be deployed to it
	// (or rolled back) until it's started again.
	ErrAppStopped = errors.New("the app is stopped; start it first")
	// ErrWrongStatus: the deployment isn't in a status that allows the
	// change, e.g. marking a failed deployment healthy.
	ErrWrongStatus = errors.New("deployment is not in a status that allows this")
	// ErrLeaseLost: the worker no longer holds the job (its lease ran out
	// and another worker claimed it, or the job was deleted with its app).
	ErrLeaseLost = errors.New("job is no longer held by this worker")
	// ErrNotRollbackable: the app can't be rolled back to that deployment
	// (it was never live, its image is gone, or it's the live version);
	// wrapped with which (CheckRollback).
	ErrNotRollbackable = errors.New("can't roll back to this deployment")
	// ErrNoDatabase: a backup was asked for an app without a database (or
	// whose database hasn't been created yet).
	ErrNoDatabase = errors.New("the app has no database to back up yet")
	// ErrBackupInProgress: the app already has a backup queued or running
	// (the backups_one_in_progress_per_app index).
	ErrBackupInProgress = errors.New("a backup of this app is already in progress")
)

// Postgres error codes (https://www.postgresql.org/docs/current/errcodes-appendix.html).
const (
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
	checkViolation      = "23514"
)

// translate maps Postgres errors onto the errors above, by constraint name.
// Anything else is returned as is, for the caller to log.
func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case uniqueViolation:
		switch pgErr.ConstraintName {
		case "apps_name_key":
			return fmt.Errorf("%w: an app with that name already exists", ErrConflict)
		case "apps_hostname_key":
			return fmt.Errorf("%w: an app with that hostname already exists", ErrConflict)
		case "apps_public_name_key":
			return fmt.Errorf("%w: another app already has that public name", ErrConflict)
		case "deployments_one_in_progress_per_app":
			return ErrDeployInProgress
		case "backups_one_in_progress_per_app":
			return ErrBackupInProgress
		case "app_databases_pkey":
			return fmt.Errorf("%w: the app already has a database", ErrConflict)
		}
		return ErrConflict
	case foreignKeyViolation:
		if pgErr.ConstraintName == "deployments_rollback_of_fkey" {
			return fmt.Errorf("deployment %w", ErrNotFound)
		}
		// The other foreign keys point at apps.
		return fmt.Errorf("app %w", ErrNotFound)
	case checkViolation:
		// The API validates input first with friendlier messages; reaching
		// this means it missed a case, and the constraint name says which.
		return fmt.Errorf("%w: %s", ErrInvalid, pgErr.ConstraintName)
	}
	return err
}
