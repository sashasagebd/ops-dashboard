// Package server wires up the dashboard's HTTP routes.
package server

import (
	"encoding/json"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/history"
	"github.com/sashasagebd/ops-dashboard/backend/internal/monitor"
)

// SnapshotSource provides the latest container data. It's declared here,
// where it's used, so tests can pass a fake; in production it's a
// *monitor.Monitor.
type SnapshotSource interface {
	Snapshot() (snap monitor.Snapshot, ok bool)
}

// HistorySource provides recent trend data; in production it's also a
// *monitor.Monitor.
type HistorySource interface {
	History(window time.Duration) history.View
}

// New returns the dashboard's HTTP handler. If static is non-nil, its files
// (the built frontend) are served for every path the API doesn't claim.
func New(snapshots SnapshotSource, hist HistorySource, static fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /api/containers", handleListContainers(snapshots))
	mux.HandleFunc("GET /api/history", handleHistory(hist))
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

// historyWindows are the windows GET /api/history accepts, matching the
// page's toggle. A fixed list rather than any duration keeps the response
// size bounded and the API easy to describe.
var historyWindows = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
}

// historyResponse is the JSON body of GET /api/history.
//
// Every series has the same length: one value per stepSeconds from start,
// oldest first, the last one being the minute still in progress. Values
// carry no timestamps of their own: at 24h a series is 1,440 values, and a
// timestamp on each would be most of the body. A value is null when nothing
// was recorded in that step (stopped, not yet available, or Docker
// unreachable), so the line has a gap instead of dropping to 0.
type historyResponse struct {
	Start       time.Time                           `json:"start"` // start of the first step
	StepSeconds int                                 `json:"stepSeconds"`
	Host        hostHistoryResponse                 `json:"host"`
	Containers  map[string]containerHistoryResponse `json:"containers"` // by name; {} if none
}

type hostHistoryResponse struct {
	CPUPercent    []*float64 `json:"cpuPercent"`    // step average, 0–100
	CPUPercentMax []*float64 `json:"cpuPercentMax"` // step maximum, so short spikes still show
	MemoryBytes   []*float64 `json:"memoryBytes"`   // step average of used memory
	DiskUsedBytes []*float64 `json:"diskUsedBytes"`
}

type containerHistoryResponse struct {
	CPUPercent    []*float64 `json:"cpuPercent"`
	CPUPercentMax []*float64 `json:"cpuPercentMax"`
	MemoryBytes   []*float64 `json:"memoryBytes"`
}

func handleHistory(hist HistorySource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		param := r.URL.Query().Get("window")
		if param == "" {
			param = "1h"
		}
		window, ok := historyWindows[param]
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "window must be 1h or 24h"})
			return
		}

		// No 502 before the first poll, unlike /api/containers: an empty
		// history is still a valid answer (every value null).
		v := hist.History(window)
		containers := make(map[string]containerHistoryResponse, len(v.Containers))
		for name, c := range v.Containers {
			containers[name] = containerHistoryResponse{
				CPUPercent:    values(c.CPU, avg, 2),
				CPUPercentMax: values(c.CPU, peak, 2),
				MemoryBytes:   values(c.Memory, avg, 0),
			}
		}
		writeJSON(w, http.StatusOK, historyResponse{
			Start:       v.Start,
			StepSeconds: int(v.Step / time.Second),
			Host: hostHistoryResponse{
				CPUPercent:    values(v.Host.CPU, avg, 2),
				CPUPercentMax: values(v.Host.CPU, peak, 2),
				MemoryBytes:   values(v.Host.Memory, avg, 0),
				DiskUsedBytes: values(v.Host.Disk, avg, 0),
			},
			Containers: containers,
		})
	}
}

func avg(p history.Point) float64  { return p.Avg }
func peak(p history.Point) float64 { return p.Max }

// values picks one number from each point, rounded to decimals places, with
// nil (null) for gaps. Rounding matters for size: an average of byte counts
// is rarely a whole number, and its full float64 digits would roughly triple
// the body for precision no chart can show.
func values(points []history.Point, pick func(history.Point) float64, decimals int) []*float64 {
	scale := math.Pow10(decimals)
	out := make([]*float64, len(points))
	for i, p := range points {
		if p.OK {
			v := math.Round(pick(p)*scale) / scale
			out[i] = &v
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("writing JSON response", "err", err)
	}
}
