package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// BackupStatus is where a backup is (see the 0011_create_backups migration).
type BackupStatus string

const (
	BackupQueued  BackupStatus = "queued"
	BackupRunning BackupStatus = "running"
	BackupDone    BackupStatus = "done"
	BackupFailed  BackupStatus = "failed"
)

// Backup is one backup of an app's database.
type Backup struct {
	ID         int64        `db:"id"`
	AppID      int64        `db:"app_id"`
	Status     BackupStatus `db:"status"`
	File       string       `db:"file"`       // the dump's name in the app's folder; "" until done
	SizeBytes  int64        `db:"size_bytes"` // 0 until done
	Error      string       `db:"error"`      // "" unless failed
	CreatedAt  time.Time    `db:"created_at"`
	StartedAt  *time.Time   `db:"started_at"`
	FinishedAt *time.Time   `db:"finished_at"`
}

const backupColumns = `id, app_id, status, COALESCE(file, '') AS file,
	COALESCE(size_bytes, 0) AS size_bytes, COALESCE(error, '') AS error,
	created_at, started_at, finished_at`

// CreateBackup queues a backup of an app's database: the backup and the job
// that runs it, in one statement. ErrNoDatabase if the app has no database,
// or it hasn't been created yet; ErrBackupInProgress if one is queued or
// running already.
func (s *Store) CreateBackup(ctx context.Context, appID int64) (Backup, error) {
	rows, _ := s.pool.Query(ctx, `
		WITH b AS (
			INSERT INTO backups (app_id)
			SELECT app_id FROM app_databases WHERE app_id = $1 AND provisioned_at IS NOT NULL
			RETURNING *
		), j AS (
			INSERT INTO jobs (kind, backup_id) SELECT 'backup', id FROM b
		)
		SELECT `+backupColumns+` FROM b`, appID)
	b, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Backup])
	if errors.Is(err, pgx.ErrNoRows) {
		return Backup{}, ErrNoDatabase
	}
	return b, translate(err)
}

// Backup returns one backup, or ErrNotFound.
func (s *Store) Backup(ctx context.Context, id int64) (Backup, error) {
	rows, _ := s.pool.Query(ctx, "SELECT "+backupColumns+" FROM backups WHERE id = $1", id)
	b, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Backup])
	return b, translate(err)
}

// Backups returns an app's most recent backups, newest first.
func (s *Store) Backups(ctx context.Context, appID int64, limit int) ([]Backup, error) {
	rows, _ := s.pool.Query(ctx,
		"SELECT "+backupColumns+" FROM backups WHERE app_id = $1 ORDER BY id DESC LIMIT $2", appID, limit)
	bs, err := pgx.CollectRows(rows, pgx.RowToStructByName[Backup])
	return bs, translate(err)
}

// DueBackups returns the apps whose database is due a scheduled backup: it
// has been created, and the app has had no backup (successful or not) for
// every, and has none in progress. A failed one counts, so a backup that
// keeps failing is tried once per period, not every few minutes (each try
// has its own retries).
func (s *Store) DueBackups(ctx context.Context, every time.Duration) ([]int64, error) {
	rows, _ := s.pool.Query(ctx, `
		SELECT d.app_id FROM app_databases d
		WHERE d.provisioned_at IS NOT NULL
		  AND NOT EXISTS (
			SELECT 1 FROM backups b
			WHERE b.app_id = d.app_id
			  AND (b.created_at > now() - $1 * interval '1 second' OR b.status IN ('queued', 'running'))
		  )
		ORDER BY d.app_id`, every.Seconds())
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	return ids, translate(err)
}

// As with deployments, each change is one UPDATE naming the statuses it may
// start from (ErrWrongStatus otherwise), so a backup's status only moves
// forward.

// StartBackup moves a backup to running. Running already means an earlier
// attempt was cut off and its job is retrying it. started_at keeps the first
// attempt's time.
func (s *Store) StartBackup(ctx context.Context, id int64) error {
	return s.backupTransition(ctx, id, "SET status = 'running', started_at = COALESCE(started_at, now())",
		[]string{string(BackupQueued), string(BackupRunning)})
}

// FinishBackup records a running backup's dump.
func (s *Store) FinishBackup(ctx context.Context, id int64, file string, size int64) error {
	return s.backupTransition(ctx, id, "SET status = 'done', file = $3, size_bytes = $4, finished_at = now()",
		[]string{string(BackupRunning)}, file, size)
}

// FailBackup records why an unfinished backup failed (shown in the
// dashboard: no secrets or internal detail).
func (s *Store) FailBackup(ctx context.Context, id int64, reason string) error {
	return s.backupTransition(ctx, id, "SET status = 'failed', error = $3, finished_at = now()",
		[]string{string(BackupQueued), string(BackupRunning)}, reason)
}

func (s *Store) backupTransition(ctx context.Context, id int64, set string, from []string, args ...any) error {
	tag, err := s.pool.Exec(ctx, "UPDATE backups "+set+" WHERE id = $1 AND status = ANY($2)",
		append([]any{id, from}, args...)...)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM backups WHERE id = $1)", id).Scan(&exists); err != nil {
		return translate(err)
	}
	if !exists {
		return ErrNotFound
	}
	return ErrWrongStatus
}
