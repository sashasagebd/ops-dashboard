package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/engine"
	"github.com/sashasagebd/ops-dashboard/backend/internal/jobs"
	"github.com/sashasagebd/ops-dashboard/backend/internal/router"
	"github.com/sashasagebd/ops-dashboard/backend/internal/source"
	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

// fakes shares one event log between all the fakes, so tests can check the
// order things happened in, which is the point of this package.
type fakes struct {
	mu     sync.Mutex
	events []string

	app         store.App
	statuses    map[int64]store.DeploymentStatus
	failReason  string
	nextID      int64
	createErr   error
	sourceErr   error
	streamErr   error // the build context fails mid-stream
	buildErr    error
	probeStatus func(n int) int // status for the nth probe
	probes      int
	applyErr    func(n int) error // error for the nth Apply
	applies     [][]router.Route
	runSpec     engine.RunSpec
	containers  []engine.ManagedContainer // what's running; Run adds, Remove takes away
	removed     []string
	onProbe     func() // hook, e.g. to cancel mid-check

	buildOutput []string // what Build reports as progress
	priorBuild  int      // build lines stored by earlier attempts
	logLines    []string // the deploy log, as "stream: line"
	pruned      int      // PruneDeploymentLogs' keep, once called

	busy        bool    // RequestDeployment finds a deploy in progress
	nextCommit  string  // the commit waiting to deploy next
	startedNext []int64 // apps StartNextDeployment started a deploy for
	latest      string  // LatestCommit's answer
}

func newFakes() *fakes {
	return &fakes{
		app: store.App{
			ID: 1, Name: "hello", RepoURL: "https://github.com/o/hello", Hostname: "hello.apps.test", Port: 8080,
			HealthCheckPath: "/healthz", MemoryLimitBytes: 256 << 20, CPULimitMillicores: 500,
			SourceDir: "web",
		},
		statuses:    map[int64]store.DeploymentStatus{6: store.StatusHealthy},
		nextID:      7,
		probeStatus: func(int) int { return 200 },
		applyErr:    func(int) error { return nil },
		// The previous version, live, and a leftover from an old failure.
		containers: []engine.ManagedContainer{
			{ID: "old6", Name: "app-hello-6", Labels: map[string]string{engine.LabelDeployment: "6"}},
			{ID: "stale3", Name: "app-hello-3", Labels: map[string]string{engine.LabelDeployment: "3"}},
		},
	}
}

func (f *fakes) event(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, fmt.Sprintf(format, args...))
}

func (f *fakes) setStatus(id int64, s store.DeploymentStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[id] = s
}

// Store

func (f *fakes) App(context.Context, int64) (store.App, error) { return f.app, nil }

func (f *fakes) CreateDeployment(_ context.Context, appID int64, commit string) (store.Deployment, error) {
	if f.createErr != nil {
		return store.Deployment{}, f.createErr
	}
	f.setStatus(f.nextID, store.StatusQueued)
	return store.Deployment{ID: f.nextID, AppID: appID, CommitSHA: commit, Status: store.StatusQueued}, nil
}

func (f *fakes) Deployment(_ context.Context, id int64) (store.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.statuses[id]
	if !ok {
		return store.Deployment{}, store.ErrNotFound
	}
	return store.Deployment{ID: id, AppID: f.app.ID, CommitSHA: sha, Status: s}, nil
}

func (f *fakes) MarkBuilding(_ context.Context, id int64) error {
	f.event("building")
	f.setStatus(id, store.StatusBuilding)
	return nil
}

func (f *fakes) MarkStarting(_ context.Context, id int64, tag string) error {
	f.event("starting %s", tag)
	f.setStatus(id, store.StatusStarting)
	return nil
}

func (f *fakes) MarkHealthy(_ context.Context, id int64) error {
	f.event("healthy")
	f.mu.Lock()
	defer f.mu.Unlock()
	for other, s := range f.statuses {
		if s == store.StatusHealthy {
			f.statuses[other] = store.StatusSuperseded
		}
	}
	f.statuses[id] = store.StatusHealthy
	return nil
}

