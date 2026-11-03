package store

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/secrets"
)

// Like the migration tests, these need a real Postgres (DATABASE_URL), since
// most of what they check is enforced by the database itself.

// testStore returns a Store on a fresh, migrated schema.
func testStore(t *testing.T) *Store {
	t.Helper()
	box, err := secrets.New(bytes.Repeat([]byte{7}, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{pool: testPool(t), box: box}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return s
}

func createTestApp(t *testing.T, s *Store, name string) App {
	t.Helper()
	app, err := s.CreateApp(t.Context(), NewApp{
		Name:     name,
		RepoURL:  "https://github.com/example/" + name,
		Hostname: name + ".example.test",
		Port:     8080,
	})
	if err != nil {
		t.Fatalf("CreateApp(%s): %v", name, err)
	}
	return app
}

const (
	sha1 = "1111111111111111111111111111111111111111"
	sha2 = "2222222222222222222222222222222222222222"
	sha3 = "3333333333333333333333333333333333333333"
)

func TestCreateAppDefaultsAndOverrides(t *testing.T) {
	s := testStore(t)

	app := createTestApp(t, s, "hello")
	if app.ID == 0 || app.CreatedAt.IsZero() {
		t.Errorf("app missing ID or CreatedAt: %+v", app)
	}
	if app.Branch != "main" || app.HealthCheckPath != "/" ||
		app.MemoryLimitBytes != 256<<20 || app.CPULimitMillicores != 500 || app.SourceDir != "" {
		t.Errorf("defaults = branch %q, path %q, memory %d, cpu %d, dir %q; want main, /, 268435456, 500, \"\"",
			app.Branch, app.HealthCheckPath, app.MemoryLimitBytes, app.CPULimitMillicores, app.SourceDir)
	}

	custom, err := s.CreateApp(t.Context(), NewApp{
		Name: "custom", RepoURL: "https://github.com/example/custom", Hostname: "custom.example.test", Port: 3000,
		Branch: "release", HealthCheckPath: "/healthz", MemoryLimitBytes: 64 << 20, CPULimitMillicores: 250,
		SourceDir: "services/web",
	})
	if err != nil {
		t.Fatal(err)
	}
	if custom.Branch != "release" || custom.HealthCheckPath != "/healthz" ||
		custom.MemoryLimitBytes != 64<<20 || custom.CPULimitMillicores != 250 || custom.SourceDir != "services/web" {
		t.Errorf("overrides not kept: %+v", custom)
	}

	got, err := s.App(t.Context(), app.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Times compared with Equal: the same instant can carry a different
	// location or monotonic reading.
	gotTime, wantTime := got.CreatedAt, app.CreatedAt
	got.CreatedAt, app.CreatedAt = wantTime, wantTime
	if got != app || !gotTime.Equal(wantTime) {
		t.Errorf("App(%d) = %+v, want %+v", app.ID, got, app)
	}
	apps, err := s.Apps(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 || apps[0].Name != "custom" || apps[1].Name != "hello" {
		t.Errorf("Apps = %v, want custom then hello (by name)", apps)
	}
}

func TestAppErrors(t *testing.T) {
	s := testStore(t)
	createTestApp(t, s, "hello")

	tests := []struct {
		name string
		app  NewApp
		want error
	}{
		{"name taken", NewApp{Name: "hello", RepoURL: "r", Hostname: "other.example.test", Port: 1}, ErrConflict},
		{"hostname taken", NewApp{Name: "other", RepoURL: "r", Hostname: "hello.example.test", Port: 1}, ErrConflict},
		{"bad name", NewApp{Name: "Hello", RepoURL: "r", Hostname: "x.example.test", Port: 1}, ErrInvalid},
		{"bad port", NewApp{Name: "x", RepoURL: "r", Hostname: "x.example.test", Port: 70000}, ErrInvalid},
		// The source directory can only select a folder inside the repo.
		{"dir with ..", NewApp{Name: "x", RepoURL: "r", Hostname: "x.example.test", Port: 1, SourceDir: "a/../.."}, ErrInvalid},
		{"dir is ..", NewApp{Name: "x", RepoURL: "r", Hostname: "x.example.test", Port: 1, SourceDir: ".."}, ErrInvalid},
		{"absolute dir", NewApp{Name: "x", RepoURL: "r", Hostname: "x.example.test", Port: 1, SourceDir: "/etc"}, ErrInvalid},
		{"dir with trailing slash", NewApp{Name: "x", RepoURL: "r", Hostname: "x.example.test", Port: 1, SourceDir: "web/"}, ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.CreateApp(t.Context(), tt.app); !errors.Is(err, tt.want) {
				t.Errorf("CreateApp error = %v, want %v", err, tt.want)
			}
		})
	}

	if _, err := s.App(t.Context(), 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("App(missing) error = %v, want ErrNotFound", err)
	}
}

func TestEnvVars(t *testing.T) {
	s := testStore(t)
	app := createTestApp(t, s, "hello")
	ctx := t.Context()

	for _, kv := range [][2]string{{"TOKEN", "first"}, {"API_URL", "https://x"}, {"TOKEN", "second"}} {
		if err := s.SetEnvVar(ctx, app.ID, kv[0], kv[1]); err != nil {
			t.Fatalf("SetEnvVar(%s): %v", kv[0], err)
		}
	}

	keys, err := s.EnvVarKeys(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(keys, []string{"API_URL", "TOKEN"}) {
		t.Errorf("EnvVarKeys = %v, want [API_URL TOKEN]", keys)
	}
	vars, err := s.EnvVars(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(vars) != 2 || vars["TOKEN"] != "second" || vars["API_URL"] != "https://x" {
		t.Errorf("EnvVars = %v, want TOKEN=second (overwritten) and API_URL", vars)
	}

	// Stored encrypted: the plaintext appears nowhere in the table.
	var stored []byte
	if err := s.pool.QueryRow(ctx, "SELECT encrypted_value FROM app_env_vars WHERE key = 'TOKEN'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte("second")) {
		t.Error("TOKEN's value is stored in plaintext")
	}

	if err := s.DeleteEnvVar(ctx, app.ID, "API_URL"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteEnvVar(ctx, app.ID, "API_URL"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting it again: error = %v, want ErrNotFound", err)
	}
	if err := s.SetEnvVar(ctx, app.ID, "BAD-NAME", "x"); !errors.Is(err, ErrInvalid) {
		t.Errorf("SetEnvVar(BAD-NAME) error = %v, want ErrInvalid", err)
	}
	if err := s.SetEnvVar(ctx, 999999, "TOKEN", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetEnvVar(missing app) error = %v, want ErrNotFound", err)
	}
}

func TestEnvVarCopiedToAnotherAppDoesNotDecrypt(t *testing.T) {
	s := testStore(t)
	victim := createTestApp(t, s, "victim")
	attacker := createTestApp(t, s, "attacker")
	ctx := t.Context()

	if err := s.SetEnvVar(ctx, victim.ID, "TOKEN", "victims-secret"); err != nil {
		t.Fatal(err)
	}
	// Someone with write access to the database (but not the key) copies the
	// victim's encrypted value into another app's row.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO app_env_vars (app_id, key, encrypted_value)
		SELECT $1, key, encrypted_value FROM app_env_vars WHERE app_id = $2`,
		attacker.ID, victim.ID); err != nil {
		t.Fatal(err)
	}

	vars, err := s.EnvVars(ctx, attacker.ID)
	if !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("EnvVars(attacker) = %v, %v; want ErrDecrypt", vars, err)
	}
	if strings.Contains(err.Error(), "victims-secret") {
		t.Errorf("error %q contains the value", err)
	}
}

func TestDeploymentLifecycle(t *testing.T) {
	s := testStore(t)
	app := createTestApp(t, s, "hello")
	ctx := t.Context()

	// The first deploy goes all the way through.
	first, err := s.CreateDeployment(ctx, app.ID, sha1)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != StatusQueued || first.StartedAt != nil {
		t.Errorf("new deployment = %+v, want queued and not started", first)
	}
	mustDo(t, s.MarkBuilding(ctx, first.ID))
	mustDo(t, s.MarkStarting(ctx, first.ID, "hello:"+sha1))
	mustDo(t, s.MarkHealthy(ctx, first.ID))

	current, err := s.CurrentDeployment(ctx, app.ID)
	if err != nil || current.ID != first.ID || current.ImageTag != "hello:"+sha1 ||
		current.StartedAt == nil || current.FinishedAt == nil {
		t.Fatalf("CurrentDeployment = %+v, %v; want the first, with image and timestamps", current, err)
	}

	// While a second is in progress, a third is refused.
	second, err := s.CreateDeployment(ctx, app.ID, sha2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment(ctx, app.ID, sha3); !errors.Is(err, ErrDeployInProgress) {
		t.Errorf("CreateDeployment while one is in progress: error = %v, want ErrDeployInProgress", err)
	}

	// The second becoming healthy supersedes the first.
	mustDo(t, s.MarkBuilding(ctx, second.ID))
	mustDo(t, s.MarkStarting(ctx, second.ID, "hello:"+sha2))
	mustDo(t, s.MarkHealthy(ctx, second.ID))
	assertStatus(t, s, first.ID, StatusSuperseded)
	assertStatus(t, s, second.ID, StatusHealthy)

	// Only the live deployment is routed; an app with none isn't listed.
	createTestApp(t, s, "never-deployed")
	live, err := s.LiveApps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].AppName != "hello" || live[0].DeploymentID != second.ID ||
		live[0].Hostname != "hello.example.test" || live[0].Port != 8080 {
		t.Errorf("LiveApps = %+v, want only hello's second deployment", live)
	}

	// A failed third leaves the second live, and frees the app for a fourth.
	third, err := s.CreateDeployment(ctx, app.ID, sha3)
	if err != nil {
		t.Fatal(err)
	}
	mustDo(t, s.MarkBuilding(ctx, third.ID))
	mustDo(t, s.MarkFailed(ctx, third.ID, "build failed: no Dockerfile"))
	got, _ := s.Deployment(ctx, third.ID)
	if got.Status != StatusFailed || got.Error != "build failed: no Dockerfile" || got.FinishedAt == nil {
		t.Errorf("failed deployment = %+v", got)
	}
	assertStatus(t, s, second.ID, StatusHealthy)
	if _, err := s.CreateDeployment(ctx, app.ID, sha3); err != nil {
		t.Errorf("CreateDeployment after a failure: %v", err)
	}

	list, err := s.Deployments(ctx, app.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 || list[0].ID < list[3].ID {
		t.Errorf("Deployments returned %d, want 4, newest first", len(list))
	}
}

func TestDeploymentStatusOnlyMovesForward(t *testing.T) {
	s := testStore(t)
	app := createTestApp(t, s, "hello")
	ctx := t.Context()
	d, err := s.CreateDeployment(ctx, app.ID, sha1)
	if err != nil {
		t.Fatal(err)
	}

	// Skipping stages is refused, and leaves the deployment as it was.
	if err := s.MarkHealthy(ctx, d.ID); !errors.Is(err, ErrWrongStatus) {
		t.Errorf("MarkHealthy(queued) error = %v, want ErrWrongStatus", err)
	}
	if err := s.MarkStarting(ctx, d.ID, "img"); !errors.Is(err, ErrWrongStatus) {
		t.Errorf("MarkStarting(queued) error = %v, want ErrWrongStatus", err)
	}
	assertStatus(t, s, d.ID, StatusQueued)

	// Nothing comes back from failed.
	mustDo(t, s.MarkFailed(ctx, d.ID, "cancelled"))
	if err := s.MarkBuilding(ctx, d.ID); !errors.Is(err, ErrWrongStatus) {
		t.Errorf("MarkBuilding(failed) error = %v, want ErrWrongStatus", err)
	}
	if err := s.MarkFailed(ctx, d.ID, "again"); !errors.Is(err, ErrWrongStatus) {
		t.Errorf("MarkFailed(failed) error = %v, want ErrWrongStatus", err)
	}

	if err := s.MarkBuilding(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkBuilding(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := s.CreateDeployment(ctx, app.ID, "main"); !errors.Is(err, ErrInvalid) {
		t.Errorf("CreateDeployment with a branch name: error = %v, want ErrInvalid", err)
	}
	if _, err := s.CreateDeployment(ctx, 999999, sha1); !errors.Is(err, ErrNotFound) {
		t.Errorf("CreateDeployment(missing app) error = %v, want ErrNotFound", err)
	}
}

func TestRetriedDeploymentCanBuildAgain(t *testing.T) {
	s := testStore(t)
	app := createTestApp(t, s, "hello")
	ctx := t.Context()
	d, _ := s.CreateDeployment(ctx, app.ID, sha1)
	mustDo(t, s.MarkBuilding(ctx, d.ID))
	mustDo(t, s.MarkStarting(ctx, d.ID, "img"))
	first, _ := s.Deployment(ctx, d.ID)

	// An attempt cut off while starting is retried from the build.
	mustDo(t, s.MarkBuilding(ctx, d.ID))
	again, _ := s.Deployment(ctx, d.ID)
	if again.Status != StatusBuilding || !again.StartedAt.Equal(*first.StartedAt) {
		t.Errorf("after retry: %+v; want building, started_at kept from the first attempt", again)
	}
}

func TestCreateDeploymentQueuesItsJob(t *testing.T) {
	s := testStore(t)
	app := createTestApp(t, s, "hello")
	ctx := t.Context()

	if _, ok, err := s.ClaimJob(ctx, "w1", time.Minute); err != nil || ok {
		t.Fatalf("ClaimJob on an empty queue = %v, %v; want nothing", ok, err)
	}
	d, err := s.CreateDeployment(ctx, app.ID, sha1)
	if err != nil {
		t.Fatal(err)
	}
	job := mustClaim(t, s, "w1")
	if job.Kind != "deploy" || job.DeploymentID != d.ID || job.Attempts != 1 {
		t.Errorf("claimed %+v; want the deployment's deploy job, attempt 1", job)
	}
	// Held, with a live lease: nobody else gets it.
	if _, ok, _ := s.ClaimJob(ctx, "w2", time.Minute); ok {
		t.Error("a held job was claimed by a second worker")
	}

	// A refused deployment queues no job either.
	if _, err := s.CreateDeployment(ctx, app.ID, sha2); !errors.Is(err, ErrDeployInProgress) {
		t.Fatalf("second CreateDeployment error = %v, want ErrDeployInProgress", err)
	}
	var jobs int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM jobs").Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Errorf("%d jobs, want 1", jobs)
	}
}

func TestJobLifecycle(t *testing.T) {
	s := testStore(t)
	app := createTestApp(t, s, "hello")
	ctx := t.Context()
	s.CreateDeployment(ctx, app.ID, sha1)
	job := mustClaim(t, s, "w1")

	// Only the holder can change it.
	for name, err := range map[string]error{
		"renew":    s.RenewLease(ctx, job.ID, "w2", time.Minute),
		"complete": s.CompleteJob(ctx, job.ID, "w2"),
		"retry":    s.RetryJob(ctx, job.ID, "w2", 0, "x"),
		"fail":     s.FailJob(ctx, job.ID, "w2", "x"),
		"release":  s.ReleaseJob(ctx, job.ID, "w2"),
	} {
		if !errors.Is(err, ErrLeaseLost) {
			t.Errorf("%s by another worker: error = %v, want ErrLeaseLost", name, err)
		}
	}
	mustDo(t, s.RenewLease(ctx, job.ID, "w1", time.Minute))

	// A retry waits out its backoff before it can be claimed again.
	mustDo(t, s.RetryJob(ctx, job.ID, "w1", time.Hour, "docker unreachable"))
	if _, ok, _ := s.ClaimJob(ctx, "w1", time.Minute); ok {
		t.Error("claimed a job still backing off")
	}
	if _, err := s.pool.Exec(ctx, "UPDATE jobs SET run_after = now()"); err != nil {
		t.Fatal(err)
	}
	if job = mustClaim(t, s, "w2"); job.Attempts != 2 {
		t.Errorf("retried job's attempts = %d, want 2", job.Attempts)
	}

	// Released at shutdown: claimable straight away, attempt not counted.
	mustDo(t, s.ReleaseJob(ctx, job.ID, "w2"))
	if job = mustClaim(t, s, "w1"); job.Attempts != 2 {
		t.Errorf("released job's attempts = %d, want 2 (the release isn't counted)", job.Attempts)
	}

	mustDo(t, s.CompleteJob(ctx, job.ID, "w1"))
	if _, ok, _ := s.ClaimJob(ctx, "w1", time.Minute); ok {
		t.Error("claimed a finished job")
	}
	if err := s.CompleteJob(ctx, job.ID, "w1"); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("completing it twice: error = %v, want ErrLeaseLost", err)
	}
}

func TestExpiredLeaseIsClaimedAgain(t *testing.T) {
	s := testStore(t)
	app := createTestApp(t, s, "hello")
	ctx := t.Context()
	s.CreateDeployment(ctx, app.ID, sha1)
	first := mustClaim(t, s, "w1")

	// w1 dies: it stops renewing, and its lease runs out.
	if _, err := s.pool.Exec(ctx, "UPDATE jobs SET locked_until = now() - interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	second := mustClaim(t, s, "w2")
	if second.ID != first.ID || second.Attempts != 2 {
		t.Errorf("claimed %+v; want job %d again, attempt 2", second, first.ID)
	}
	// If w1 was only slow, not dead, it can't touch the job now.
	if err := s.CompleteJob(ctx, first.ID, "w1"); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("old holder completing: error = %v, want ErrLeaseLost", err)
	}
	mustDo(t, s.FailJob(ctx, second.ID, "w2", "build failed"))
}

func TestConcurrentClaimsTakeDifferentJobs(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	const jobs, workers = 30, 8
	for i := range jobs {
		app := createTestApp(t, s, fmt.Sprintf("app-%d", i))
		if _, err := s.CreateDeployment(ctx, app.ID, sha1); err != nil {
			t.Fatal(err)
		}
	}

	// Every worker claims until the queue is empty, all at once.
	var mu sync.Mutex
	claimed := map[int64]string{}
	var wg sync.WaitGroup
	for w := range workers {
		worker := fmt.Sprintf("w%d", w)
		wg.Go(func() {
			for {
				job, ok, err := s.ClaimJob(ctx, worker, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				if other, dup := claimed[job.ID]; dup {
					t.Errorf("job %d claimed by both %s and %s", job.ID, other, worker)
				}
				claimed[job.ID] = worker
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(claimed) != jobs {
		t.Errorf("%d jobs claimed, want all %d", len(claimed), jobs)
	}
}

func mustClaim(t *testing.T, s *Store, worker string) Job {
	t.Helper()
	job, ok, err := s.ClaimJob(t.Context(), worker, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("%s found no job to claim", worker)
	}
	return job
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func assertStatus(t *testing.T, s *Store, id int64, want DeploymentStatus) {
	t.Helper()
	d, err := s.Deployment(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != want {
		t.Errorf("deployment %d status = %s, want %s", id, d.Status, want)
	}
}

func TestDeploymentLogs(t *testing.T) {
	s := testStore(t)
	app := createTestApp(t, s, "hello")
	ctx := t.Context()
	d, _ := s.CreateDeployment(ctx, app.ID, sha1)
	at := time.Now()

	mustDo(t, s.AppendDeploymentLogs(ctx, d.ID, []NewLogLine{
		{At: at, Stream: LogPlatform, Line: "Building"},
		{At: at, Stream: LogBuild, Line: "Step 1/2 : FROM busybox"},
		{At: at, Stream: LogBuild, Line: "Step 2/2 : RUN true"},
	}))
	mustDo(t, s.AppendDeploymentLogs(ctx, d.ID, []NewLogLine{{At: at, Stream: LogPlatform, Line: "Healthy"}}))

	all, err := s.DeploymentLogs(ctx, d.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range all {
		got = append(got, l.Line)
	}
	if want := []string{"Building", "Step 1/2 : FROM busybox", "Step 2/2 : RUN true", "Healthy"}; !slices.Equal(got, want) {
		t.Fatalf("lines = %q, want %q (in the order written)", got, want)
	}
	// Resuming after a line, with a limit.
	rest, _ := s.DeploymentLogs(ctx, d.ID, all[0].ID, 2)
	if len(rest) != 2 || rest[0].ID != all[1].ID || rest[1].ID != all[2].ID {
		t.Errorf("after line 1, limit 2: %+v", rest)
	}
	if n, _ := s.CountDeploymentLogs(ctx, d.ID, LogBuild); n != 2 {
		t.Errorf("build lines = %d, want 2", n)
	}
	if err := s.AppendDeploymentLogs(ctx, 999999, []NewLogLine{{At: at, Stream: LogBuild, Line: "x"}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("appending to a missing deployment: error = %v, want ErrNotFound", err)
	}
}

func TestDeploymentLogCompleteFollowsTheJob(t *testing.T) {
	s := testStore(t)
	app := createTestApp(t, s, "hello")
	ctx := t.Context()
	d, _ := s.CreateDeployment(ctx, app.ID, sha1)

	complete := func() bool {
		t.Helper()
		c, err := s.DeploymentLogComplete(ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if complete() {
		t.Error("complete while queued")
	}
	job := mustClaim(t, s, "w1")
	// Waiting to be retried: more lines are coming.
	mustDo(t, s.RetryJob(ctx, job.ID, "w1", 0, "docker unreachable"))
	if complete() {
		t.Error("complete while waiting for a retry")
	}
	job = mustClaim(t, s, "w1")
	mustDo(t, s.CompleteJob(ctx, job.ID, "w1"))
	if !complete() {
		t.Error("not complete once the job is done")
	}
}

func TestPruneDeploymentLogsKeepsTheMostRecent(t *testing.T) {
	s := testStore(t)
	app := createTestApp(t, s, "hello")
	other := createTestApp(t, s, "other")
	ctx := t.Context()

	var ids []int64
	for range 4 {
		d, err := s.CreateDeployment(ctx, app.ID, sha1)
		if err != nil {
			t.Fatal(err)
		}
		mustDo(t, s.AppendDeploymentLogs(ctx, d.ID, []NewLogLine{{At: time.Now(), Stream: LogPlatform, Line: "x"}}))
		mustDo(t, s.MarkFailed(ctx, d.ID, "x")) // frees the app for the next one
		ids = append(ids, d.ID)
	}
	o, _ := s.CreateDeployment(ctx, other.ID, sha1)
	mustDo(t, s.AppendDeploymentLogs(ctx, o.ID, []NewLogLine{{At: time.Now(), Stream: LogPlatform, Line: "x"}}))

	n, err := s.PruneDeploymentLogs(ctx, app.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("deleted %d lines, want 2 (the two oldest deployments')", n)
	}
	for i, id := range ids {
		lines, _ := s.DeploymentLogs(ctx, id, 0, 10)
		if kept := len(lines) > 0; kept != (i >= 2) {
			t.Errorf("deployment %d (#%d of 4): kept %v", id, i+1, kept)
		}
	}
	if lines, _ := s.DeploymentLogs(ctx, o.ID, 0, 10); len(lines) != 1 {
		t.Error("pruning one app deleted another app's logs")
	}
}
