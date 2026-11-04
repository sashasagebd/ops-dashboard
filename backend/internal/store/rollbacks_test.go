package store

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

// deployLive takes a new deployment of commit all the way to live, with the
// image tag "img:<commit>".
func deployLive(t *testing.T, s *Store, appID int64, commit string) Deployment {
	t.Helper()
	ctx := t.Context()
	d, err := s.CreateDeployment(ctx, appID, commit)
	if err != nil {
		t.Fatal(err)
	}
	mustDo(t, s.MarkBuilding(ctx, d.ID))
	mustDo(t, s.MarkStarting(ctx, d.ID, "img:"+commit))
	mustDo(t, s.MarkHealthy(ctx, d.ID))
	return d
}

// commitN is a valid commit SHA for n.
func commitN(n int) string { return fmt.Sprintf("%040x", n) }

func TestRollbackLifecycle(t *testing.T) {
	s := testStore(t)
	app := createTestApp(t, s, "hello")
	ctx := t.Context()
	good := deployLive(t, s, app.ID, sha1)
	bad := deployLive(t, s, app.ID, sha2)
	// A pushed commit waiting to deploy (auto-deploy is on by default).
	if _, err := s.pool.Exec(ctx, "UPDATE apps SET next_commit_sha = $2 WHERE id = $1", app.ID, sha3); err != nil {
		t.Fatal(err)
	}

	rb, err := s.CreateRollback(ctx, app.ID, good.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rb.Status != StatusQueued || rb.RollbackOf != good.ID || rb.ImageTag != "img:"+sha1 || rb.CommitSHA != sha1 {
		t.Errorf("rollback = %+v; want queued, of %d, with its image and commit", rb, good.ID)
	}
	// Auto-deploy is off, and the waiting push is gone.
	got, _ := s.App(ctx, app.ID)
	if got.AutoDeploy || got.NextCommitSHA != "" {
		t.Errorf("app after rollback: autoDeploy %v, next %q; want off, none", got.AutoDeploy, got.NextCommitSHA)
	}
	// It has a job, like any deploy.
	var jobs int
	_ = s.pool.QueryRow(ctx, "SELECT count(*) FROM jobs WHERE deployment_id = $1", rb.ID).Scan(&jobs)
	if jobs != 1 {
		t.Errorf("%d jobs for the rollback, want 1", jobs)
	}

	// No building stage: queued -> starting (again, on a retry) -> healthy.
	if err := s.MarkStarting(ctx, rb.ID, "other"); !errors.Is(err, ErrWrongStatus) {
		t.Errorf("MarkStarting(queued rollback) = %v, want ErrWrongStatus", err)
	}
	mustDo(t, s.StartRollback(ctx, rb.ID))
	mustDo(t, s.StartRollback(ctx, rb.ID))
	mustDo(t, s.MarkHealthy(ctx, rb.ID))
	// The version it replaced is rolled back, not superseded.
	assertStatus(t, s, bad.ID, StatusRolledBack)
	assertStatus(t, s, good.ID, StatusSuperseded)
	assertStatus(t, s, rb.ID, StatusHealthy)
	if err := s.StartRollback(ctx, rb.ID); !errors.Is(err, ErrWrongStatus) {
		t.Errorf("StartRollback(healthy) = %v, want ErrWrongStatus", err)
	}

	// The rolled-back version can be gone back to in turn.
	if _, err := s.CreateRollback(ctx, app.ID, bad.ID); err != nil {
		t.Errorf("rollback to the rolled-back version: %v", err)
	}
}

func TestRollbackRefusals(t *testing.T) {
	s := testStore(t)
	app := createTestApp(t, s, "hello")
	other := createTestApp(t, s, "other")
	ctx := t.Context()
	deployLive(t, s, app.ID, sha1)
	again := deployLive(t, s, app.ID, sha1) // a redeploy: the same image as the one before
	live := deployLive(t, s, app.ID, sha2)
	failed, _ := s.CreateDeployment(ctx, app.ID, sha3)
	mustDo(t, s.MarkFailed(ctx, failed.ID, "build failed"))
	elsewhere := deployLive(t, s, other.ID, sha3)

	tests := []struct {
		name   string
		target int64
		want   error
	}{
		{"the live version", live.ID, ErrNotRollbackable},
		{"never live", failed.ID, ErrNotRollbackable},
		{"another app's", elsewhere.ID, ErrNotFound},
		{"no such deployment", 999999, ErrNotFound},
	}
	for _, tt := range tests {
		if _, err := s.CreateRollback(ctx, app.ID, tt.target); !errors.Is(err, tt.want) {
			t.Errorf("%s: error = %v, want %v", tt.name, err, tt.want)
		}
	}

	// While a deploy is in progress, nothing changes: not even auto-deploy.
	if _, err := s.CreateDeployment(ctx, app.ID, sha3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRollback(ctx, app.ID, again.ID); !errors.Is(err, ErrDeployInProgress) {
		t.Errorf("during a deploy: error = %v, want ErrDeployInProgress", err)
	}
	if got, _ := s.App(ctx, app.ID); !got.AutoDeploy {
		t.Error("a refused rollback turned auto-deploy off")
	}

	// The database refuses a rollback of another app's deployment even if
	// Go code let one through.
	_, err := s.pool.Exec(ctx, `
		INSERT INTO deployments (app_id, commit_sha, image_tag, rollback_of) VALUES ($1, $2, 'img', $3)`,
		other.ID, sha1, live.ID)
	if err == nil {
		t.Error("inserted a rollback of another app's deployment")
	}
}

func TestKeptImagesAreTheRecentLiveOnes(t *testing.T) {
	s := testStore(t)
	app := createTestApp(t, s, "hello")
	ctx := t.Context()
	for i := 1; i <= 8; i++ {
		deployLive(t, s, app.ID, commitN(i))
	}
	deployLive(t, s, app.ID, commitN(5)) // a redeploy of an older commit
	// Built but never live: not kept.
	failed, _ := s.CreateDeployment(ctx, app.ID, commitN(9))
	mustDo(t, s.MarkBuilding(ctx, failed.ID))
	mustDo(t, s.MarkStarting(ctx, failed.ID, "img:"+commitN(9)))
	mustDo(t, s.MarkFailed(ctx, failed.ID, "health check failed"))

	kept, err := s.KeptImages(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, n := range []int{5, 8, 7, 6, 4, 3} { // newest first; 5 counts once, at its latest
		want = append(want, "img:"+commitN(n))
	}
	if !slices.Equal(kept, want) {
		t.Errorf("kept = %q,\n want %q", kept, want)
	}

	// So 3 can still be rolled back to, 2 can't.
	ds, _ := s.Deployments(ctx, app.ID, 20)
	byCommit := map[string]Deployment{}
	for _, d := range ds {
		if _, seen := byCommit[d.CommitSHA]; !seen {
			byCommit[d.CommitSHA] = d
		}
	}
	if err := CheckRollback(byCommit[commitN(3)], kept, kept[0]); err != nil {
		t.Errorf("rollback to 3: %v, want allowed", err)
	}
	if err := CheckRollback(byCommit[commitN(2)], kept, kept[0]); !errors.Is(err, ErrNotRollbackable) {
		t.Errorf("rollback to 2: %v, want ErrNotRollbackable (image not kept)", err)
	}
}
