// Package jobs runs the platform's background work from its queue in Postgres
// (store.ClaimJob). A few workers claim jobs, keep renewing each job's lease
// while it runs, and record how it ended: done, retried after a delay, failed
// for good, or handed back at shutdown.
//
// Recovery from a crash needs no code here: a dead worker stops renewing, its
// lease runs out, and the job becomes claimable again (see
// docs/decisions/platform.md).
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

// Queue is the job table; in production it's *store.Store. Every method but
// ClaimJob returns store.ErrLeaseLost if worker no longer holds the job.
type Queue interface {
	ClaimJob(ctx context.Context, worker string, lease time.Duration) (store.Job, bool, error)
	RenewLease(ctx context.Context, id int64, worker string, lease time.Duration) error
	CompleteJob(ctx context.Context, id int64, worker string) error
	RetryJob(ctx context.Context, id int64, worker string, after time.Duration, reason string) error
	FailJob(ctx context.Context, id int64, worker string, reason string) error
	ReleaseJob(ctx context.Context, id int64, worker string) error
}

// Handler does the work; in production it's *deploy.Deployer.
type Handler interface {
	// Run makes one attempt at a job. It must be safe to run again after an
	// attempt that was cut off at any point. An error wrapped with Permanent
	// fails the job at once; any other error is retried.
	Run(ctx context.Context, job store.Job) error
	// GiveUp is called once a job has failed for good, with the error that
	// ended it, to record the failure where users will see it. It may be
	// called again for the same job if the platform dies just after it.
	GiveUp(ctx context.Context, job store.Job, err error)
}

// Permanent marks an error that trying again won't fix (a build that fails,
// a commit that doesn't exist), so the job fails without retrying.
func Permanent(err error) error { return &permanentError{err} }

type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// IsPermanent reports whether err, or an error it wraps, was marked
// Permanent.
func IsPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

// ErrNoAttemptsLeft is what GiveUp gets for a job that was claimed
// MaxAttempts times without any attempt finishing: each time, the platform
// stopped (or crashed) partway through.
var ErrNoAttemptsLeft = errors.New("the platform stopped during every attempt")

// errLeaseLost cancels a job whose worker can no longer be sure it holds it.
var errLeaseLost = errors.New("lease lost")

// Config holds the runner's settings.
type Config struct {
	Workers      int           // jobs run at once
	MaxAttempts  int           // attempts per job, the first included
	Lease        time.Duration // how long a claim lasts unless renewed; renewed every third of it
	PollInterval time.Duration // how often idle workers look for due jobs (retries, expired leases)
	RetryDelay   time.Duration // before the first retry, doubling for each one after
}

// Runner claims and runs jobs.
type Runner struct {
	queue   Queue
	handler Handler
	cfg     Config
	// id names this process in locked_by, so a job's holder can be traced
	// to a platform instance in the log.
	id   string
	wake chan struct{}
}

// New returns a Runner; nothing runs until Run is called.
func New(cfg Config, q Queue, h Handler) *Runner {
	host, _ := os.Hostname() // in Docker, the container's ID
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	return &Runner{
		queue: q, handler: h, cfg: cfg,
		id:   host + "-" + hex.EncodeToString(suffix),
		wake: make(chan struct{}, 1),
	}
}

// Wake tells an idle worker to look for jobs now rather than at its next
// poll; called after queueing one, so it starts straight away. It never
// blocks, and a wake with no worker idle is kept until one is.
func (r *Runner) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run starts the workers and blocks until ctx is cancelled and every job in
// flight has been handed back or finished.
func (r *Runner) Run(ctx context.Context) {
	slog.Info("job workers started", "runner", r.id, "workers", r.cfg.Workers)
	var wg sync.WaitGroup
	for i := range r.cfg.Workers {
		worker := fmt.Sprintf("%s/%d", r.id, i)
		wg.Go(func() { r.work(ctx, worker) })
	}
	wg.Wait()
}

func (r *Runner) work(ctx context.Context, worker string) {
	for ctx.Err() == nil {
		job, ok, err := r.queue.ClaimJob(ctx, worker, r.cfg.Lease)
		if err != nil && ctx.Err() == nil {
			slog.Error("claiming a job", "worker", worker, "err", err)
		}
		if ok {
			// There may be more waiting: pass the wake on to another idle
			// worker, so one Wake can start several jobs.
			r.Wake()
			r.runJob(ctx, worker, job)
			continue
		}
		select {
		case <-ctx.Done():
		case <-r.wake:
		case <-time.After(r.cfg.PollInterval):
		}
	}
}

