package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/engine"
	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

// Deploy logs, in the fake store.

func (f *fakeStore) DeploymentLogs(_ context.Context, _, afterID int64, limit int) ([]store.LogLine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.LogLine
	for _, l := range f.logs {
		if l.ID > afterID && len(out) < limit {
			out = append(out, l)
		}
	}
	return out, nil
}

func (f *fakeStore) DeploymentLogComplete(context.Context, int64) (bool, error) {
	f.mu.Lock()
	f.polls++
	n, hook := f.polls, f.onPoll
	f.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.complete, nil
}

func (f *fakeStore) addLog(lines ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range lines {
		f.logs = append(f.logs, store.LogLine{ID: int64(len(f.logs) + 1), Stream: store.LogPlatform, Line: l})
	}
}

// fakeEngine has one container per deployment ID in containers, and
// FollowLogs emits lines then returns err (nil: the container stopped).
type fakeEngine struct {
	containers []engine.ManagedContainer
	lines      []engine.LogLine
	err        error
	since      time.Time // FollowLogs' since, as last called
}

func (f *fakeEngine) ManagedContainers(context.Context, string) ([]engine.ManagedContainer, error) {
	return f.containers, nil
}

func (f *fakeEngine) FollowLogs(_ context.Context, _ string, _ int, since time.Time, emit func(engine.LogLine)) error {
	f.since = since
	for _, l := range f.lines {
		emit(l)
	}
	return f.err
}

// event is one Server-Sent Event, as the browser would see it.
type event struct{ id, name, data string }

// events parses an SSE body, skipping comments (keepalives).
func events(body string) []event {
	var out []event
	for block := range strings.SplitSeq(body, "\n\n") {
		var e event
		for line := range strings.SplitSeq(block, "\n") {
			k, v, _ := strings.Cut(line, ": ")
			switch k {
			case "id":
				e.id = v
			case "event":
				e.name = v
			case "data":
				e.data = v
			}
		}
		if e.data != "" {
			out = append(out, e)
		}
	}
	return out
}

func lineEvent(id string, line string) event {
	return event{id: id, data: `{"at":"0001-01-01T00:00:00Z","stream":"platform","line":"` + line + `"}`}
}

var endEvent = event{name: "end", data: "{}"}

func withFastPolling(t *testing.T) {
	t.Helper()
	poll, keep := logPollInterval, keepaliveInterval
	logPollInterval, keepaliveInterval = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { logPollInterval, keepaliveInterval = poll, keep })
}

func newLogsTestAPI(t *testing.T) (http.Handler, *fakeStore, *fakeEngine) {
	t.Helper()
	withFastPolling(t)
	st := &fakeStore{env: map[string]string{}}
	st.apps = []store.App{{ID: 1, Name: "hello"}, {ID: 2, Name: "other"}}
	st.deployments = []store.Deployment{{ID: 7, AppID: 1, Status: store.StatusHealthy}}
	eng := &fakeEngine{containers: []engine.ManagedContainer{
		{ID: "c6", Labels: map[string]string{engine.LabelDeployment: "6"}},
		{ID: "c7", Labels: map[string]string{engine.LabelDeployment: "7"}},
	}}
	return New(st, &fakeDeployer{}, eng, nil, "apps.test"), st, eng
}

