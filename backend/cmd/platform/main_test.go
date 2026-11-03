package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWaitForDatabase(t *testing.T) {
	tests := []struct {
		name      string
		failures  int // pings that fail before one succeeds
		wantPings int
	}{
		{name: "up straight away", failures: 0, wantPings: 1},
		{name: "up after a few tries", failures: 3, wantPings: 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pings := 0
			ping := func(context.Context) error {
				pings++
				if pings <= tt.failures {
					return errors.New("connection refused")
				}
				return nil
			}

			if err := waitForDatabase(t.Context(), ping, time.Millisecond, 4*time.Millisecond); err != nil {
				t.Fatalf("waitForDatabase: %v", err)
			}
			if pings != tt.wantPings {
				t.Errorf("pinged %d times, want %d", pings, tt.wantPings)
			}
		})
	}
}

func TestWaitForDatabaseStopsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	ping := func(context.Context) error {
		cancel() // shutdown arrives while the database is still down
		return errors.New("connection refused")
	}

	err := waitForDatabase(ctx, ping, time.Hour, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("waitForDatabase error = %v, want context.Canceled", err)
	}
}

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	newHandler(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got, want := rec.Body.String(), `{"status":"ok"}`+"\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}
