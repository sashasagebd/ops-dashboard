package history

import (
	"testing"
	"time"
)

// A small history keeps the tests readable: 1-minute buckets, 10 of them.
const (
	testBucket    = time.Minute
	testRetention = 10 * time.Minute
)

// t0 is on a minute boundary, so t0 + n minutes starts bucket n.
var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func minute(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }

func cpu(v float64) ContainerSample {
	return ContainerSample{Name: "mc", HasCPU: true, CPUPercent: v, HasMemory: true, MemoryUsed: 1 << 30}
}

// okPoints returns which points of ps are present, as a string of 'x' (OK)
// and '.' (gap), so a test can state the whole shape of a series at once.
func okPoints(ps []Point) string {
	b := make([]byte, len(ps))
	for i, p := range ps {
		b[i] = '.'
		if p.OK {
			b[i] = 'x'
		}
	}
	return string(b)
}

func TestBucketKeepsAverageAndMax(t *testing.T) {
	h := New(testBucket, testRetention)
	// Five polls in one minute, one of them a spike.
	for i, v := range []float64{10, 10, 90, 10, 30} {
		h.Record(t0.Add(time.Duration(i)*10*time.Second), nil, []ContainerSample{cpu(v)})
	}

	containers := h.Window(t0.Add(50*time.Second), testBucket).Containers

	got := containers["mc"].CPU
	if len(got) != 1 {
		t.Fatalf("got %d points, want 1", len(got))
	}
	if p := got[0]; !p.OK || p.Avg != 30 || p.Max != 90 || !p.Start.Equal(t0) {
		t.Errorf("point = %+v, want avg 30, max 90, starting %v", p, t0)
	}
}

func TestWindowShape(t *testing.T) {
	h := New(testBucket, testRetention)
	h.Record(minute(0), nil, []ContainerSample{cpu(1)})
	h.Record(minute(2), nil, []ContainerSample{cpu(2)})
	h.Record(minute(3), nil, []ContainerSample{cpu(3)})

	tests := []struct {
		name   string
		window time.Duration
		want   string // oldest first; the last point is minute 3's
	}{
		{"exact minutes", 4 * time.Minute, "x.xx"},
		{"rounded up to whole buckets", 90 * time.Second, "xx"},
		{"at least one bucket", 0, "x"},
		{"capped at retention", time.Hour, "......x.xx"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := h.Window(minute(3).Add(30*time.Second), tt.window)
			got := v.Containers["mc"].CPU
			if s := okPoints(got); s != tt.want {
				t.Errorf("points = %q, want %q", s, tt.want)
			}
			if !v.Start.Equal(got[0].Start) || v.Step != testBucket {
				t.Errorf("Start, Step = %v, %v; want the first point's %v, %v", v.Start, v.Step, got[0].Start, testBucket)
			}
			for i := 1; i < len(got); i++ {
				if d := got[i].Start.Sub(got[i-1].Start); d != testBucket {
					t.Fatalf("points %d and %d are %v apart, want %v", i-1, i, d, testBucket)
				}
			}
		})
	}
}

