package jobs

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

// fakeQueue hands out its jobs in order and records what happened to them.
type fakeQueue struct {
	mu       sync.Mutex
	pending  []store.Job
	outcomes []string
	renewals int
	renewErr error
}

func (q *fakeQueue) ClaimJob(context.Context, string, time.Duration) (store.Job, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return store.Job{}, false, nil
	}
	job := q.pending[0]
	q.pending = q.pending[1:]
	return job, true, nil
}

func (q *fakeQueue) add(job store.Job) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, job)
}

func (q *fakeQueue) outcome(s string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.outcomes = append(q.outcomes, s)
	return nil
}

func (q *fakeQueue) RenewLease(context.Context, int64, string, time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.renewals++
	return q.renewErr
}

func (q *fakeQueue) CompleteJob(context.Context, int64, string) error { return q.outcome("done") }

func (q *fakeQueue) RetryJob(_ context.Context, _ int64, _ string, after time.Duration, reason string) error {
	return q.outcome("retry in " + after.String() + ": " + reason)
}

func (q *fakeQueue) FailJob(_ context.Context, _ int64, _ string, reason string) error {
	return q.outcome("failed: " + reason)
}

func (q *fakeQueue) ReleaseJob(context.Context, int64, string) error { return q.outcome("released") }

// fakeHandler runs run (if set) for each attempt and records give-ups.
type fakeHandler struct {
	mu      sync.Mutex
	run     func(ctx context.Context, job store.Job) error
	ran     int
	gaveUp  []error
	running chan int64 // receives each job's ID as it starts, if set
}

func (h *fakeHandler) Run(ctx context.Context, job store.Job) error {
	h.mu.Lock()
	h.ran++
	h.mu.Unlock()
	if h.running != nil {
		h.running <- job.ID
	}
	if h.run == nil {
		return nil
	}
	return h.run(ctx, job)
}

func (h *fakeHandler) GiveUp(_ context.Context, _ store.Job, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gaveUp = append(h.gaveUp, err)
}

func testConfig() Config {
	return Config{
		Workers:      1,
		MaxAttempts:  3,
		Lease:        30 * time.Millisecond,
		PollInterval: time.Hour, // only Wake starts work, unless a test says otherwise
		RetryDelay:   10 * time.Second,
	}
}

func TestOutcomes(t *testing.T) {
	transient := errors.New("dial tcp: connection refused")
	tests := []struct {
		name       string
		attempt    int
		err        error
		wantRuns   int
		wantResult string
		wantGiveUp error
	}{
		{name: "success", attempt: 1, wantRuns: 1, wantResult: "done"},
		{
			name: "transient error, first attempt", attempt: 1, err: transient, wantRuns: 1,
			wantResult: "retry in 10s: dial tcp: connection refused",
		},
		{
			name: "transient error, backoff doubles", attempt: 2, err: transient, wantRuns: 1,
			wantResult: "retry in 20s: dial tcp: connection refused",
		},
		{
			name: "transient error, last attempt", attempt: 3, err: transient, wantRuns: 1,
			wantResult: "failed: dial tcp: connection refused", wantGiveUp: transient,
		},
		{
			name: "permanent error fails at once", attempt: 1, err: Permanent(transient), wantRuns: 1,
			wantResult: "failed: dial tcp: connection refused", wantGiveUp: transient,
		},
		{
			// Claimed a fourth time: three attempts were cut off by the
			// platform dying, so it isn't run again.
			name: "no attempts left", attempt: 4, wantRuns: 0,
			wantResult: "failed: " + ErrNoAttemptsLeft.Error(), wantGiveUp: ErrNoAttemptsLeft,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := &fakeQueue{}
			h := &fakeHandler{run: func(context.Context, store.Job) error { return tt.err }}
			r := New(testConfig(), q, h)
			r.runJob(t.Context(), "w", store.Job{ID: 1, Attempts: tt.attempt})

			if h.ran != tt.wantRuns {
				t.Errorf("ran %d times, want %d", h.ran, tt.wantRuns)
			}
			if !slices.Equal(q.outcomes, []string{tt.wantResult}) {
				t.Errorf("outcomes = %q, want [%q]", q.outcomes, tt.wantResult)
			}
			switch {
			case tt.wantGiveUp == nil && len(h.gaveUp) > 0:
				t.Errorf("gave up with %v, want no give-up", h.gaveUp)
			case tt.wantGiveUp != nil && (len(h.gaveUp) != 1 || !errors.Is(h.gaveUp[0], tt.wantGiveUp)):
				t.Errorf("gave up with %v, want [%v]", h.gaveUp, tt.wantGiveUp)
			}
		})
	}
}

