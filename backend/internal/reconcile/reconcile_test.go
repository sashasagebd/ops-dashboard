package reconcile

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/deploy"
	"github.com/sashasagebd/ops-dashboard/backend/internal/engine"
	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

// fakes is the store, Docker and the deployer at once, recording what the
// reconciler did as "actions".
type fakes struct {
	apps       []store.App
	appsLater  []store.App // what Apps returns from its second call on, if set
	appsCalls  int
	live       map[int64]store.Deployment // by app ID
	containers []engine.ManagedContainer
	exit       engine.ExitInfo
	logs       []engine.LogLine
	startErr   error
	removeErr  error

	locked      map[int64]bool // apps with a deploy running
	recreateErr error
	routesDiff  bool // SyncRoutes finds Caddy different
	routesErr   error
	storeErr    error

	actions []string
	events  []store.NewEvent
	pruned  []time.Time
}

func (f *fakes) Apps(context.Context) ([]store.App, error) {
	f.appsCalls++
	if f.storeErr != nil {
		return nil, f.storeErr
	}
	if f.appsCalls > 1 && f.appsLater != nil {
		return f.appsLater, nil
	}
	return f.apps, nil
}

func (f *fakes) CurrentDeployment(_ context.Context, appID int64) (store.Deployment, error) {
	d, ok := f.live[appID]
	if !ok {
		return store.Deployment{}, store.ErrNotFound
	}
	return d, nil
}

func (f *fakes) AddEvent(_ context.Context, e store.NewEvent) error {
	f.events = append(f.events, e)
	return nil
}

func (f *fakes) PruneEvents(_ context.Context, before time.Time) (int64, error) {
	f.pruned = append(f.pruned, before)
	return 0, nil
}

