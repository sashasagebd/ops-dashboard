// Package history keeps recent resource usage in memory for the dashboard's
// trend lines.
//
// Samples arrive every poll (5s by default), far more often than a small
// chart can draw, so they're folded into fixed-size time buckets (a minute
// in production), each keeping the average and the maximum: an average alone
// would hide a short spike. Each series is a ring of buckets covering the
// retention period, so memory use is fixed and old data falls off on its own.
//
// Nothing is persisted: a restart (i.e. every redeploy) starts empty.
package history

import (
	"sync"
	"time"
)

// HostSample is one poll's reading of the host.
type HostSample struct {
	// HasCPU is false when there's no CPU % this poll (the first poll after
	// startup); that bucket's CPU is then left alone, not recorded as 0.
	HasCPU     bool
	CPUPercent float64
	MemoryUsed uint64 // bytes
	DiskUsed   uint64 // bytes
}

// ContainerSample is one poll's reading of a container.
type ContainerSample struct {
	// Name keys the history, not the ID: a Compose recreate (every deploy)
	// gives the container a new ID but keeps its name, and its trend line
	// should carry on across it.
	Name string

	HasCPU     bool // false until the container has two samples in a row
	CPUPercent float64

	HasMemory  bool // false when stopped, or its stats couldn't be read
	MemoryUsed uint64
}

// Point is one bucket of a series.
type Point struct {
	Start time.Time // the start of the bucket

	// OK is false when nothing was recorded in the bucket: the container was
	// stopped, the value wasn't available, or every poll in it failed. It's
	// a gap in the line, never a 0.
	OK       bool
	Avg, Max float64
}

// HostHistory is the host's series, each oldest first.
type HostHistory struct {
	CPU, Memory, Disk []Point
}

// ContainerHistory is a container's series, each oldest first.
type ContainerHistory struct {
	CPU, Memory []Point
}

// History is safe for concurrent use: the poller records while HTTP handlers
// read.
type History struct {
	bucket time.Duration
	size   int // buckets per series: retention / bucket

	mu         sync.Mutex
	recorded   bool  // whether latest is set
	latest     int64 // the newest bucket recorded into
	host       hostSeries
	containers map[string]*containerSeries
}

type hostSeries struct {
	cpu, memory, disk series
}

type containerSeries struct {
	cpu, memory series
	lastSeen    int64 // the last bucket the container was listed in
}

// New returns an empty History of retention/bucket buckets per series.
func New(bucket, retention time.Duration) *History {
	size := max(int(retention/bucket), 1)
	return &History{
		bucket:     bucket,
		size:       size,
		host:       hostSeries{cpu: newSeries(size), memory: newSeries(size), disk: newSeries(size)},
		containers: map[string]*containerSeries{},
	}
}

// Record adds one poll's readings, taken at t. host is nil if the host
// couldn't be read that poll.
//
// A container that hasn't been listed for the whole retention period is
// forgotten, so removed containers don't pile up. One that's merely stopped
// is still listed, so it keeps its history (with gaps).
func (h *History) Record(t time.Time, host *HostSample, containers []ContainerSample) {
	idx := h.index(t)

	h.mu.Lock()
	defer h.mu.Unlock()

	// If the clock steps backwards, writing into an older bucket would
	// overwrite the slot of a newer one in the ring. Dropping a few samples
	// until the clock catches up is the lesser evil.
	if h.recorded && idx < h.latest {
		return
	}
	h.recorded, h.latest = true, idx

	if host != nil {
		if host.HasCPU {
			h.host.cpu.add(idx, host.CPUPercent)
		}
		h.host.memory.add(idx, float64(host.MemoryUsed))
		h.host.disk.add(idx, float64(host.DiskUsed))
	}

	for _, c := range containers {
		s, ok := h.containers[c.Name]
		if !ok {
			s = &containerSeries{cpu: newSeries(h.size), memory: newSeries(h.size)}
			h.containers[c.Name] = s
		}
		s.lastSeen = idx
		if c.HasCPU {
			s.cpu.add(idx, c.CPUPercent)
		}
		if c.HasMemory {
			s.memory.add(idx, float64(c.MemoryUsed))
		}
	}

	for name, s := range h.containers {
		if idx-s.lastSeen >= int64(h.size) {
			delete(h.containers, name)
		}
	}
}

// View is a window of history. Every series in it has the same length, one
// point per Step from Start, oldest first.
type View struct {
	Start      time.Time // the start of the first bucket
	Step       time.Duration
	Host       HostHistory
	Containers map[string]ContainerHistory // by name; never nil
}

// Window returns the buckets covering the last window up to now, ending with
// the bucket now falls in (which may still be filling). window is rounded up
// to whole buckets, at least one and at most the retention period.
func (h *History) Window(now time.Time, window time.Duration) View {
	n := int((window + h.bucket - 1) / h.bucket)
	n = min(max(n, 1), h.size)
	last := h.index(now)
	first := last - int64(n) + 1

	h.mu.Lock()
	defer h.mu.Unlock()

	v := View{
		Start: time.Unix(0, first*int64(h.bucket)).UTC(),
		Step:  h.bucket,
		Host: HostHistory{
			CPU:    h.host.cpu.points(first, last, h.bucket),
			Memory: h.host.memory.points(first, last, h.bucket),
			Disk:   h.host.disk.points(first, last, h.bucket),
		},
		Containers: make(map[string]ContainerHistory, len(h.containers)),
	}
	for name, s := range h.containers {
		v.Containers[name] = ContainerHistory{
			CPU:    s.cpu.points(first, last, h.bucket),
			Memory: s.memory.points(first, last, h.bucket),
		}
	}
	return v
}

// index is the number of the bucket t falls in. Buckets are aligned to the
// Unix epoch, so with 1-minute buckets they start on the minute.
func (h *History) index(t time.Time) int64 {
	return t.UnixNano() / int64(h.bucket)
}

// series is a ring of buckets: bucket number i lives in slot i % len. A slot
// remembers which bucket it holds, so a slot last written a full lap ago
// reads as empty rather than as stale data, and gaps need no filling in.
type series []bucket

type bucket struct {
	idx      int64
	n        int // samples added; 0 means empty
	sum, max float64
}

func newSeries(size int) series { return make(series, size) }

func (s series) add(idx int64, v float64) {
	b := &s[idx%int64(len(s))]
	if b.n == 0 || b.idx != idx {
		*b = bucket{idx: idx, max: v}
	}
	b.n++
	b.sum += v
	b.max = max(b.max, v)
}

func (s series) points(first, last int64, size time.Duration) []Point {
	points := make([]Point, 0, last-first+1)
	for idx := first; idx <= last; idx++ {
		p := Point{Start: time.Unix(0, idx*int64(size)).UTC()}
		if b := s[idx%int64(len(s))]; b.n > 0 && b.idx == idx {
			p.OK, p.Avg, p.Max = true, b.sum/float64(b.n), b.max
		}
		points = append(points, p)
	}
	return points
}
