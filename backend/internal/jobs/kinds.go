package jobs

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

// Kinds is a Handler that hands each job to the handler for its kind
// ("deploy", "backup"), so one runner, with one set of workers and one
// queue, runs every kind of job.
type Kinds map[string]Handler

// Run runs the job with its kind's handler. A kind with no handler fails at
// once: no attempt would go differently.
func (k Kinds) Run(ctx context.Context, job store.Job) error {
	h, ok := k[job.Kind]
	if !ok {
		return Permanent(fmt.Errorf("no handler for jobs of kind %q", job.Kind))
	}
	return h.Run(ctx, job)
}

// GiveUp passes the failure to the job's kind's handler.
func (k Kinds) GiveUp(ctx context.Context, job store.Job, err error) {
	h, ok := k[job.Kind]
	if !ok {
		slog.Error("job of unknown kind failed", "job", job.ID, "kind", job.Kind, "err", err)
		return
	}
	h.GiveUp(ctx, job, err)
}
