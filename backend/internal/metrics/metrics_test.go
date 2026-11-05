package metrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// caddyOutput is a reading as Caddy's /metrics gives it, trimmed to what
// matters: hello's requests (200s and a 502, two methods), a probe host, the
// catch-all, another server's, and metrics that aren't read; and on the
// public server, 2 requests to hello's public hostname.
func caddyOutput(hello200, hello502 int) string {
	var b strings.Builder
	b.WriteString("# HELP caddy_http_request_duration_seconds Histogram of round-trip request durations.\n")
	b.WriteString("# TYPE caddy_http_request_duration_seconds histogram\n")
	histOn := func(server, host, code, method string, n int, bounds map[string]int) {
		for _, le := range []string{"0.005", "0.01", "0.025", "0.05", "0.1", "0.25", "0.5", "1", "2.5", "5", "10", "+Inf"} {
			fmt.Fprintf(&b, "caddy_http_request_duration_seconds_bucket{code=%q,handler=\"reverse_proxy\",host=%q,method=%q,server=%q,le=%q} %d\n",
				code, host, method, server, le, bounds[le])
		}
		fmt.Fprintf(&b, "caddy_http_request_duration_seconds_sum{code=%q,handler=\"reverse_proxy\",host=%q,method=%q,server=%q} 1.5\n", code, host, method, server)
		fmt.Fprintf(&b, "caddy_http_request_duration_seconds_count{code=%q,handler=\"reverse_proxy\",host=%q,method=%q,server=%q} %d\n", code, host, method, server, n)
	}
	hist := func(host, code, method string, n int, bounds map[string]int) {
		histOn("apps", host, code, method, n, bounds)
	}
	// hello's 200s all took between 10 and 25ms.
	fast := func(n int) map[string]int {
		m := map[string]int{}
		for _, le := range []string{"0.025", "0.05", "0.1", "0.25", "0.5", "1", "2.5", "5", "10", "+Inf"} {
			m[le] = n
		}
		return m
	}
	hist("hello.apps.test", "200", "GET", hello200, fast(hello200))
	hist("hello.apps.test:80", "502", "POST", hello502, fast(hello502)) // a port in Host: still hello
	hist("probe-7.platform.internal", "200", "GET", 9, fast(9))
	hist("_other", "404", "GET", 4, fast(4))
	histOn("public", "hello.example.com", "200", "GET", 2, fast(2))
	b.WriteString(`caddy_http_request_duration_seconds_count{code="200",handler="file_server",host="hello.apps.test",method="GET",server="srv0"} 1000` + "\n")
	b.WriteString(`caddy_http_requests_total{handler="reverse_proxy",host="hello.apps.test",server="apps"} 77` + "\n")
	b.WriteString(`go_goroutines 42` + "\n")
	return b.String()
}

func TestParseRequests(t *testing.T) {
	got, err := parseRequests([]byte(caddyOutput(10, 2)))
	if err != nil {
		t.Fatal(err)
	}
	hello := got["hello.apps.test"]
	if hello == nil || hello.requests != 12 || hello.errors != 2 || hello.buckets[math.Inf(1)] != 12 || hello.buckets[0.01] != 0 {
		t.Errorf("hello = %+v", hello)
	}
	// Only the apps server; the catch-all and other servers aren't hosts.
	if _, ok := got["_other"]; ok {
		t.Error("the catch-all counted as a host")
	}
	if public := got["hello.example.com"]; public == nil || public.requests != 2 {
		t.Errorf("public host = %+v, want 2 requests", public)
	}
	if len(got) != 3 {
		t.Errorf("hosts = %v, want hello, its public hostname and the probe", len(got))
	}
}

func TestParseLineEscapesAndTimestamps(t *testing.T) {
	name, labels, value, err := parseLine(`x_count{path="a \"b\" \\ c",le="+Inf"} 3 1700000000000`)
	if err != nil || name != "x_count" || labels["path"] != `a "b" \ c` || labels["le"] != "+Inf" || value != 3 {
		t.Errorf("parseLine = %q, %v, %v, %v", name, labels, value, err)
	}
	for _, bad := range []string{`x{a="b} 1`, `x{a="b"}`, `x{a="b"} nope`} {
		if _, _, _, err := parseLine(bad); err == nil {
			t.Errorf("parseLine(%q) succeeded", bad)
		}
	}
}

func TestQuantile(t *testing.T) {
	inf := math.Inf(1)
	tests := []struct {
		name    string
		buckets map[float64]float64
		want    float64
	}{
		// 100 requests: 50 under 0.1s, all under 0.5s. The 95th is the 45th
		// of the 50 between 0.1 and 0.5: 0.1 + 0.4*45/50.
		{"interpolated", map[float64]float64{0.1: 50, 0.5: 100, inf: 100}, 0.46},
		{"in the first bucket", map[float64]float64{0.1: 100, inf: 100}, 0.095},
		{"past the last bound", map[float64]float64{0.1: 10, inf: 100}, 0.1},
	}
	for _, tt := range tests {
		if got := quantile(0.95, tt.buckets); math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("%s: quantile = %v, want %v", tt.name, got, tt.want)
		}
	}
	if !math.IsNaN(quantile(0.95, map[float64]float64{inf: 0})) || !math.IsNaN(quantile(0.95, nil)) {
		t.Error("an empty histogram has a quantile")
	}
}

