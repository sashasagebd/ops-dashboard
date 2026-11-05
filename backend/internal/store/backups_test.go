package store

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// provisionedDB gives an app a database the reconciler has created.
func provisionedDB(t *testing.T, s *Store, appID int64) {
	t.Helper()
	if _, err := s.CreateDatabase(t.Context(), appID); err != nil {
		t.Fatal(err)
	}
	mustDo(t, s.MarkDatabaseProvisioned(t.Context(), appID))
}

func TestBackupLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	hello := createTestApp(t, s, "hello")

	// No database, or one not created yet: nothing to back up.
	if _, err := s.CreateBackup(ctx, hello.ID); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("no database: error = %v, want ErrNoDatabase", err)
	}
	if _, err := s.CreateDatabase(ctx, hello.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBackup(ctx, hello.ID); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("database not created yet: error = %v, want ErrNoDatabase", err)
	}
	mustDo(t, s.MarkDatabaseProvisioned(ctx, hello.ID))

	b, err := s.CreateBackup(ctx, hello.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != BackupQueued || b.AppID != hello.ID || b.File != "" {
		t.Errorf("new backup = %+v", b)
	}
	// Its job, a backup job.
	job, ok, err := s.ClaimJob(ctx, "w1", time.Minute)
	if err != nil || !ok || job.Kind != "backup" || job.BackupID != b.ID || job.DeploymentID != 0 {
		t.Fatalf("claimed %+v, %v, %v; want the backup's job", job, ok, err)
	}

	// One in progress per app.
	if _, err := s.CreateBackup(ctx, hello.ID); !errors.Is(err, ErrBackupInProgress) {
		t.Errorf("second backup: error = %v, want ErrBackupInProgress", err)
	}

	mustDo(t, s.StartBackup(ctx, b.ID))
	mustDo(t, s.StartBackup(ctx, b.ID)) // a retry
	if err := s.FinishBackup(ctx, b.ID, "../escape.dump", 10); !errors.Is(err, ErrInvalid) {
		t.Errorf("a file name outside the pattern: error = %v, want ErrInvalid", err)
	}
	mustDo(t, s.FinishBackup(ctx, b.ID, "hello-20261104-120000.dump", 4096))
	got, _ := s.Backup(ctx, b.ID)
	if got.Status != BackupDone || got.File != "hello-20261104-120000.dump" || got.SizeBytes != 4096 ||
		got.StartedAt == nil || got.FinishedAt == nil {
		t.Errorf("done backup = %+v", got)
	}
	// Finished stays finished.
	if err := s.FailBackup(ctx, b.ID, "x"); !errors.Is(err, ErrWrongStatus) {
		t.Errorf("failing a done backup: error = %v, want ErrWrongStatus", err)
	}
	if err := s.StartBackup(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing backup: error = %v, want ErrNotFound", err)
	}

	// The next can start, and fail.
	b2, err := s.CreateBackup(ctx, hello.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustDo(t, s.FailBackup(ctx, b2.ID, "pg_dump failed"))
	list, _ := s.Backups(ctx, hello.ID, 10)
	if len(list) != 2 || list[0].ID != b2.ID || list[0].Error != "pg_dump failed" {
		t.Errorf("backups = %+v, want newest first", list)
	}
}

func TestDueBackups(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	fresh := createTestApp(t, s, "fresh") // database, no backup yet: due
	recent := createTestApp(t, s, "recent")
	old := createTestApp(t, s, "old")
	busy := createTestApp(t, s, "busy")
	createTestApp(t, s, "nodb") // never due
	notYet := createTestApp(t, s, "notyet")
	for _, a := range []App{fresh, recent, old, busy} {
		provisionedDB(t, s, a.ID)
	}
	if _, err := s.CreateDatabase(ctx, notYet.ID); err != nil { // not created by the reconciler yet
		t.Fatal(err)
	}

	r, _ := s.CreateBackup(ctx, recent.ID)
	mustDo(t, s.FailBackup(ctx, r.ID, "x")) // even a failed one counts
	o, _ := s.CreateBackup(ctx, old.ID)
	mustDo(t, s.FailBackup(ctx, o.ID, "x"))
	if _, err := s.pool.Exec(ctx, "UPDATE backups SET created_at = now() - interval '25 hours' WHERE id = $1", o.ID); err != nil {
		t.Fatal(err)
	}
	b, _ := s.CreateBackup(ctx, busy.ID) // in progress, and old
	if _, err := s.pool.Exec(ctx, "UPDATE backups SET created_at = now() - interval '25 hours' WHERE id = $1", b.ID); err != nil {
		t.Fatal(err)
	}

	due, err := s.DueBackups(ctx, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(due, []int64{fresh.ID, old.ID}) {
		t.Errorf("due = %v, want fresh (%d) and old (%d)", due, fresh.ID, old.ID)
	}
}

func TestJobsTakeExactlyTheirOwnInput(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	app := createTestApp(t, s, "hello")
	d, _ := s.CreateDeployment(ctx, app.ID, sha1)
	// A deploy job with a backup, or a backup job without one, is refused.
	if _, err := s.pool.Exec(ctx, "INSERT INTO jobs (kind) VALUES ('backup')"); err == nil {
		t.Error("inserted a backup job with no backup")
	}
	if _, err := s.pool.Exec(ctx, "INSERT INTO jobs (kind, deployment_id) VALUES ('backup', $1)", d.ID); err == nil {
		t.Error("inserted a backup job pointing at a deployment")
	}
}
