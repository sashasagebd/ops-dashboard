package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
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
	onProbe     func()                              // hook, e.g. to cancel mid-check
	inFlight    func(n int) (map[string]int, error) // Caddy's in-flight counts at the nth check; none if nil
	drainChecks int

	buildOutput []string // what Build reports as progress
	priorBuild  int      // build lines stored by earlier attempts
	logLines    []string // the deploy log, as "stream: line"
	pruned      int      // PruneDeploymentLogs' keep, once called

	busy        bool    // RequestDeployment finds a deploy in progress
	nextCommit  string  // the commit waiting to deploy next
	startedNext []int64 // apps StartNextDeployment started a deploy for
	latest      string  // LatestCommit's answer

	rollbackOf   map[int64]int64  // rollback deployments: the one whose image each runs
	imageTags    map[int64]string // deployments' image tags, once known
	kept         []string         // KeptImages' answer
	recent       []store.Deployment
	images       []engine.Image // what AppImages lists
	imageMissing bool           // ImageExists says no
	imageInUse   map[string]bool

	caddyRoutes []router.Route      // what Routes reports Caddy has
	database    *store.Database     // the app's database, if it has one
	dbSpec      engine.DatabaseSpec // what RunDatabase was asked for
	userEnv     map[string]string   // EnvVars' answer, if set
	onRun       func()              // hook, called as Run starts a container

	lifecycleErr error // StopApp, StartApp and DeleteApp refuse with it
	deleted      bool  // DeleteApp ran: nothing of the app is live any more

	head         string          // the branch's newest commit, for LatestCommitIfChanged
	headETag     string          // its ETag
	sentETags    []string        // the ETags the poller sent, in order
	knownCommits map[string]bool // commits RequestDeployment finds known
	requestErr   error           // RequestDeployment fails with it
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
	return store.Deployment{ID: id, AppID: f.app.ID, CommitSHA: sha, Status: s,
		RollbackOf: f.rollbackOf[id], ImageTag: f.imageTags[id]}, nil
}

func (f *fakes) CreateRollback(_ context.Context, appID, targetID int64) (store.Deployment, error) {
	if f.createErr != nil {
		return store.Deployment{}, f.createErr
	}
	f.setStatus(f.nextID, store.StatusQueued)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rollbackOf == nil {
		f.rollbackOf = map[int64]int64{}
	}
	if f.imageTags == nil {
		f.imageTags = map[int64]string{}
	}
	f.rollbackOf[f.nextID] = targetID
	f.imageTags[f.nextID] = f.imageTags[targetID]
	return store.Deployment{ID: f.nextID, AppID: appID, CommitSHA: sha, Status: store.StatusQueued,
		RollbackOf: targetID, ImageTag: f.imageTags[targetID]}, nil
}

func (f *fakes) StartRollback(_ context.Context, id int64) error {
	f.event("starting rollback")
	f.setStatus(id, store.StatusStarting)
	return nil
}

func (f *fakes) KeptImages(context.Context, int64) ([]string, error) { return f.kept, nil }

func (f *fakes) Deployments(context.Context, int64, int) ([]store.Deployment, error) {
	return f.recent, nil
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
	if f.userEnv != nil {
		return maps.Clone(f.userEnv), nil
	}
	return map[string]string{"TOKEN": "s3cret"}, nil
}

func (f *fakes) Database(context.Context, int64) (store.Database, error) {
	if f.database == nil {
		return store.Database{}, store.ErrNotFound
	}
	return *f.database, nil
}

func (f *fakes) EnsureNetwork(_ context.Context, name string, _ map[string]string) error {
	f.event("network %s", name)
	return nil
}

func (f *fakes) Pull(_ context.Context, ref string) error {
	f.event("pull %s", ref)
	return nil
}

func (f *fakes) RunDatabase(_ context.Context, spec engine.DatabaseSpec) (string, error) {
	f.event("run database %s", spec.Name)
	f.dbSpec = spec
	return "db", nil
}