func (f *fakes) MarkFailed(_ context.Context, id int64, reason string) error {
	f.event("failed")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[id] = store.StatusFailed
	f.failReason = reason
	return nil
}

func (f *fakes) EnvVars(context.Context, int64) (map[string]string, error) {
	return map[string]string{"TOKEN": "s3cret"}, nil
}

func (f *fakes) LiveApps(context.Context) ([]store.LiveApp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var live []store.LiveApp
	for id, s := range f.statuses {
		if s == store.StatusHealthy {
			live = append(live, store.LiveApp{AppName: f.app.Name, Hostname: "hello.apps.test", Port: f.app.Port, DeploymentID: id})
		}
	}
	return live, nil
}

// Source

func (f *fakes) BuildContext(_ context.Context, repo, commit, dir string) (io.ReadCloser, error) {
	f.event("download %s /%s", commit, dir)
	if f.sourceErr != nil {
		return nil, f.sourceErr
	}
	if f.streamErr != nil {
		return io.NopCloser(io.MultiReader(strings.NewReader("tar"), iotest.ErrReader(f.streamErr))), nil
	}
	return io.NopCloser(strings.NewReader("tar")), nil
}

// Engine

func (f *fakes) Build(_ context.Context, buildContext io.Reader, tag string, _ map[string]string, progress func(string)) error {
	f.event("build %s", tag)
	// Like Docker's client: a broken upload comes back as an error of its
	// own, which may or may not wrap the reader's.
	if _, err := io.ReadAll(buildContext); err != nil {
		return errors.New("error during connect: unexpected EOF")
	}
	for _, l := range f.buildOutput {
		progress(l)
	}
	return f.buildErr
}

// Deploy logs

func (f *fakes) AppendDeploymentLogs(_ context.Context, _ int64, lines []store.NewLogLine) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range lines {
		f.logLines = append(f.logLines, l.Stream+": "+l.Line)
	}
	return nil
}

func (f *fakes) CountDeploymentLogs(context.Context, int64, string) (int, error) {
	return f.priorBuild, nil
}

func (f *fakes) RequestDeployment(ctx context.Context, appID int64, commit string) (store.Deployment, bool, error) {
	if f.busy {
		f.nextCommit = commit
		return store.Deployment{}, false, nil
	}
	d, err := f.CreateDeployment(ctx, appID, commit)
	return d, err == nil, err
}

func (f *fakes) StartNextDeployment(ctx context.Context, appID int64) (store.Deployment, bool, error) {
	if f.nextCommit == "" {
		return store.Deployment{}, false, nil
	}
	f.startedNext = append(f.startedNext, appID)
	f.nextCommit = ""
	return store.Deployment{ID: 8, AppID: appID}, true, nil
}

func (f *fakes) LatestCommit(_ context.Context, repo, branch string) (string, error) {
	f.event("latest %s %s", repo, branch)
	if f.latest == "" {
		return "", source.ErrBranchNotFound
	}
	return f.latest, nil
}

func (f *fakes) PruneDeploymentLogs(_ context.Context, _ int64, keep int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruned = keep
	return 0, nil
}

func (f *fakes) Run(_ context.Context, spec engine.RunSpec) (string, error) {
	f.event("run %s", spec.Name)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runSpec = spec
	f.containers = append(f.containers, newContainer)
	return newContainer.ID, nil
}

// newContainer is what Run starts for deployment 7, the one under test.
var newContainer = engine.ManagedContainer{ID: "new", Name: "app-hello-7", Labels: map[string]string{engine.LabelDeployment: "7"}}

func (f *fakes) Remove(_ context.Context, id string) error {
	f.event("remove %s", id)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	f.containers = slices.DeleteFunc(f.containers, func(c engine.ManagedContainer) bool { return c.ID == id })
	return nil
}

func (f *fakes) ManagedContainers(context.Context, string) ([]engine.ManagedContainer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.containers), nil
}

// Router