func TestMissingValuesAreGaps(t *testing.T) {
	h := New(testBucket, testRetention)
	// Minute 0: CPU not available yet (first poll). Minute 1: everything.
	// Minute 2: stopped, so no stats at all. Minute 3: host unreadable.
	h.Record(minute(0),
		&HostSample{MemoryUsed: 1, DiskUsed: 1},
		[]ContainerSample{{Name: "mc", HasMemory: true, MemoryUsed: 1}})
	h.Record(minute(1),
		&HostSample{HasCPU: true, CPUPercent: 5, MemoryUsed: 1, DiskUsed: 1},
		[]ContainerSample{cpu(5)})
	h.Record(minute(2),
		&HostSample{HasCPU: true, CPUPercent: 5, MemoryUsed: 1, DiskUsed: 1},
		[]ContainerSample{{Name: "mc"}})
	h.Record(minute(3), nil, []ContainerSample{cpu(5)})

	v := h.Window(minute(3), 4*time.Minute)
	host, containers := v.Host, v.Containers

	tests := []struct {
		name   string
		points []Point
		want   string
	}{
		{"host CPU", host.CPU, ".xx."},
		{"host memory", host.Memory, "xxx."},
		{"host disk", host.Disk, "xxx."},
		{"container CPU", containers["mc"].CPU, ".x.x"},
		{"container memory", containers["mc"].Memory, "xx.x"},
	}
	for _, tt := range tests {
		if s := okPoints(tt.points); s != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, s, tt.want)
		}
	}
	for _, p := range host.CPU {
		if !p.OK && (p.Avg != 0 || p.Max != 0) {
			t.Errorf("gap %+v has values, want zero values with OK false", p)
		}
	}
}

func TestRingDoesNotShowDataFromALapAgo(t *testing.T) {
	h := New(testBucket, testRetention)
	h.Record(minute(0), nil, []ContainerSample{cpu(99)})
	// Minute 10 lands in the same slot as minute 0. Nothing is recorded in
	// minutes 1-9, but mc is still listed, so it isn't forgotten.
	h.Record(minute(9), nil, []ContainerSample{{Name: "mc"}})
	h.Record(minute(10), nil, []ContainerSample{cpu(1)})

	containers := h.Window(minute(10), testRetention).Containers

	got := containers["mc"].CPU
	if s := okPoints(got); s != ".........x" {
		t.Fatalf("points = %q, want only minute 10", s)
	}
	if last := got[len(got)-1]; last.Avg != 1 || last.Max != 1 {
		t.Errorf("minute 10 = %+v, want 1 (not mixed with minute 0's 99)", last)
	}

	// Reading well after the last record: everything has aged out.
	containers = h.Window(minute(25), testRetention).Containers
	if s := okPoints(containers["mc"].CPU); s != ".........." {
		t.Errorf("points 15 minutes later = %q, want all gaps", s)
	}
}

func TestContainerForgottenAfterRetention(t *testing.T) {
	h := New(testBucket, testRetention)
	h.Record(minute(0), nil, []ContainerSample{cpu(1), {Name: "old"}})

	h.Record(minute(9), nil, []ContainerSample{cpu(1)})
	if containers := h.Window(minute(9), testRetention).Containers; len(containers) != 2 {
		t.Fatalf("after 9 minutes got %d containers, want 2 (old is still within retention)", len(containers))
	}

	h.Record(minute(10), nil, []ContainerSample{cpu(1)})
	containers := h.Window(minute(10), testRetention).Containers
	if _, ok := containers["old"]; ok || len(containers) != 1 {
		t.Errorf("after 10 minutes got %v, want only mc (old unlisted for the whole retention)", containers)
	}
}

func TestClockGoingBackwardsIsIgnored(t *testing.T) {
	h := New(testBucket, testRetention)
	h.Record(minute(5), nil, []ContainerSample{cpu(50)})
	// minute(-5) shares minute 5's slot; writing it would erase minute 5.
	h.Record(minute(-5), nil, []ContainerSample{cpu(1)})

	containers := h.Window(minute(5), testBucket).Containers
	if p := containers["mc"].CPU[0]; !p.OK || p.Avg != 50 {
		t.Errorf("minute 5 = %+v, want 50 untouched", p)
	}
}

func TestEmptyHistory(t *testing.T) {
	h := New(testBucket, testRetention)

	v := h.Window(t0, 3*time.Minute)
	host, containers := v.Host, v.Containers

	if containers == nil || len(containers) != 0 {
		t.Errorf("containers = %#v, want an empty, non-nil map", containers)
	}
	if s := okPoints(host.CPU); s != "..." {
		t.Errorf("host CPU = %q, want three gaps", s)
	}
}