func (f *fakes) ManagedContainers(_ context.Context, app string) ([]engine.ManagedContainer, error) {
	var out []engine.ManagedContainer
	for _, c := range f.containers {
		if app == "" || c.Labels[engine.LabelApp] == app {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakes) Exit(context.Context, string) (engine.ExitInfo, error) { return f.exit, nil }

func (f *fakes) TailLogs(_ context.Context, _ string, n int) ([]engine.LogLine, error) {
	return f.logs[max(0, len(f.logs)-n):], nil
}

func (f *fakes) Start(_ context.Context, id string) error {
	f.actions = append(f.actions, "start "+id)
	if f.startErr != nil {
		return f.startErr
	}
	f.setState(id, "running")
	return nil
}

func (f *fakes) Remove(_ context.Context, id string) error {
	f.actions = append(f.actions, "remove "+id)
	if f.removeErr != nil {
		return f.removeErr
	}
	f.containers = slices.DeleteFunc(f.containers, func(c engine.ManagedContainer) bool { return c.ID == id })
	return nil
}

func (f *fakes) TryLockApp(appID int64) (func(), bool) {
	if f.locked[appID] {
		return nil, false
	}
	return func() {}, true
}

func (f *fakes) Recreate(_ context.Context, app store.App, dep store.Deployment) error {
	f.actions = append(f.actions, fmt.Sprintf("recreate %s #%d", app.Name, dep.ID))
	if f.recreateErr != nil {
		return f.recreateErr
	}
	f.containers = append(f.containers, container(app.Name, dep.ID, "running"))
	return nil
}

func (f *fakes) SyncRoutes(context.Context) (bool, error) {
	if f.routesDiff {
		f.actions = append(f.actions, "apply routes")
	}
	return f.routesDiff, f.routesErr
}

func (f *fakes) setState(id, state string) {
	for i := range f.containers {
		if f.containers[i].ID == id {
			f.containers[i].State = state
		}
	}
}

func container(app string, deployment int64, state string) engine.ManagedContainer {
	id := fmt.Sprintf("%s-%d", app, deployment)
	return engine.ManagedContainer{
		ID: id, Name: deploy.ContainerName(app, deployment), State: state,
		Labels: map[string]string{engine.LabelApp: app, engine.LabelDeployment: fmt.Sprint(deployment)},
	}
}

var hello = store.App{ID: 1, Name: "hello"}

// newFakes: hello is live as deployment 6, running.
func newFakes() *fakes {
	return &fakes{
		apps:       []store.App{hello},
		live:       map[int64]store.Deployment{1: {ID: 6, AppID: 1, Status: store.StatusHealthy, ImageTag: "ops-platform/hello:abc"}},
		containers: []engine.ManagedContainer{container("hello", 6, "running")},
		locked:     map[int64]bool{},
	}
}

// clock is a fake time the tests move on by hand.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock               { return &clock{t: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)} }

func kinds(es []store.NewEvent) []string {
	var out []string
	for _, e := range es {
		out = append(out, string(e.Kind))
	}
	return out
}

func (f *fakes) reconciler(c *clock) *Reconciler {
	r := New(Config{
		Interval: time.Second, MinBackoff: 10 * time.Second, MaxBackoff: 40 * time.Second,
		StableAfter: 5 * time.Minute, KeepEvents: 30 * 24 * time.Hour, OutputLines: 2,
	}, f, f, f)
	r.now = c.now
	return r
}

func TestNothingToDo(t *testing.T) {
	f := newFakes()
	if err := f.reconciler(newClock()).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.actions) != 0 || len(f.events) != 0 {
		t.Errorf("actions %q, events %v; want none", f.actions, kinds(f.events))
	}
}

func TestRestartsAStoppedLiveContainer(t *testing.T) {
	f := newFakes()
	f.containers[0].State = "exited" // docker kill, a crash, or a reboot
	f.exit = engine.ExitInfo{ExitCode: 137}
	f.logs = []engine.LogLine{{Line: "listening"}, {Line: "GET /"}, {Line: "panic: nil map"}}
	if err := f.reconciler(newClock()).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.actions, []string{"start hello-6"}) {
		t.Errorf("actions = %q, want hello-6 started", f.actions)
	}
	if len(f.events) != 1 {
		t.Fatalf("events = %+v, want one", f.events)
	}
	e := f.events[0]
	if e.Kind != store.EventRestarted || e.AppID != 1 ||
		e.Message != "Restarted hello: its container app-hello-6 had exited with code 137" ||
		!slices.Equal(e.Output, []string{"GET /", "panic: nil map"}) { // the last OutputLines
		t.Errorf("event = %+v", e)
	}
}

func TestOutOfMemoryIsSaid(t *testing.T) {
	f := newFakes()
	f.containers[0].State = "exited"
	f.exit = engine.ExitInfo{ExitCode: 137, OOMKilled: true}
	f.reconciler(newClock()).Pass(t.Context())
	if len(f.events) != 1 || !strings.Contains(f.events[0].Message, "more than its memory limit") {
		t.Errorf("events = %+v, want the restart to say it ran out of memory", f.events)
	}
}

func TestCrashLoopsBackOff(t *testing.T) {
	f := newFakes()
	c := newClock()
	r := f.reconciler(c)
	crash := func() { f.setState("hello-6", "exited") }

	// Restarted at once, then not again until the backoff has passed:
	// 10s, 20s, 40s, then capped at 40s.
	var starts []time.Duration
	start := c.t
	for range 60 {
		crash()
		before := len(f.actions)
		r.Pass(t.Context())
		if len(f.actions) > before {
			starts = append(starts, c.t.Sub(start))
		}
		c.add(5 * time.Second)
	}
	want := []time.Duration{0, 10 * time.Second, 30 * time.Second, 70 * time.Second, 110 * time.Second,
		150 * time.Second, 190 * time.Second, 230 * time.Second, 270 * time.Second}
	if !slices.Equal(starts, want) {
		t.Errorf("restarted at %v,\n want %v", starts, want)
	}
	// From the third, it's called crash-looping.
	got := kinds(f.events)
	if got[0] != "restarted" || got[1] != "restarted" || got[2] != "crash_looping" {
		t.Errorf("events = %v, want two restarts, then crash-looping", got)
	}
	if !strings.Contains(f.events[2].Message, "restarted 3 times") {
		t.Errorf("crash-looping message = %q", f.events[2].Message)
	}

	// Running long enough forgives it: the next crash restarts at once,
	// and counts from one again.
	f.setState("hello-6", "running")
	c.add(5 * time.Minute)
	r.Pass(t.Context())
	crash()
	before := len(f.actions)
	r.Pass(t.Context())
	if len(f.actions) != before+1 || f.events[len(f.events)-1].Kind != store.EventRestarted {
		t.Errorf("after running stably, a crash wasn't restarted at once as a plain restart: %v", kinds(f.events))
	}
}

