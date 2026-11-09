package deploy

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

const head = "89abcdef0123456789abcdef0123456789abcdef"

func pollFakes() *fakes {
	f := newFakes()
	f.app.AutoDeploy = true
	f.app.Branch = "main"
	f.head, f.headETag = head, `"e1"`
	return f
}

func TestPollDeploysANewCommitOnce(t *testing.T) {
	f := pollFakes()
	d := f.deployer()
	notified := 0
	d.Notify = func() { notified++ }

	if err := d.PollBranches(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.statuses[7] != store.StatusQueued || notified != 1 {
		t.Fatalf("statuses %v, notified %d; want deployment 7 queued and a worker woken", f.statuses, notified)
	}
	// Its log says where it came from.
	if len(f.logLines) != 1 || !strings.HasPrefix(f.logLines[0], "platform: Found 89abcde on main: checked every minute") {
		t.Errorf("log = %q", f.logLines)
	}

	// Next minute: asked with the ETag, the branch hasn't moved, nothing
	// happens (and GitHub doesn't count the request).
	if err := d.PollBranches(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.sentETags, []string{"", `"e1"`}) || notified != 1 {
		t.Errorf("ETags sent %q, notified %d; want the second request conditional, no new deploy", f.sentETags, notified)
	}
}

func TestPollLeavesAKnownCommit(t *testing.T) {
	f := pollFakes()
	// Already deployed (by the webhook, or tried and failed): not again.
	f.knownCommits = map[string]bool{head: true}
	d := f.deployer()
	if err := d.PollBranches(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.statuses[7]; ok || len(f.logLines) != 0 {
		t.Errorf("statuses %v, log %q; want nothing deployed", f.statuses, f.logLines)
	}
}

func TestPollSkipsAppsNotFollowingTheirBranch(t *testing.T) {
	for name, change := range map[string]func(*store.App){
		"auto-deploy off": func(a *store.App) { a.AutoDeploy = false },
		"stopped":         func(a *store.App) { a.Stopped = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := pollFakes()
			change(&f.app)
			if err := f.deployer().PollBranches(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(f.sentETags) != 0 {
				t.Errorf("asked GitHub %d times, want never", len(f.sentETags))
			}
		})
	}
}

func TestPollRetriesAfterAFailure(t *testing.T) {
	f := pollFakes()
	f.requestErr = errors.New("database down")
	d := f.deployer()
	if err := d.PollBranches(t.Context()); err == nil {
		t.Fatal("PollBranches succeeded although the deploy couldn't be queued")
	}
	// The ETag wasn't kept, so the next pass gets the commit again.
	f.requestErr = nil
	if err := d.PollBranches(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.sentETags, []string{"", ""}) || f.statuses[7] != store.StatusQueued {
		t.Errorf("ETags sent %q, statuses %v; want it asked afresh and deployed", f.sentETags, f.statuses)
	}
}
