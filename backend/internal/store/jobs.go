package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Job is a claimed piece of background work (see the 0005_create_jobs
// migration). Only deploys exist so far; CreateDeployment queues one per
// deployment.
type Job struct {
	ID           int64  `db:"id"`
	Kind         string `db:"kind"`
	DeploymentID int64  `db:"deployment_id"`
	// Attempts counts claims, this one included: 1 on the first run.
	Attempts int `db:"attempts"`
}

// ClaimJob takes the oldest job that's ready to run, for worker, with a lease
// that runs out after lease unless renewed. ok is false if there's none.
//
// Ready means queued and due, or running with a lease that has run out (its
// worker died). SKIP LOCKED makes concurrent claims each take a different job
// without waiting on one another; the claim is a single statement, so a job
// is never handed to two workers.
func (s *Store) ClaimJob(ctx context.Context, worker string, lease time.Duration) (job Job, ok bool, err error) {
	rows, _ := s.pool.Query(ctx, `
		UPDATE jobs SET status = 'running', attempts = attempts + 1,
			locked_by = $1, locked_until = now() + $2 * interval '1 second'
		WHERE id = (
			SELECT id FROM jobs
			WHERE (status = 'queued' AND run_after <= now())
			   OR (status = 'running' AND locked_until < now())
			ORDER BY id
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, kind, deployment_id, attempts`,
		worker, lease.Seconds())
	job, err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Job])
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, translate(err)
	}
	return job, true, nil
}

// The changes below only apply while worker still holds the job, so a worker
// whose lease ran out (and whose job was claimed again) can't change it any
// more: they return ErrLeaseLost instead.

// RenewLease extends worker's lease on a running job to lease from now.
func (s *Store) RenewLease(ctx context.Context, id int64, worker string, lease time.Duration) error {
	return s.updateHeldJob(ctx, id, worker,
		"SET locked_until = now() + $3 * interval '1 second'", lease.Seconds())
}

// CompleteJob marks a job done.
func (s *Store) CompleteJob(ctx context.Context, id int64, worker string) error {
	return s.updateHeldJob(ctx, id, worker,
		"SET status = 'done', finished_at = now(), locked_by = NULL, locked_until = NULL")
}

// RetryJob puts a job back in the queue, to be claimed again no sooner than
// after from now, recording why this attempt failed.
func (s *Store) RetryJob(ctx context.Context, id int64, worker string, after time.Duration, reason string) error {
	return s.updateHeldJob(ctx, id, worker, `
		SET status = 'queued', run_after = now() + $3 * interval '1 second', last_error = $4,
			locked_by = NULL, locked_until = NULL`,
		after.Seconds(), reason)
}

// FailJob marks a job failed for good, recording why.
func (s *Store) FailJob(ctx context.Context, id int64, worker string, reason string) error {
	return s.updateHeldJob(ctx, id, worker, `
		SET status = 'failed', finished_at = now(), last_error = $3, locked_by = NULL, locked_until = NULL`,
		reason)
}

// ReleaseJob hands a job back unfinished, ready to be claimed straight away,
// without counting the attempt. For a platform shutting down cleanly: the
// interruption wasn't the job's fault, and releasing it means the next start
// needn't wait for the lease to run out.
func (s *Store) ReleaseJob(ctx context.Context, id int64, worker string) error {
	return s.updateHeldJob(ctx, id, worker, `
		SET status = 'queued', attempts = attempts - 1, run_after = now(), locked_by = NULL, locked_until = NULL`)
}

// updateHeldJob applies set ("SET ...", which may use $3 onwards) to job id if
// worker holds it.
func (s *Store) updateHeldJob(ctx context.Context, id int64, worker, set string, args ...any) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE jobs "+set+" WHERE id = $1 AND locked_by = $2 AND status = 'running'",
		append([]any{id, worker}, args...)...)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}