func (f *fakes) LiveApps(context.Context) ([]store.LiveApp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var live []store.LiveApp
	for id, s := range f.statuses {
		if s == store.StatusHealthy && !f.app.Stopped && !f.deleted {
			live = append(live, store.LiveApp{AppName: f.app.Name, Hostname: "hello.apps.test", PublicName: f.app.PublicName,
				Port: f.app.Port, DeploymentID: id})
		}
	}
	return live, nil
}

func (f *fakes) CurrentDeployment(_ context.Context, appID int64) (store.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, s := range f.statuses {
		if s == store.StatusHealthy {
			return store.Deployment{ID: id, AppID: appID, CommitSHA: sha, Status: s, ImageTag: ImageTag(f.app.Name, sha)}, nil
		}
	}
	return store.Deployment{}, store.ErrNotFound
}

func (f *fakes) StopApp(context.Context, int64) (store.App, error) {
	if f.lifecycleErr != nil {
		return store.App{}, f.lifecycleErr
	}
	f.event("store: stopped")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.app.Stopped = true
	return f.app, nil
}

func (f *fakes) StartApp(context.Context, int64) (store.App, error) {
	if f.lifecycleErr != nil {
		return store.App{}, f.lifecycleErr
	}
	f.event("store: started")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.app.Stopped = false
	return f.app, nil
}

func (f *fakes) DeleteApp(context.Context, int64) (store.DeletedApp, error) {
	if f.lifecycleErr != nil {
		return store.DeletedApp{}, f.lifecycleErr
	}
	f.event("store: deleted")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = true
	return store.DeletedApp{App: f.app, HadDatabase: f.database != nil}, nil
}

