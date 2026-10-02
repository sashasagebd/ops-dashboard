package docker

import (
	"context"
	"fmt"
	"net/url"
	"time"
)

// Stats is one resource-usage sample for a container.
//
// The CPU fields are cumulative counters, so a single sample says nothing
// about current load; CPUPercent compares two.
type Stats struct {
	Read time.Time // when Docker took the sample

	CPUUsage       uint64 // CPU time used by the container so far, in ns
	SystemCPUUsage uint64 // CPU time of the whole host so far, all cores summed, in ns

	MemoryUsed  uint64 // bytes, excluding file cache the kernel can reclaim
	MemoryLimit uint64 // bytes; the host's total memory if no limit is set
}

// apiStats is the subset of `GET /containers/{id}/stats` we use.
type apiStats struct {
	Read     time.Time `json:"read"`
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
}

// ContainerStats returns one resource-usage sample for a running container.
// (Docker answers for stopped containers too, but with every value zero.)
//
// It uses one-shot mode: by default Docker waits about a second to take a
// second CPU sample for each container, which would make a poll of N
// containers take N seconds. One-shot answers immediately, and CPUPercent
// compares against the previous poll's sample instead.
func (c *Client) ContainerStats(ctx context.Context, id string) (Stats, error) {
	var raw apiStats
	path := "/containers/" + url.PathEscape(id) + "/stats?stream=false&one-shot=true"
	if err := c.get(ctx, path, &raw); err != nil {
		return Stats{}, fmt.Errorf("container %s stats: %w", id, err)
	}

	return Stats{
		Read:           raw.Read,
		CPUUsage:       raw.CPUStats.CPUUsage.TotalUsage,
		SystemCPUUsage: raw.CPUStats.SystemCPUUsage,
		MemoryUsed:     memoryUsed(raw.MemoryStats.Usage, raw.MemoryStats.Stats),
		MemoryLimit:    raw.MemoryStats.Limit,
	}, nil
}

// memoryUsed subtracts inactive file cache from usage, the same way
// `docker stats` does. Raw usage counts cached file pages the kernel can drop
// at any time, so it makes e.g. Minecraft look far bigger than it is.
//
// The key is "inactive_file" on cgroup v2 (current Ubuntu) and
// "total_inactive_file" on cgroup v1.
func memoryUsed(usage uint64, stats map[string]uint64) uint64 {
	for _, key := range []string{"total_inactive_file", "inactive_file"} {
		if cache, ok := stats[key]; ok && cache < usage {
			return usage - cache
		}
	}
	return usage
}

// CPUPercent returns the container's CPU use between two samples as a share
// of the whole host: 100% means every core is busy, so it's on the same
// scale as host CPU. (`docker stats` instead counts 100% per core, which
// would be this times the number of cores.)
//
// ok is false when there's no meaningful answer: a counter went backwards
// (the container restarted between the samples), or no host time passed.
func CPUPercent(prev, cur Stats) (pct float64, ok bool) {
	if cur.CPUUsage < prev.CPUUsage || cur.SystemCPUUsage <= prev.SystemCPUUsage {
		return 0, false
	}
	// The counters are uint64; the checks above make sure neither
	// subtraction wraps around. SystemCPUUsage sums every core, so the ratio
	// is already a share of the whole host.
	cpuDelta := float64(cur.CPUUsage - prev.CPUUsage)
	systemDelta := float64(cur.SystemCPUUsage - prev.SystemCPUUsage)
	return cpuDelta / systemDelta * 100, true
}
