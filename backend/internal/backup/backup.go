// Package backup backs up apps' databases: a pg_dump of each, daily and on
// demand, as files in the backup directory (BACKUP_DIR, ~/backups on the
// server).
//
// Each backup is a job in the platform's queue (internal/jobs), so it's
// retried and survives the platform restarting; the Backuper is its handler.
// The dump runs in a short-lived postgres container on the app's database
// network, as the owner's user, writing into a volume compose defines as a
// bind of the backup directory: the platform itself never names a host path.
// The platform also mounts that volume, read-only, to check each dump landed.
package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/deploy"
	"github.com/sashasagebd/ops-dashboard/backend/internal/engine"
	"github.com/sashasagebd/ops-dashboard/backend/internal/jobs"
	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

// The interfaces below are what backups need; in production they're
// *store.Store and *engine.Client.

type Store interface {
	App(ctx context.Context, id int64) (store.App, error)
	Database(ctx context.Context, appID int64) (store.Database, error)
	Backup(ctx context.Context, id int64) (store.Backup, error)
	CreateBackup(ctx context.Context, appID int64) (store.Backup, error)
	DueBackups(ctx context.Context, every time.Duration) ([]int64, error)
	StartBackup(ctx context.Context, id int64) error
	FinishBackup(ctx context.Context, id int64, file string, size int64) error
	FailBackup(ctx context.Context, id int64, reason string) error
}

type Engine interface {
	ManagedContainers(ctx context.Context, app string) ([]engine.ManagedContainer, error)
	Pull(ctx context.Context, ref string) error
	RunTask(ctx context.Context, spec engine.TaskSpec) (string, error)
	Wait(ctx context.Context, id string) (int, error)
	TailLogs(ctx context.Context, id string, n int) ([]engine.LogLine, error)
	Remove(ctx context.Context, id string) error
}

// Config holds the backup settings.
type Config struct {
	Volume     string        // the Docker volume backups are written to (compose's platform-backups)
	Dir        string        // where the platform sees that volume, read-only (/backups)
	User       string        // "uid:gid" the dump runs as, so the files are the owner's
	Keep       int           // dumps kept per app
	Every      time.Duration // how often each database is backed up
	CheckEvery time.Duration // how often the scheduler looks for databases due one
	Timeout    time.Duration // one attempt at a backup
}

// Backuper queues backups and runs them as jobs.
type Backuper struct {
	cfg    Config
	store  Store
	engine Engine
	now    func() time.Time

	// Notify, if set, is called after a backup is queued, so a worker starts
	// it straight away (the job runner's Wake).
	Notify func()
}

// New returns a Backuper.
func New(cfg Config, st Store, eng Engine) *Backuper {
	return &Backuper{cfg: cfg, store: st, engine: eng, now: time.Now}
}

// Start queues a backup of an app's database now ("Back up now"). Errors as
// store.CreateBackup's: ErrNoDatabase, ErrBackupInProgress.
func (b *Backuper) Start(ctx context.Context, appID int64) (store.Backup, error) {
	bk, err := b.store.CreateBackup(ctx, appID)
	if err != nil {
		return store.Backup{}, err
	}
	b.notify()
	return bk, nil
}

func (b *Backuper) notify() {
	if b.Notify != nil {
		b.Notify()
	}
}

