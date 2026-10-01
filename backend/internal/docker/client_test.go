package docker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// newTestClient starts a fake Docker API that answers every request with
// handler, and returns a Client pointed at it.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := NewClient(strings.Replace(srv.URL, "http://", "tcp://", 1))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestNewClient(t *testing.T) {
	tests := []struct {
		host        string
		wantBaseURL string
		wantErr     bool
	}{
		{host: "tcp://docker-proxy:2375", wantBaseURL: "http://docker-proxy:2375"},
		{host: "tcp://127.0.0.1:2375", wantBaseURL: "http://127.0.0.1:2375"},
		{host: "unix:///var/run/docker.sock", wantErr: true},
		{host: "http://docker-proxy:2375", wantErr: true},
		{host: "docker-proxy:2375", wantErr: true},
		{host: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			c, err := NewClient(tt.host)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NewClient(%q) succeeded, want error", tt.host)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewClient(%q): %v", tt.host, err)
			}
			if c.baseURL != tt.wantBaseURL {
				t.Errorf("baseURL = %q, want %q", c.baseURL, tt.wantBaseURL)
			}
		})
	}
}

// listResponse is trimmed from a real `GET /containers/json` response; Docker
// sends many more fields, which the client must ignore.
const listResponse = `[
  {
    "Id": "8dfafdbc3a40",
    "Names": ["/minecraft"],
    "Image": "itzg/minecraft-server",
    "ImageID": "sha256:abc",
    "Command": "/start",
    "Created": 1727800000,
    "State": "running",
    "Status": "Up 3 hours (healthy)",
    "Ports": [{"PrivatePort": 25565, "PublicPort": 25565, "Type": "tcp"}],
    "Labels": {"com.docker.compose.project": "minecraft"}
  },
  {
    "Id": "9cd87474be90",
    "Names": [],
    "Image": "discord-bot:latest",
    "State": "running",
    "Status": "Up 2 days"
  }
]`

func TestListContainers(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/containers/json" {
			t.Errorf("request = %s %s, want GET /containers/json", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(listResponse))
	})

	got, err := c.ListContainers(context.Background())
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}

	want := []Container{
		{ID: "8dfafdbc3a40", Name: "minecraft", Image: "itzg/minecraft-server", State: "running", Status: "Up 3 hours (healthy)"},
		{ID: "9cd87474be90", Name: "9cd87474be90", Image: "discord-bot:latest", State: "running", Status: "Up 2 days"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("ListContainers =\n  %+v\nwant\n  %+v", got, want)
	}
}

func TestListContainersErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{
			name:    "proxy forbids the endpoint",
			status:  http.StatusForbidden,
			body:    "<html><body><h1>403 Forbidden</h1></body></html>",
			wantErr: "403 Forbidden",
		},
		{
			name:    "Docker error message is included",
			status:  http.StatusInternalServerError,
			body:    `{"message":"something broke"}`,
			wantErr: "something broke",
		},
		{
			name:    "malformed JSON",
			status:  http.StatusOK,
			body:    `{not json`,
			wantErr: "decoding response",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})

			_, err := c.ListContainers(context.Background())
			if err == nil {
				t.Fatal("ListContainers succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestListContainersUnreachable(t *testing.T) {
	// Start a server and close it straight away, so the port is known to be
	// free and nothing is listening on it.
	srv := httptest.NewServer(nil)
	srv.Close()
	c, err := NewClient(strings.Replace(srv.URL, "http://", "tcp://", 1))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if _, err := c.ListContainers(context.Background()); err == nil {
		t.Fatal("ListContainers succeeded, want connection error")
	}
}
