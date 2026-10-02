package docker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
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

// listResponse is trimmed from a real `GET /containers/json?all=true`
// response; Docker sends many more fields, which the client must ignore.
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
    "State": "exited",
    "Status": "Exited (1) 20 minutes ago"
  }
]`

// inspectResponses are trimmed `GET /containers/{id}/json` responses, keyed by
// ID. Docker writes times it doesn't have as the zero time, like minecraft's
// FinishedAt here. Only minecraft has a healthcheck, so only it has Health.
var inspectResponses = map[string]string{
	"8dfafdbc3a40": `{
	  "Id": "8dfafdbc3a40",
	  "State": {
	    "Status": "running",
	    "Running": true,
	    "StartedAt": "2026-10-01T09:00:00.123456789Z",
	    "FinishedAt": "0001-01-01T00:00:00Z",
	    "Health": {"Status": "healthy", "FailingStreak": 0, "Log": []}
	  }
	}`,
	"9cd87474be90": `{
	  "Id": "9cd87474be90",
	  "State": {
	    "Status": "exited",
	    "ExitCode": 1,
	    "StartedAt": "2026-09-29T04:30:00Z",
	    "FinishedAt": "2026-10-01T11:40:00Z"
	  }
	}`,
}

// fakeDockerAPI serves listResponse for the list endpoint and inspect
// (by container ID) for the inspect endpoint. A missing ID gets a 404, like
// Docker for a container that no longer exists.
func fakeDockerAPI(t *testing.T, inspect map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/containers/json" {
			if got := r.URL.Query().Get("all"); got != "true" {
				t.Errorf("list query all = %q, want true (stopped containers too)", got)
			}
			_, _ = w.Write([]byte(listResponse))
			return
		}

		id, ok := strings.CutPrefix(r.URL.Path, "/containers/")
		id, ok2 := strings.CutSuffix(id, "/json")
		if !ok || !ok2 {
			t.Errorf("unexpected request path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		body, found := inspect[id]
		if !found {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"No such container: ` + id + `"}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}
}

func TestListContainers(t *testing.T) {
	c := newTestClient(t, fakeDockerAPI(t, inspectResponses))

	got, err := c.ListContainers(context.Background())
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}

	want := []Container{
		{
			ID: "8dfafdbc3a40", Name: "minecraft", Image: "itzg/minecraft-server",
			State: "running", Status: "Up 3 hours (healthy)", Health: "healthy",
			StartedAt: time.Date(2026, 10, 1, 9, 0, 0, 123456789, time.UTC),
		},
		{
			ID: "9cd87474be90", Name: "9cd87474be90", Image: "discord-bot:latest",
			State: "exited", Status: "Exited (1) 20 minutes ago",
			StartedAt:  time.Date(2026, 9, 29, 4, 30, 0, 0, time.UTC),
			FinishedAt: time.Date(2026, 10, 1, 11, 40, 0, 0, time.UTC),
		},
	}
	// Compare times with Equal, not ==: == also compares the location and
	// monotonic clock reading, so the same instant can compare unequal.
	if !slices.EqualFunc(got, want, containersEqual) {
		t.Errorf("ListContainers =\n  %+v\nwant\n  %+v", got, want)
	}
	if !got[0].FinishedAt.IsZero() {
		t.Errorf("minecraft FinishedAt = %v, want zero time (Docker's 0001-01-01)", got[0].FinishedAt)
	}
}

func containersEqual(a, b Container) bool {
	return a.ID == b.ID && a.Name == b.Name && a.Image == b.Image &&
		a.State == b.State && a.Status == b.Status && a.Health == b.Health &&
		a.StartedAt.Equal(b.StartedAt) && a.FinishedAt.Equal(b.FinishedAt)
}

func TestListContainersSkipsRemovedContainer(t *testing.T) {
	// The discord-bot container is in the list but gone by the time it's
	// inspected (404).
	inspect := map[string]string{"8dfafdbc3a40": inspectResponses["8dfafdbc3a40"]}
	c := newTestClient(t, fakeDockerAPI(t, inspect))

	got, err := c.ListContainers(context.Background())
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(got) != 1 || got[0].Name != "minecraft" {
		t.Errorf("ListContainers = %+v, want only minecraft", got)
	}
}

func TestListContainersInspectError(t *testing.T) {
	inspect := map[string]string{
		"8dfafdbc3a40": inspectResponses["8dfafdbc3a40"],
		"9cd87474be90": `{not json`,
	}
	c := newTestClient(t, fakeDockerAPI(t, inspect))

	_, err := c.ListContainers(context.Background())
	if err == nil {
		t.Fatal("ListContainers succeeded, want error")
	}
	if !strings.Contains(err.Error(), "inspecting container 9cd87474be90") {
		t.Errorf("error = %q, want it to name the container", err)
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
