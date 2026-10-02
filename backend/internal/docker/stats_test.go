package docker

import (
	"context"
	"errors"
	"math"
	"net/http"
	"testing"
	"time"
)

// statsCgroupV2 is trimmed from a real one-shot stats response on cgroup v2
// (current Ubuntu). One-shot leaves precpu_stats empty.
const statsCgroupV2 = `{
  "read": "2026-10-01T12:00:00.5Z",
  "preread": "0001-01-01T00:00:00Z",
  "cpu_stats": {
    "cpu_usage": {"total_usage": 52000000000, "usage_in_kernelmode": 9000000000, "usage_in_usermode": 43000000000},
    "system_cpu_usage": 9100000000000,
    "online_cpus": 4,
    "throttling_data": {"periods": 0, "throttled_periods": 0, "throttled_time": 0}
  },
  "precpu_stats": {"cpu_usage": {"total_usage": 0}, "throttling_data": {}},
  "memory_stats": {
    "usage": 2147483648,
    "limit": 16596942848,
    "stats": {"active_file": 104857600, "inactive_file": 536870912, "anon": 1400000000}
  },
  "networks": {"eth0": {"rx_bytes": 1000, "tx_bytes": 2000}}
}`

// statsCgroupV1 is the older layout, with "total_inactive_file".
const statsCgroupV1 = `{
  "read": "2026-10-01T12:00:00Z",
  "cpu_stats": {
    "cpu_usage": {"total_usage": 1000, "percpu_usage": [400, 300, 200, 100]},
    "system_cpu_usage": 50000
  },
  "memory_stats": {
    "usage": 1000,
    "limit": 8000,
    "stats": {"total_inactive_file": 300, "inactive_file": 999}
  }
}`

func TestContainerStats(t *testing.T) {
	tests := []struct {
		name string
		body string
		want Stats
	}{
		{
			name: "cgroup v2",
			body: statsCgroupV2,
			want: Stats{
				Read:           time.Date(2026, 10, 1, 12, 0, 0, 500_000_000, time.UTC),
				CPUUsage:       52_000_000_000,
				SystemCPUUsage: 9_100_000_000_000,
				MemoryUsed:     2147483648 - 536870912,
				MemoryLimit:    16596942848,
			},
		},
		{
			name: "cgroup v1",
			body: statsCgroupV1,
			want: Stats{
				Read:           time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
				CPUUsage:       1000,
				SystemCPUUsage: 50000,
				MemoryUsed:     700, // total_inactive_file wins on cgroup v1
				MemoryLimit:    8000,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/containers/8dfafdbc3a40/stats" {
					t.Errorf("path = %s, want /containers/8dfafdbc3a40/stats", r.URL.Path)
				}
				q := r.URL.Query()
				if q.Get("stream") != "false" || q.Get("one-shot") != "true" {
					t.Errorf("query = %s, want stream=false and one-shot=true", r.URL.RawQuery)
				}
				_, _ = w.Write([]byte(tt.body))
			})

			got, err := c.ContainerStats(context.Background(), "8dfafdbc3a40")
			if err != nil {
				t.Fatalf("ContainerStats: %v", err)
			}
			if !got.Read.Equal(tt.want.Read) {
				t.Errorf("Read = %v, want %v", got.Read, tt.want.Read)
			}
			got.Read, tt.want.Read = time.Time{}, time.Time{}
			if got != tt.want {
				t.Errorf("ContainerStats =\n  %+v\nwant\n  %+v", got, tt.want)
			}
		})
	}
}

func TestContainerStatsNotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"No such container: gone"}`))
	})

	_, err := c.ContainerStats(context.Background(), "gone")
	if !errors.Is(err, errNotFound) {
		t.Errorf("error = %v, want errNotFound", err)
	}
}

func TestMemoryUsed(t *testing.T) {
	tests := []struct {
		name  string
		usage uint64
		stats map[string]uint64
		want  uint64
	}{
		{name: "cgroup v2 subtracts inactive_file", usage: 1000, stats: map[string]uint64{"inactive_file": 300}, want: 700},
		{name: "no stats map", usage: 1000, stats: nil, want: 1000},
		{name: "cache larger than usage is ignored", usage: 1000, stats: map[string]uint64{"inactive_file": 5000}, want: 1000},
		{name: "stopped container is zero", usage: 0, stats: nil, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := memoryUsed(tt.usage, tt.stats); got != tt.want {
				t.Errorf("memoryUsed = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCPUPercent(t *testing.T) {
	// prev is a sample on a 4-core host; each case varies cur. Between the
	// samples the host's 4 cores together pass 400 units of CPU time (100
	// each).
	prev := Stats{CPUUsage: 10_000, SystemCPUUsage: 1_000_000}

	tests := []struct {
		name   string
		cur    Stats
		want   float64
		wantOK bool
	}{
		{
			name:   "one full core of four is 25% of the host",
			cur:    Stats{CPUUsage: 10_100, SystemCPUUsage: 1_000_400},
			want:   25,
			wantOK: true,
		},
		{
			name:   "all four cores is 100%",
			cur:    Stats{CPUUsage: 10_400, SystemCPUUsage: 1_000_400},
			want:   100,
			wantOK: true,
		},
		{
			name:   "idle",
			cur:    Stats{CPUUsage: 10_000, SystemCPUUsage: 1_000_400},
			want:   0,
			wantOK: true,
		},
		{
			name: "container restarted, so its counter went backwards",
			cur:  Stats{CPUUsage: 50, SystemCPUUsage: 1_000_400},
		},
		{
			name: "no host time passed",
			cur:  Stats{CPUUsage: 10_100, SystemCPUUsage: 1_000_000},
		},
		{
			name: "stopped container (all zero)",
			cur:  Stats{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := CPUPercent(prev, tt.cur)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("CPUPercent = %v, want %v", got, tt.want)
			}
		})
	}
}
