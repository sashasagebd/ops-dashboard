package deploy

import (
	"slices"
	"strings"
	"testing"

	"github.com/sashasagebd/ops-dashboard/backend/internal/engine"
	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

// dbContainer is the app's database container, which Stop leaves alone.
var dbContainer = engine.ManagedContainer{ID: "db", Name: "ops-db-hello",
	Labels: map[string]string{engine.LabelRole: engine.RoleDatabase}}

func TestStopApp(t *testing.T) {
	f := newFakes() // deployment 6 live, a leftover 3
	f.containers = append(f.containers, dbContainer)
	app, err := f.deployer().StopApp(t.Context(), 1)
	if err != nil || !app.Stopped {
		t.Fatalf("StopApp = %+v, %v", app, err)
	}
	// Recorded first, then the route goes, then the app's containers (not
	// its database's).
	want := []string{"store: stopped", "apply []", "remove old6", "remove stale3"}
	if !slices.Equal(f.events, want) {
		t.Errorf("events = %q, want %q", f.events, want)
	}
	if len(f.containers) != 1 || f.containers[0].ID != "db" {
		t.Errorf("containers left = %+v, want only the database", f.containers)
	}
}

func TestStopAppRefused(t *testing.T) {
	f := newFakes()
	f.lifecycleErr = store.ErrDeployInProgress
	if _, err := f.deployer().StopApp(t.Context(), 1); err != store.ErrDeployInProgress {
		t.Fatalf("error = %v, want ErrDeployInProgress", err)
	}
	if len(f.events) != 0 {
		t.Errorf("events = %q, want nothing touched", f.events)
	}
}

func TestStartApp(t *testing.T) {
	f := newFakes()
	f.app.Stopped = true
	f.containers = nil // as Stop left it
	app, err := f.deployer().StartApp(t.Context(), 1)
	if err != nil || app.Stopped {
		t.Fatalf("StartApp = %+v, %v", app, err)
	}
	// The live version runs again from its image, with no build, before
	// its route comes back.
	want := []string{"store: started", "image exists? ops-platform/hello:" + sha, "run app-hello-6",
		"apply [hello.apps.test->app-hello-6:8080]"}
	if !slices.Equal(f.events, want) {
		t.Errorf("events = %q, want %q", f.events, want)
	}
	if f.runSpec.Image != ImageTag("hello", sha) {
		t.Errorf("ran %q, want the live deployment's image", f.runSpec.Image)
	}
}

func TestStartAppWithNothingLive(t *testing.T) {
	f := newFakes()
	f.app.Stopped = true
	f.statuses = map[int64]store.DeploymentStatus{6: store.StatusFailed}
	f.containers = nil
	if _, err := f.deployer().StartApp(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if want := []string{"store: started"}; !slices.Equal(f.events, want) {
		t.Errorf("events = %q, want %q: nothing to run", f.events, want)
	}
}

func TestDeleteApp(t *testing.T) {
	f := newFakes()
	f.database = &store.Database{AppID: 1}
	f.containers = append(f.containers, dbContainer)
	f.images = []engine.Image{
		{ID: "i1", Tags: []string{"ops-platform/hello:" + sha}},
		{ID: "i2"}, // untagged: removed by ID
	}
	deleted, err := f.deployer().DeleteApp(t.Context(), 1)
	if err != nil || !deleted.HadDatabase {
		t.Fatalf("DeleteApp = %+v, %v", deleted, err)
	}
	// Recorded first; then routes, every container (the database's too),
	// the images and the database's network. No volume: that stays.
	want := []string{"store: deleted", "apply []", "remove old6", "remove stale3", "remove db",
		"remove image ops-platform/hello:" + sha, "remove image i2", "remove network ops-db-hello"}
	if !slices.Equal(f.events, want) {
		t.Errorf("events = %q, want %q", f.events, want)
	}
	for _, e := range f.events {
		if strings.Contains(e, "volume") {
			t.Errorf("event %q: the platform must never remove a volume", e)
		}
	}
}

func TestDeleteAppWithoutDatabase(t *testing.T) {
	f := newFakes()
	if _, err := f.deployer().DeleteApp(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(f.events, "remove network ops-db-hello") {
		t.Error("removed a database network the app never had")
	}
}