func (f *fakes) Apply(_ context.Context, routes []router.Route) error {
	f.mu.Lock()
	n := len(f.applies)
	f.applies = append(f.applies, slices.Clone(routes))
	f.mu.Unlock()
	if err := f.applyErr(n); err != nil {
		f.event("apply failed")
		return err
	}
	f.event("apply %s", describe(routes))
	return nil
}

func (f *fakes) Probe(context.Context, string, string) (int, error) {
	f.mu.Lock()
	n := f.probes
	f.probes++
	hook := f.onProbe
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return f.probeStatus(n), nil
}

func describe(routes []router.Route) string {
	var parts []string
	for _, r := range routes {
		parts = append(parts, r.Host+"->"+r.Upstream)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func (f *fakes) deployer() *Deployer {
	return New(Config{
		AppsNetwork:    "platform-apps",
		DeployTimeout:  time.Minute,
		HealthTimeout:  200 * time.Millisecond,
		HealthInterval: time.Millisecond,
	}, f, f, f, f)
}

// deploy queues a deploy and runs one attempt at it as the job runner would,
// on its last attempt: an error gives up, recording the failure.
func deploy(t *testing.T, f *fakes, ctx context.Context) (store.Deployment, error) {
	t.Helper()
	d := f.deployer()
	dep, err := d.Start(t.Context(), 1, sha)
	if err != nil {
		t.Fatal(err)
	}
	job := store.Job{ID: 1, Kind: "deploy", DeploymentID: dep.ID, Attempts: 1}
	err = d.Run(ctx, job)
	if err != nil && ctx.Err() == nil {
		d.GiveUp(t.Context(), job, err)
	}
	return dep, err
}

// retry runs another attempt at deployment 7, as after a crash.
func retry(t *testing.T, f *fakes) error {
	t.Helper()
	return f.deployer().Run(t.Context(), store.Job{ID: 1, Kind: "deploy", DeploymentID: 7, Attempts: 2})
}

func TestDeploySuccess(t *testing.T) {
	f := newFakes()
	f.probeStatus = func(n int) int { return []int{502, 502, 200}[min(n, 2)] } // up on the third try
	dep, err := deploy(t, f, t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if f.statuses[dep.ID] != store.StatusHealthy || f.statuses[6] != store.StatusSuperseded {
		t.Errorf("statuses = %v, want 7 healthy, 6 superseded", f.statuses)
	}
	want := []string{
		"building",
		"download " + sha + " /web",
		"build ops-platform/hello:" + sha,
		"starting ops-platform/hello:" + sha,
		"run app-hello-7",
		// The old version keeps its route while the new one is probed.
		"apply [hello.apps.test->app-hello-6:8080 probe-7.platform.internal->app-hello-7:8080]",
		"apply [hello.apps.test->app-hello-6:8080]",
		"healthy",
		"apply [hello.apps.test->app-hello-7:8080]",
		// Only once the route has switched does the old version go.
		"remove old6",
		"remove stale3",
	}
	if !slices.Equal(f.events, want) {
		t.Errorf("events:\n  got  %q\n  want %q", f.events, want)
	}

	spec := f.runSpec
	if spec.Network != "platform-apps" || spec.MemoryBytes != 256<<20 || spec.NanoCPUs != 500_000_000 ||
		spec.Env["TOKEN"] != "s3cret" || spec.Labels[engine.LabelDeployment] != "7" {
		t.Errorf("run spec = %+v", spec)
	}
}

func TestDeployFailures(t *testing.T) {
	tests := []struct {
		name          string
		setup         func(f *fakes)
		timeout       time.Duration // DeployTimeout, if not the default
		wantPermanent bool
		wantReason    string
		wantRemove    []string
	}{
		{
			name:          "build fails",
			setup:         func(f *fakes) { f.buildErr = &engine.BuildError{Message: "npm ci returned 1"} },
			wantPermanent: true,
			wantReason:    "build failed: npm ci returned 1",
		},
		{
			name:          "repo not found",
			setup:         func(f *fakes) { f.sourceErr = fmt.Errorf("fetching: %w", source.ErrNotFound) },
			wantPermanent: true,
			wantReason:    source.ErrNotFound.Error(),
		},
		{
			name:          "source directory missing",
			setup:         func(f *fakes) { f.streamErr = source.ErrDirNotFound },
			wantPermanent: true,
			wantReason:    source.ErrDirNotFound.Error(),
		},
		{
			name:          "never healthy",
			setup:         func(f *fakes) { f.probeStatus = func(int) int { return 502 } },
			wantPermanent: true,
			wantReason:    "health check failed: GET /healthz didn't succeed within 200ms (last result: 502)",
			wantRemove:    []string{"new"},
		},
		{
			// The attempt's own deadline, while waiting for the app.
			name:          "deploy times out",
			setup:         func(f *fakes) { f.probeStatus = func(int) int { return 502 } },
			timeout:       50 * time.Millisecond,
			wantPermanent: true,
			wantReason:    "deploy timed out after 50ms",
			wantRemove:    []string{"new"},
		},
		{
			// Docker or GitHub unreachable: worth another attempt, and if
			// the attempts run out, the reason stays generic.
			name:       "internal error is retried, and stays generic",
			setup:      func(f *fakes) { f.buildErr = errors.New("dial tcp 10.0.0.3:2375: connection refused") },
			wantReason: "internal error; see the platform's logs",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakes()
			tt.setup(f)
			d := f.deployer()
			if tt.timeout != 0 {
				d.cfg.DeployTimeout = tt.timeout
			}
			dep, _ := d.Start(t.Context(), 1, sha)
			job := store.Job{ID: 1, DeploymentID: dep.ID, Attempts: 1}
			err := d.Run(t.Context(), job)
			if err == nil || jobs.IsPermanent(err) != tt.wantPermanent {
				t.Fatalf("Run error = %v; want an error, permanent %v", err, tt.wantPermanent)
			}
			d.GiveUp(t.Context(), job, err)

			if f.statuses[dep.ID] != store.StatusFailed || f.failReason != tt.wantReason {
				t.Errorf("status %s, reason %q; want failed, %q", f.statuses[dep.ID], f.failReason, tt.wantReason)
			}
			// The old version is untouched and still live.
			if f.statuses[6] != store.StatusHealthy {
				t.Errorf("previous deployment is %s, want still healthy", f.statuses[6])
			}
			if !slices.Equal(f.removed, tt.wantRemove) {
				t.Errorf("removed %v, want %v", f.removed, tt.wantRemove)
			}
			// Any probe route is gone again.
			if n := len(f.applies); n > 0 && strings.Contains(describe(f.applies[n-1]), "probe-") {
				t.Errorf("last routes still have the probe: %v", describe(f.applies[n-1]))
			}
		})
	}
}

func TestRouteSwitchFailureKeepsOldContainer(t *testing.T) {
	f := newFakes()
	// Applies: probe on, probe off, then the switch, which fails.
	f.applyErr = func(n int) error {
		if n == 2 {
			return errors.New("caddy unreachable")
		}
		return nil
	}
	dep, err := deploy(t, f, t.Context())

	// The deployment is healthy (it is), but Caddy still points at the old
	// container, so that must keep running.
	if err != nil || f.statuses[dep.ID] != store.StatusHealthy {
		t.Errorf("status = %s, err %v; want healthy, no error", f.statuses[dep.ID], err)
	}
	if len(f.removed) != 0 {
		t.Errorf("removed %v; the old container must stay while Caddy still routes to it", f.removed)
	}
}

func TestShutdownDuringHealthCheck(t *testing.T) {
	f := newFakes()
	ctx, cancel := context.WithCancel(t.Context())
	f.probeStatus = func(int) int { return 502 }
	f.onProbe = cancel // the platform shuts down while the new version is being checked
	dep, err := deploy(t, f, ctx)

	// Not a failure: the job is handed back and retried after the restart,
	// so the deployment stays in progress.
	if !errors.Is(err, context.Canceled) || jobs.IsPermanent(err) {
		t.Errorf("Run error = %v, want a (non-permanent) cancellation", err)
	}
	if f.statuses[dep.ID] != store.StatusStarting {
		t.Errorf("status %s, want still starting", f.statuses[dep.ID])
	}
	if !slices.Contains(f.removed, "new") {
		t.Errorf("removed %v, want the half-started container removed", f.removed)
	}
}

func TestRetryAfterCutOffAttempt(t *testing.T) {
	f := newFakes()
	// The first attempt got as far as starting a container, then the
	// platform died.
	f.statuses[7] = store.StatusStarting
	f.containers = append(f.containers, newContainer)
	if err := retry(t, f); err != nil {
		t.Fatal(err)
	}

	// Its container goes first, so the new one can take the same name; then
	// the deploy runs from the build, as normal.
	want := []string{"remove new", "building", "download " + sha + " /web", "build ops-platform/hello:" + sha}
	if len(f.events) < len(want) || !slices.Equal(f.events[:len(want)], want) {
		t.Errorf("events start %q, want %q", f.events, want)
	}
	if f.statuses[7] != store.StatusHealthy {
		t.Errorf("status = %s, want healthy", f.statuses[7])
	}
}

func TestRetryOfHealthyDeploymentFinishesSwitch(t *testing.T) {
	f := newFakes()
	// The first attempt marked 7 live, then died before switching the
	// route or removing the old version.
	f.statuses = map[int64]store.DeploymentStatus{6: store.StatusSuperseded, 7: store.StatusHealthy}
	f.containers = append(f.containers, newContainer)
	if err := retry(t, f); err != nil {
		t.Fatal(err)
	}

	want := []string{"apply [hello.apps.test->app-hello-7:8080]", "remove old6", "remove stale3"}
	if !slices.Equal(f.events, want) {
		t.Errorf("events:\n  got  %q\n  want %q (no rebuild)", f.events, want)
	}
}

func TestRetryOfFinishedDeploymentDoesNothing(t *testing.T) {
	for _, status := range []store.DeploymentStatus{store.StatusFailed, store.StatusSuperseded} {
		f := newFakes()
		f.statuses[7] = status
		if err := retry(t, f); err != nil {
			t.Errorf("%s: Run error = %v, want nil", status, err)
		}
		if len(f.events) != 0 {
			t.Errorf("%s: events %q, want none", status, f.events)
		}
	}

	f := newFakes() // no deployment 7: deleted with its app
	if err := retry(t, f); !jobs.IsPermanent(err) {
		t.Errorf("deleted deployment: Run error = %v, want permanent", err)
	}
}

func TestNewerDeploymentsContainerIsKept(t *testing.T) {
	f := newFakes()
	// Deployment 9 was queued as soon as 7 went live, and has started its
	// container already.
	newer := engine.ManagedContainer{ID: "newer9", Name: "app-hello-9", Labels: map[string]string{engine.LabelDeployment: "9"}}
	f.containers = append(f.containers, newer)
	if _, err := deploy(t, f, t.Context()); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(f.removed, "newer9") {
		t.Errorf("removed %v; a later deployment's container must be left alone", f.removed)
	}
}

func TestGaveUpAfterRepeatedCrashes(t *testing.T) {
	f := newFakes()
	f.statuses[7] = store.StatusBuilding
	f.deployer().GiveUp(t.Context(), store.Job{DeploymentID: 7, Attempts: 4}, jobs.ErrNoAttemptsLeft)
	if f.statuses[7] != store.StatusFailed || f.failReason != "gave up: the platform stopped during every attempt" {
		t.Errorf("status %s, reason %q", f.statuses[7], f.failReason)
	}
}

func TestRemoveOrphansKeepsOnlyLiveDeployments(t *testing.T) {
	f := newFakes() // deployment 6 is live; containers for 6, 3 and 7 exist
	f.containers = append(f.containers, newContainer)
	if err := f.deployer().RemoveOrphans(t.Context()); err != nil {
		t.Fatal(err)
	}
	slices.Sort(f.removed)
	if !slices.Equal(f.removed, []string{"new", "stale3"}) {
		t.Errorf("removed %v, want the non-live ones (new, stale3) and not old6", f.removed)
	}
}

func TestStartQueuesAndNotifies(t *testing.T) {
	f := newFakes()
	d := f.deployer()
	notified := 0
	d.Notify = func() { notified++ }

	if _, err := d.Start(t.Context(), 1, sha); err != nil || notified != 1 {
		t.Errorf("Start: err %v, notified %d times; want queued and notified once", err, notified)
	}
	// Queueing runs nothing itself: that's the workers' job.
	if len(f.events) != 0 {
		t.Errorf("events %v; nothing should run", f.events)
	}

	f.createErr = store.ErrDeployInProgress
	if _, err := d.Start(t.Context(), 1, sha); !errors.Is(err, store.ErrDeployInProgress) || notified != 1 {
		t.Errorf("Start while in progress: err %v, notified %d; want ErrDeployInProgress, no notify", err, notified)
	}
}

func TestDeployLog(t *testing.T) {
	f := newFakes()
	f.buildOutput = []string{"Step 1/2 : FROM busybox", "Step 2/2 : CMD [\"httpd\"]"}
	if _, err := deploy(t, f, t.Context()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"platform: Deploying https://github.com/o/hello at 0123456 (attempt 1)",
		"platform: Building image ops-platform/hello:" + sha + " from web/",
		"build: Step 1/2 : FROM busybox",
		"build: Step 2/2 : CMD [\"httpd\"]",
		"platform: Starting container app-hello-7 (256 MiB memory, 0.5 CPU, 1 env vars)",
		"platform: Waiting up to 200ms for GET /healthz to succeed",
		"platform: Healthy",
		"platform: Removed the previous container app-hello-6",
		"platform: Removed the previous container app-hello-3",
		"platform: Live at http://hello.apps.test/",
	}
	if !slices.Equal(f.logLines, want) {
		t.Errorf("log:\n  got  %q\n  want %q", f.logLines, want)
	}
	// Env var values never reach the log.
	for _, l := range f.logLines {
		if strings.Contains(l, "s3cret") {
			t.Errorf("log line %q contains an env var's value", l)
		}
	}
	if f.pruned != keepLogsFor {
		t.Errorf("pruned keeping %d deployments' logs, want %d", f.pruned, keepLogsFor)
	}
}

func TestDeployLogOnFailure(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *fakes)
		ctx   func(t *testing.T, f *fakes) context.Context
		want  string // the log's last line
	}{
		{
			name:  "permanent: the reason, once given up",
			setup: func(f *fakes) { f.buildErr = &engine.BuildError{Message: "npm ci returned 1"} },
			want:  "platform: Deploy failed: build failed: npm ci returned 1",
		},
		{
			name:  "transient: the attempt failed, and why (generically)",
			setup: func(f *fakes) { f.sourceErr = errors.New("dial tcp: i/o timeout") },
			want:  "platform: Attempt 1 failed: internal error; see the platform's logs",
		},
		{
			name:  "shutdown: it carries on later",
			setup: func(f *fakes) { f.probeStatus = func(int) int { return 502 } },
			ctx: func(t *testing.T, f *fakes) context.Context {
				ctx, cancel := context.WithCancel(t.Context())
				f.onProbe = cancel
				return ctx
			},
			want: "platform: Interrupted: the platform is shutting down. The deploy carries on when it's back.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakes()
			tt.setup(f)
			d := f.deployer()
			dep, _ := d.Start(t.Context(), 1, sha)
			ctx := t.Context()
			if tt.ctx != nil {
				ctx = tt.ctx(t, f)
			}
			job := store.Job{ID: 1, DeploymentID: dep.ID, Attempts: 1}
			// As the runner does: only a permanent error gives up here.
			if err := d.Run(ctx, job); jobs.IsPermanent(err) {
				d.GiveUp(t.Context(), job, err)
			}
			if n := len(f.logLines); n == 0 || f.logLines[n-1] != tt.want {
				t.Errorf("log ends %q, want %q", f.logLines, tt.want)
			}
		})
	}
}

