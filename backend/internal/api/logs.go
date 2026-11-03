package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/engine"
	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

// Log streams use Server-Sent Events (text/event-stream): one-way, plain
// HTTP, so they pass through the dashboard's proxy, and the browser's
// EventSource reconnects by itself, sending the last event's id as
// Last-Event-ID so the stream resumes where it broke off. Each line is a
// message whose data is a logLineResponse; a stream that has nothing more to
// send ends with an "end" event, so the browser knows not to reconnect.

// Tunables, variables so tests can shorten them.
var (
	// logPollInterval is how often a deploy log stream checks the database
	// for new lines. The deployer writes in batches every 500ms, so polling
	// faster gains little; one indexed query per viewer per interval costs
	// nothing that matters.
	logPollInterval = 500 * time.Millisecond
	// keepaliveInterval is how often an idle stream sends a comment, so
	// proxies on the way (tailscale serve) don't close a quiet connection.
	keepaliveInterval = 15 * time.Second
)

const (
	logBatch = 500 // deploy log lines read per query
	// appLogTail is how much past output an app log stream starts with.
	appLogTail = 200
)

// logLineResponse is one log line, the data of each SSE message. Mirrored in
// frontend/src/api.ts.
type logLineResponse struct {
	At     time.Time `json:"at"`
	Stream string    `json:"stream"` // platform or build (deploys); stdout or stderr (apps)
	Line   string    `json:"line"`
}

// eventStream writes Server-Sent Events.
type eventStream struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func startEventStream(w http.ResponseWriter) *eventStream {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	s := &eventStream{w: w, rc: http.NewResponseController(w)}
	s.flush() // so the client (and the dashboard's proxy) sees the stream open at once
	return s
}

// send writes one event; id and event are left out if empty.
func (s *eventStream) send(id, event string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if id != "" {
		fmt.Fprintf(s.w, "id: %s\n", id)
	}
	if event != "" {
		fmt.Fprintf(s.w, "event: %s\n", event)
	}
	// JSON has no raw newlines, so the data is always a single data: line.
	_, err = fmt.Fprintf(s.w, "data: %s\n\n", b)
	return err
}

// end tells the client the stream is over.
func (s *eventStream) end() {
	_ = s.send("", "end", struct{}{})
	s.flush()
}

func (s *eventStream) keepalive() error {
	if _, err := fmt.Fprint(s.w, ": keepalive\n\n"); err != nil {
		return err
	}
	return s.flush()
}

func (s *eventStream) flush() error { return s.rc.Flush() }

// deploymentLogs streams a deployment's log: what's stored, then new lines
// as they're written, until the deploy has finished for good.
func (a *api) deploymentLogs(w http.ResponseWriter, r *http.Request) {
	appID, ok := pathID(w, r)
	if !ok {
		return
	}
	depID, err := strconv.ParseInt(r.PathValue("deploymentId"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{"not found"})
		return
	}
	ctx := r.Context()
	dep, err := a.store.Deployment(ctx, depID)
	if err == nil && dep.AppID != appID {
		err = store.ErrNotFound
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	// On a reconnect, carry on after the last line the browser got.
	after, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)

	s := startEventStream(w)
	poll := time.NewTicker(logPollInterval)
	defer poll.Stop()
	lastSent := time.Now()
	for {
		// Checked before reading: every line is stored before the log is
		// complete, so if it was complete then, this read gets the rest.
		complete, err := a.store.DeploymentLogComplete(ctx, depID)
		var lines []store.LogLine
		if err == nil {
			lines, err = a.store.DeploymentLogs(ctx, depID, after, logBatch)
		}
		if err != nil {
			// Ends the response without "end", so the browser reconnects
			// and resumes after the last line it got.
			if ctx.Err() == nil {
				slog.Error("streaming a deploy log", "deployment", depID, "err", err)
			}
			return
		}
		for _, l := range lines {
			id := strconv.FormatInt(l.ID, 10)
			if err := s.send(id, "", logLineResponse{At: l.At, Stream: l.Stream, Line: l.Line}); err != nil {
				return // the client has gone
			}
			after = l.ID
		}
		if len(lines) == logBatch {
			continue // more stored already: no need to wait
		}
		if complete {
			s.end()
			return
		}
		if len(lines) > 0 {
			if s.flush() != nil {
				return
			}
			lastSent = time.Now()
		} else if time.Since(lastSent) >= keepaliveInterval {
			if s.keepalive() != nil {
				return
			}
			lastSent = time.Now()
		}
		select {
		case <-ctx.Done(): // the client went away, or the platform is shutting down
			return
		case <-poll.C:
		}
	}
}

// appLogs streams the output of an app's live container: its recent lines,
// then new ones as it writes them, until the container stops (it crashed, or
// a deploy replaced it).
func (a *api) appLogs(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	app, err := a.store.App(ctx, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	live, err := a.current(ctx, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if live == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{"this app has nothing live"})
		return
	}
	containers, err := a.engine.ManagedContainers(ctx, app.Name)
	if err != nil {
		writeError(w, r, err)
		return
	}
	containerID := ""
	for _, c := range containers {
		if c.Labels[engine.LabelDeployment] == strconv.FormatInt(live.ID, 10) {
			containerID = c.ID
		}
	}
	if containerID == "" {
		// Docker and the database disagree (e.g. the container was
		// removed by hand); P4's reconciler fixes that.
		writeJSON(w, http.StatusNotFound, errorResponse{"the live deployment's container isn't running"})
		return
	}

	// Lines' times are their IDs: on a reconnect, resume after the last one
	// the browser got.
	since, _ := time.Parse(time.RFC3339Nano, r.Header.Get("Last-Event-ID"))

	s := startEventStream(w)
	// Docker's stream is read on its own goroutine, so the loop below can
	// also send keepalives while the app is quiet. The buffer lets a burst
	// of output (the initial tail) through without waiting on each write.
	lines := make(chan engine.LogLine, 256)
	followErr := make(chan error, 1)
	go func() {
		defer close(lines)
		followErr <- a.engine.FollowLogs(ctx, containerID, appLogTail, since, func(l engine.LogLine) {
			select {
			case lines <- l:
			case <-ctx.Done():
			}
		})
	}()

	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()
	for {
		select {
		case l, open := <-lines:
			if !open {
				if err := <-followErr; err != nil {
					// Like the deploy log: no "end", so the browser
					// reconnects.
					slog.Error("streaming app logs", "app", app.Name, "err", err)
					return
				}
				// The container stopped. Ending the stream (rather than
				// letting the browser reconnect to a container that's gone)
				// lets the page say so and offer to reconnect.
				s.end()
				return
			}
			// Docker's since is inclusive at its precision; skip what the
			// browser already has.
			if !since.IsZero() && !l.At.After(since) {
				continue
			}
			err := s.send(l.At.Format(time.RFC3339Nano), "", logLineResponse{At: l.At, Stream: l.Stream, Line: l.Line})
			if err == nil && len(lines) == 0 {
				err = s.flush() // flush once a burst has been written, not per line
			}
			if err != nil {
				return
			}
		case <-keepalive.C:
			if s.keepalive() != nil {
				return
			}
		case <-ctx.Done():
			// The reader stops too: its emit and Docker's stream both
			// watch ctx. It never writes to the response itself.
			return
		}
	}
}
