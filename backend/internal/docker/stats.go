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
	SystemCPUUsage uint64 // CPU time used by the whole host so far, in ns
	OnlineCPUs     uint32

	MemoryUsed  uint64 // bytes, excluding file cache the kernel can reclaim
	MemoryLimit uint64 // bytes; the host's total memory if no limit is set
}

// apiStats is the subset of `GET /containers/{id}/stats` we use.
type apiStats struct {
	Read     time.Time `json:"read"`
	CPUStats struct {
		CPUUsage struct {
			TotalUsage  uint64   `json:"total_usage"`
			PercpuUsage []uint64 `json:"percpu_usage"` // cgroup v1 only
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs     uint32 `json:"online_cpus"`
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

	online := raw.CPUStats.OnlineCPUs
	if online == 0 {
		// Older engines on cgroup v1 may omit online_cpus.
		online = uint32(len(raw.CPUStats.CPUUsage.PercpuUsage))
	}
	return Stats{
		Read:           raw.Read,
		CPUUsage:       raw.CPUStats.CPUUsage.TotalUsage,
		SystemCPUUsage: raw.CPUStats.SystemCPUUsage,
		OnlineCPUs:     online,
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

// CPUPercent returns the container's CPU use between two samples, like
// `docker stats`: 100% is one full core, so a 4-core host can show up to
// 400%.
//
// ok is false when there's no meaningful answer: the samples are out of
// order, a counter went backwards (the container restarted between them), or
// no host time passed.
func CPUPercent(prev, cur Stats) (pct float64, ok bool) {
	if cur.CPUUsage < prev.CPUUsage || cur.SystemCPUUsage <= prev.SystemCPUUsage || cur.OnlineCPUs == 0 {
		return 0, false
	}
	// The counters are uint64; the checks above make sure neither
	// subtraction wraps around.
	cpuDelta := float64(cur.CPUUsage - prev.CPUUsage)
	systemDelta := float64(cur.SystemCPUUsage - prev.SystemCPUUsage)
	return cpuDelta / systemDelta * float64(cur.OnlineCPUs) * 100, true
}
