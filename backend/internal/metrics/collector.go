// Package metrics keeps each app's recent request counts, from Caddy, which
// sees every request to every app: requests per minute, how many were 5xx,
// and the 95th percentile of how long they took, for the last 24 hours, in
// memory (see docs/decisions/platform.md).
//
// Caddy's numbers are running totals since it started. The collector reads
// them every minute and keeps the difference from the last reading, so each
// minute has its own count, and a Caddy restart (totals back to zero) loses
// at most a minute.
package metrics

import (
	"context"
	"log/slog"
	"math"
	"slices"
	"sync"
	"time"
)

// Source is where the readings come from; in production it's a
// *router.Caddy.
type Source interface {
	Metrics(ctx context.Context) ([]byte, error)
}

// Collector reads Caddy's request metrics every Step and keeps Retention of
// per-step numbers per host. Safe for concurrent use.
type Collector struct {
	src  Source
	step time.Duration
	size int // steps kept
	now  func() time.Time

	mu      sync.Mutex
	prev    map[string]*counters // the last reading; nil before the first
	ok      []bool               // per step slot: a reading was taken in it
	slotIdx []int64              // which step each slot holds now
	hosts   map[string]*hostSeries

	lastErr string // Run's last logged error; only Run touches it
}

// hostSeries is one host's ring of per-step numbers, indexed like ok.
type hostSeries struct {
	requests, errors []float64
	p95              []float64 // seconds; NaN when there were no requests
	idx              []int64
}

// New returns a Collector that keeps retention of per-step numbers.
func New(src Source, step, retention time.Duration) *Collector {
	n := int(retention / step)
	return &Collector{
		src: src, step: step, size: n, now: time.Now,
		ok: make([]bool, n), slotIdx: make([]int64, n), hosts: map[string]*hostSeries{},
	}
}

// Run takes a reading every step until ctx is done. A failed reading leaves a
// gap; it's logged once until it changes.
func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(c.step)
	defer t.Stop()
	for {
		err := c.Collect(ctx)
		switch {
		case err != nil && ctx.Err() == nil && err.Error() != c.lastErr:
			slog.Warn("reading Caddy's request metrics", "err", err)
			c.lastErr = err.Error()
		case err == nil:
			c.lastErr = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Collect takes one reading and records the difference from the last one in
// the step it falls in. The first reading after the platform starts is only
// a baseline: the totals then cover an unknown stretch of time.
func (c *Collector) Collect(ctx context.Context) error {
	data, err := c.src.Metrics(ctx)
	if err != nil {
		return err
	}
	cur, err := parseRequests(data)
	if err != nil {
		return err
	}
	idx := c.now().UnixNano() / int64(c.step)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.prev == nil {
		c.prev = cur
		return nil
	}
	slot := c.slot(idx)
	if c.slotIdx[slot] != idx {
		c.slotIdx[slot], c.ok[slot] = idx, false
	}
	c.ok[slot] = true
	for host, now := range cur {
		d := delta(c.prev[host], now)
		s := c.hosts[host]
		if s == nil {
			s = &hostSeries{requests: make([]float64, c.size), errors: make([]float64, c.size),
				p95: make([]float64, c.size), idx: make([]int64, c.size)}
			for i := range s.idx {
				s.idx[i] = -1
			}
			c.hosts[host] = s
		}
		if s.idx[slot] != idx {
			s.idx[slot], s.requests[slot], s.errors[slot] = idx, 0, 0
		}
		s.requests[slot] += d.requests
		s.errors[slot] += d.errors
		s.p95[slot] = quantile(0.95, d.buckets)
	}
	c.prev = cur
	return nil
}

func (c *Collector) slot(idx int64) int { return int(idx % int64(c.size)) }

// delta is what happened between two readings of a host. A host with no
// earlier reading counts from zero (it got its first request since). A total
// lower than before means Caddy restarted and started again from zero, so
// the new total is all there is.
func delta(prev, cur *counters) counters {
	if prev == nil || cur.requests < prev.requests {
		return *cur
	}
	d := counters{requests: cur.requests - prev.requests, errors: max(cur.errors-prev.errors, 0),
		buckets: make(map[float64]float64, len(cur.buckets))}
	for le, n := range cur.buckets {
		d.buckets[le] = max(n-prev.buckets[le], 0)
	}
	return d
}

// quantile estimates the q-th quantile (0-1) of a duration histogram, the
// way Prometheus's histogram_quantile does: find the bucket the rank falls
// in and interpolate linearly inside it. NaN if it's empty. A rank in the
// last, unbounded bucket gives the largest finite bound: all that's known is
// "at least that long".
func quantile(q float64, buckets map[float64]float64) float64 {
	bounds := make([]float64, 0, len(buckets))
	for le := range buckets {
		bounds = append(bounds, le)
	}
	slices.Sort(bounds)
	if len(bounds) == 0 {
		return math.NaN()
	}
	total := buckets[bounds[len(bounds)-1]]
	if total <= 0 {
		return math.NaN()
	}
	rank := q * total
	lowerBound, lowerCount := 0.0, 0.0
	for _, le := range bounds {
		count := buckets[le]
		if count >= rank {
			if math.IsInf(le, 1) {
				return lowerBound
			}
			if count == lowerCount {
				return le
			}
			return lowerBound + (le-lowerBound)*(rank-lowerCount)/(count-lowerCount)
		}
		lowerBound, lowerCount = le, count
	}
	return lowerBound
}

// Series is a window of one host's numbers, one value per Step from Start,
// oldest first; nil where no reading was taken (the platform was down, or
// Caddy couldn't be read).
type Series struct {
	Start    time.Time
	Step     time.Duration
	Requests []*float64
	Errors   []*float64 // 5xx responses
	P95      []*float64 // seconds; nil also when there were no requests
}

// Window returns host's last window of numbers, ending with the current
// step. A host with no requests in a step that was read has 0 for it.
func (c *Collector) Window(host string, window time.Duration) Series {
	n := min(max(int((window+c.step-1)/c.step), 1), c.size)
	end := c.now().UnixNano() / int64(c.step)
	first := end - int64(n) + 1
	s := Series{Start: time.Unix(0, first*int64(c.step)).UTC(), Step: c.step,
		Requests: make([]*float64, n), Errors: make([]*float64, n), P95: make([]*float64, n)}

	c.mu.Lock()
	defer c.mu.Unlock()
	hs := c.hosts[host]
	for i := range n {
		idx := first + int64(i)
		slot := c.slot(idx)
		if idx < 0 || c.slotIdx[slot] != idx || !c.ok[slot] {
			continue
		}
		var req, errs, p95 = 0.0, 0.0, math.NaN()
		if hs != nil && hs.idx[slot] == idx {
			req, errs, p95 = hs.requests[slot], hs.errors[slot], hs.p95[slot]
		}
		s.Requests[i], s.Errors[i] = &req, &errs
		if !math.IsNaN(p95) {
			s.P95[i] = &p95
		}
	}
	return s
}

// Total is how many requests host had over the last window, and false if
// no reading was taken in it at all.
func (c *Collector) Total(host string, window time.Duration) (float64, bool) {
	s := c.Window(host, window)
	total, any := 0.0, false
	for _, r := range s.Requests {
		if r != nil {
			total += *r
			any = true
		}
	}
	return total, any
}
