package store

import (
	"errors"
	"testing"
)

// liveTestApp creates an app with one healthy deployment.
func liveTestApp(t *testing.T, s *Store, name string) (App, Deployment) {
	t.Helper()
	ctx := t.Context()
	app := createTestApp(t, s, name)
	d, err := s.CreateDeployment(ctx, app.ID, sha1)
	if err != nil {
		t.Fatal(err)
	}
	mustDo(t, s.MarkBuilding(ctx, d.ID))
	mustDo(t, s.MarkStarting(ctx, d.ID, name+":"+sha1))
	mustDo(t, s.MarkHealthy(ctx, d.ID))
	return app, d
}

func TestStopAndStartApp(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	app, live := liveTestApp(t, s, "hello")

	stopped, err := s.StopApp(ctx, app.ID)
	if err != nil || !stopped.Stopped {
		t.Fatalf("StopApp = %+v, %v; want stopped", stopped, err)
	}
	// Not routed while stopped, but its live deployment stays live: it's
	// what Start runs again.
	if routed, _ := s.LiveApps(ctx); len(routed) != 0 {
		t.Errorf("LiveApps = %+v, want none while stopped", routed)
	}
	if cur, err := s.CurrentDeployment(ctx, app.ID); err != nil || cur.ID != live.ID {
		t.Errorf("CurrentDeployment = %+v, %v; want the live one kept", cur, err)
	}
	// Nothing can be deployed or rolled back meanwhile, by any route.
	if _, err := s.CreateDeployment(ctx, app.ID, sha2); !errors.Is(err, ErrAppStopped) {
		t.Errorf("CreateDeployment: error = %v, want ErrAppStopped", err)
	}
	if _, _, err := s.RequestDeployment(ctx, app.ID, sha2); !errors.Is(err, ErrAppStopped) {
		t.Errorf("RequestDeployment: error = %v, want ErrAppStopped", err)
	}
	if _, err := s.CreateRollback(ctx, app.ID, live.ID); !errors.Is(err, ErrAppStopped) {
		t.Errorf("CreateRollback: error = %v, want ErrAppStopped", err)
	}

	started, err := s.StartApp(ctx, app.ID)
	if err != nil || started.Stopped {
		t.Fatalf("StartApp = %+v, %v; want running", started, err)
	}
	if routed, _ := s.LiveApps(ctx); len(routed) != 1 {
		t.Errorf("LiveApps = %+v, want it routed again", routed)
	}
	d, err := s.CreateDeployment(ctx, app.ID, sha2)
	if err != nil {
		t.Fatalf("deploying after Start: %v", err)
	}

	// Not while a deploy runs.
	if _, err := s.StopApp(ctx, app.ID); !errors.Is(err, ErrDeployInProgress) {
		t.Errorf("StopApp mid-deploy: error = %v, want ErrDeployInProgress", err)
	}
	if again, _ := s.App(ctx, app.ID); again.Stopped {
		t.Error("a refused StopApp stopped the app")
	}
	mustDo(t, s.MarkFailed(ctx, d.ID, "test"))

	for _, f := range []func() error{
		func() error { _, err := s.StopApp(ctx, 999999); return err },
		func() error { _, err := s.StartApp(ctx, 999999); return err },
		func() error { _, err := s.CreateDeployment(ctx, 999999, sha1); return err },
	} {
		if err := f(); !errors.Is(err, ErrNotFound) {
			t.Errorf("missing app: error = %v, want ErrNotFound", err)
		}
	}
}

func TestStopDropsAWaitingPush(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	app, _ := liveTestApp(t, s, "hello")
	// A push while a deploy runs waits as the next commit; the deploy then
	// fails (the deployer would start the waiting one, but not in this test).
	d, err := s.CreateDeployment(ctx, app.ID, sha2)
	if err != nil {
		t.Fatal(err)
	}
	if _, started, err := s.RequestDeployment(ctx, app.ID, sha3); err != nil || started {
		t.Fatalf("RequestDeployment = started %v, %v; want it waiting", started, err)
	}
	mustDo(t, s.MarkFailed(ctx, d.ID, "test"))
	stopped, err := s.StopApp(ctx, app.ID)
	if err != nil || stopped.NextCommitSHA != "" {
		t.Errorf("StopApp = %+v, %v; want the waiting push dropped", stopped, err)
	}
}

func TestDeleteApp(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	app, live := liveTestApp(t, s, "hello")
	other, _ := liveTestApp(t, s, "other")
	provisionedDB(t, s, app.ID)
	mustDo(t, s.SetEnvVar(ctx, app.ID, "TOKEN", "s3cret"))
	mustDo(t, s.AddEvent(ctx, NewEvent{AppID: app.ID, Kind: EventRestarted, Message: "restarted"}))
	// A newer version, then a rollback to the first: deleting also has to
	// get past deployments that refer to each other (rollback_of).
	newer, err := s.CreateDeployment(ctx, app.ID, sha2)
	if err != nil {
		t.Fatal(err)
	}
	mustDo(t, s.MarkBuilding(ctx, newer.ID))
	mustDo(t, s.MarkStarting(ctx, newer.ID, "hello:"+sha2))
	mustDo(t, s.MarkHealthy(ctx, newer.ID))
	rb, err := s.CreateRollback(ctx, app.ID, live.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustDo(t, s.MarkFailed(ctx, rb.ID, "test"))

	// Not mid-backup.
	b, err := s.CreateBackup(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteApp(ctx, app.ID); !errors.Is(err, ErrBackupInProgress) {
		t.Errorf("DeleteApp mid-backup: error = %v, want ErrBackupInProgress", err)
	}
	mustDo(t, s.FailBackup(ctx, b.ID, "test"))
	// Nor mid-deploy.
	d, err := s.CreateDeployment(ctx, app.ID, sha2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteApp(ctx, app.ID); !errors.Is(err, ErrDeployInProgress) {
		t.Errorf("DeleteApp mid-deploy: error = %v, want ErrDeployInProgress", err)
	}
	mustDo(t, s.MarkFailed(ctx, d.ID, "test"))

	deleted, err := s.DeleteApp(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.App.Name != "hello" || !deleted.HadDatabase {
		t.Errorf("DeleteApp = %+v, want hello, which had a database", deleted)
	}
	if _, err := s.App(ctx, app.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("App after delete: error = %v, want ErrNotFound", err)
	}
	// Everything of its went with it.
	for _, table := range []string{"deployments", "app_env_vars", "reconciler_events", "app_databases", "backups"} {
		var n int
		if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE app_id = $1", app.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%d rows left in %s", n, table)
		}
	}
	var jobs int
	_ = s.pool.QueryRow(ctx, "SELECT count(*) FROM jobs").Scan(&jobs)
	if jobs != 1 { // other's deploy
		t.Errorf("%d jobs left, want only the other app's", jobs)
	}
	// The other app is untouched, and the name is free again.
	if _, err := s.App(ctx, other.ID); err != nil {
		t.Errorf("other app: %v", err)
	}
	createTestApp(t, s, "hello")
	if _, err := s.DeleteApp(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing app: error = %v, want ErrNotFound", err)
	}
}
