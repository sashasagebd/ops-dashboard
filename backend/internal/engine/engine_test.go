package engine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewOnlyAcceptsTCP(t *testing.T) {
	tests := []struct {
		host    string
		wantErr bool
	}{
		{host: "tcp://docker-proxy-rw:2375"},
		{host: "unix:///var/run/docker.sock", wantErr: true},
		{host: "npipe:////./pipe/docker_engine", wantErr: true},
		{host: "docker-proxy-rw:2375", wantErr: true},
		{host: "tcp://", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			c, err := New(tt.host)
			if tt.wantErr {
				if err == nil {
					t.Errorf("New(%q) succeeded, want error", tt.host)
				}
				return
			}
			if err != nil {
				t.Fatalf("New(%q): %v", tt.host, err)
			}
			c.Close()
		})
	}
}

func TestVersionNegotiatesAPIVersion(t *testing.T) {
	// A daemon older than the SDK: the client must ask what it speaks
	// (/_ping) and then use that version in its request paths, which is what
	// keeps the platform working across Docker upgrades and downgrades.
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/_ping":
			w.Header().Set("Api-Version", "1.47")
			_, _ = w.Write([]byte("OK"))
		case "/v1.47/version":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Version":"27.5.1","ApiVersion":"1.47"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	daemon, api, err := c.Version(t.Context())
	if err != nil {
		t.Fatalf("Version: %v (requests: %v)", err, paths)
	}
	if daemon != "27.5.1" || api != "1.47" {
		t.Errorf("Version = %q, %q; want 27.5.1, 1.47", daemon, api)
	}
}
