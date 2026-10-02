// Package host reads the whole server's CPU, memory and disk usage.
//
// It runs inside the dashboard container without any extra mounts:
// /proc/stat and /proc/meminfo aren't namespaced, so a container sees the
// host's numbers, and statfs on the container's "/" reports the filesystem
// Docker stores everything on, which is the host's root disk.
package host

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Sample is one reading of the host's resources.
type Sample struct {
	CPU    CPUTimes
	Memory Memory
	Disk   Disk
}

// CPUTimes are cumulative CPU counters for all cores together, in clock
// ticks. Like container CPU, a single sample says nothing about current
// load; CPUPercent compares two.
type CPUTimes struct {
	Busy  uint64
	Total uint64
}

// Memory is in bytes.
type Memory struct {
	Total     uint64
	Available uint64 // what's free plus what the kernel can reclaim (cache)
}

// Used is memory that can't be reclaimed: total minus available. That's how
// current versions of `free` compute "used", and it matches container memory
// leaving out cache.
func (m Memory) Used() uint64 {
	if m.Available > m.Total {
		return 0
	}
	return m.Total - m.Available
}

// Disk is in bytes, with the same meaning as `df`'s columns.
type Disk struct {
	Total     uint64 // df "Size"
	Used      uint64 // df "Used"
	Available uint64 // df "Avail": free space usable by normal users
}

// Reader reads host stats from a proc directory and a disk path. Use
// NewReader("/proc", "/") in production; tests point it elsewhere.
type Reader struct {
	procDir  string
	diskPath string
}

func NewReader(procDir, diskPath string) *Reader {
	return &Reader{procDir: procDir, diskPath: diskPath}
}

// Read takes one sample. It fails if any part can't be read.
func (r *Reader) Read() (Sample, error) {
	cpu, err := readFile(filepath.Join(r.procDir, "stat"), ParseCPU)
	if err != nil {
		return Sample{}, err
	}
	mem, err := readFile(filepath.Join(r.procDir, "meminfo"), ParseMemInfo)
	if err != nil {
		return Sample{}, err
	}
	disk, err := diskUsage(r.diskPath)
	if err != nil {
		return Sample{}, fmt.Errorf("disk usage of %s: %w", r.diskPath, err)
	}
	return Sample{CPU: cpu, Memory: mem, Disk: disk}, nil
}

func readFile[T any](path string, parse func(io.Reader) (T, error)) (T, error) {
	f, err := os.Open(path)
	if err != nil {
		var zero T
		return zero, err
	}
	defer f.Close()
	v, err := parse(f)
	if err != nil {
		return v, fmt.Errorf("parsing %s: %w", path, err)
	}
	return v, nil
}

// ParseCPU reads the aggregate "cpu" line of /proc/stat:
//
//	cpu  user nice system idle iowait irq softirq steal guest guest_nice
//
// Idle time is idle + iowait (waiting on disk isn't using the CPU). guest
// and guest_nice are left out because the kernel already counts them inside
// user and nice. Older kernels have fewer columns; missing ones count as 0.
func ParseCPU(r io.Reader) (CPUTimes, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 || fields[0] != "cpu" {
			continue // per-core lines ("cpu0"), interrupts, etc.
		}
		if len(fields) < 5 {
			return CPUTimes{}, fmt.Errorf("cpu line has %d fields, want at least 4 values", len(fields)-1)
		}

		var vals [8]uint64 // user nice system idle iowait irq softirq steal
		for i := range vals {
			if i+1 >= len(fields) {
				break
			}
			v, err := strconv.ParseUint(fields[i+1], 10, 64)
			if err != nil {
				return CPUTimes{}, fmt.Errorf("cpu field %d: %w", i+1, err)
			}
			vals[i] = v
		}

		var total uint64
		for _, v := range vals {
			total += v
		}
		idle := vals[3] + vals[4]
		return CPUTimes{Busy: total - idle, Total: total}, nil
	}
	if err := sc.Err(); err != nil {
		return CPUTimes{}, err
	}
	return CPUTimes{}, errors.New("no aggregate cpu line")
}

// CPUPercent returns the host's CPU use between two samples, 0–100 (100 =
// every core busy). ok is false if the counters didn't move forward, e.g.
// two samples taken in the same clock tick.
func CPUPercent(prev, cur CPUTimes) (pct float64, ok bool) {
	if cur.Total <= prev.Total || cur.Busy < prev.Busy {
		return 0, false
	}
	return float64(cur.Busy-prev.Busy) / float64(cur.Total-prev.Total) * 100, true
}

// ParseMemInfo reads MemTotal and MemAvailable from /proc/meminfo, whose
// lines look like "MemTotal:       15734656 kB".
//
// MemAvailable is the kernel's own estimate of memory available without
// swapping, which accounts for reclaimable cache properly; computing it from
// MemFree + Cached would overcount.
func ParseMemInfo(r io.Reader) (Memory, error) {
	var m Memory
	var haveTotal, haveAvail bool
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok || (key != "MemTotal" && key != "MemAvailable") {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 || fields[1] != "kB" {
			return Memory{}, fmt.Errorf("%s: unexpected format %q", key, rest)
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return Memory{}, fmt.Errorf("%s: %w", key, err)
		}
		// "kB" in meminfo is really KiB.
		if key == "MemTotal" {
			m.Total, haveTotal = kb*1024, true
		} else {
			m.Available, haveAvail = kb*1024, true
		}
	}
	if err := sc.Err(); err != nil {
		return Memory{}, err
	}
	if !haveTotal || !haveAvail {
		return Memory{}, errors.New("missing MemTotal or MemAvailable")
	}
	return m, nil
}