func TestBuildOutputIsCapped(t *testing.T) {
	f := newFakes()
	f.priorBuild = maxBuildLines - 1 // an earlier attempt nearly used it up
	f.buildOutput = []string{"one more", "too many", "and more"}
	if _, err := deploy(t, f, t.Context()); err != nil {
		t.Fatal(err)
	}
	var build []string
	for _, l := range f.logLines {
		if strings.HasPrefix(l, "build: ") || strings.Contains(l, "cut off") {
			build = append(build, l)
		}
	}
	want := []string{"build: one more", "platform: Build output cut off after 5000 lines; the build carries on."}
	if !slices.Equal(build, want) {
		t.Errorf("build lines = %q, want %q", build, want)
	}
}

func TestPushDeploysNowOrNext(t *testing.T) {
	f := newFakes()
	d := f.deployer()
	notified := 0
	d.Notify = func() { notified++ }

	if dep, started, err := d.Push(t.Context(), 1, sha); err != nil || !started || dep.ID != 7 || notified != 1 {
		t.Errorf("Push when idle: %+v, started %v, %v, notified %d; want deployment 7 started", dep, started, err, notified)
	}
	f.busy = true
	if _, started, err := d.Push(t.Context(), 1, sha); err != nil || started || notified != 1 {
		t.Errorf("Push while busy: started %v, %v, notified %d; want it to wait, no wake", started, err, notified)
	}
	if f.nextCommit != sha {
		t.Errorf("next commit = %q, want the push", f.nextCommit)
	}
}

