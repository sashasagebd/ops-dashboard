// Package server wires up the dashboard's HTTP routes.
package server

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/docker"
)

// ContainerLister is the slice of Docker access the server needs. It's
// declared here, where it's used, so tests can pass a fake.
type ContainerLister interface {
	ListContainers(ctx context.Context) ([]docker.Container, error)
}

// New returns the dashboard's HTTP handler. If static is non-nil, its files
// (the built frontend) are served for every path the API doesn't claim.
func New(containers ContainerLister, static fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /api/containers", handleListContainers(containers))
	if static != nil {
		// "GET /" matches everything, but ServeMux always prefers the most
		// specific pattern, so the API routes above still win.
		mux.Handle("GET /", http.FileServerFS(static))
	}
	return mux
}

// handleHealthz reports that the process is up and serving requests.
// It deliberately doesn't check Docker: a liveness check that fails when a
// dependency is down would get the dashboard restarted for no reason.
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// containerResponse is the JSON shape of one container in the API. It's kept
// separate from docker.Container so the API contract doesn't change by
// accident when the internal type does.
//
// Times are sent as timestamps rather than a computed uptime, so the browser
// can work out "up 3h 12m" against its own clock however old the response is.
type containerResponse struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Image      string     `json:"image"`
	State      string     `json:"state"`
	Status     string     `json:"status"`
	StartedAt  *time.Time `json:"startedAt"`  // null if never started
	FinishedAt *time.Time `json:"finishedAt"` // null if never stopped
}

// timeOrNil maps Docker's "no time" (the zero time) to nil, which encodes as
// null instead of "0001-01-01T00:00:00Z".
func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func handleListContainers(containers ContainerLister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := containers.ListContainers(r.Context())
		if err != nil {
			// The details go to the log, not the client: Docker errors can
			// include internal hostnames and paths.
			slog.Error("listing containers", "err", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not list containers"})
			return
		}

		// Docker returns newest-first; sort by name so the page doesn't
		// reshuffle when a container restarts.
		slices.SortFunc(list, func(a, b docker.Container) int {
			return strings.Compare(a.Name, b.Name)
		})

		// make, not a nil slice: an empty result must encode as [], not null.
		resp := make([]containerResponse, 0, len(list))
		for _, c := range list {
			resp = append(resp, containerResponse{
				ID:         c.ID,
				Name:       c.Name,
				Image:      c.Image,
				State:      c.State,
				Status:     c.Status,
				StartedAt:  timeOrNil(c.StartedAt),
				FinishedAt: timeOrNil(c.FinishedAt),
			})
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("writing JSON response", "err", err)
	}
}
