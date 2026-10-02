package server

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/docker"
)

// fakeLister is a ContainerLister that returns canned results.
type fakeLister struct {
	containers []docker.Container
	err        error
}

func (f *fakeLister) ListContainers(context.Context) ([]docker.Container, error) {
	return f.containers, f.err
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

			New(&fakeLister{}, nil).ServeHTTP(rec, req)

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
	tests := []struct {
		name       string
		lister     *fakeLister
		wantStatus int
		wantBody   string
	}{
		{
			name: "returns containers sorted by name",
			lister: &fakeLister{containers: []docker.Container{
				{ID: "b2", Name: "minecraft", Image: "itzg/minecraft-server", State: "running", Status: "Up 3 hours"},
				{ID: "a1", Name: "discord-bot", Image: "discord-bot:latest", State: "running", Status: "Up 2 days"},
			}},
			wantStatus: http.StatusOK,
			wantBody: `[{"id":"a1","name":"discord-bot","image":"discord-bot:latest","state":"running","status":"Up 2 days","startedAt":null,"finishedAt":null},` +
				`{"id":"b2","name":"minecraft","image":"itzg/minecraft-server","state":"running","status":"Up 3 hours","startedAt":null,"finishedAt":null}]`,
		},
		{
			name: "times are RFC 3339, and zero times are null",
			lister: &fakeLister{containers: []docker.Container{
				{
					ID: "a1", Name: "discord-bot", Image: "discord-bot:latest", State: "exited", Status: "Exited (1) 20 minutes ago",
					StartedAt:  time.Date(2026, 9, 29, 4, 30, 0, 0, time.UTC),
					FinishedAt: time.Date(2026, 10, 1, 11, 40, 0, 0, time.UTC),
				},
				{
					ID: "b2", Name: "minecraft", Image: "itzg/minecraft-server", State: "running", Status: "Up 3 hours",
					StartedAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
				},
			}},
			wantStatus: http.StatusOK,
			wantBody: `[{"id":"a1","name":"discord-bot","image":"discord-bot:latest","state":"exited","status":"Exited (1) 20 minutes ago",` +
				`"startedAt":"2026-09-29T04:30:00Z","finishedAt":"2026-10-01T11:40:00Z"},` +
				`{"id":"b2","name":"minecraft","image":"itzg/minecraft-server","state":"running","status":"Up 3 hours",` +
				`"startedAt":"2026-10-01T09:00:00Z","finishedAt":null}]`,
		},
		{
			name:       "no containers encodes as empty array",
			lister:     &fakeLister{},
			wantStatus: http.StatusOK,
			wantBody:   `[]`,
		},
		{
			name:       "Docker error returns 502 without leaking details",
			lister:     &fakeLister{err: errors.New("dial tcp docker-proxy:2375: connection refused")},
			wantStatus: http.StatusBadGateway,
			wantBody:   `{"error":"could not list containers"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/containers", nil)
			rec := httptest.NewRecorder()

			New(tt.lister, nil).ServeHTTP(rec, req)

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
		{name: "API still wins over static files", static: static, path: "/api/containers", wantStatus: http.StatusOK, wantBody: "[]"},
		{name: "no static files configured", static: nil, path: "/", wantStatus: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()

			New(&fakeLister{}, tt.static).ServeHTTP(rec, req)

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