// RunScheduler queues the backups that are due, every CheckEvery, until ctx
// is done.
func (b *Backuper) RunScheduler(ctx context.Context) {
	t := time.NewTicker(b.cfg.CheckEvery)
	defer t.Stop()
	for {
		if _, err := b.Schedule(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("scheduling backups", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Schedule queues a backup for every database due one (no backup for
// Every), returning how many it queued.
func (b *Backuper) Schedule(ctx context.Context) (int, error) {
	due, err := b.store.DueBackups(ctx, b.cfg.Every)
	if err != nil {
		return 0, err
	}
	queued := 0
	for _, appID := range due {
		_, err := b.store.CreateBackup(ctx, appID)
		switch {
		case errors.Is(err, store.ErrBackupInProgress), errors.Is(err, store.ErrNoDatabase):
			// "Back up now" got there first, or the app went: fine.
		case err != nil:
			return queued, err
		default:
			queued++
			slog.Info("scheduled a backup", "app", appID)
		}
	}
	if queued > 0 {
		b.notify()
	}
	return queued, nil
}

// ContainerName is a backup's container, e.g. ops-backup-hello-12.
func ContainerName(app string, backupID int64) string {
	return "ops-backup-" + app + "-" + strconv.FormatInt(backupID, 10)
}

// dumpScript is what the backup container runs. Its inputs come as env vars
// (app names are only lowercase letters, digits and hyphens anyway), never
// spliced into the script. It dumps to a hidden partial file and renames it
// when complete, so a dump cut off halfway never looks like a backup; then it
// keeps the newest KEEP dumps. Only one backup of an app runs at a time (a
// database rule), so partial files left by an earlier, cut-off attempt are
// safe to clear first.
const dumpScript = `set -eu
dir="/backups/$APP"
mkdir -p "$dir"
rm -f "$dir"/.*.partial
pg_dump --format=custom --file="$dir/.$FILE.partial"
mv "$dir/.$FILE.partial" "$dir/$FILE"
ls -1t "$dir"/*.dump | tail -n +$((KEEP + 1)) | xargs -r rm -f --
`

// DumpError is a pg_dump that ran and failed; its message is pg_dump's last
// words, which are about the user's own database and safe to show.
type DumpError struct {
	ExitCode int
	Message  string
}

func (e *DumpError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("pg_dump exited with code %d", e.ExitCode)
	}
	return fmt.Sprintf("pg_dump failed: %s", e.Message)
}

// Run makes one attempt at a backup job (jobs.Handler). A failed dump is
// retried, as the database may just have been restarting; a backup whose app
// or database is gone fails at once.
func (b *Backuper) Run(ctx context.Context, job store.Job) error {
	bk, err := b.store.Backup(ctx, job.BackupID)
	if errors.Is(err, store.ErrNotFound) {
		return jobs.Permanent(err) // deleted with its app
	}
	if err != nil {
		return err
	}
	if bk.Status == store.BackupDone || bk.Status == store.BackupFailed {
		return nil // finished just before the platform died
	}
	app, err := b.store.App(ctx, bk.AppID)
	if err != nil {
		return permanentIfGone(err)
	}
	db, err := b.store.Database(ctx, app.ID)
	if errors.Is(err, store.ErrNotFound) {
		return jobs.Permanent(store.ErrNoDatabase)
	}
	if err != nil {
		return err
	}
	if err := b.store.StartBackup(ctx, bk.ID); err != nil {
		return permanentIfGone(err)
	}
	log := slog.With("app", app.Name, "backup", bk.ID, "attempt", job.Attempts)

	attemptCtx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()
	name := ContainerName(app.Name, bk.ID)
	// An earlier attempt's container, if it was cut off, has this name.
	if err := b.removeContainer(attemptCtx, app.Name, name); err != nil {
		return err
	}
	if err := b.engine.Pull(attemptCtx, deploy.DatabaseImage); err != nil {
		return err
	}
	file := fmt.Sprintf("%s-%s.dump", app.Name, b.now().UTC().Format("20060102-150405"))
	id, err := b.engine.RunTask(attemptCtx, engine.TaskSpec{
		Name:  name,
		Image: deploy.DatabaseImage,
		Cmd:   []string{"sh", "-c", dumpScript},
		Env: map[string]string{
			"PGHOST":     deploy.DatabaseName(app.Name),
			"PGUSER":     deploy.DatabaseUser,
			"PGDATABASE": deploy.DatabaseDB,
			"PGPASSWORD": db.Password,
			"APP":        app.Name,
			"FILE":       file,
			"KEEP":       strconv.Itoa(b.cfg.Keep),
		},
		Labels: map[string]string{
			engine.LabelApp:   app.Name,
			engine.LabelAppID: strconv.FormatInt(app.ID, 10),
			engine.LabelRole:  engine.RoleBackup,
		},
		Network:      deploy.DatabaseName(app.Name),
		User:         b.cfg.User,
		Volume:       b.cfg.Volume,
		VolumeTarget: "/backups",
		MemoryBytes:  256 << 20,
	})
	if err != nil {
		return err
	}
	// Whatever happens next, the container goes: it holds the password in
	// its env, and it has done its one job.
	defer func() {
		if err := b.engine.Remove(context.WithoutCancel(ctx), id); err != nil {
			log.Error("removing a backup container", "err", err)
		}
	}()
	code, err := b.engine.Wait(attemptCtx, id)
	if err != nil {
		return err
	}
	if code != 0 {
		dumpErr := &DumpError{ExitCode: code}
		if lines, err := b.engine.TailLogs(context.WithoutCancel(ctx), id, 5); err == nil && len(lines) > 0 {
			dumpErr.Message = lines[len(lines)-1].Line
		}
		log.Warn("backup failed", "err", dumpErr)
		return dumpErr
	}

	// The dump should now be in the backup directory, which the platform
	// sees too: its size is the proof.
	info, err := os.Stat(filepath.Join(b.cfg.Dir, app.Name, file))
	if err != nil {
		return fmt.Errorf("the dump isn't in the backup directory (is BACKUP_DIR the platform's /backups too?): %w", err)
	}
	if err := b.store.FinishBackup(ctx, bk.ID, file, info.Size()); err != nil {
		return permanentIfGone(err)
	}
	log.Info("backup done", "file", file, "bytes", info.Size())
	return nil
}

// GiveUp records why a backup failed for good (jobs.Handler).
func (b *Backuper) GiveUp(ctx context.Context, job store.Job, err error) {
	reason := publicReason(err)
	if err := b.store.FailBackup(ctx, job.BackupID, reason); err != nil &&
		!errors.Is(err, store.ErrWrongStatus) && !errors.Is(err, store.ErrNotFound) {
		slog.Error("recording a failed backup", "backup", job.BackupID, "err", err)
	}
}

// removeContainer removes app's container called name, if there is one.
func (b *Backuper) removeContainer(ctx context.Context, app, name string) error {
	containers, err := b.engine.ManagedContainers(ctx, app)
	if err != nil {
		return err
	}
	for _, c := range containers {
		if c.Name == name {
			return b.engine.Remove(ctx, c.ID)
		}
	}
	return nil
}

func permanentIfGone(err error) error {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrWrongStatus) {
		return jobs.Permanent(err)
	}
	return err
}

// publicReason is the reason the dashboard shows: pg_dump's own words for a
// dump that failed, generic otherwise.
func publicReason(err error) string {
	var dumpErr *DumpError
	switch {
	case errors.As(err, &dumpErr):
		return strings.TrimSpace(dumpErr.Error())
	case errors.Is(err, store.ErrNoDatabase):
		return store.ErrNoDatabase.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "the backup timed out"
	case errors.Is(err, jobs.ErrNoAttemptsLeft):
		return "gave up: " + jobs.ErrNoAttemptsLeft.Error()
	}
	return "internal error; see the platform's logs"
}
