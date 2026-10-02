// Package server wires up the dashboard's HTTP routes.
package server

import (
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/monitor"
)

// SnapshotSource provides the latest container data. It's declared here,
// where it's used, so tests can pass a fake; in production it's a
// *monitor.Monitor.
type SnapshotSource interface {
	Snapshot() (snap monitor.Snapshot, ok bool)
}

// New returns the dashboard's HTTP handler. If static is non-nil, its files
// (the built frontend) are served for every path the API doesn't claim.
func New(snapshots SnapshotSource, static fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /api/containers", handleListContainers(snapshots))
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

// containersResponse is the JSON body of GET /api/containers. The API types
// are kept separate from the monitor's so the API contract doesn't change by
// accident when an internal type does.
type containersResponse struct {
	UpdatedAt  time.Time           `json:"updatedAt"`
	Stale      bool                `json:"stale"` // the latest poll failed; data is from UpdatedAt
	Host       *hostResponse       `json:"host"`  // null if the host couldn't be read that poll
	Containers []containerResponse `json:"containers"`
}

// hostResponse is the whole server's resource usage. Disk fields have df's
// meanings: root's reserved blocks are in neither used nor available, so
// df's "Use%" is used / (used + available), not used / total.
type hostResponse struct {
	CPUPercent         *float64 `json:"cpuPercent"` // 0–100; null until two samples
	MemoryBytes        uint64   `json:"memoryBytes"`
	MemoryTotalBytes   uint64   `json:"memoryTotalBytes"`
	DiskUsedBytes      uint64   `json:"diskUsedBytes"`
	DiskAvailableBytes uint64   `json:"diskAvailableBytes"`
	DiskTotalBytes     uint64   `json:"diskTotalBytes"`
}

func newHostResponse(h *monitor.HostStatus) *hostResponse {
	if h == nil {
		return nil
	}
	return &hostResponse{
		CPUPercent:         ptrIf(h.CPUPercent, h.HasCPU),
		MemoryBytes:        h.Memory.Used(),
		MemoryTotalBytes:   h.Memory.Total,
		DiskUsedBytes:      h.Disk.Used,
		DiskAvailableBytes: h.Disk.Available,
		DiskTotalBytes:     h.Disk.Total,
	}
}

// containerResponse is one container in the API.
//
// Times are sent as timestamps rather than a computed uptime, so the browser
// can work out "up 3h 12m" against its own clock however old the response is.
// Values that don't apply are null rather than 0, so "no data" can't be
// mistaken for "idle".
type containerResponse struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Image            string     `json:"image"`
	State            string     `json:"state"`
	Health           *string    `json:"health"` // "starting", "healthy" or "unhealthy"; null if no healthcheck or not running
	Status           string     `json:"status"`
	StartedAt        *time.Time `json:"startedAt"`        // null if never started
	FinishedAt       *time.Time `json:"finishedAt"`       // null if never stopped
	CPUPercent       *float64   `json:"cpuPercent"`       // share of the whole host, 0–100; null until two samples
	MemoryBytes      *uint64    `json:"memoryBytes"`      // null if not running or unreadable
	MemoryLimitBytes *uint64    `json:"memoryLimitBytes"` // host memory if no limit is set
}

// timeOrNil maps Docker's "no time" (the zero time) to nil, which encodes as
// null instead of "0001-01-01T00:00:00Z".
func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// ptrIf returns &v if ok, else nil (null in JSON).
func ptrIf[T any](v T, ok bool) *T {
	if !ok {
		return nil
	}
	return &v
}

func handleListContainers(snapshots SnapshotSource) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		snap, ok := snapshots.Snapshot()
		if !ok {
			// No poll has succeeded yet. The monitor logs each failure, so
			// there's nothing to add to the log here.
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not list containers"})
			return
		}

		// make, not a nil slice: an empty result must encode as [], not null.
		list := make([]containerResponse, 0, len(snap.Containers))
		for _, c := range snap.Containers {
			list = append(list, containerResponse{
				ID:    c.ID,
				Name:  c.Name,
				Image: c.Image,
				State: c.State,
				// Docker keeps the last health result after a container
				// stops; it's stale then, so it isn't sent.
				Health:           ptrIf(c.Health, c.Health != "" && c.State == "running"),
				Status:           c.Status,
				StartedAt:        timeOrNil(c.StartedAt),
				FinishedAt:       timeOrNil(c.FinishedAt),
				CPUPercent:       ptrIf(c.CPUPercent, c.HasCPU),
				MemoryBytes:      ptrIf(c.MemoryUsed, c.HasStats),
				MemoryLimitBytes: ptrIf(c.MemoryLimit, c.HasStats),
			})
		}
		writeJSON(w, http.StatusOK, containersResponse{
			UpdatedAt:  snap.UpdatedAt,
			Stale:      snap.Stale,
			Host:       newHostResponse(snap.Host),
			Containers: list,
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("writing JSON response", "err", err)
	}
}
