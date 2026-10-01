package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

			New(&fakeLister{}).ServeHTTP(rec, req)

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
			wantBody: `[{"id":"a1","name":"discord-bot","image":"discord-bot:latest","state":"running","status":"Up 2 days"},` +
				`{"id":"b2","name":"minecraft","image":"itzg/minecraft-server","state":"running","status":"Up 3 hours"}]`,
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

			New(tt.lister).ServeHTTP(rec, req)

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