// runJob runs one claimed job and records how it ended.
func (r *Runner) runJob(ctx context.Context, worker string, job store.Job) {
	log := slog.With("job", job.ID, "kind", job.Kind, "deployment", job.DeploymentID,
		"attempt", job.Attempts, "worker", worker)
	// Outcomes are recorded even once ctx is cancelled for shutdown: that's
	// when ReleaseJob matters most.
	record := context.WithoutCancel(ctx)

	if job.Attempts > r.cfg.MaxAttempts {
		log.Error("job failed: no attempts left")
		r.giveUp(record, log, worker, job, ErrNoAttemptsLeft)
		return
	}
	log.Info("job started")

	jobCtx, cancel := context.WithCancelCause(ctx)
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		r.keepLease(jobCtx, cancel, log, worker, job.ID)
	}()
	err := r.handler.Run(jobCtx, job)
	// Read before cancel(nil) below, which would otherwise be the cause.
	leaseLost := errors.Is(context.Cause(jobCtx), errLeaseLost)
	cancel(nil)
	<-renewed

	var recordErr error
	switch {
	case leaseLost:
		// Another worker may hold the job now; anything recorded here
		// could overwrite its outcome, and the store would refuse it
		// anyway.
		log.Error("job abandoned: its lease was lost", "err", err)
		return
	case err == nil:
		log.Info("job done")
		recordErr = r.queue.CompleteJob(record, job.ID, worker)
	case ctx.Err() != nil:
		log.Info("job handed back: shutting down", "err", err)
		recordErr = r.queue.ReleaseJob(record, job.ID, worker)
	case IsPermanent(err) || job.Attempts >= r.cfg.MaxAttempts:
		log.Error("job failed", "err", err)
		r.giveUp(record, log, worker, job, err)
		return
	default:
		after := r.cfg.RetryDelay << (job.Attempts - 1)
		log.Warn("job attempt failed; retrying", "err", err, "retryIn", after)
		recordErr = r.queue.RetryJob(record, job.ID, worker, after, err.Error())
	}
	if recordErr != nil {
		// The lease will run out and the job will be claimed again, so
		// nothing is lost, only delayed.
		log.Error("recording the job's outcome", "err", recordErr)
	}
}

// giveUp records a job's failure: first where users see it (the handler),
// then on the job, so that if the platform dies in between, the job is
// claimed again and gives up again rather than leaving the failure unshown.
func (r *Runner) giveUp(ctx context.Context, log *slog.Logger, worker string, job store.Job, err error) {
	r.handler.GiveUp(ctx, job, err)
	if err := r.queue.FailJob(ctx, job.ID, worker, err.Error()); err != nil {
		log.Error("recording the job's failure", "err", err)
	}
}

// keepLease renews a job's lease until ctx is done. If the lease turns out to
// be lost, or can't be renewed before it runs out (Postgres unreachable), it
// cancels the job: past that point another worker may claim it, and two
// workers must never run the same job.
//
// "Runs out" is judged by this process's clock against Postgres's now(),
// which is fine while both are on the same host.
func (r *Runner) keepLease(ctx context.Context, cancel context.CancelCauseFunc, log *slog.Logger, worker string, id int64) {
	every := r.cfg.Lease / 3
	expires := time.Now().Add(r.cfg.Lease)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
		start := time.Now()
		renewCtx, cancelRenew := context.WithTimeout(ctx, every)
		err := r.queue.RenewLease(renewCtx, id, worker, r.cfg.Lease)
		cancelRenew()
		switch {
		case err == nil:
			expires = start.Add(r.cfg.Lease)
		case ctx.Err() != nil:
			return
		case errors.Is(err, store.ErrLeaseLost):
			cancel(errLeaseLost)
			return
		default:
			log.Warn("renewing the job's lease", "err", err)
			// Stop while the lease still holds, not once it has run
			// out: the next renewal would come too late.
			if time.Now().Add(every).After(expires) {
				cancel(errLeaseLost)
				return
			}
		}
	}
}
