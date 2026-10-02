package server

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/docker"
	"github.com/sashasagebd/ops-dashboard/backend/internal/host"
	"github.com/sashasagebd/ops-dashboard/backend/internal/monitor"
)

// fakeSource is a SnapshotSource that returns a canned snapshot.
type fakeSource struct {
	snap monitor.Snapshot
	ok   bool
}

func (f *fakeSource) Snapshot() (monitor.Snapshot, bool) {
	return f.snap, f.ok
}

func TestHealthz(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		wantStatus int
		wantBody   string
	}{
		{name: "GET returns ok", method: http.MethodGet, wantStatus: http.StatusOK, wantBody: `{"status":"ok"}` + "\n"},
		{name: "POST is not allowed", method: http.MethodPost, wantStatus: http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/healthz", nil)
			rec := httptest.NewRecorder()

			New(&fakeSource{}, nil).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantBody == "" {
				return
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
		})
	}
}

func TestListContainers(t *testing.T) {
	updated := time.Date(2026, 10, 1, 12, 0, 5, 0, time.UTC)

	tests := []struct {
		name       string
		source     *fakeSource
		wantStatus int
		wantBody   string
	}{
		{
			name: "running container with stats, stopped one without",
			source: &fakeSource{ok: true, snap: monitor.Snapshot{
				UpdatedAt: updated,
				Containers: []monitor.ContainerStatus{
					{
						Container: docker.Container{
							ID: "a1", Name: "discordbot", Image: "discordbot", State: "exited", Status: "Exited (1) 20 minutes ago",
							StartedAt:  time.Date(2026, 9, 29, 4, 30, 0, 0, time.UTC),
							FinishedAt: time.Date(2026, 10, 1, 11, 40, 0, 0, time.UTC),
						},
					},
					{
						Container: docker.Container{
							ID: "b2", Name: "mc", Image: "itzg/minecraft-server", State: "running", Status: "Up 3 hours",
							StartedAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
						},
						HasStats: true, MemoryUsed: 2147483648, MemoryLimit: 16596942848,
						HasCPU: true, CPUPercent: 12.5,
					},
				},
			}},
			wantStatus: http.StatusOK,
			wantBody: `{"updatedAt":"2026-10-01T12:00:05Z","stale":false,"host":null,"containers":[` +
				`{"id":"a1","name":"discordbot","image":"discordbot","state":"exited","status":"Exited (1) 20 minutes ago",` +
				`"startedAt":"2026-09-29T04:30:00Z","finishedAt":"2026-10-01T11:40:00Z",` +
				`"cpuPercent":null,"memoryBytes":null,"memoryLimitBytes":null},` +
				`{"id":"b2","name":"mc","image":"itzg/minecraft-server","state":"running","status":"Up 3 hours",` +
				`"startedAt":"2026-10-01T09:00:00Z","finishedAt":null,` +
				`"cpuPercent":12.5,"memoryBytes":2147483648,"memoryLimitBytes":16596942848}]}`,
		},
		{
			name: "memory without CPU on the first poll",
			source: &fakeSource{ok: true, snap: monitor.Snapshot{
				UpdatedAt: updated,
				Containers: []monitor.ContainerStatus{{
					Container: docker.Container{ID: "b2", Name: "mc", State: "running"},
					HasStats:  true, MemoryUsed: 100, MemoryLimit: 1000,
				}},
			}},
			wantStatus: http.StatusOK,
			wantBody: `{"updatedAt":"2026-10-01T12:00:05Z","stale":false,"host":null,"containers":[` +
				`{"id":"b2","name":"mc","image":"","state":"running","status":"",` +
				`"startedAt":null,"finishedAt":null,"cpuPercent":null,"memoryBytes":100,"memoryLimitBytes":1000}]}`,
		},
		{
			name: "host stats, with memory used = total - available",
			source: &fakeSource{ok: true, snap: monitor.Snapshot{
				UpdatedAt: updated,
				Host: &monitor.HostStatus{
					HasCPU: true, CPUPercent: 4.5,
					Memory: host.Memory{Total: 16_000, Available: 10_000},
					Disk:   host.Disk{Total: 250_000, Used: 50_000, Available: 187_500},
				},
			}},
			wantStatus: http.StatusOK,
			wantBody: `{"updatedAt":"2026-10-01T12:00:05Z","stale":false,"host":{"cpuPercent":4.5,` +
				`"memoryBytes":6000,"memoryTotalBytes":16000,` +
				`"diskUsedBytes":50000,"diskAvailableBytes":187500,"diskTotalBytes":250000},"containers":[]}`,
		},
		{
			name: "host CPU is null on the first poll",
			source: &fakeSource{ok: true, snap: monitor.Snapshot{
				UpdatedAt: updated,
				Host:      &monitor.HostStatus{Memory: host.Memory{Total: 1, Available: 1}},
			}},
			wantStatus: http.StatusOK,
			wantBody: `{"updatedAt":"2026-10-01T12:00:05Z","stale":false,"host":{"cpuPercent":null,` +
				`"memoryBytes":0,"memoryTotalBytes":1,` +
				`"diskUsedBytes":0,"diskAvailableBytes":0,"diskTotalBytes":0},"containers":[]}`,
		},
		{
			name:       "stale snapshot is still served, marked stale",
			source:     &fakeSource{ok: true, snap: monitor.Snapshot{UpdatedAt: updated, Stale: true}},
			wantStatus: http.StatusOK,
			wantBody:   `{"updatedAt":"2026-10-01T12:00:05Z","stale":true,"host":null,"containers":[]}`,
		},
		{
			name:       "no successful poll yet returns 502 without details",
			source:     &fakeSource{},
			wantStatus: http.StatusBadGateway,
			wantBody:   `{"error":"could not list containers"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/containers", nil)
			rec := httptest.NewRecorder()

			New(tt.source, nil).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if got := strings.TrimSpace(rec.Body.String()); got != tt.wantBody {
				t.Errorf("body =\n  %s\nwant\n  %s", got, tt.wantBody)
			}
		})
	}
}

func TestStaticFiles(t *testing.T) {
	static := fstest.MapFS{
		"index.html":      {Data: []byte("<h1>dashboard</h1>")},
		"assets/index.js": {Data: []byte("console.log('hi')")},
	}

	tests := []struct {
		name       string
		static     fs.FS
		path       string
		wantStatus int
		wantBody   string
	}{
		{name: "root serves index.html", static: static, path: "/", wantStatus: http.StatusOK, wantBody: "<h1>dashboard</h1>"},
		{name: "assets are served", static: static, path: "/assets/index.js", wantStatus: http.StatusOK, wantBody: "console.log('hi')"},
		{name: "missing file is 404", static: static, path: "/nope.js", wantStatus: http.StatusNotFound},
		{
			name: "API still wins over static files", static: static, path: "/api/containers",
			wantStatus: http.StatusOK, wantBody: `{"updatedAt":"0001-01-01T00:00:00Z","stale":false,"host":null,"containers":[]}`,
		},
		{name: "no static files configured", static: nil, path: "/", wantStatus: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()

			New(&fakeSource{ok: true}, tt.static).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantBody == "" {
				return
			}
			if got := strings.TrimSpace(rec.Body.String()); got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}
		})
	}
}
