package engine

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"testing"
)

func TestRunJoinsExtraNetworks(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.handle("POST /containers/create", respondJSON(http.StatusCreated, `{"Id":"c1"}`))
	f.handle("POST /containers/c1/start", respondJSON(http.StatusNoContent, ""))
	if _, err := c.Run(t.Context(), RunSpec{Name: "app-hello-7", Image: "x", Network: "platform-apps",
		ExtraNetworks: []string{"ops-db-hello"}}); err != nil {
		t.Fatal(err)
	}
	req, _ := f.request("POST /containers/create")
	var got createBody
	if err := json.Unmarshal(req.body, &got); err != nil {
		t.Fatal(err)
	}
	var nets []string
	for n := range got.NetworkingConfig.EndpointsConfig {
		nets = append(nets, n)
	}
	slices.Sort(nets)
	// Both at creation; Caddy's network is still the main one.
	if !slices.Equal(nets, []string{"ops-db-hello", "platform-apps"}) || got.HostConfig.NetworkMode != "platform-apps" {
		t.Errorf("networks = %v (mode %q), want platform-apps and ops-db-hello", nets, got.HostConfig.NetworkMode)
	}
	if got.Labels[LabelRole] != RoleApp {
		t.Errorf("role label = %q, want %q", got.Labels[LabelRole], RoleApp)
	}
}

func TestEnsureNetwork(t *testing.T) {
	t.Run("exists", func(t *testing.T) {
		f, c := newFakeDaemon(t)
		f.handle("GET /networks/ops-db-hello", respondJSON(http.StatusOK, `{"Name":"ops-db-hello"}`))
		if err := c.EnsureNetwork(t.Context(), "ops-db-hello", nil); err != nil {
			t.Fatal(err)
		}
		if _, created := f.request("POST /networks/create"); created {
			t.Error("created a network that exists")
		}
	})
	t.Run("missing", func(t *testing.T) {
		f, c := newFakeDaemon(t)
		f.handle("POST /networks/create", respondJSON(http.StatusCreated, `{"Id":"n1"}`))
		if err := c.EnsureNetwork(t.Context(), "ops-db-hello", map[string]string{LabelApp: "hello"}); err != nil {
			t.Fatal(err)
		}
		req, _ := f.request("POST /networks/create")
		var got struct {
			Name     string
			Driver   string
			Internal bool
			Labels   map[string]string
		}
		if err := json.Unmarshal(req.body, &got); err != nil {
			t.Fatal(err)
		}
		// Internal: no way out, not even to the internet.
		if got.Name != "ops-db-hello" || got.Driver != "bridge" || !got.Internal ||
			got.Labels[LabelManaged] != "true" || got.Labels[LabelApp] != "hello" {
			t.Errorf("network create = %+v", got)
		}
	})
	t.Run("created meanwhile", func(t *testing.T) {
		f, c := newFakeDaemon(t)
		f.handle("POST /networks/create", respondJSON(http.StatusConflict, `{"message":"network with name ops-db-hello already exists"}`))
		if err := c.EnsureNetwork(t.Context(), "ops-db-hello", nil); err != nil {
			t.Errorf("EnsureNetwork = %v, want nil: it exists", err)
		}
	})
}

func TestPull(t *testing.T) {
	t.Run("already there", func(t *testing.T) {
		f, c := newFakeDaemon(t)
		f.handle("GET /images/postgres:18.6/json", respondJSON(http.StatusOK, `{"Id":"sha256:p"}`))
		if err := c.Pull(t.Context(), "postgres:18.6"); err != nil {
			t.Fatal(err)
		}
		if _, pulled := f.request("POST /images/create"); pulled {
			t.Error("pulled an image Docker has")
		}
	})
	t.Run("missing", func(t *testing.T) {
		f, c := newFakeDaemon(t)
		f.handle("POST /images/create", respondJSON(http.StatusOK,
			`{"status":"Pulling from library/postgres"}`+"\n"+`{"status":"Status: Downloaded newer image for postgres:18.6"}`))
		if err := c.Pull(t.Context(), "postgres:18.6"); err != nil {
			t.Fatal(err)
		}
		req, _ := f.request("POST /images/create")
		// The SDK spells out Docker Hub's official-image name.
		if q, _ := url.ParseQuery(req.query); q.Get("fromImage") != "docker.io/library/postgres" || q.Get("tag") != "18.6" {
			t.Errorf("pull query = %q, want postgres at 18.6", req.query)
		}
	})
	t.Run("fails mid-stream", func(t *testing.T) {
		f, c := newFakeDaemon(t)
		f.handle("POST /images/create", respondJSON(http.StatusOK,
			`{"errorDetail":{"message":"manifest unknown"},"error":"manifest unknown"}`))
		if err := c.Pull(t.Context(), "postgres:18.6"); err == nil {
			t.Error("Pull succeeded although the stream reported an error")
		}
	})
}

func TestRunDatabase(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.handle("POST /containers/create", respondJSON(http.StatusCreated, `{"Id":"d1"}`))
	f.handle("POST /containers/d1/start", respondJSON(http.StatusNoContent, ""))
	id, err := c.RunDatabase(t.Context(), DatabaseSpec{
		Name: "ops-db-hello", Image: "postgres:18.6", Env: map[string]string{"POSTGRES_PASSWORD": "pw"},
		Labels: map[string]string{LabelApp: "hello"}, Network: "ops-db-hello", Volume: "ops-db-hello",
		MemoryBytes: 256 << 20,
	})
	if err != nil || id != "d1" {
		t.Fatalf("RunDatabase = %q, %v", id, err)
	}
	req, _ := f.request("POST /containers/create")
	var got struct {
		createBody
		HostConfig struct {
			NetworkMode   string
			Privileged    bool
			Binds         []string
			PortBindings  map[string]any
			RestartPolicy struct{ Name string }
			Memory        int64
			SecurityOpt   []string
			Mounts        []struct{ Type, Source, Target string }
		}
	}
	if err := json.Unmarshal(req.body, &got); err != nil {
		t.Fatal(err)
	}
	hc := got.HostConfig
	// Its one mount: the named volume, at Postgres 18's data directory.
	if len(hc.Mounts) != 1 || hc.Mounts[0].Type != "volume" || hc.Mounts[0].Source != "ops-db-hello" ||
		hc.Mounts[0].Target != "/var/lib/postgresql" {
		t.Errorf("mounts = %+v", hc.Mounts)
	}
	// Otherwise locked down like an app.
	if hc.Privileged || len(hc.Binds) > 0 || len(hc.PortBindings) > 0 || hc.RestartPolicy.Name != "no" ||
		hc.Memory != 256<<20 || !slices.Contains(hc.SecurityOpt, "no-new-privileges:true") {
		t.Errorf("host config = %+v", hc)
	}
	if hc.NetworkMode != "ops-db-hello" || len(got.NetworkingConfig.EndpointsConfig) != 1 {
		t.Errorf("networks = %q, %v; want only its own", hc.NetworkMode, got.NetworkingConfig.EndpointsConfig)
	}
	if got.Labels[LabelManaged] != "true" || got.Labels[LabelRole] != RoleDatabase || got.Labels[LabelApp] != "hello" {
		t.Errorf("labels = %v", got.Labels)
	}
}
