package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/engine"
	"github.com/sashasagebd/ops-dashboard/backend/internal/jobs"
	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

// fakes is the store and Docker at once. The fake "container" writes the
// dump into dir, as the real one writes into the bind-mounted volume.
type fakes struct {
	dir      string
	backups  map[int64]*store.Backup
	hasDB    bool
	due      []int64
	created  []int64 // apps CreateBackup was called for
	inFlight bool    // CreateBackup finds one in progress

	exitCode   int
	writeDump  bool
	waitErr    error
	leftover   bool // a container from an earlier attempt exists
	actions    []string
	spec       engine.TaskSpec
	tailOutput []engine.LogLine
}

func newFakes(t *testing.T) *fakes {
	return &fakes{
		dir:       t.TempDir(),
		backups:   map[int64]*store.Backup{3: {ID: 3, AppID: 1, Status: store.BackupQueued}},
		hasDB:     true,
		writeDump: true,
	}
}

func (f *fakes) App(_ context.Context, id int64) (store.App, error) {
	return store.App{ID: id, Name: "hello"}, nil
}

func (f *fakes) Database(context.Context, int64) (store.Database, error) {
	if !f.hasDB {
		return store.Database{}, store.ErrNotFound
	}
	return store.Database{AppID: 1, Password: "pw"}, nil
}

func (f *fakes) Backup(_ context.Context, id int64) (store.Backup, error) {
	b, ok := f.backups[id]
	if !ok {
		return store.Backup{}, store.ErrNotFound
	}
	return *b, nil
}

func (f *fakes) CreateBackup(_ context.Context, appID int64) (store.Backup, error) {
	if f.inFlight {
		return store.Backup{}, store.ErrBackupInProgress
	}
	f.created = append(f.created, appID)
	return store.Backup{ID: 9, AppID: appID, Status: store.BackupQueued}, nil
}

func (f *fakes) DueBackups(context.Context, time.Duration) ([]int64, error) { return f.due, nil }

func (f *fakes) StartBackup(_ context.Context, id int64) error {
	f.backups[id].Status = store.BackupRunning
	return nil
}

func (f *fakes) FinishBackup(_ context.Context, id int64, file string, size int64) error {
	b := f.backups[id]
	b.Status, b.File, b.SizeBytes = store.BackupDone, file, size
	return nil
}

func (f *fakes) FailBackup(_ context.Context, id int64, reason string) error {
	b := f.backups[id]
	if b.Status == store.BackupDone {
		return store.ErrWrongStatus
	}
	b.Status, b.Error = store.BackupFailed, reason
	return nil
}

func (f *fakes) ManagedContainers(context.Context, string) ([]engine.ManagedContainer, error) {
	if f.leftover {
		return []engine.ManagedContainer{{ID: "old", Name: "ops-backup-hello-3"}, {ID: "app", Name: "app-hello-7"}}, nil
	}
	return nil, nil
}

func (f *fakes) Pull(_ context.Context, ref string) error {
	f.actions = append(f.actions, "pull "+ref)
	return nil
}

func (f *fakes) RunTask(_ context.Context, spec engine.TaskSpec) (string, error) {
	f.actions = append(f.actions, "run "+spec.Name)
	f.spec = spec
	return "task", nil
}

func (f *fakes) Wait(ctx context.Context, id string) (int, error) {
	f.actions = append(f.actions, "wait "+id)
	if f.waitErr != nil {
		return 0, f.waitErr
	}
	if f.exitCode == 0 && f.writeDump {
		dir := filepath.Join(f.dir, f.spec.Env["APP"])
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, f.spec.Env["FILE"]), []byte("PGDMP dump"), 0o644)
	}
	return f.exitCode, nil
}

func (f *fakes) TailLogs(context.Context, string, int) ([]engine.LogLine, error) {
	return f.tailOutput, nil
}

func (f *fakes) Remove(_ context.Context, id string) error {
	f.actions = append(f.actions, "remove "+id)
	return nil
}

func (f *fakes) backuper() *Backuper {
	b := New(Config{Volume: "platform-backups", Dir: f.dir, User: "1000:1000", Keep: 7,
		Every: 24 * time.Hour, CheckEvery: time.Minute, Timeout: time.Minute}, f, f)
	b.now = func() time.Time { return time.Date(2026, 11, 4, 12, 30, 5, 0, time.UTC) }
	return b
}

var job = store.Job{ID: 1, Kind: "backup", BackupID: 3, Attempts: 1}