// stream makes a GET with optional Last-Event-ID and returns the response.
func stream(t *testing.T, h http.Handler, path, lastEventID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestDeploymentLogReplaysThenEnds(t *testing.T) {
	h, st, _ := newLogsTestAPI(t)
	st.addLog("Deploying", "Building", "Live")
	st.complete = true

	rec := stream(t, h, "/api/platform/apps/1/deployments/7/logs", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d, content type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	want := []event{lineEvent("1", "Deploying"), lineEvent("2", "Building"), lineEvent("3", "Live"), endEvent}
	if got := events(rec.Body.String()); !slices.Equal(got, want) {
		t.Errorf("events:\n  got  %q\n  want %q", got, want)
	}

	// A reconnect resumes after the last line the browser got.
	rec = stream(t, h, "/api/platform/apps/1/deployments/7/logs", "2")
	if got := events(rec.Body.String()); !slices.Equal(got, []event{lineEvent("3", "Live"), endEvent}) {
		t.Errorf("after Last-Event-ID 2: %q", got)
	}
}

func TestDeploymentLogFollowsNewLines(t *testing.T) {
	h, st, _ := newLogsTestAPI(t)
	st.addLog("Deploying")
	// The deploy goes on writing while the browser watches; quiet polls in
	// between get keepalives.
	st.onPoll = func(n int) {
		switch n {
		case 10:
			st.addLog("Building")
		case 20:
			st.addLog("Live")
			st.mu.Lock()
			st.complete = true
			st.mu.Unlock()
		}
	}
	rec := stream(t, h, "/api/platform/apps/1/deployments/7/logs", "")

	want := []event{lineEvent("1", "Deploying"), lineEvent("2", "Building"), lineEvent("3", "Live"), endEvent}
	if got := events(rec.Body.String()); !slices.Equal(got, want) {
		t.Errorf("events:\n  got  %q\n  want %q", got, want)
	}
	if !strings.Contains(rec.Body.String(), ": keepalive") {
		t.Error("no keepalive while the log was quiet")
	}
}

func TestDeploymentLogStopsWhenTheClientGoes(t *testing.T) {
	h, _, _ := newLogsTestAPI(t) // never complete
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, "GET", "/api/platform/apps/1/deployments/7/logs", nil)
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream didn't stop when the request's context ended")
	}
}

func TestDeploymentLogNotFound(t *testing.T) {
	h, _, _ := newLogsTestAPI(t)
	for _, path := range []string{
		"/api/platform/apps/2/deployments/7/logs", // another app's deployment
		"/api/platform/apps/1/deployments/8/logs", // no such deployment
		"/api/platform/apps/1/deployments/x/logs",
	} {
		if rec := stream(t, h, path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", path, rec.Code)
		}
	}
}

func TestAppLogs(t *testing.T) {
	h, _, eng := newLogsTestAPI(t)
	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	eng.lines = []engine.LogLine{
		{At: t0, Stream: "stdout", Line: "listening"},
		{At: t0.Add(time.Second), Stream: "stderr", Line: "warning"},
		{At: t0.Add(2 * time.Second), Stream: "stdout", Line: "GET / 200"},
	}

	rec := stream(t, h, "/api/platform/apps/1/logs", "")
	got := events(rec.Body.String())
	if len(got) != 4 || got[3] != endEvent {
		t.Fatalf("events = %q, want 3 lines then end (the container stopped)", got)
	}
	if got[1].id != t0.Add(time.Second).Format(time.RFC3339Nano) ||
		got[1].data != `{"at":"2026-11-03T10:00:01Z","stream":"stderr","line":"warning"}` {
		t.Errorf("second event = %q", got[1])
	}

	// A reconnect asks Docker for lines from the last one's time, and skips
	// the ones the browser already has.
	rec = stream(t, h, "/api/platform/apps/1/logs", got[1].id)
	if !eng.since.Equal(t0.Add(time.Second)) {
		t.Errorf("since = %v, want the last event's time", eng.since)
	}
	if again := events(rec.Body.String()); len(again) != 2 || !strings.Contains(again[0].data, "GET / 200") {
		t.Errorf("after reconnect: %q, want only the third line, then end", again)
	}
}

func TestAppLogsErrors(t *testing.T) {
	t.Run("nothing live", func(t *testing.T) {
		h, st, _ := newLogsTestAPI(t)
		st.deployments[0].Status = store.StatusFailed
		if rec := stream(t, h, "/api/platform/apps/1/logs", ""); rec.Code != http.StatusNotFound {
			t.Errorf("status %d, want 404", rec.Code)
		}
	})
	t.Run("live container missing", func(t *testing.T) {
		h, _, eng := newLogsTestAPI(t)
		eng.containers = eng.containers[:1] // only deployment 6's
		if rec := stream(t, h, "/api/platform/apps/1/logs", ""); rec.Code != http.StatusNotFound {
			t.Errorf("status %d, want 404", rec.Code)
		}
	})
	t.Run("docker fails mid-stream", func(t *testing.T) {
		h, _, eng := newLogsTestAPI(t)
		eng.lines = []engine.LogLine{{Line: "one"}}
		eng.err = errors.New("connection reset")
		rec := stream(t, h, "/api/platform/apps/1/logs", "")
		// No "end": the browser reconnects.
		if got := events(rec.Body.String()); len(got) != 1 || got[0].name != "" {
			t.Errorf("events = %q, want the line and no end", got)
		}
	})
}