// fakeCaddy answers with the next reading each time.
type fakeCaddy struct {
	readings []string
	err      error
}

func (f *fakeCaddy) Metrics(context.Context) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	r := f.readings[0]
	f.readings = f.readings[1:]
	return []byte(r), nil
}

func TestCollector(t *testing.T) {
	caddy := &fakeCaddy{readings: []string{
		caddyOutput(100, 0), // baseline: since whenever
		caddyOutput(130, 1), // +30 OK, +1 502
		caddyOutput(130, 1), // nothing
		caddyOutput(5, 0),   // Caddy restarted: 5 since
	}}
	c := New(caddy, time.Minute, time.Hour)
	now := time.Date(2026, 11, 4, 12, 0, 30, 0, time.UTC)
	c.now = func() time.Time { return now }
	collect := func() {
		t.Helper()
		if err := c.Collect(t.Context()); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	collect() // 12:00
	collect() // 12:01
	collect() // 12:02
	now = now.Add(time.Minute)
	caddy.err = errors.New("caddy down") // 12:03: no reading
	if err := c.Collect(t.Context()); err == nil {
		t.Fatal("Collect succeeded with Caddy down")
	}
	caddy.err = nil
	collect() // 12:04

	now = now.Add(-time.Minute) // look from 12:04
	s := c.Window("hello.apps.test", 5*time.Minute)
	if !s.Start.Equal(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)) || s.Step != time.Minute || len(s.Requests) != 5 {
		t.Fatalf("window = start %v, step %v, %d points", s.Start, s.Step, len(s.Requests))
	}
	got := func(vs []*float64) string {
		var out []string
		for _, v := range vs {
			if v == nil {
				out = append(out, "-")
			} else {
				out = append(out, fmt.Sprint(*v))
			}
		}
		return strings.Join(out, " ")
	}
	// 12:00 is the baseline (no number), 12:03 is a gap.
	if r := got(s.Requests); r != "- 31 0 - 5" {
		t.Errorf("requests = %s, want - 31 0 - 5", r)
	}
	if e := got(s.Errors); e != "- 1 0 - 0" {
		t.Errorf("errors = %s, want - 1 0 - 0", e)
	}
	// All took 10-25ms, so the p95 is in that bucket; none when idle.
	if s.P95[1] == nil || *s.P95[1] <= 0.01 || *s.P95[1] > 0.025 || s.P95[2] != nil {
		t.Errorf("p95 = %s", got(s.P95))
	}

	// A host Caddy never reported: zeros where readings were taken.
	if r := got(c.Window("quiet.apps.test", 5*time.Minute).Requests); r != "- 0 0 - 0" {
		t.Errorf("quiet host = %s, want - 0 0 - 0", r)
	}
	if total, ok := c.Total("hello.apps.test", time.Hour); !ok || total != 36 {
		t.Errorf("Total = %v, %v; want 36", total, ok)
	}
}

func TestCollectorGroupsHostsByApp(t *testing.T) {
	caddy := &fakeCaddy{readings: []string{caddyOutput(100, 0), caddyOutput(130, 0), caddyOutput(130, 0)}}
	c := New(caddy, time.Minute, time.Hour)
	c.Group = func(context.Context) (map[string]string, error) {
		return map[string]string{"hello.apps.test": "hello", "hello.example.com": "hello"}, nil
	}
	now := time.Date(2026, 11, 4, 12, 0, 30, 0, time.UTC)
	c.now = func() time.Time { return now }
	c.Collect(t.Context()) // baseline, the public hostname's 2 included
	now = now.Add(time.Minute)
	c.Collect(t.Context()) // +30 on the tailnet hostname, none public
	if total, ok := c.Total("hello", time.Hour); !ok || total != 30 {
		t.Errorf("hello = %v, %v; want 30", total, ok)
	}
	// Hosts that aren't an app's aren't kept, nor are hosts by their own name.
	for _, key := range []string{"probe-7.platform.internal", "hello.apps.test"} {
		if total, _ := c.Total(key, time.Hour); total != 0 {
			t.Errorf("%s = %v, want nothing kept", key, total)
		}
	}

	// A failed lookup is a failed reading: a gap, not numbers under the
	// wrong key.
	c.Group = func(context.Context) (map[string]string, error) { return nil, errors.New("db down") }
	if err := c.Collect(t.Context()); err == nil {
		t.Error("Collect succeeded although the lookup failed")
	}
}

func TestCollectorForgetsOldSteps(t *testing.T) {
	caddy := &fakeCaddy{readings: []string{caddyOutput(0, 0), caddyOutput(10, 0), caddyOutput(10, 0)}}
	c := New(caddy, time.Minute, 3*time.Minute) // 3 slots
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	c.Collect(t.Context())
	now = now.Add(time.Minute)
	c.Collect(t.Context()) // 12:01: 10
	now = now.Add(3 * time.Minute)
	c.Collect(t.Context()) // 12:04 reuses 12:01's slot
	if total, _ := c.Total("hello.apps.test", 3*time.Minute); total != 0 {
		t.Errorf("total = %v, want 0: 12:01 is outside the window, and its slot was reused", total)
	}
}
