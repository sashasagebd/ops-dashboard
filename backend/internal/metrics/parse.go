package metrics

import (
	"bufio"
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// counters is one host's request counters at one moment, as Caddy reports
// them: totals since Caddy started (or since the host first got a request),
// summed over response codes and methods.
type counters struct {
	requests float64
	errors   float64             // 5xx responses
	buckets  map[float64]float64 // duration histogram: upper bound (seconds, +Inf included) -> requests that took at most that long
}

// servers are the Caddy servers whose traffic is the apps': the tailnet one
// and the public one (internal/router).
var servers = map[string]bool{"apps": true, "public": true}

// parseRequests reads Caddy's metrics (Prometheus's text format) and returns
// each host's request counters, for the platform's servers only. It reads the
// request duration histogram, which has everything needed: its _count by
// code gives requests and 5xx, its buckets give latency percentiles.
//
// Prometheus's own parser would be a large dependency for the few lines
// needed here; the format is line-based and simple.
func parseRequests(data []byte) (map[string]*counters, error) {
	const name = "caddy_http_request_duration_seconds"
	out := map[string]*counters{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, name+"_") {
			continue // comments, other metrics
		}
		metric, labels, value, err := parseLine(line)
		if err != nil {
			return nil, err
		}
		host := labels["host"]
		if !servers[labels["server"]] || host == "" || host == "_other" {
			continue
		}
		host = stripPort(host)
		c := out[host]
		if c == nil {
			c = &counters{buckets: map[float64]float64{}}
			out[host] = c
		}
		switch metric {
		case name + "_count":
			c.requests += value
			if strings.HasPrefix(labels["code"], "5") {
				c.errors += value
			}
		case name + "_bucket":
			le, err := strconv.ParseFloat(labels["le"], 64) // "+Inf" parses as infinity
			if err != nil {
				return nil, fmt.Errorf("metrics: bucket bound %q: %w", labels["le"], err)
			}
			c.buckets[le] += value
		}
	}
	return out, sc.Err()
}

// parseLine splits one sample line: `name{label="value",...} value`, with an
// optional timestamp after the value, which is ignored.
func parseLine(line string) (name string, labels map[string]string, value float64, err error) {
	labels = map[string]string{}
	rest := line
	if i := strings.IndexAny(rest, "{ "); i >= 0 && rest[i] == '{' {
		name = rest[:i]
		rest = rest[i+1:]
		for {
			if strings.HasPrefix(rest, "}") {
				rest = rest[1:]
				break
			}
			eq := strings.Index(rest, `="`)
			if eq < 0 {
				return "", nil, 0, fmt.Errorf("metrics: bad labels in %q", line)
			}
			key := strings.TrimLeft(rest[:eq], ",")
			val, n, ok := unquote(rest[eq+2:])
			if !ok {
				return "", nil, 0, fmt.Errorf("metrics: bad label value in %q", line)
			}
			labels[key] = val
			rest = strings.TrimPrefix(rest[eq+2+n:], ",")
		}
	} else {
		name, rest, _ = strings.Cut(rest, " ")
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", nil, 0, fmt.Errorf("metrics: no value in %q", line)
	}
	value, err = strconv.ParseFloat(fields[0], 64)
	if err != nil || math.IsNaN(value) {
		return "", nil, 0, fmt.Errorf("metrics: bad value in %q", line)
	}
	return name, labels, value, nil
}

// unquote reads a label value up to its closing quote, undoing the format's
// escapes (\\, \", \n). It returns the value and how many bytes it read,
// closing quote included.
func unquote(s string) (string, int, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			return b.String(), i + 1, true
		case '\\':
			if i+1 == len(s) {
				return "", 0, false
			}
			i++
			if s[i] == 'n' {
				b.WriteByte('\n')
			} else {
				b.WriteByte(s[i])
			}
		default:
			b.WriteByte(s[i])
		}
	}
	return "", 0, false
}

// stripPort drops a port from a Host header ("hello.example:80"): the same
// app either way.
func stripPort(host string) string {
	if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.Contains(host[i:], "]") {
		return host[:i]
	}
	return host
}
