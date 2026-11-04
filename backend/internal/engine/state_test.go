package engine

import (
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"
)

func TestExit(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.handle("GET /containers/c1/json", respondJSON(http.StatusOK,
		`{"Id":"c1","State":{"Status":"exited","ExitCode":137,"OOMKilled":true,"FinishedAt":"2026-11-04T10:00:00.5Z"}}`))
	f.handle("GET /containers/c2/json", respondJSON(http.StatusOK,
		`{"Id":"c2","State":{"Status":"created","ExitCode":0,"FinishedAt":"0001-01-01T00:00:00Z"}}`))

	got, err := c.Exit(t.Context(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	want := ExitInfo{ExitCode: 137, OOMKilled: true, FinishedAt: time.Date(2026, 11, 4, 10, 0, 0, 5e8, time.UTC)}
	if got != want {
		t.Errorf("Exit = %+v, want %+v", got, want)
	}
	// Never stopped: no finish time, rather than year 1.
	if got, _ := c.Exit(t.Context(), "c2"); !got.FinishedAt.IsZero() {
		t.Errorf("never-stopped container finished at %v, want zero", got.FinishedAt)
	}
	if _, err := c.Exit(t.Context(), "missing"); err == nil {
		t.Error("Exit of a missing container succeeded")
	}
}

func TestStart(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.handle("POST /containers/c1/start", respondJSON(http.StatusNoContent, ""))
	if err := c.Start(t.Context(), "c1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(t.Context(), "missing"); err == nil {
		t.Error("Start of a missing container succeeded")
	}
}

func TestTailLogs(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.handle("GET /containers/abc/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
		_, _ = w.Write(frame(1, "2026-11-04T10:00:00Z starting\n2026-11-04T10:00:02Z still going\n"))
		_, _ = w.Write(frame(2, "2026-11-04T10:00:01Z panic: nil map\n"))
	})

	got, err := c.TailLogs(t.Context(), "abc", 20)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range got {
		lines = append(lines, l.Stream+": "+l.Line)
	}
	// In time order, across both streams.
	want := []string{"stdout: starting", "stderr: panic: nil map", "stdout: still going"}
	if !slices.Equal(lines, want) {
		t.Errorf("lines = %q, want %q", lines, want)
	}
	req, _ := f.request("GET /containers/abc/logs")
	q, _ := url.ParseQuery(req.query)
	if q.Get("follow") == "1" || q.Get("tail") != "20" || q.Get("timestamps") != "1" {
		t.Errorf("query = %q, want tail=20, timestamps, not following", req.query)
	}
}
