package engine

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"testing"
)

// createBody is the part of Docker's container-create request the tests check.
type createBody struct {
	Image       string
	Env         []string
	Labels      map[string]string
	StopTimeout *int
	HostConfig  struct {
		NetworkMode   string
		Privileged    bool
		Binds         []string
		PortBindings  map[string]any
		RestartPolicy struct{ Name string }
		Memory        int64
		NanoCpus      int64
		PidsLimit     *int64
		SecurityOpt   []string
		LogConfig     struct {
			Type   string
			Config map[string]string
		}
	}
	NetworkingConfig struct {
		EndpointsConfig map[string]any
	}
}

func TestRunCreatesLockedDownContainer(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.handle("POST /containers/create", respondJSON(http.StatusCreated, `{"Id":"c1","Warnings":[]}`))
	f.handle("POST /containers/c1/start", respondJSON(http.StatusNoContent, ""))

	id, err := c.Run(t.Context(), RunSpec{
		Name:        "app-hello-7",
		Image:       "hello:abc123",
		Env:         map[string]string{"TOKEN": "s3cret", "API_URL": "https://x"},
		Labels:      map[string]string{LabelApp: "hello", LabelDeployment: "7"},
		Network:     "platform-apps",
		MemoryBytes: 256 << 20,
		NanoCPUs:    500_000_000,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if id != "c1" {
		t.Errorf("id = %q, want c1", id)
	}

	req, _ := f.request("POST /containers/create")
	if q, _ := url.ParseQuery(req.query); q.Get("name") != "app-hello-7" {
		t.Errorf("name = %q, want app-hello-7", q.Get("name"))
	}
	var got createBody
	if err := json.Unmarshal(req.body, &got); err != nil {
		t.Fatal(err)
	}
	hc := got.HostConfig

	// What the spec asked for.
	if got.Image != "hello:abc123" || hc.Memory != 256<<20 || hc.NanoCpus != 500_000_000 {
		t.Errorf("image/limits = %q, %d, %d", got.Image, hc.Memory, hc.NanoCpus)
	}
	if !slices.Equal(got.Env, []string{"API_URL=https://x", "TOKEN=s3cret"}) {
		t.Errorf("env = %v, want sorted KEY=value", got.Env)
	}
	if got.Labels[LabelManaged] != "true" || got.Labels[LabelApp] != "hello" || got.Labels[LabelDeployment] != "7" {
		t.Errorf("labels = %v, want managed=true plus the spec's", got.Labels)
	}
	if hc.NetworkMode != "platform-apps" || len(got.NetworkingConfig.EndpointsConfig) != 1 {
		t.Errorf("networks = %q, %v; want only platform-apps", hc.NetworkMode, got.NetworkingConfig.EndpointsConfig)
	}

	// What every app gets, whatever the spec says.
	if hc.Privileged || len(hc.Binds) > 0 || len(hc.PortBindings) > 0 {
		t.Errorf("privileged/binds/ports = %v, %v, %v; want none", hc.Privileged, hc.Binds, hc.PortBindings)
	}
	// The reconciler restarts apps, not Docker.
	if hc.RestartPolicy.Name != "no" {
		t.Errorf("restart policy = %q, want no", hc.RestartPolicy.Name)
	}
	if hc.PidsLimit == nil || *hc.PidsLimit != pidsLimit {
		t.Errorf("pids limit = %v, want %d", hc.PidsLimit, pidsLimit)
	}
	if !slices.Contains(hc.SecurityOpt, "no-new-privileges:true") {
		t.Errorf("security opts = %v, want no-new-privileges", hc.SecurityOpt)
	}
	if hc.LogConfig.Config["max-size"] == "" || hc.LogConfig.Config["max-file"] == "" {
		t.Errorf("log config = %+v, want rotation limits", hc.LogConfig)
	}
	if got.StopTimeout == nil || *got.StopTimeout != stopTimeoutSeconds {
		t.Errorf("stop timeout = %v, want %d", got.StopTimeout, stopTimeoutSeconds)
	}
}

func TestRunRemovesContainerThatFailsToStart(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.handle("POST /containers/create", respondJSON(http.StatusCreated, `{"Id":"c1"}`))
	f.handle("POST /containers/c1/start", respondJSON(http.StatusInternalServerError, `{"message":"port already in use"}`))
	f.handle("POST /containers/c1/stop", respondJSON(http.StatusNoContent, ""))
	f.handle("DELETE /containers/c1", respondJSON(http.StatusNoContent, ""))

	if _, err := c.Run(t.Context(), RunSpec{Name: "app-hello-7", Image: "hello:abc", Network: "platform-apps"}); err == nil {
		t.Fatal("Run succeeded although start failed")
	}
	if _, ok := f.request("DELETE /containers/c1"); !ok {
		t.Errorf("the created container wasn't removed; requests: %v", f.routes())
	}
}

func TestRemove(t *testing.T) {
	tests := []struct {
		name          string
		stop, remove  int // status codes the fake daemon answers with
		wantErr       bool
		wantRemoveReq bool
	}{
		{name: "running", stop: http.StatusNoContent, remove: http.StatusNoContent, wantRemoveReq: true},
		{name: "already stopped", stop: http.StatusNotModified, remove: http.StatusNoContent, wantRemoveReq: true},
		// Gone already, e.g. when a deploy is retried: not an error.
		{name: "already gone", stop: http.StatusNotFound, remove: http.StatusNotFound, wantRemoveReq: true},
		{name: "daemon error", stop: http.StatusInternalServerError, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c := newFakeDaemon(t)
			f.handle("POST /containers/c1/stop", respondJSON(tt.stop, `{"message":"x"}`))
			f.handle("DELETE /containers/c1", respondJSON(tt.remove, `{"message":"x"}`))

			err := c.Remove(t.Context(), "c1")
			if (err != nil) != tt.wantErr {
				t.Errorf("Remove error = %v, wantErr %v", err, tt.wantErr)
			}
			if _, ok := f.request("DELETE /containers/c1"); ok != tt.wantRemoveReq {
				t.Errorf("remove request sent = %v, want %v", ok, tt.wantRemoveReq)
			}
		})
	}
}

func TestManagedContainersFiltersOnDockersSide(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.handle("GET /containers/json", respondJSON(http.StatusOK, `[
		{"Id":"c1","Names":["/app-hello-7"],"State":"running","Labels":{"ops.platform.app":"hello"}}
	]`))

	got, err := c.ManagedContainers(t.Context(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "app-hello-7" || got[0].State != "running" {
		t.Errorf("ManagedContainers = %+v", got)
	}

	req, _ := f.request("GET /containers/json")
	q, _ := url.ParseQuery(req.query)
	if q.Get("all") != "1" {
		t.Errorf("all = %q, want 1 (stopped containers too)", q.Get("all"))
	}
	var filters map[string]map[string]bool
	if err := json.Unmarshal([]byte(q.Get("filters")), &filters); err != nil {
		t.Fatalf("filters %q: %v", q.Get("filters"), err)
	}
	if !filters["label"][LabelManaged+"=true"] || !filters["label"][LabelApp+"=hello"] {
		t.Errorf("label filters = %v, want managed=true and app=hello", filters["label"])
	}
}
