package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthURL(t *testing.T) {
	tests := []struct {
		addr    string
		want    string
		wantErr bool
	}{
		{addr: ":8080", want: "http://127.0.0.1:8080/healthz"},
		{addr: "0.0.0.0:8080", want: "http://127.0.0.1:8080/healthz"},
		{addr: "[::]:8080", want: "http://127.0.0.1:8080/healthz"},
		{addr: "127.0.0.1:18080", want: "http://127.0.0.1:18080/healthz"},
		{addr: "[::1]:8080", want: "http://[::1]:8080/healthz"},
		{addr: "8080", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			got, err := healthURL(tt.addr)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("healthURL(%q) = %q, want error", tt.addr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("healthURL(%q): %v", tt.addr, err)
			}
			if got != tt.want {
				t.Errorf("healthURL(%q) = %q, want %q", tt.addr, got, tt.want)
			}
		})
	}
}

func TestHealthcheck(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "healthy", status: http.StatusOK},
		{name: "unhealthy status", status: http.StatusServiceUnavailable, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/healthz" {
					t.Errorf("path = %s, want /healthz", r.URL.Path)
				}
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			err := healthcheck(strings.TrimPrefix(srv.URL, "http://"))
			if (err != nil) != tt.wantErr {
				t.Errorf("healthcheck error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestHealthcheckNothingListening(t *testing.T) {
	// Closed straight away, so the port is known to be free.
	srv := httptest.NewServer(nil)
	srv.Close()

	if err := healthcheck(strings.TrimPrefix(srv.URL, "http://")); err == nil {
		t.Error("healthcheck succeeded with nothing listening, want error")
	}
}