func TestBackupRuns(t *testing.T) {
	f := newFakes(t)
	if err := f.backuper().Run(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	want := []string{"pull postgres:18.6", "run ops-backup-hello-3", "wait task", "remove task"}
	if !slices.Equal(f.actions, want) {
		t.Errorf("actions = %q, want %q", f.actions, want)
	}
	b := f.backups[3]
	if b.Status != store.BackupDone || b.File != "hello-20261104-123005.dump" || b.SizeBytes != int64(len("PGDMP dump")) {
		t.Errorf("backup = %+v", b)
	}
	// The container: the app's database network, as the owner, into the
	// backups volume, with its inputs as env vars.
	s := f.spec
	if s.Image != "postgres:18.6" || s.Network != "ops-db-hello" || s.User != "1000:1000" ||
		s.Volume != "platform-backups" || s.VolumeTarget != "/backups" ||
		s.Env["PGHOST"] != "ops-db-hello" || s.Env["PGPASSWORD"] != "pw" || s.Env["KEEP"] != "7" ||
		s.Labels[engine.LabelRole] != engine.RoleBackup || s.Labels[engine.LabelApp] != "hello" {
		t.Errorf("task spec = %+v", s)
	}
	// Inputs never spliced into the script.
	if strings.Contains(s.Cmd[2], "hello") || strings.Contains(s.Cmd[2], "pw") {
		t.Errorf("script %q contains an input", s.Cmd[2])
	}
}

func TestBackupRetryRemovesTheEarlierAttemptsContainer(t *testing.T) {
	f := newFakes(t)
	f.leftover = true
	if err := f.backuper().Run(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	if f.actions[0] != "remove old" || slices.Contains(f.actions, "remove app") {
		t.Errorf("actions = %q, want only the earlier backup container removed first", f.actions)
	}
}

func TestBackupFailures(t *testing.T) {
	tests := []struct {
		name          string
		setup         func(f *fakes)
		wantPermanent bool
		wantReason    string
	}{
		{
			name: "pg_dump fails: retried, with its words",
			setup: func(f *fakes) {
				f.exitCode = 1
				f.tailOutput = []engine.LogLine{{Line: "pg_dump: error: connection to server failed"}}
			},
			wantReason: "pg_dump failed: pg_dump: error: connection to server failed",
		},
		{
			name:       "the dump isn't where the platform looks",
			setup:      func(f *fakes) { f.writeDump = false },
			wantReason: "internal error; see the platform's logs",
		},
		{
			name:          "the app's database is gone",
			setup:         func(f *fakes) { f.hasDB = false },
			wantPermanent: true,
			wantReason:    "the app has no database to back up yet",
		},
		{
			name:          "the backup is gone (its app deleted)",
			setup:         func(f *fakes) { delete(f.backups, 3) },
			wantPermanent: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakes(t)
			tt.setup(f)
			b := f.backuper()
			err := b.Run(t.Context(), job)
			if err == nil || jobs.IsPermanent(err) != tt.wantPermanent {
				t.Fatalf("Run error = %v, want an error, permanent %v", err, tt.wantPermanent)
			}
			if tt.wantReason == "" {
				return
			}
			b.GiveUp(t.Context(), job, err)
			if got := f.backups[3]; got.Status != store.BackupFailed || got.Error != tt.wantReason {
				t.Errorf("backup = %+v, want failed with %q", got, tt.wantReason)
			}
			// The container never outlives the attempt.
			if slices.Contains(f.actions, "run ops-backup-hello-3") && !slices.Contains(f.actions, "remove task") {
				t.Errorf("actions = %q: the container wasn't removed", f.actions)
			}
		})
	}
}

func TestShutdownDuringABackup(t *testing.T) {
	f := newFakes(t)
	f.waitErr = context.Canceled
	err := f.backuper().Run(t.Context(), job)
	if !errors.Is(err, context.Canceled) || jobs.IsPermanent(err) {
		t.Errorf("Run error = %v, want a non-permanent cancellation (released, run again later)", err)
	}
	if !slices.Contains(f.actions, "remove task") {
		t.Errorf("actions = %q: the half-done dump's container must go", f.actions)
	}
}

func TestAFinishedBackupIsntRunAgain(t *testing.T) {
	f := newFakes(t)
	f.backups[3].Status = store.BackupDone
	if err := f.backuper().Run(t.Context(), job); err != nil || len(f.actions) != 0 {
		t.Errorf("Run = %v, actions %q; want nothing done", err, f.actions)
	}
}

func TestScheduleAndStart(t *testing.T) {
	f := newFakes(t)
	f.due = []int64{1, 2}
	b := f.backuper()
	woken := 0
	b.Notify = func() { woken++ }
	n, err := b.Schedule(t.Context())
	if err != nil || n != 2 || !slices.Equal(f.created, []int64{1, 2}) || woken != 1 {
		t.Errorf("Schedule = %d, %v; created %v, woken %d", n, err, f.created, woken)
	}
	// "Back up now" racing a scheduled one is fine for the scheduler...
	f.inFlight = true
	if n, err := b.Schedule(t.Context()); err != nil || n != 0 {
		t.Errorf("Schedule with one in progress = %d, %v; want 0, nil", n, err)
	}
	// ...and an error for "Back up now".
	if _, err := b.Start(t.Context(), 1); !errors.Is(err, store.ErrBackupInProgress) {
		t.Errorf("Start = %v, want ErrBackupInProgress", err)
	}
}
