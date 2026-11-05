package engine

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"testing"
)

func TestRunTask(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.handle("POST /containers/create", respondJSON(http.StatusCreated, `{"Id":"t1"}`))
	f.handle("POST /containers/t1/start", respondJSON(http.StatusNoContent, ""))
	id, err := c.RunTask(t.Context(), TaskSpec{
		Name: "ops-backup-hello-3", Image: "postgres:18.6", Cmd: []string{"sh", "-c", "pg_dump"},
		Env: map[string]string{"PGPASSWORD": "pw"}, Labels: map[string]string{LabelRole: RoleBackup},
		Network: "ops-db-hello", User: "1000:1000", Volume: "platform-backups", VolumeTarget: "/backups",
		MemoryBytes: 256 << 20,
	})
	if err != nil || id != "t1" {
		t.Fatalf("RunTask = %q, %v", id, err)
	}
	req, _ := f.request("POST /containers/create")
	if q, _ := url.ParseQuery(req.query); q.Get("name") != "ops-backup-hello-3" {
		t.Errorf("name = %q", q.Get("name"))
	}
	var got struct {
		Image      string
		Cmd        []string
		User       string
		Labels     map[string]string
		HostConfig struct {
			NetworkMode   string
			Privileged    bool
			Binds         []string
			RestartPolicy struct{ Name string }
			SecurityOpt   []string
			Mounts        []struct{ Type, Source, Target string }
		}
	}
	if err := json.Unmarshal(req.body, &got); err != nil {
		t.Fatal(err)
	}
	hc := got.HostConfig
	if got.User != "1000:1000" || !slices.Equal(got.Cmd, []string{"sh", "-c", "pg_dump"}) ||
		got.Labels[LabelManaged] != "true" || got.Labels[LabelRole] != RoleBackup {
		t.Errorf("config = %+v", got)
	}
	if len(hc.Mounts) != 1 || hc.Mounts[0].Type != "volume" || hc.Mounts[0].Source != "platform-backups" ||
		hc.Mounts[0].Target != "/backups" || len(hc.Binds) != 0 || hc.Privileged ||
		hc.NetworkMode != "ops-db-hello" || !slices.Contains(hc.SecurityOpt, "no-new-privileges:true") {
		t.Errorf("host config = %+v", hc)
	}
	// Meant to stop: never restarted.
	if hc.RestartPolicy.Name != "" && hc.RestartPolicy.Name != "no" {
		t.Errorf("restart policy = %q", hc.RestartPolicy.Name)
	}
}

func TestWait(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.handle("POST /containers/t1/wait", respondJSON(http.StatusOK, `{"StatusCode":1}`))
	code, err := c.Wait(t.Context(), "t1")
	if err != nil || code != 1 {
		t.Errorf("Wait = %d, %v; want exit code 1", code, err)
	}
	req, _ := f.request("POST /containers/t1/wait")
	if q, _ := url.ParseQuery(req.query); q.Get("condition") != "not-running" {
		t.Errorf("condition = %q, want not-running", q.Get("condition"))
	}
	if _, err := c.Wait(t.Context(), "missing"); err == nil {
		t.Error("Wait on a missing container succeeded")
	}
}