func TestShutdownHandsTheJobBack(t *testing.T) {
	q := &fakeQueue{}
	ctx, cancel := context.WithCancel(t.Context())
	h := &fakeHandler{run: func(ctx context.Context, _ store.Job) error {
		cancel() // the platform is stopped mid-build
		<-ctx.Done()
		return ctx.Err()
	}}
	New(testConfig(), q, h).runJob(ctx, "w", store.Job{ID: 1, Attempts: 1})

	if !slices.Equal(q.outcomes, []string{"released"}) || len(h.gaveUp) != 0 {
		t.Errorf("outcomes %q, gave up %v; want only released", q.outcomes, h.gaveUp)
	}
}

func TestLeaseIsRenewedWhileRunning(t *testing.T) {
	q := &fakeQueue{}
	h := &fakeHandler{run: func(context.Context, store.Job) error {
		time.Sleep(100 * time.Millisecond) // a few renewal intervals (10ms)
		return nil
	}}
	New(testConfig(), q, h).runJob(t.Context(), "w", store.Job{ID: 1, Attempts: 1})

	if q.renewals < 3 {
		t.Errorf("lease renewed %d times in 100ms, want several", q.renewals)
	}
	if !slices.Equal(q.outcomes, []string{"done"}) {
		t.Errorf("outcomes = %q, want [done]", q.outcomes)
	}
}

func TestLostLeaseStopsTheJob(t *testing.T) {
	tests := []struct {
		name     string
		renewErr error
	}{
		// Another worker claimed it after this one stalled.
		{"lease taken", store.ErrLeaseLost},
		// Postgres is unreachable: the lease can't be kept, so the job
		// must stop before another worker could claim it.
		{"database unreachable", errors.New("connection refused")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := &fakeQueue{renewErr: tt.renewErr}
			h := &fakeHandler{run: func(ctx context.Context, _ store.Job) error {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(5 * time.Second):
					return errors.New("not stopped")
				}
			}}
			New(testConfig(), q, h).runJob(t.Context(), "w", store.Job{ID: 1, Attempts: 1})

			// Nothing recorded: the job may belong to another worker now.
			if len(q.outcomes) != 0 || len(h.gaveUp) != 0 {
				t.Errorf("outcomes %q, gave up %v; want nothing recorded", q.outcomes, h.gaveUp)
			}
		})
	}
}

func TestWakeAndParallelWorkers(t *testing.T) {
	q := &fakeQueue{}
	cfg := testConfig()
	cfg.Workers = 2
	both := make(chan struct{})
	h := &fakeHandler{running: make(chan int64, 2), run: func(ctx context.Context, _ store.Job) error {
		<-both // each job waits for the other to have started
		return nil
	}}
	r := New(cfg, q, h)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	// Workers are idle until woken (the poll interval is an hour).
	q.add(store.Job{ID: 1, Attempts: 1})
	q.add(store.Job{ID: 2, Attempts: 1})
	r.Wake()
	for range 2 {
		select {
		case <-h.running:
		case <-time.After(5 * time.Second):
			t.Fatal("jobs didn't start in parallel after Wake")
		}
	}
	close(both)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't return after cancel")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if !slices.Equal(q.outcomes, []string{"done", "done"}) {
		t.Errorf("outcomes = %q, want both done", q.outcomes)
	}
}