func TestANewLiveDeploymentStartsAfresh(t *testing.T) {
	f := newFakes()
	c := newClock()
	r := f.reconciler(c)
	f.containers[0].State = "exited"
	r.Pass(t.Context())
	// Deployment 7 goes live and crashes straight away: not held back by
	// 6's backoff.
	f.live[1] = store.Deployment{ID: 7, AppID: 1, Status: store.StatusHealthy}
	f.containers = []engine.ManagedContainer{container("hello", 7, "exited")}
	r.Pass(t.Context())
	if !slices.Equal(f.actions, []string{"start hello-6", "start hello-7"}) {
		t.Errorf("actions = %q", f.actions)
	}
}

func TestRecreatesAMissingLiveContainer(t *testing.T) {
	f := newFakes()
	f.containers = nil // docker rm -f
	if err := f.reconciler(newClock()).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.actions, []string{"recreate hello #6"}) || len(f.events) != 1 ||
		f.events[0].Kind != store.EventRecreated || !strings.Contains(f.events[0].Message, "app-hello-6") {
		t.Errorf("actions %q, events %+v; want hello #6 recreated", f.actions, f.events)
	}
}

func TestADeadContainerIsReplaced(t *testing.T) {
	f := newFakes()
	f.containers[0].State = "dead"
	f.reconciler(newClock()).Pass(t.Context())
	if !slices.Equal(f.actions, []string{"remove hello-6", "recreate hello #6"}) {
		t.Errorf("actions = %q, want it removed, then recreated", f.actions)
	}
}

func TestAFailingFixIsRecordedOnce(t *testing.T) {
	f := newFakes()
	f.containers = nil
	f.recreateErr = fmt.Errorf("wrapped: %w", deploy.ErrImageGone)
	r := f.reconciler(newClock())
	for range 3 {
		r.Pass(t.Context())
	}
	// Tried every pass, recorded once.
	if len(f.actions) != 3 || len(f.events) != 1 || f.events[0].Kind != store.EventFixFailed ||
		!strings.Contains(f.events[0].Message, "its image no longer exists") {
		t.Errorf("actions %q, events %+v; want 3 tries and one fix_failed event saying why", f.actions, f.events)
	}
	// Once it works, a later failure is recorded again.
	f.recreateErr = nil
	r.Pass(t.Context())
	f.containers = nil
	f.recreateErr = errors.New("docker unreachable")
	r.Pass(t.Context())
	if got := kinds(f.events); !slices.Equal(got, []string{"fix_failed", "recreated", "fix_failed"}) {
		t.Errorf("events = %v", got)
	}
	// The error's detail stays out of the event.
	if strings.Contains(f.events[2].Message, "unreachable") {
		t.Errorf("event message %q leaks the error", f.events[2].Message)
	}
}

func TestOrphansAreRemoved(t *testing.T) {
	f := newFakes()
	f.containers = append(f.containers,
		container("hello", 3, "exited"),  // an old failure's
		container("hello", 8, "running"), // a deploy cut off by a crash
	)
	f.apps = append(f.apps, store.App{ID: 2, Name: "api"}) // registered, nothing live
	f.containers = append(f.containers, container("api", 4, "running"))
	f.reconciler(newClock()).Pass(t.Context())

	if !slices.Equal(f.actions, []string{"remove hello-3", "remove hello-8", "remove api-4"}) {
		t.Errorf("actions = %q, want every container but the live one removed", f.actions)
	}
	if len(f.events) != 3 || f.events[0].Kind != store.EventRemovedOrphan || f.events[2].AppID != 2 {
		t.Errorf("events = %+v", f.events)
	}
}