func (f *fakes) RemoveNetwork(_ context.Context, name string) error {
	f.event("remove network %s", name)
	return nil
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

func (f *fakes) RequestDeployment(ctx context.Context, appID int64, commit string) (store.Deployment, store.RequestOutcome, error) {
	if f.requestErr != nil {
		return store.Deployment{}, 0, f.requestErr
	}
	if f.knownCommits[commit] {
		return store.Deployment{}, store.RequestKnown, nil
	}
	if f.busy {
		f.nextCommit = commit
		return store.Deployment{}, store.RequestWaiting, nil
	}
	d, err := f.CreateDeployment(ctx, appID, commit)
	if err != nil {
		return d, 0, err
	}
	if f.knownCommits == nil {
		f.knownCommits = map[string]bool{}
	}
	f.knownCommits[commit] = true
	return d, store.RequestStarted, nil
}

func (f *fakes) Apps(context.Context) ([]store.App, error) { return []store.App{f.app}, nil }

func (f *fakes) LatestCommitIfChanged(_ context.Context, repo, branch, etag string) (string, string, bool, error) {
	f.event("poll %s %s", repo, branch)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sentETags = append(f.sentETags, etag)
	if etag != "" && etag == f.headETag {
		return "", etag, false, nil
	}
	return f.head, f.headETag, true, nil
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
	if f.onRun != nil {
		f.onRun()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runSpec = spec
	f.containers = append(f.containers, newContainer)
	return newContainer.ID, nil
}

func (f *fakes) AppImages(context.Context, string) ([]engine.Image, error) { return f.images, nil }

func (f *fakes) ImageExists(_ context.Context, ref string) (bool, error) {
	f.event("image exists? %s", ref)
	return !f.imageMissing, nil
}

func (f *fakes) RemoveImage(_ context.Context, ref string) error {
	f.event("remove image %s", ref)
	if f.imageInUse[ref] {
		return engine.ErrImageInUse
	}
	return nil
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

func (f *fakes) Routes(context.Context) ([]router.Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.caddyRoutes), nil
}

func (f *fakes) SyncMetrics(context.Context) (bool, error) { return false, nil }

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

func (f *fakes) InFlight(context.Context) (map[string]int, error) {
	f.mu.Lock()
	n := f.drainChecks
	f.drainChecks++
	fn := f.inFlight
	f.mu.Unlock()
	if fn == nil {
		return map[string]int{}, nil
	}
	m, err := fn(n)
	if err != nil {
		f.event("in flight: error")
	} else {
		f.event("in flight %d", m["app-hello-6:8080"]+m["app-hello-3:8080"])
	}
	return m, err
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
		DrainTimeout:   200 * time.Millisecond,
		DrainInterval:  time.Millisecond,
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

func TestDrainWaitsForInFlightRequests(t *testing.T) {
	f := newFakes()
	// Two requests still on the old version at the switch, then one, then
	// none; the new version's own requests don't count.
	f.inFlight = func(n int) (map[string]int, error) {
		return map[string]int{"app-hello-6:8080": max(2-n, 0), "app-hello-7:8080": 5}, nil
	}
	if _, err := deploy(t, f, t.Context()); err != nil {
		t.Fatal(err)
	}
	i := slices.Index(f.events, "apply [hello.apps.test->app-hello-7:8080]")
	want := []string{"apply [hello.apps.test->app-hello-7:8080]", "in flight 2", "in flight 1", "in flight 0", "remove old6", "remove stale3"}
	if i < 0 || !slices.Equal(f.events[i:], want) {
		t.Errorf("events:\n  got  %q\n  want them to end %q", f.events, want)
	}
	for _, l := range []string{
		"platform: Waiting up to 200ms for the previous version's requests to finish (2 in flight)",
		"platform: The previous version's requests have finished",
	} {
		if !slices.Contains(f.logLines, l) {
			t.Errorf("log %q is missing %q", f.logLines, l)
		}
	}
}

func TestDrainIsBounded(t *testing.T) {
	tests := []struct {
		name     string
		inFlight func(int) (map[string]int, error)
		wantLog  string
	}{
		{
			name:     "a request never finishes",
			inFlight: func(int) (map[string]int, error) { return map[string]int{"app-hello-6:8080": 1}, nil },
			wantLog:  "platform: Stopping the previous version after 200ms with requests still in flight (1)",
		},
		{
			name:     "Caddy can't be asked",
			inFlight: func(int) (map[string]int, error) { return nil, errors.New("connection refused") },
			wantLog:  "platform: Couldn't check the previous version's requests; stopping it after 200ms",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakes()
			f.inFlight = tt.inFlight
			start := time.Now()
			if _, err := deploy(t, f, t.Context()); err != nil {
				t.Fatal(err)
			}
			if waited := time.Since(start); waited < 200*time.Millisecond {
				t.Errorf("deploy took %s; want it to wait out the 200ms drain timeout", waited)
			}
			if !slices.Equal(f.removed, []string{"old6", "stale3"}) {
				t.Errorf("removed %v, want the old containers removed after the timeout", f.removed)
			}
			if !slices.Contains(f.logLines, tt.wantLog) {
				t.Errorf("log %q is missing %q", f.logLines, tt.wantLog)
			}
		})
	}
}

func TestShutdownDuringDrain(t *testing.T) {
	f := newFakes()
	ctx, cancel := context.WithCancel(t.Context())
	f.inFlight = func(int) (map[string]int, error) {
		cancel() // the platform shuts down while the old version is draining
		return map[string]int{"app-hello-6:8080": 1}, nil
	}
	dep, err := deploy(t, f, ctx)

	// The deployment is live; the job is handed back, and the old containers
	// stay until the retry has drained them.
	if !errors.Is(err, context.Canceled) || jobs.IsPermanent(err) {
		t.Errorf("Run error = %v, want a (non-permanent) cancellation", err)
	}
	if f.statuses[dep.ID] != store.StatusHealthy || len(f.removed) != 0 {
		t.Errorf("status %s, removed %v; want healthy, nothing removed", f.statuses[dep.ID], f.removed)
	}

	f.inFlight = nil // drained by the time the platform is back
	if err := retry(t, f); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.removed, []string{"old6", "stale3"}) {
		t.Errorf("removed %v after the retry, want old6 and stale3", f.removed)
	}
}

func TestGiveUpOnLiveDeploymentIsNotAFailure(t *testing.T) {
	f := newFakes()
	// 7 went live, but every attempt to finish the switch was cut off.
	f.statuses = map[int64]store.DeploymentStatus{6: store.StatusSuperseded, 7: store.StatusHealthy}
	f.nextCommit = sha
	f.deployer().GiveUp(t.Context(), store.Job{DeploymentID: 7, Attempts: 3}, errTimedOut)

	if f.statuses[7] != store.StatusHealthy || slices.Contains(f.events, "failed") {
		t.Errorf("status %s, events %q; want still healthy, never marked failed", f.statuses[7], f.events)
	}
	want := "platform: The new version is live, but finishing the switch from the previous one was cut off: deploy timed out after 1m0s"
	if n := len(f.logLines); n == 0 || f.logLines[n-1] != want {
		t.Errorf("log ends %q, want %q", f.logLines, want)
	}
	if !slices.Equal(f.startedNext, []int64{1}) {
		t.Errorf("started next for %v, want app 1", f.startedNext)
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

func TestSyncRoutesAppliesOnlyWhenCaddyDiffers(t *testing.T) {
	live := router.Route{Host: "hello.apps.test", Upstream: "app-hello-6:8080"}
	tests := []struct {
		name        string
		caddy       []router.Route
		wantChanged bool
	}{
		{name: "same", caddy: []router.Route{live}},
		{name: "route deleted", caddy: []router.Route{}, wantChanged: true},
		{name: "pointing elsewhere", caddy: []router.Route{{Host: "hello.apps.test", Upstream: "app-hello-3:8080"}}, wantChanged: true},
		{name: "an extra one", caddy: []router.Route{live, {Host: "x.apps.test", Upstream: "y:1"}}, wantChanged: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakes() // deployment 6 is live
			f.caddyRoutes = tt.caddy
			changed, err := f.deployer().SyncRoutes(t.Context())
			if err != nil || changed != tt.wantChanged {
				t.Fatalf("SyncRoutes = %v, %v; want changed %v", changed, err, tt.wantChanged)
			}
			if applied := len(f.applies) > 0; applied != tt.wantChanged {
				t.Errorf("applied %v, want %v", applied, tt.wantChanged)
			}
			if tt.wantChanged && describe(f.applies[0]) != "[hello.apps.test->app-hello-6:8080]" {
				t.Errorf("applied %s, want the database's table", describe(f.applies[0]))
			}
		})
	}
}

func TestRouteTableWithPublicHostnames(t *testing.T) {
	tailnet := router.Route{Host: "hello.apps.test", Upstream: "app-hello-6:8080"}
	public := router.Route{Host: "hi.example.com", Upstream: "app-hello-6:8080", Public: true}
	hooks := router.Route{Host: "hooks.example.com", Upstream: "platform:8082", Public: true}
	tests := []struct {
		name               string
		publicName, domain string
		webhook            string
		want               []router.Route
	}{
		{name: "tailnet only", domain: "example.com", want: []router.Route{tailnet}},
		{name: "public", publicName: "hi", domain: "example.com", want: []router.Route{tailnet, public}},
		{name: "public, webhook too", publicName: "hi", domain: "example.com", webhook: "platform:8082",
			want: []router.Route{tailnet, public, hooks}},
		// No domain configured: nothing is public, whatever the database says.
		{name: "no domain", publicName: "hi", webhook: "platform:8082", want: []router.Route{tailnet}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakes() // deployment 6 is live
			f.app.PublicName = tt.publicName
			d := f.deployer()
			d.cfg.PublicDomain, d.cfg.WebhookUpstream = tt.domain, tt.webhook
			if err := d.ApplyRoutes(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(f.applies[0], tt.want) {
				t.Errorf("applied %+v, want %+v", f.applies[0], tt.want)
			}
		})
	}
}

func TestRecreateRunsTheLiveDeploymentAgain(t *testing.T) {
	f := newFakes()
	d := f.deployer()
	live := store.Deployment{ID: 6, AppID: 1, CommitSHA: sha, Status: store.StatusHealthy, ImageTag: "ops-platform/hello:" + sha}
	if err := d.Recreate(t.Context(), f.app, live); err != nil {
		t.Fatal(err)
	}
	// The same container a deploy would start: name, image, env, labels,
	// limits.
	spec := f.runSpec
	if spec.Name != "app-hello-6" || spec.Image != live.ImageTag || spec.Env["TOKEN"] != "s3cret" ||
		spec.Labels[engine.LabelDeployment] != "6" || spec.Network != "platform-apps" || spec.MemoryBytes != 256<<20 {
		t.Errorf("run spec = %+v", spec)
	}

	f.imageMissing = true
	if err := d.Recreate(t.Context(), f.app, live); !errors.Is(err, ErrImageGone) {
		t.Errorf("image gone: error = %v, want ErrImageGone", err)
	}
}

func TestDeployHoldsTheAppLock(t *testing.T) {
	f := newFakes()
	d := f.deployer()
	// While the deploy runs, the reconciler can't take the app.
	var duringDeploy bool
	f.onRun = func() {
		unlock, ok := d.TryLockApp(f.app.ID)
		duringDeploy = ok
		if ok {
			unlock()
		}
	}
	dep, err := d.Start(t.Context(), 1, sha)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Run(t.Context(), store.Job{DeploymentID: dep.ID, Attempts: 1}); err != nil {
		t.Fatal(err)
	}
	if duringDeploy {
		t.Error("TryLockApp succeeded while the app's deploy was running")
	}
	// Afterwards it can, and only one holder at a time.
	unlock, ok := d.TryLockApp(f.app.ID)
	if !ok {
		t.Fatal("TryLockApp failed after the deploy")
	}
	if _, again := d.TryLockApp(f.app.ID); again {
		t.Error("the app's lock was taken twice")
	}
	unlock()
	// Another app's lock is its own.
	if u, ok := d.TryLockApp(2); !ok {
		t.Error("app 2's lock was taken")
	} else {
		u()
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

	if dep, outcome, err := d.Push(t.Context(), 1, sha); err != nil || outcome != store.RequestStarted || dep.ID != 7 || notified != 1 {
		t.Errorf("Push when idle: %+v, %v, %v, notified %d; want deployment 7 started", dep, outcome, err, notified)
	}
	f.busy = true
	const other = "fedcba9876543210fedcba9876543210fedcba98"
	if _, outcome, err := d.Push(t.Context(), 1, other); err != nil || outcome != store.RequestWaiting || notified != 1 {
		t.Errorf("Push while busy: %v, %v, notified %d; want it to wait, no wake", outcome, err, notified)
	}
	if f.nextCommit != other {
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

// rollbackFakes: deployment 5 was live before 6 (now live); 5's image is
// still there.
func rollbackFakes() *fakes {
	f := newFakes()
	f.statuses[5] = store.StatusSuperseded
	f.imageTags = map[int64]string{5: "ops-platform/hello:old5", 6: "ops-platform/hello:" + sha}
	f.containers = []engine.ManagedContainer{
		{ID: "old6", Name: "app-hello-6", Labels: map[string]string{engine.LabelDeployment: "6"}},
	}
	return f
}

func TestRollbackReusesTheImage(t *testing.T) {
	f := rollbackFakes()
	d := f.deployer()
	notified := 0
	d.Notify = func() { notified++ }
	dep, err := d.Rollback(t.Context(), 1, 5)
	if err != nil || dep.RollbackOf != 5 || notified != 1 {
		t.Fatalf("Rollback = %+v, %v, notified %d; want a rollback of 5, queued and notified", dep, err, notified)
	}
	if err := d.Run(t.Context(), store.Job{ID: 1, DeploymentID: dep.ID, Attempts: 1}); err != nil {
		t.Fatal(err)
	}

	// No download or build: the old image runs, through the same health
	// check, switch and drain as any deploy.
	want := []string{
		"image exists? ops-platform/hello:old5",
		"starting rollback",
		"run app-hello-7",
		"apply [hello.apps.test->app-hello-6:8080 probe-7.platform.internal->app-hello-7:8080]",
		"apply [hello.apps.test->app-hello-6:8080]",
		"healthy",
		"apply [hello.apps.test->app-hello-7:8080]",
		"remove old6",
	}
	if !slices.Equal(f.events, want) {
		t.Errorf("events:\n  got  %q\n  want %q", f.events, want)
	}
	if f.runSpec.Image != "ops-platform/hello:old5" || f.runSpec.Labels[engine.LabelDeployment] != "7" {
		t.Errorf("run spec image %q, deployment label %q; want the old image, as deployment 7",
			f.runSpec.Image, f.runSpec.Labels[engine.LabelDeployment])
	}
	if f.statuses[7] != store.StatusHealthy {
		t.Errorf("status = %s, want healthy", f.statuses[7])
	}
	for _, l := range []string{
		"platform: Rolling back to deployment #5 (https://github.com/o/hello at 0123456), reusing its image (attempt 1)",
		"platform: Using image ops-platform/hello:old5 (no build)",
	} {
		if !slices.Contains(f.logLines, l) {
			t.Errorf("log %q is missing %q", f.logLines, l)
		}
	}
}

func TestRollbackToAnImageThatsGone(t *testing.T) {
	f := rollbackFakes()
	f.imageMissing = true // removed by hand since
	d := f.deployer()
	dep, err := d.Rollback(t.Context(), 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	job := store.Job{ID: 1, DeploymentID: dep.ID, Attempts: 1}
	err = d.Run(t.Context(), job)
	if !jobs.IsPermanent(err) {
		t.Fatalf("Run error = %v, want permanent", err)
	}
	d.GiveUp(t.Context(), job, err)
	if f.statuses[7] != store.StatusFailed || f.failReason != ErrImageGone.Error() {
		t.Errorf("status %s, reason %q; want failed, %q", f.statuses[7], f.failReason, ErrImageGone.Error())
	}
	if f.statuses[6] != store.StatusHealthy || len(f.removed) != 0 {
		t.Errorf("live deployment %s, removed %v; want 6 still live and untouched", f.statuses[6], f.removed)
	}
}

func TestRollbackRefusedIsPassedOn(t *testing.T) {
	f := rollbackFakes()
	f.createErr = store.ErrDeployInProgress
	d := f.deployer()
	notified := 0
	d.Notify = func() { notified++ }
	if _, err := d.Rollback(t.Context(), 1, 5); !errors.Is(err, store.ErrDeployInProgress) || notified != 0 {
		t.Errorf("Rollback during a deploy: %v, notified %d; want ErrDeployInProgress, no wake", err, notified)
	}
}

func TestPruneImagesKeepsRecentLiveVersions(t *testing.T) {
	f := newFakes()
	tag := func(c string) string { return ImageTag("hello", c) }
	next := "9999999999999999999999999999999999999999"
	f.kept = []string{tag(sha), tag("kept1")}
	// The app's next deploy has started, and its image isn't recorded yet.
	f.recent = []store.Deployment{{ID: 8, CommitSHA: next, Status: store.StatusBuilding}}
	f.images = []engine.Image{
		{ID: "sha256:live", Tags: []string{tag(sha)}},
		{ID: "sha256:kept", Tags: []string{tag("kept1"), tag("old2")}}, // one image, two commits
		{ID: "sha256:old", Tags: []string{tag("old1")}},
		{ID: "sha256:next", Tags: []string{tag(next)}},
		{ID: "sha256:untagged"},
		{ID: "sha256:busy", Tags: []string{tag("old3")}},
		{ID: "sha256:foreign", Tags: []string{"someone/else:1"}}, // not under the app's prefix
	}
	f.imageInUse = map[string]bool{tag("old3"): true}
	if _, err := deploy(t, f, t.Context()); err != nil {
		t.Fatal(err)
	}

	var removed []string
	for _, e := range f.events {
		if r, ok := strings.CutPrefix(e, "remove image "); ok {
			removed = append(removed, r)
		}
	}
	want := []string{tag("old2"), tag("old1"), "sha256:untagged", tag("old3")}
	if !slices.Equal(removed, want) {
		t.Errorf("removed images %q,\n want %q", removed, want)
	}
	// After the old containers, so none of their images is still in use.
	if i, j := slices.Index(f.events, "remove stale3"), slices.Index(f.events, "remove image "+tag("old2")); i < 0 || j < i {
		t.Errorf("events %q: images must go after the old containers", f.events)
	}
	// The one still in use isn't counted.
	if !slices.Contains(f.logLines, "platform: Removed 3 old images; the last 6 live versions are kept for rollbacks") {
		t.Errorf("log %q: want the count of removed images", f.logLines)
	}
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

func TestDeployWithADatabase(t *testing.T) {
	f := newFakes()
	f.database = &store.Database{AppID: 1, Password: "pw0123"}
	// The user's own DATABASE_URL gives way to the platform's.
	f.userEnv = map[string]string{"TOKEN": "s3cret", "DATABASE_URL": "postgres://elsewhere"}
	if _, err := deploy(t, f, t.Context()); err != nil {
		t.Fatal(err)
	}
	spec := f.runSpec
	if spec.Env["DATABASE_URL"] != "postgres://app:pw0123@ops-db-hello:5432/app?sslmode=disable" || spec.Env["TOKEN"] != "s3cret" {
		t.Errorf("env = %v, want the platform's DATABASE_URL and the app's own vars", spec.Env)
	}
	// On Caddy's network, and on its database's, made sure to exist first.
	if spec.Network != "platform-apps" || !slices.Equal(spec.ExtraNetworks, []string{"ops-db-hello"}) {
		t.Errorf("networks = %q + %v", spec.Network, spec.ExtraNetworks)
	}
	if i, j := slices.Index(f.events, "network ops-db-hello"), slices.Index(f.events, "run app-hello-7"); i < 0 || j < i {
		t.Errorf("events %q: the network must exist before the container starts", f.events)
	}
	// The log says so, and never shows the URL (it has the password).
	if !slices.Contains(f.logLines, "platform: Starting container app-hello-7 (256 MiB memory, 0.5 CPU, 2 env vars, and its database as DATABASE_URL)") {
		t.Errorf("log = %q", f.logLines)
	}
	for _, l := range f.logLines {
		if strings.Contains(l, "pw0123") {
			t.Errorf("log line %q contains the database password", l)
		}
	}
}

func TestDeployWithoutADatabaseJoinsNothingElse(t *testing.T) {
	f := newFakes()
	if _, err := deploy(t, f, t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.runSpec.ExtraNetworks) != 0 || f.runSpec.Env["DATABASE_URL"] != "" {
		t.Errorf("spec = %+v, want no database network or URL", f.runSpec)
	}
}

func TestRecreateKeepsTheDatabase(t *testing.T) {
	f := newFakes()
	f.database = &store.Database{AppID: 1, Password: "pw"}
	live := store.Deployment{ID: 6, AppID: 1, Status: store.StatusHealthy, ImageTag: "ops-platform/hello:" + sha}
	if err := f.deployer().Recreate(t.Context(), f.app, live); err != nil {
		t.Fatal(err)
	}
	if f.runSpec.Env["DATABASE_URL"] == "" || len(f.runSpec.ExtraNetworks) != 1 {
		t.Errorf("recreated spec = %+v, want its database too", f.runSpec)
	}
}

func TestRunDatabase(t *testing.T) {
	f := newFakes()
	db := store.Database{AppID: 1, Password: "pw0123"}
	if err := f.deployer().RunDatabase(t.Context(), f.app, db); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.events, []string{"network ops-db-hello", "pull postgres:18.6", "run database ops-db-hello"}) {
		t.Errorf("events = %q, want network, image, then the container", f.events)
	}
	spec := f.dbSpec
	if spec.Image != "postgres:18.6" || spec.Network != "ops-db-hello" || spec.Volume != "ops-db-hello" ||
		spec.Env["POSTGRES_PASSWORD"] != "pw0123" || spec.Env["POSTGRES_USER"] != "app" || spec.Env["POSTGRES_DB"] != "app" ||
		spec.Labels[engine.LabelApp] != "hello" || spec.MemoryBytes != 256<<20 {
		t.Errorf("database spec = %+v", spec)
	}
}
