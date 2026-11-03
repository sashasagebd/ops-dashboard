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
	containers  []engine.ManagedContainer
	removed     []string
	onProbe     func() // hook, e.g. to cancel mid-check
}

func newFakes() *fakes {
	return &fakes{
		app: store.App{
			ID: 1, Name: "hello", RepoURL: "https://github.com/o/hello", Port: 8080,
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

func (f *fakes) Build(_ context.Context, buildContext io.Reader, tag string, _ map[string]string) error {
	f.event("build %s", tag)
	// Like Docker's client: a broken upload comes back as an error of its
	// own, which may or may not wrap the reader's.
	if _, err := io.ReadAll(buildContext); err != nil {
		return errors.New("error during connect: unexpected EOF")
	}
	return f.buildErr
}

func (f *fakes) Run(_ context.Context, spec engine.RunSpec) (string, error) {
	f.event("run %s", spec.Name)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runSpec = spec
	return "new", nil
}

func (f *fakes) Remove(_ context.Context, id string) error {
	f.event("remove %s", id)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakes) ManagedContainers(context.Context, string) ([]engine.ManagedContainer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	new := engine.ManagedContainer{ID: "new", Name: "app-hello-7", Labels: map[string]string{engine.LabelDeployment: "7"}}
	return append(slices.Clone(f.containers), new), nil
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

func (f *fakes) deployer(ctx context.Context) *Deployer {
	return New(ctx, Config{
		AppsNetwork:    "platform-apps",
		DeployTimeout:  time.Minute,
		HealthTimeout:  200 * time.Millisecond,
		HealthInterval: time.Millisecond,
	}, f, f, f, f)
}

// deployAndWait starts a deploy and waits for it to finish.
func deployAndWait(t *testing.T, f *fakes, ctx context.Context) store.Deployment {
	t.Helper()
	d := f.deployer(ctx)
	dep, err := d.Start(t.Context(), 1, sha)
	if err != nil {
		t.Fatal(err)
	}
	d.Wait()
	return dep
}

func TestDeploySuccess(t *testing.T) {
	f := newFakes()
	f.probeStatus = func(n int) int { return []int{502, 502, 200}[min(n, 2)] } // up on the third try
	dep := deployAndWait(t, f, t.Context())

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
		name       string
		setup      func(f *fakes)
		wantReason string
		wantRemove []string
	}{
		{
			name:       "build fails",
			setup:      func(f *fakes) { f.buildErr = &engine.BuildError{Message: "npm ci returned 1"} },
			wantReason: "build failed: npm ci returned 1",
		},
		{
			name:       "repo not found",
			setup:      func(f *fakes) { f.sourceErr = fmt.Errorf("fetching: %w", source.ErrNotFound) },
			wantReason: source.ErrNotFound.Error(),
		},
		{
			name:       "source directory missing",
			setup:      func(f *fakes) { f.streamErr = source.ErrDirNotFound },
			wantReason: source.ErrDirNotFound.Error(),
		},
		{
			name:       "never healthy",
			setup:      func(f *fakes) { f.probeStatus = func(int) int { return 502 } },
			wantReason: "health check failed: GET /healthz didn't succeed within 200ms (last result: 502)",
			wantRemove: []string{"new"},
		},
		{
			name:       "internal error stays generic",
			setup:      func(f *fakes) { f.buildErr = errors.New("dial tcp 10.0.0.3:2375: connection refused") },
			wantReason: "internal error; see the platform's logs",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakes()
			tt.setup(f)
			dep := deployAndWait(t, f, t.Context())

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
	dep := deployAndWait(t, f, t.Context())

	// The deployment is healthy (it is), but Caddy still points at the old
	// container, so that must keep running.
	if f.statuses[dep.ID] != store.StatusHealthy {
		t.Errorf("status = %s, want healthy", f.statuses[dep.ID])
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
	dep := deployAndWait(t, f, ctx)

	if f.statuses[dep.ID] != store.StatusFailed || !strings.HasPrefix(f.failReason, "interrupted") {
		t.Errorf("status %s, reason %q; want failed, interrupted", f.statuses[dep.ID], f.failReason)
	}
	if !slices.Contains(f.removed, "new") {
		t.Errorf("removed %v, want the half-started container removed", f.removed)
	}
}

func TestRemoveOrphansKeepsOnlyLiveDeployments(t *testing.T) {
	f := newFakes() // deployment 6 is live; containers for 6, 3 and 7 exist
	if err := f.deployer(t.Context()).RemoveOrphans(t.Context()); err != nil {
		t.Fatal(err)
	}
	slices.Sort(f.removed)
	if !slices.Equal(f.removed, []string{"new", "stale3"}) {
		t.Errorf("removed %v, want the non-live ones (new, stale3) and not old6", f.removed)
	}
}

func TestStartRefusedWhileDeployInProgress(t *testing.T) {
	f := newFakes()
	f.createErr = store.ErrDeployInProgress
	d := f.deployer(t.Context())

	if _, err := d.Start(t.Context(), 1, sha); !errors.Is(err, store.ErrDeployInProgress) {
		t.Errorf("Start error = %v, want ErrDeployInProgress", err)
	}
	d.Wait()
	if len(f.events) != 0 {
		t.Errorf("events %v; nothing should run", f.events)
	}
}
