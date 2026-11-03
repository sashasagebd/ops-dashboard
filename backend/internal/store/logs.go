package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Where a deploy log line came from.
const (
	LogPlatform = "platform" // the platform's own stage messages
	LogBuild    = "build"    // Docker's build output
)

// LogLine is a stored line of a deployment's log.
type LogLine struct {
	ID     int64     `db:"id"`
	At     time.Time `db:"at"`
	Stream string    `db:"stream"`
	Line   string    `db:"line"`
}

// NewLogLine is a line to append to a deployment's log.
type NewLogLine struct {
	At     time.Time
	Stream string // LogPlatform or LogBuild
	Line   string // valid UTF-8 without NUL bytes, which Postgres text can't hold
}

// AppendDeploymentLogs adds lines to the end of a deployment's log, in order.
// ErrNotFound if the deployment doesn't exist (any more).
func (s *Store) AppendDeploymentLogs(ctx context.Context, deploymentID int64, lines []NewLogLine) error {
	if len(lines) == 0 {
		return nil
	}
	// COPY: one round trip for the whole batch, and rows get their IDs in
	// the order given.
	_, err := s.pool.CopyFrom(ctx, pgx.Identifier{"deployment_logs"},
		[]string{"deployment_id", "at", "stream", "line"},
		pgx.CopyFromSlice(len(lines), func(i int) ([]any, error) {
			l := lines[i]
			return []any{deploymentID, l.At, l.Stream, l.Line}, nil
		}))
	return translate(err)
}

// DeploymentLogs returns up to limit lines of a deployment's log after the
// line with ID afterID (0 for the start), oldest first.
func (s *Store) DeploymentLogs(ctx context.Context, deploymentID, afterID int64, limit int) ([]LogLine, error) {
	rows, _ := s.pool.Query(ctx, `
		SELECT id, at, stream, line FROM deployment_logs
		WHERE deployment_id = $1 AND id > $2
		ORDER BY id LIMIT $3`,
		deploymentID, afterID, limit)
	lines, err := pgx.CollectRows(rows, pgx.RowToStructByName[LogLine])
	return lines, translate(err)
}

// CountDeploymentLogs returns how many lines of stream a deployment's log
// has, for capping it across attempts.
func (s *Store) CountDeploymentLogs(ctx context.Context, deploymentID int64, stream string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM deployment_logs WHERE deployment_id = $1 AND stream = $2",
		deploymentID, stream).Scan(&n)
	return n, translate(err)
}

// DeploymentLogComplete reports whether a deployment's log will get no more
// lines: its job has finished for good (done or failed). Every line is
// written before the job is marked finished, so a reader that checks this
// first and then reads the remaining lines has the whole log. A deployment
// with no job (from before the job queue) is complete too.
func (s *Store) DeploymentLogComplete(ctx context.Context, deploymentID int64) (bool, error) {
	var complete bool
	err := s.pool.QueryRow(ctx, `
		SELECT NOT EXISTS (
			SELECT 1 FROM jobs WHERE deployment_id = $1 AND status IN ('queued', 'running')
		)`, deploymentID).Scan(&complete)
	return complete, translate(err)
}

// PruneDeploymentLogs deletes the logs of all but an app's keep most recent
// deployments, returning how many lines it deleted.
func (s *Store) PruneDeploymentLogs(ctx context.Context, appID int64, keep int) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM deployment_logs WHERE deployment_id IN (
			SELECT id FROM deployments WHERE app_id = $1 ORDER BY id DESC OFFSET $2
		)`, appID, keep)
	if err != nil {
		return 0, translate(err)
	}
	return tag.RowsAffected(), nil
}