func TestAppWithADeployRunningIsLeftAlone(t *testing.T) {
	f := newFakes()
	f.locked[1] = true
	f.containers = []engine.ManagedContainer{container("hello", 6, "exited"), container("hello", 7, "running")}
	f.reconciler(newClock()).Pass(t.Context())
	if len(f.actions) != 0 {
		t.Errorf("actions = %q; a running deploy owns the app's containers", f.actions)
	}
}

func TestContainersOfUnknownApps(t *testing.T) {
	f := newFakes()
	f.containers = append(f.containers, container("gone", 2, "running"), container("brand-new", 1, "running"))
	// brand-new was registered (and its first deploy started) after the
	// first list of apps: it's in the second.
	f.appsLater = []store.App{hello, {ID: 3, Name: "brand-new"}}
	f.reconciler(newClock()).Pass(t.Context())
	if !slices.Equal(f.actions, []string{"remove gone-2"}) {
		t.Errorf("actions = %q, want only the deleted app's container removed", f.actions)
	}
	if len(f.events) != 1 || f.events[0].AppID != 0 {
		t.Errorf("events = %+v, want one, about no app", f.events)
	}
}

func TestRoutes(t *testing.T) {
	f := newFakes()
	f.routesDiff = true // a route was deleted in Caddy
	if err := f.reconciler(newClock()).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.events) != 1 || f.events[0].Kind != store.EventRoutesReapplied {
		t.Errorf("events = %+v, want routes re-applied", f.events)
	}

	// Caddy unreachable: the pass fails, with no event.
	f = newFakes()
	f.routesErr = errors.New("dial tcp 10.203.0.2:2019: connection refused")
	if err := f.reconciler(newClock()).Pass(t.Context()); err == nil || len(f.events) != 0 {
		t.Errorf("Pass = %v, events %+v; want the error, no event", err, f.events)
	}
}

func TestNoOrphanRemovedWhileRoutesAreUnknown(t *testing.T) {
	// A deploy made 7 live, but its route switch failed: Caddy still routes
	// to 6, which must keep running until the routes are fixed. Caddy's
	// admin API is still failing.
	f := newFakes()
	f.live[1] = store.Deployment{ID: 7, AppID: 1, Status: store.StatusHealthy}
	f.containers = append(f.containers, container("hello", 7, "exited"), container("gone", 2, "running"))
	f.routesErr = errors.New("caddy unreachable")
	r := f.reconciler(newClock())
	if err := r.Pass(t.Context()); err == nil {
		t.Error("Pass succeeded with Caddy unreachable")
	}
	// The live container is still started; nothing is removed.
	if !slices.Equal(f.actions, []string{"start hello-7"}) {
		t.Errorf("actions = %q, want only hello-7 started", f.actions)
	}

	// Once Caddy answers (and has been put right), the orphans go.
	f.routesErr = nil
	r.Pass(t.Context())
	if !slices.Equal(f.actions, []string{"start hello-7", "remove hello-6", "remove gone-2"}) {
		t.Errorf("actions = %q, want the orphans removed once routes are right", f.actions)
	}
}

func TestPassCarriesOnAndReportsErrors(t *testing.T) {
	f := newFakes()
	f.storeErr = errors.New("database down")
	if err := f.reconciler(newClock()).Pass(t.Context()); err == nil {
		t.Error("Pass succeeded with the database down")
	}
}

func TestEventsArePrunedHourly(t *testing.T) {
	f := newFakes()
	c := newClock()
	r := f.reconciler(c)
	for range 3 {
		r.Pass(t.Context())
		c.add(20 * time.Minute)
	}
	r.Pass(t.Context()) // an hour after the first
	if len(f.pruned) != 2 || !f.pruned[0].Equal(newClock().t.Add(-30*24*time.Hour)) {
		t.Errorf("pruned at %v, want twice (an hour apart), keeping 30 days", f.pruned)
	}
}
