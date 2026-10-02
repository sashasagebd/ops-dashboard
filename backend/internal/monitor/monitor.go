// Package monitor polls Docker in the background and keeps the latest view of
// every container in memory, so HTTP requests never wait on Docker.
package monitor

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/docker"
)

// Docker is the Docker access the monitor needs. It's declared here, where
// it's used, so tests can pass a fake.
type Docker interface {
	ListContainers(ctx context.Context) ([]docker.Container, error)
	ContainerStats(ctx context.Context, id string) (docker.Stats, error)
}

// ContainerStatus is one container plus its latest resource usage.
type ContainerStatus struct {
	docker.Container

	// HasStats is false for stopped containers, and for running ones whose
	// stats couldn't be read this poll.
	HasStats    bool
	MemoryUsed  uint64
	MemoryLimit uint64

	// HasCPU is false until a container has two samples in a row: on the
	// first poll after startup, or after it (re)starts.
	HasCPU     bool
	CPUPercent float64 // share of the whole host, 0–100
}

// Snapshot is the result of the last successful poll.
type Snapshot struct {
	UpdatedAt  time.Time         // when that poll finished
	Containers []ContainerStatus // sorted by name

	// Stale is true when a later poll failed, so Containers may be out of
	// date. Showing slightly old data, clearly marked, beats an error page
	// while Docker is briefly unreachable.
	Stale bool
}

// Monitor holds the latest Snapshot. Create one with New, then call Run in a
// goroutine.
type Monitor struct {
	docker Docker
	now    func() time.Time // time.Now, replaceable in tests

	// prev holds each running container's last stats sample, for CPU %.
	// Only Poll touches it, and Poll isn't called concurrently, so it needs
	// no lock.
	prev map[string]docker.Stats

	mu     sync.RWMutex
	snap   Snapshot
	polled bool // whether any poll has ever succeeded
}

// New returns a Monitor that reads from d. It has no data until the first
// successful Poll.
func New(d Docker) *Monitor {
	return &Monitor{docker: d, now: time.Now, prev: map[string]docker.Stats{}}
}

// Snapshot returns the latest data. ok is false if no poll has succeeded yet.
//
// The returned Containers slice is shared with other callers and must not be
// modified (e.g. sorted). Each poll builds a new slice, so a snapshot never
// changes after it's been handed out.
func (m *Monitor) Snapshot() (snap Snapshot, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snap, m.polled
}

// Run polls immediately and then every interval, until ctx is cancelled.
// Each poll gets at most one interval to finish, so a slow Docker can't make
// polls pile up.
func (m *Monitor) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		pollCtx, cancel := context.WithTimeout(ctx, interval)
		if err := m.Poll(pollCtx); err != nil && ctx.Err() == nil {
			slog.Error("polling docker", "err", err)
		}
		cancel()

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Poll reads every container and its stats once and publishes a new
// Snapshot. If listing containers fails, the previous Snapshot is kept and
// marked stale. Poll must not be called concurrently with itself.
func (m *Monitor) Poll(ctx context.Context) error {
	containers, err := m.docker.ListContainers(ctx)
	if err != nil {
		m.mu.Lock()
		m.snap.Stale = true
		m.mu.Unlock()
		return err // already says "listing containers: ..."
	}

	// Sorted once here, before publishing: handlers share the slice, so
	// they can't sort it themselves without racing each other. By name, so
	// the page doesn't reshuffle when a container restarts.
	slices.SortFunc(containers, func(a, b docker.Container) int {
		return strings.Compare(a.Name, b.Name)
	})

	statuses := make([]ContainerStatus, 0, len(containers))
	next := make(map[string]docker.Stats, len(containers))
	for _, c := range containers {
		status := ContainerStatus{Container: c}
		if c.State == "running" {
			m.addStats(ctx, &status, next)
		}
		statuses = append(statuses, status)
	}
	// Replacing prev (rather than updating it) drops samples for containers
	// that stopped or were removed, so a restart never compares against a
	// sample from before it.
	m.prev = next

	m.mu.Lock()
	m.snap = Snapshot{UpdatedAt: m.now(), Containers: statuses}
	m.polled = true
	m.mu.Unlock()
	return nil
}

// addStats fills in s's resource usage and records the sample in next. A
// failure only affects this container, so it's logged rather than failing
// the whole poll.
func (m *Monitor) addStats(ctx context.Context, s *ContainerStatus, next map[string]docker.Stats) {
	stats, err := m.docker.ContainerStats(ctx, s.ID)
	if err != nil {
		slog.Warn("reading container stats", "container", s.Name, "err", err)
		return
	}
	next[s.ID] = stats

	s.HasStats = true
	s.MemoryUsed = stats.MemoryUsed
	s.MemoryLimit = stats.MemoryLimit
	if prev, ok := m.prev[s.ID]; ok {
		s.CPUPercent, s.HasCPU = docker.CPUPercent(prev, stats)
	}
}
