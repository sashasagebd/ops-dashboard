package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

// Limits on deploy logs, which are stored (see the 0006 migration).
const (
	// maxBuildLines caps one deployment's build output, across attempts.
	// A large npm or Go build prints hundreds of lines; past this, it's
	// runaway output, and the stage messages after it matter more.
	maxBuildLines = 5000
	// keepLogsFor is how many of an app's most recent deployments keep
	// their logs.
	keepLogsFor = 10
	// logFlushInterval is how often buffered lines are written: one insert
	// per batch rather than per line, at the cost of up to this much delay
	// in the live view.
	logFlushInterval = 500 * time.Millisecond
)

// deployLog is one attempt's log: the platform's stage messages and Docker's
// build output, buffered and written to the database in batches.
//
// Logging is best effort: if a batch can't be written, it's dropped (and
// noted in the platform's own log) rather than failing a deploy that is
// otherwise fine.
type deployLog struct {
	store        Store
	deploymentID int64

	mu         sync.Mutex
	pending    []store.NewLogLine
	buildLines int // already stored or pending, this attempt and earlier ones

	stop chan struct{}
	done chan struct{}
}

// openLog starts the log for an attempt at a deployment; Close it when the
// attempt ends.
func (d *Deployer) openLog(deploymentID int64) *deployLog {
	l := &deployLog{
		store: d.store, deploymentID: deploymentID,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	// Earlier attempts' build output counts towards the cap.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if n, err := d.store.CountDeploymentLogs(ctx, deploymentID, store.LogBuild); err == nil {
		l.buildLines = n
	}
	go l.flushEvery(logFlushInterval)
	return l
}

// Printf adds one of the platform's own messages.
func (l *deployLog) Printf(format string, args ...any) {
	l.add(store.LogPlatform, fmt.Sprintf(format, args...))
}

// Build adds a line of Docker's build output, already cleaned up by the
// engine, up to maxBuildLines.
func (l *deployLog) Build(line string) {
	l.mu.Lock()
	n := l.buildLines
	if n <= maxBuildLines {
		l.buildLines++
	}
	l.mu.Unlock()
	switch {
	case n < maxBuildLines:
		l.add(store.LogBuild, line)
	case n == maxBuildLines:
		l.add(store.LogPlatform, fmt.Sprintf("Build output cut off after %d lines; the build carries on.", maxBuildLines))
	}
}

func (l *deployLog) add(stream, line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending = append(l.pending, store.NewLogLine{At: time.Now(), Stream: stream, Line: line})
}

func (l *deployLog) flushEvery(interval time.Duration) {
	defer close(l.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			l.flush()
		}
	}
}

// flush writes what's pending. Only one flush runs at a time (the ticker's,
// then Close's), so batches are stored in order.
func (l *deployLog) flush() {
	l.mu.Lock()
	batch := l.pending
	l.pending = nil
	l.mu.Unlock()
	// Not the attempt's context: the last lines of an attempt cut off by a
	// shutdown or timeout are the ones most worth keeping.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := l.store.AppendDeploymentLogs(ctx, l.deploymentID, batch)
	if err != nil && !errors.Is(err, store.ErrNotFound) { // not found: deleted with its app
		slog.Warn("writing deploy log lines; dropped", "deployment", l.deploymentID, "lines", len(batch), "err", err)
	}
}

// Close writes the remaining lines. Nothing may be added after it.
func (l *deployLog) Close() {
	close(l.stop)
	<-l.done
	l.flush()
}

// appendLogLine writes one platform message to a deployment's log straight
// away, for messages outside an attempt (GiveUp).
func (d *Deployer) appendLogLine(ctx context.Context, deploymentID int64, line string) {
	err := d.store.AppendDeploymentLogs(ctx, deploymentID, []store.NewLogLine{
		{At: time.Now(), Stream: store.LogPlatform, Line: line},
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Warn("writing a deploy log line", "deployment", deploymentID, "err", err)
	}
}
