package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/source"
	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

// The branch poller: push-to-deploy for repos without the GitHub webhook.
// Every minute it asks GitHub for the newest commit on each auto-deployed
// app's branch, and a commit the app hasn't seen goes through the same path
// as a pushed one (Push, store.RequestDeployment), which ignores the commit
// of the app's latest deployment and the one waiting. So the webhook stays the instant
// path, the poller catches whatever it didn't send, and a push both see
// deploys once (see docs/decisions/platform.md).
//
// Requests are conditional (ETags): GitHub answers an unchanged branch with
// 304, which an authenticated request doesn't count against the rate limit.
// Apps that share a repo and branch share one request.

// branchKey is one branch of one repo, as GitHub sees it (owner and repo
// names aren't case-sensitive).
type branchKey struct{ repo, branch string }

// RunPoller polls every interval until ctx is done, starting at once. A
// failing lookup is logged once until it changes.
func (d *Deployer) RunPoller(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	lastErr := ""
	for {
		err := d.PollBranches(ctx)
		switch {
		case err != nil && ctx.Err() == nil && err.Error() != lastErr:
			slog.Warn("polling apps' branches for new commits", "err", err)
			lastErr = err.Error()
		case err == nil:
			lastErr = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// PollBranches checks each auto-deployed, running app's branch once and
// deploys new commits. One pass; RunPoller repeats it.
func (d *Deployer) PollBranches(ctx context.Context) error {
	apps, err := d.store.Apps(ctx)
	if err != nil {
		return err
	}
	groups := map[branchKey][]store.App{}
	var order []branchKey
	for _, app := range apps {
		if !app.AutoDeploy || app.Stopped {
			continue
		}
		owner, repo, err := source.ParseRepoURL(app.RepoURL)
		if err != nil {
			continue // can't happen: the API only registers GitHub URLs
		}
		k := branchKey{repo: strings.ToLower(owner + "/" + repo), branch: app.Branch}
		if groups[k] == nil {
			order = append(order, k)
		}
		groups[k] = append(groups[k], app)
	}

	var errs []error
	for _, k := range order {
		group := groups[k]
		d.pollMu.Lock()
		etag := d.etags[k]
		d.pollMu.Unlock()
		sha, newETag, changed, err := d.source.LatestCommitIfChanged(ctx, group[0].RepoURL, k.branch, etag)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", k.repo, k.branch, err))
			continue
		}
		ok := true
		if changed {
			for _, app := range group {
				if err := d.pollDeploy(ctx, app, sha); err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", app.Name, err))
					ok = false
				}
			}
		}
		// Only once every app has had the new commit: otherwise the next
		// pass asks without the ETag, gets the commit again, and retries
		// (apps that took it already find it known).
		if ok {
			d.pollMu.Lock()
			if d.etags == nil {
				d.etags = map[branchKey]string{}
			}
			d.etags[k] = newETag
			d.pollMu.Unlock()
		}
	}
	return errors.Join(errs...)
}

// pollDeploy hands a branch's newest commit to the app, as a push would.
func (d *Deployer) pollDeploy(ctx context.Context, app store.App, sha string) error {
	dep, outcome, err := d.Push(ctx, app.ID, sha)
	switch {
	case errors.Is(err, store.ErrAppStopped), errors.Is(err, store.ErrNotFound):
		return nil // stopped or deleted since the list was read
	case err != nil:
		return err
	}
	switch outcome {
	case store.RequestStarted:
		slog.Info("deploying a new commit found on the app's branch", "app", app.Name, "commit", sha, "deployment", dep.ID)
		// Says where the deploy came from at the top of its log. Best
		// effort: the deploy matters more.
		line := fmt.Sprintf("Found %s on %s: checked every minute (a GitHub webhook on the repo makes this instant)",
			shortSHA(sha), app.Branch)
		if err := d.store.AppendDeploymentLogs(ctx, dep.ID, []store.NewLogLine{
			{At: time.Now(), Stream: store.LogPlatform, Line: line},
		}); err != nil {
			slog.Warn("writing the deploy log", "deployment", dep.ID, "err", err)
		}
	case store.RequestWaiting:
		slog.Info("a new commit found on the app's branch waits for the deploy in progress", "app", app.Name, "commit", sha)
	}
	return nil
}
