package engine

import (
	"encoding/binary"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"
)

// frame is one chunk of Docker's multiplexed log stream: a header (stream
// type, three zero bytes, payload length) then the payload.
func frame(stream byte, payload string) []byte {
	h := make([]byte, 8, 8+len(payload))
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(payload)))
	return append(h, payload...)
}

func TestFollowLogs(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.handle("GET /containers/abc/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
		// A line split across two chunks, and output on both streams.
		_, _ = w.Write(frame(1, "2026-11-03T10:00:00.000000001Z listening on :8080\n2026-11-03T10:00:01Z GET /"))
		_, _ = w.Write(frame(1, " 200\n"))
		_, _ = w.Write(frame(2, "2026-11-03T10:00:02Z \x1b[31mwarning\x1b[0m: slow\n"))
	})

	var got []LogLine
	err := c.FollowLogs(t.Context(), "abc", 100, time.Time{}, func(l LogLine) { got = append(got, l) })
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range got {
		lines = append(lines, l.Stream+": "+l.Line)
	}
	want := []string{"stdout: listening on :8080", "stdout: GET / 200", "stderr: warning: slow"}
	if !slices.Equal(lines, want) {
		t.Errorf("lines = %q, want %q", lines, want)
	}
	if !got[0].At.Equal(time.Date(2026, 11, 3, 10, 0, 0, 1, time.UTC)) {
		t.Errorf("first line's time = %v, want Docker's timestamp", got[0].At)
	}

	req, _ := f.request("GET /containers/abc/logs")
	q, _ := url.ParseQuery(req.query)
	if q.Get("follow") != "1" || q.Get("timestamps") != "1" || q.Get("tail") != "100" || q.Get("since") != "" {
		t.Errorf("query = %q, want follow, timestamps, tail=100, no since", req.query)
	}
}

func TestFollowLogsResumesAfterSince(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.handle("GET /containers/abc/logs", func(http.ResponseWriter, *http.Request) {})

	since := time.Date(2026, 11, 3, 10, 0, 0, 5, time.UTC)
	if err := c.FollowLogs(t.Context(), "abc", 100, since, func(LogLine) {}); err != nil {
		t.Fatal(err)
	}
	req, _ := f.request("GET /containers/abc/logs")
	q, _ := url.ParseQuery(req.query)
	// The SDK leaves tail out for "all", Docker's default.
	if q.Get("since") != "1793700000.000000005" || (q.Has("tail") && q.Get("tail") != "all") {
		t.Errorf("query = %q, want since with nanoseconds and every line after it", req.query)
	}
}
