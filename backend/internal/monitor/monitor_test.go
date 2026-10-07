package monitor

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/docker"
	"github.com/sashasagebd/ops-dashboard/backend/internal/host"
)

// fakeDocker returns whatever its fields hold at the time of the call; tests
// change them between polls. It's locked so the Run test can use it from
// another goroutine.
type fakeDocker struct {
	mu         sync.Mutex
	containers []docker.Container
	listErr    error
	stats      map[string]docker.Stats // by ID; missing means an error
	statsCalls []string                // IDs, in call order
	listed     chan struct{}           // if non-nil, signalled on each list
}

func (f *fakeDocker) ListContainers(context.Context) ([]docker.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listed != nil {
		select {
		case f.listed <- struct{}{}:
		default:
		}
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	// A copy, as the real client returns a fresh slice each time; Poll sorts
	// it.
	return append([]docker.Container(nil), f.containers...), nil
}

func (f *fakeDocker) ContainerStats(_ context.Context, id string) (docker.Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statsCalls = append(f.statsCalls, id)
	s, ok := f.stats[id]
	if !ok {
		return docker.Stats{}, errors.New("no stats for " + id)
	}
	return s, nil
}

func (f *fakeDocker) set(fn func(f *fakeDocker)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// fakeHost returns its sample, or err if set. Tests change the fields between
// polls; no lock, as only the Run test uses another goroutine and it doesn't
// touch these.
type fakeHost struct {
	sample host.Sample
	err    error
	reads  int
}

func (h *fakeHost) Read() (host.Sample, error) {
	h.reads++
	return h.sample, h.err
}

var (
	mc  = docker.Container{ID: "m1", Name: "mc", State: "running"}
	bot = docker.Container{ID: "b1", Name: "discordbot", State: "running"}
)

// newTestMonitor returns a Monitor whose clock reads clock.
func newTestMonitor(f *fakeDocker, clock *time.Time) *Monitor {
	m := New(f, &fakeHost{})
	m.now = func() time.Time { return *clock }
	return m
}

func mustPoll(t *testing.T, m *Monitor) Snapshot {
	t.Helper()
	if err := m.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	snap, ok := m.Snapshot()
	if !ok {
		t.Fatal("Snapshot not ok after a successful poll")
	}
	return snap
}

func TestNoSnapshotBeforeFirstPoll(t *testing.T) {
	m := New(&fakeDocker{}, &fakeHost{})
	if _, ok := m.Snapshot(); ok {
		t.Error("Snapshot ok before any poll, want not ok")
	}
}

func TestFirstPoll(t *testing.T) {
	stopped := docker.Container{ID: "s1", Name: "old", State: "exited"}
	f := &fakeDocker{
		containers: []docker.Container{mc, stopped, bot},
		stats: map[string]docker.Stats{
			"m1": {CPUUsage: 100, SystemCPUUsage: 1000, MemoryUsed: 2 << 30, MemoryLimit: 16 << 30},
			"b1": {CPUUsage: 10, SystemCPUUsage: 1000, MemoryUsed: 80 << 20, MemoryLimit: 16 << 30},
		},
	}
	clock := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	m := newTestMonitor(f, &clock)

	snap := mustPoll(t, m)

	if !snap.UpdatedAt.Equal(clock) || snap.Stale {
		t.Errorf("UpdatedAt, Stale = %v, %v; want %v, false", snap.UpdatedAt, snap.Stale, clock)
	}
	var names []string
	for _, c := range snap.Containers {
		names = append(names, c.Name)
	}
	if want := []string{"discordbot", "mc", "old"}; !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v (sorted)", names, want)
	}

	got := snap.Containers[1] // mc
	if !got.HasStats || got.MemoryUsed != 2<<30 || got.MemoryLimit != 16<<30 {
		t.Errorf("mc stats = %+v, want memory 2 GiB of 16 GiB", got)
	}
	if got.HasCPU {
		t.Error("mc HasCPU on the first poll, want false (no previous sample)")
	}
	if old := snap.Containers[2]; old.HasStats {
		t.Error("stopped container HasStats, want false")
	}
	if want := []string{"b1", "m1"}; !slices.Equal(f.statsCalls, want) {
		t.Errorf("stats requested for %v, want only running containers %v", f.statsCalls, want)
	}
}

func TestCPUFromPreviousPoll(t *testing.T) {
	f := &fakeDocker{
		containers: []docker.Container{mc},
		stats:      map[string]docker.Stats{"m1": {CPUUsage: 100, SystemCPUUsage: 1000}},
	}
	clock := time.Now()
	m := newTestMonitor(f, &clock)
	mustPoll(t, m)

	// Between polls the host passes 400 units of CPU time and mc uses 100.
	f.set(func(f *fakeDocker) {
		f.stats["m1"] = docker.Stats{CPUUsage: 200, SystemCPUUsage: 1400}
	})
	snap := mustPoll(t, m)

	got := snap.Containers[0]
	if !got.HasCPU || got.CPUPercent != 25 {
		t.Errorf("CPU = %v (HasCPU %v), want 25%%", got.CPUPercent, got.HasCPU)
	}
}

func TestNoCPUAfterRestart(t *testing.T) {
	f := &fakeDocker{
		containers: []docker.Container{mc},
		stats:      map[string]docker.Stats{"m1": {CPUUsage: 100, SystemCPUUsage: 1000}},
	}
	clock := time.Now()
	m := newTestMonitor(f, &clock)
	mustPoll(t, m)

	// mc stops for a poll, then comes back. Its new counters happen to be
	// higher than the old sample, which would give a wrong CPU % if that
	// sample were still around.
	f.set(func(f *fakeDocker) { f.containers = []docker.Container{{ID: "m1", Name: "mc", State: "exited"}} })
	mustPoll(t, m)
	f.set(func(f *fakeDocker) {
		f.containers = []docker.Container{mc}
		f.stats["m1"] = docker.Stats{CPUUsage: 900, SystemCPUUsage: 1400}
	})
	snap := mustPoll(t, m)

	if snap.Containers[0].HasCPU {
		t.Error("HasCPU on the first poll after a restart, want false")
	}
}

func TestStatsErrorAffectsOnlyThatContainer(t *testing.T) {
	f := &fakeDocker{
		containers: []docker.Container{mc, bot},
		stats:      map[string]docker.Stats{"m1": {MemoryUsed: 1}}, // none for bot
	}
	clock := time.Now()
	m := newTestMonitor(f, &clock)

	snap := mustPoll(t, m)

	if snap.Containers[0].HasStats { // discordbot
		t.Error("discordbot HasStats despite a stats error, want false")
	}
	if !snap.Containers[1].HasStats { // mc
		t.Error("mc HasStats = false, want true")
	}
}

func TestListErrorKeepsLastSnapshotAsStale(t *testing.T) {
	f := &fakeDocker{
		containers: []docker.Container{mc},
		stats:      map[string]docker.Stats{"m1": {}},
	}
	first := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	clock := first
	m := newTestMonitor(f, &clock)
	mustPoll(t, m)

	clock = first.Add(5 * time.Second)
	f.set(func(f *fakeDocker) { f.listErr = errors.New("proxy unreachable") })
	if err := m.Poll(context.Background()); err == nil {
		t.Fatal("Poll succeeded, want error")
	}

	snap, ok := m.Snapshot()
	if !ok || !snap.Stale {
		t.Fatalf("ok, Stale = %v, %v; want true, true", ok, snap.Stale)
	}
	if !snap.UpdatedAt.Equal(first) || len(snap.Containers) != 1 {
		t.Errorf("snapshot = %+v, want the first poll's data (UpdatedAt %v)", snap, first)
	}

	// Docker comes back: fresh data, no longer stale.
	f.set(func(f *fakeDocker) { f.listErr = nil })
	if snap := mustPoll(t, m); snap.Stale || !snap.UpdatedAt.Equal(clock) {
		t.Errorf("after recovery Stale, UpdatedAt = %v, %v; want false, %v", snap.Stale, snap.UpdatedAt, clock)
	}
}

func TestListErrorBeforeAnySuccess(t *testing.T) {
	m := New(&fakeDocker{listErr: errors.New("proxy unreachable")}, &fakeHost{})
	if err := m.Poll(context.Background()); err == nil {
		t.Fatal("Poll succeeded, want error")
	}
	if _, ok := m.Snapshot(); ok {
		t.Error("Snapshot ok with no successful poll, want not ok")
	}
}

func TestRunPollsImmediatelyAndStops(t *testing.T) {
	f := &fakeDocker{listed: make(chan struct{}, 1)}
	m := New(f, &fakeHost{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	// An hour's interval: the only poll this test can see is the immediate
	// one.
	go func() {
		m.Run(ctx, time.Hour)
		close(done)
	}()

	select {
	case <-f.listed:
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't poll immediately")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't return after cancel")
	}
}

func TestHostStats(t *testing.T) {
	h := &fakeHost{sample: host.Sample{
		CPU:    host.CPUTimes{Busy: 1000, Total: 10_000},
		Memory: host.Memory{Total: 16 << 30, Available: 10 << 30},
		Disk:   host.Disk{Total: 250 << 30, Used: 50 << 30, Available: 187 << 30},
	}}
	m := New(&fakeDocker{containers: []docker.Container{mc}, stats: map[string]docker.Stats{"m1": {}}}, h)

	snap := mustPoll(t, m)

	if snap.Host == nil {
		t.Fatal("Host = nil, want host stats")
	}
	if snap.Host.Memory != h.sample.Memory || snap.Host.Disk != h.sample.Disk {
		t.Errorf("Host = %+v, want the sample's memory and disk", snap.Host)
	}
	if snap.Host.HasCPU {
		t.Error("Host HasCPU on the first poll, want false (no previous sample)")
	}

	// 400 ticks pass on the host, 100 of them busy.
	h.sample.CPU = host.CPUTimes{Busy: 1100, Total: 10_400}
	snap = mustPoll(t, m)

	if !snap.Host.HasCPU || snap.Host.CPUPercent != 25 {
		t.Errorf("Host CPU = %v (HasCPU %v), want 25%%", snap.Host.CPUPercent, snap.Host.HasCPU)
	}
}

func TestHostErrorKeepsContainers(t *testing.T) {
	h := &fakeHost{err: errors.New("no /proc/stat")}
	m := New(&fakeDocker{containers: []docker.Container{mc}, stats: map[string]docker.Stats{"m1": {}}}, h)

	snap := mustPoll(t, m)

	if snap.Host != nil {
		t.Errorf("Host = %+v, want nil after a read error", snap.Host)
	}
	if len(snap.Containers) != 1 || snap.Stale {
		t.Errorf("Containers, Stale = %v, %v; want mc, not stale", snap.Containers, snap.Stale)
	}
}

func TestHostCPUSpansAFailedRead(t *testing.T) {
	h := &fakeHost{sample: host.Sample{CPU: host.CPUTimes{Busy: 1000, Total: 10_000}}}
	m := New(&fakeDocker{}, h)
	mustPoll(t, m)

	h.err = errors.New("temporary")
	mustPoll(t, m)

	// The previous sample survived the failed read, so CPU is available
	// straight away, averaged over both intervals.
	h.err = nil
	h.sample.CPU = host.CPUTimes{Busy: 1200, Total: 10_800}
	snap := mustPoll(t, m)
	if snap.Host == nil || !snap.Host.HasCPU || snap.Host.CPUPercent != 25 {
		t.Errorf("Host = %+v, want CPU 25%% from the sample before the failure", snap.Host)
	}
}

func TestHostNotReadWhenListFails(t *testing.T) {
	h := &fakeHost{}
	m := New(&fakeDocker{listErr: errors.New("proxy unreachable")}, h)

	_ = m.Poll(context.Background())

	if h.reads != 0 {
		t.Errorf("host read %d times, want 0 (a failed poll publishes nothing)", h.reads)
	}
}

func TestPollRecordsHistory(t *testing.T) {
	f := &fakeDocker{
		containers: []docker.Container{mc},
		stats:      map[string]docker.Stats{"m1": {CPUUsage: 100, SystemCPUUsage: 1000, MemoryUsed: 1 << 30}},
	}
	h := &fakeHost{sample: host.Sample{
		CPU:    host.CPUTimes{Busy: 1000, Total: 10_000},
		Memory: host.Memory{Total: 16 << 30, Available: 12 << 30},
		Disk:   host.Disk{Used: 50 << 30},
	}}
	clock := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	m := New(f, h)
	m.now = func() time.Time { return clock }

	// Minute 0: first poll, so no CPU % yet.
	mustPoll(t, m)
	// Minute 1: mc is recreated by a deploy (new ID, same name); the host
	// passes 400 ticks, 100 busy.
	clock = clock.Add(time.Minute)
	f.set(func(f *fakeDocker) {
		f.containers = []docker.Container{{ID: "m2", Name: "mc", State: "running"}}
		f.stats = map[string]docker.Stats{"m2": {CPUUsage: 5, SystemCPUUsage: 2000, MemoryUsed: 1 << 30}}
	})
	h.sample.CPU = host.CPUTimes{Busy: 1100, Total: 10_400}
	mustPoll(t, m)
	// Minute 2: Docker unreachable for the whole minute.
	clock = clock.Add(time.Minute)
	f.set(func(f *fakeDocker) { f.listErr = errors.New("proxy unreachable") })
	_ = m.Poll(context.Background())

	v := m.History(3 * time.Minute)
	hostHist, containers := v.Host, v.Containers

	if got := hostHist.CPU; !got[1].OK || got[1].Avg != 25 || got[0].OK || got[2].OK {
		t.Errorf("host CPU = %+v, want only minute 1, at 25%%", got)
	}
	if got := hostHist.Memory[0]; !got.OK || got.Avg != 4<<30 {
		t.Errorf("host memory at minute 0 = %+v, want 4 GiB (total minus available)", got)
	}
	if got := hostHist.Disk[1]; !got.OK || got.Avg != 50<<30 {
		t.Errorf("host disk at minute 1 = %+v, want 50 GiB", got)
	}
	mem := containers["mc"].Memory
	if len(containers) != 1 || !mem[0].OK || !mem[1].OK || mem[2].OK {
		t.Errorf("containers = %+v, want one mc series with memory in minutes 0 and 1 across the recreate, and a gap for the failed poll", containers)
	}
}