func TestFinishingADeployStartsTheNextPush(t *testing.T) {
	t.Run("after success", func(t *testing.T) {
		f := newFakes()
		f.nextCommit = sha // pushed while 7 was deploying
		d := f.deployer()
		woken := 0
		d.Notify = func() { woken++ }
		dep, err := d.Start(t.Context(), 1, sha)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Run(t.Context(), store.Job{DeploymentID: dep.ID, Attempts: 1}); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(f.startedNext, []int64{1}) || woken != 2 {
			t.Errorf("started next for %v, woken %d times; want app 1, and a wake for each deploy", f.startedNext, woken)
		}
	})
	t.Run("after giving up", func(t *testing.T) {
		f := newFakes()
		f.nextCommit = sha
		f.statuses[7] = store.StatusBuilding
		f.deployer().GiveUp(t.Context(), store.Job{DeploymentID: 7, Attempts: 1}, &engine.BuildError{Message: "x"})
		if !slices.Equal(f.startedNext, []int64{1}) {
			t.Errorf("started next for %v, want app 1: a failed deploy mustn't strand the push", f.startedNext)
		}
	})
	t.Run("not after a retry is scheduled", func(t *testing.T) {
		f := newFakes()
		f.nextCommit = sha
		f.sourceErr = errors.New("dial tcp: i/o timeout")
		d := f.deployer()
		dep, _ := d.Start(t.Context(), 1, sha)
		if err := d.Run(t.Context(), store.Job{DeploymentID: dep.ID, Attempts: 1}); err == nil {
			t.Fatal("want the transient error")
		}
		if len(f.startedNext) != 0 {
			t.Error("started the next push while this deploy is still to be retried")
		}
	})
}

func TestStartLatest(t *testing.T) {
	f := newFakes()
	f.app.Branch = "main"
	f.latest = sha
	dep, err := f.deployer().StartLatest(t.Context(), 1)
	if err != nil || dep.CommitSHA != sha {
		t.Errorf("StartLatest = %+v, %v; want a deploy of the branch's head", dep, err)
	}
	if !slices.Contains(f.events, "latest https://github.com/o/hello main") {
		t.Errorf("events %q: didn't look up the app's branch", f.events)
	}

	f.latest = ""
	if _, err := f.deployer().StartLatest(t.Context(), 1); !errors.Is(err, source.ErrBranchNotFound) {
		t.Errorf("unknown branch: error = %v, want ErrBranchNotFound", err)
	}
}
