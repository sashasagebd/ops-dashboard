// Package deploy runs a deployment from start to finish: download the code,
// build it, start a container, check it's healthy through Caddy, switch the
// app's route to it, let the previous version finish its requests, and
// remove it.
//
// Each deploy is a job in the platform's queue (internal/jobs): the
// Deployer is its handler, and Run makes one attempt. An attempt can be cut
// off anywhere (the platform restarting, Docker unreachable) and run again,
// so each one starts by clearing up after the last. The rules that must hold
// under concurrency (one deploy in progress and one live deployment per app)
// are enforced by the database, not here.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/engine"
	"github.com/sashasagebd/ops-dashboard/backend/internal/jobs"
	"github.com/sashasagebd/ops-dashboard/backend/internal/router"
	"github.com/sashasagebd/ops-dashboard/backend/internal/source"
	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

// The interfaces below are what the deployer needs from each part of the
// platform; in production they're *store.Store, *source.GitHub,
// *engine.Client and *router.Caddy.

type Store interface {
	App(ctx context.Context, id int64) (store.App, error)
	CreateDeployment(ctx context.Context, appID int64, commitSHA string) (store.Deployment, error)
	Deployment(ctx context.Context, id int64) (store.Deployment, error)
	MarkBuilding(ctx context.Context, id int64) error
	MarkStarting(ctx context.Context, id int64, imageTag string) error
	MarkHealthy(ctx context.Context, id int64) error
	MarkFailed(ctx context.Context, id int64, reason string) error
	EnvVars(ctx context.Context, appID int64) (map[string]string, error)
	LiveApps(ctx context.Context) ([]store.LiveApp, error)
	AppendDeploymentLogs(ctx context.Context, deploymentID int64, lines []store.NewLogLine) error
	CountDeploymentLogs(ctx context.Context, deploymentID int64, stream string) (int, error)
	PruneDeploymentLogs(ctx context.Context, appID int64, keep int) (int64, error)
	RequestDeployment(ctx context.Context, appID int64, commitSHA string) (store.Deployment, bool, error)
	StartNextDeployment(ctx context.Context, appID int64) (store.Deployment, bool, error)
	Deployments(ctx context.Context, appID int64, limit int) ([]store.Deployment, error)
	CreateRollback(ctx context.Context, appID, targetID int64) (store.Deployment, error)
	StartRollback(ctx context.Context, id int64) error
	KeptImages(ctx context.Context, appID int64) ([]string, error)
	Database(ctx context.Context, appID int64) (store.Database, error)
}

type Source interface {
	BuildContext(ctx context.Context, repoURL, commitSHA, dir string) (io.ReadCloser, error)
	LatestCommit(ctx context.Context, repoURL, branch string) (string, error)
}

type Engine interface {
	Build(ctx context.Context, buildContext io.Reader, tag string, labels map[string]string, progress func(line string)) error
	Run(ctx context.Context, spec engine.RunSpec) (string, error)
	Remove(ctx context.Context, id string) error
	ManagedContainers(ctx context.Context, app string) ([]engine.ManagedContainer, error)
	AppImages(ctx context.Context, app string) ([]engine.Image, error)
	ImageExists(ctx context.Context, ref string) (bool, error)
	RemoveImage(ctx context.Context, ref string) error
	EnsureNetwork(ctx context.Context, name string, labels map[string]string) error
	Pull(ctx context.Context, ref string) error
	RunDatabase(ctx context.Context, spec engine.DatabaseSpec) (string, error)
}

type Router interface {
	Apply(ctx context.Context, routes []router.Route) error
	Probe(ctx context.Context, host, path string) (int, error)
	InFlight(ctx context.Context) (map[string]int, error)
	Routes(ctx context.Context) ([]router.Route, error)
	SyncMetrics(ctx context.Context) (changed bool, err error)
}

// Config holds the deployer's settings.
type Config struct {
	AppsNetwork    string        // the Docker network app containers join
	DeployTimeout  time.Duration // one attempt at a deploy, download to switch
	HealthTimeout  time.Duration // how long a new container gets to answer healthily
	HealthInterval time.Duration // time between health probes
	DrainTimeout   time.Duration // how long the old version gets to finish its requests after the switch
	DrainInterval  time.Duration // time between checks that it has
}

// Deployer queues deployments and runs them as jobs.
type Deployer struct {
	store  Store
	source Source
	engine Engine
	router Router
	cfg    Config

	// Notify, if set, is called after a deploy is queued, so a worker
	// starts it straight away (the job runner's Wake).
	Notify func()

	// applyMu makes route updates one at a time, so two deploys finishing
	// together can't send Caddy their tables in the wrong order.
	applyMu sync.Mutex
	mu      sync.Mutex
	probes  map[int64]router.Route // deployment ID -> its temporary probe route

	// locks holds one lock per app, taken by each deploy attempt for as long
	// as it runs (drain included) and tried by the reconciler, which leaves
	// an app alone while its deploy runs (TryLockApp).
	locks appLocks
}

// New returns a Deployer.
func New(cfg Config, st Store, src Source, eng Engine, rt Router) *Deployer {
	return &Deployer{
		store: st, source: src, engine: eng, router: rt, cfg: cfg,
		probes: map[int64]router.Route{},
	}
}

// Start queues a deploy of commitSHA for an app (the deployment and its job),
// returning the new deployment; a worker runs it in the background. If the
// app already has a deploy in progress, it returns store.ErrDeployInProgress
// and queues nothing.
func (d *Deployer) Start(ctx context.Context, appID int64, commitSHA string) (store.Deployment, error) {
	dep, err := d.store.CreateDeployment(ctx, appID, commitSHA)
	if err != nil {
		return store.Deployment{}, err
	}
	d.notify()
	return dep, nil
}

// StartLatest deploys the newest commit on the app's branch, looked up on
// GitHub, like Start.
func (d *Deployer) StartLatest(ctx context.Context, appID int64) (store.Deployment, error) {
	app, err := d.store.App(ctx, appID)
	if err != nil {
		return store.Deployment{}, err
	}
	sha, err := d.source.LatestCommit(ctx, app.RepoURL, app.Branch)
	if err != nil {
		return store.Deployment{}, err
	}
	return d.Start(ctx, appID, sha)
}

// Rollback queues a rollback of an app to one of its earlier deployments: a
// new deployment that runs that one's image, with no build (see
// store.CreateRollback, which also turns the app's auto-deploy off). Errors
// as CreateRollback's.
func (d *Deployer) Rollback(ctx context.Context, appID, targetID int64) (store.Deployment, error) {
	dep, err := d.store.CreateRollback(ctx, appID, targetID)
	if err != nil {
		return store.Deployment{}, err
	}
	d.notify()
	return dep, nil
}

// Push deploys a pushed commit: straight away if the app has no deploy in
// progress (started), otherwise once that one finishes, as the app's next
// commit (replacing an older push waiting there). See
// store.RequestDeployment.
func (d *Deployer) Push(ctx context.Context, appID int64, commitSHA string) (dep store.Deployment, started bool, err error) {
	dep, started, err = d.store.RequestDeployment(ctx, appID, commitSHA)
	if started {
		d.notify()
	}
	return dep, started, err
}

// startNext starts the deploy of a pushed commit that was waiting for
// deployment's deploy to finish, if there is one. Called whenever a deploy
// finishes, whichever way. Best effort: if it fails, the commit stays
// waiting, and the app's next deploy to finish starts it.
func (d *Deployer) startNext(ctx context.Context, deploymentID int64) {
	finished, err := d.store.Deployment(ctx, deploymentID)
	if err == nil {
		var next store.Deployment
		var ok bool
		next, ok, err = d.store.StartNextDeployment(ctx, finished.AppID)
		if ok {
			slog.Info("deploying the next pushed commit", "app", finished.AppID, "deployment", next.ID, "commit", next.CommitSHA)
			d.notify()
		}
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Error("starting the next pushed commit", "after", deploymentID, "err", err)
	}
}

func (d *Deployer) notify() {
	if d.Notify != nil {
		d.Notify()
	}
}

// Names the platform gives things, derived so they never need storing.

// ImageTag is the image a deployment builds, e.g. ops-platform/hello:<sha>.
func ImageTag(app, commitSHA string) string { return "ops-platform/" + app + ":" + commitSHA }

// ContainerName is a deployment's container, e.g. app-hello-42. It's also
// the hostname Caddy reaches it at on the apps network.
func ContainerName(app string, deploymentID int64) string {
	return "app-" + app + "-" + strconv.FormatInt(deploymentID, 10)
}

// probeHost is the temporary hostname a new deployment is checked through.
// It only exists in Caddy while that deployment is being checked.
func probeHost(deploymentID int64) string {
	return "probe-" + strconv.FormatInt(deploymentID, 10) + ".platform.internal"
}

// errTimedOut ends an attempt that ran past DeployTimeout.
var errTimedOut = errors.New("deploy timed out")

// Run makes one attempt at a deploy job (jobs.Handler). Errors that another
// attempt can't fix (the build failed, the app never got healthy, the commit
// doesn't exist, the attempt timed out) are marked jobs.Permanent; anything
// else, such as Docker or GitHub being unreachable, is retried.
//
// What happens is written to the deployment's log as it goes, for the
// dashboard's live view.
func (d *Deployer) Run(ctx context.Context, job store.Job) error {
	out := d.openLog(job.DeploymentID)
	defer out.Close()
	attemptCtx, cancel := context.WithTimeoutCause(ctx, d.cfg.DeployTimeout, errTimedOut)
	defer cancel()
	log := slog.With("deployment", job.DeploymentID, "attempt", job.Attempts)

	err := d.deploy(attemptCtx, log, out, job)
	if err != nil && context.Cause(attemptCtx) == errTimedOut {
		// Whatever failed, it failed because time ran out. (Checked on
		// the context rather than err: an HTTP client's own timeout also
		// matches context.DeadlineExceeded, and that's worth a retry.)
		err = errTimedOut
	}
	switch {
	case err == nil:
		d.startNext(ctx, job.DeploymentID)
	case permanent(err):
		// GiveUp writes the reason to the log.
		err = jobs.Permanent(err)
	case ctx.Err() != nil:
		out.Printf("Interrupted: the platform is shutting down. The deploy carries on when it's back.")
	default:
		out.Printf("Attempt %d failed: %s", job.Attempts, publicReason(err, d.cfg.DeployTimeout))
	}
	return err
}

// GiveUp records why a deploy failed for good on its deployment, where the
// dashboard shows it, and at the end of its log (jobs.Handler).
func (d *Deployer) GiveUp(ctx context.Context, job store.Job, err error) {
	reason := publicReason(err, d.cfg.DeployTimeout)
	// Already live: the deploy succeeded, and only what follows the switch
	// (routing to it, draining and removing the old version) was cut off.
	// That isn't a failure to record; the reconciler re-applies the routes
	// and removes the old container within seconds.
	if dep, err := d.store.Deployment(ctx, job.DeploymentID); err == nil && dep.Status == store.StatusHealthy {
		d.appendLogLine(ctx, job.DeploymentID, "The new version is live, but finishing the switch from the previous one was cut off: "+reason)
		d.startNext(ctx, job.DeploymentID)
		return
	}
	d.appendLogLine(ctx, job.DeploymentID, "Deploy failed: "+reason)
	err = d.store.MarkFailed(ctx, job.DeploymentID, reason)
	// Already finished (recorded by an earlier call that the platform died
	// just after), or deleted with its app: nothing left to record.
	if err != nil && !errors.Is(err, store.ErrWrongStatus) && !errors.Is(err, store.ErrNotFound) {
		slog.Error("recording a failed deploy", "deployment", job.DeploymentID, "err", err)
		return
	}
	d.startNext(ctx, job.DeploymentID)
}

// permanent reports whether err is one that trying the deploy again won't
// fix.
func permanent(err error) bool {
	var buildErr *engine.BuildError
	var healthErr *HealthError
	return errors.As(err, &buildErr) || errors.As(err, &healthErr) ||
		errors.Is(err, source.ErrNotFound) || errors.Is(err, source.ErrTooLarge) ||
		errors.Is(err, source.ErrDirNotFound) || errors.Is(err, errTimedOut) ||
		errors.Is(err, ErrImageGone) ||
		// The app or deployment was deleted, or the deployment finished
		// some other way.
		errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrWrongStatus)
}

func (d *Deployer) deploy(ctx context.Context, log *slog.Logger, out *deployLog, job store.Job) error {
	dep, err := d.store.Deployment(ctx, job.DeploymentID)
	if err != nil {
		return err
	}
	app, err := d.store.App(ctx, dep.AppID)
	if err != nil {
		return err
	}
	log = log.With("app", app.Name, "commit", dep.CommitSHA)
	unlock := d.locks.lock(app.ID)
	defer unlock()

	switch dep.Status {
	case store.StatusQueued, store.StatusBuilding, store.StatusStarting:
		// Older deployments' logs go as each new one starts, so an app
		// keeps only its recent ones.
		if _, err := d.store.PruneDeploymentLogs(ctx, app.ID, keepLogsFor); err != nil {
			log.Warn("pruning old deploy logs", "err", err)
		}
		if dep.RollbackOf != 0 {
			out.Printf("Rolling back to deployment #%d (%s at %s), reusing its image (attempt %d)",
				dep.RollbackOf, app.RepoURL, shortSHA(dep.CommitSHA), job.Attempts)
		} else {
			out.Printf("Deploying %s at %s (attempt %d)", app.RepoURL, shortSHA(dep.CommitSHA), job.Attempts)
		}
		// Building or starting: an earlier attempt was cut off. Its
		// container, if it got that far, has this deployment's name, so it
		// goes first; the build is simply redone, mostly from Docker's
		// cache. A probe route it left is dropped by the next ApplyRoutes.
		log.Info("deploy started", "from", dep.Status)
		if err := d.removeDeploymentContainers(ctx, out, app.Name, dep.ID); err != nil {
			return err
		}
	case store.StatusHealthy:
		// An earlier attempt made it live, then was cut off before
		// switching the route or removing the old version: finish those.
		log.Info("deploy already healthy; finishing the route switch")
		out.Printf("Already healthy; finishing the switch to the new version (attempt %d)", job.Attempts)
		if err := d.ApplyRoutes(ctx); err != nil {
			return err
		}
		if err := d.retireOlder(ctx, log, out, app, dep.ID); err != nil {
			return err
		}
		d.pruneImages(ctx, log, out, app)
		out.Printf("Live at http://%s/", app.Hostname)
		return nil
	default:
		// Finished some other way (superseded by a later deploy, or failed
		// and recorded just before the platform died): nothing to do.
		log.Info("deploy already finished", "status", dep.Status)
		return nil
	}

	labels := deploymentLabels(app, dep.ID)
	var tag string
	if dep.RollbackOf != 0 {
		tag, err = d.reuseImage(ctx, out, dep)
	} else {
		tag, err = d.build(ctx, out, app, dep, labels)
	}
	if err != nil {
		return err
	}

	// Start the new container next to the old one, which keeps serving.
	env, networks, err := d.appEnv(ctx, app)
	if err != nil {
		return err
	}
	name := ContainerName(app.Name, dep.ID)
	withDB := ""
	if len(networks) > 0 {
		withDB = ", and its database as DATABASE_URL"
	}
	out.Printf("Starting container %s (%d MiB memory, %g CPU, %d env vars%s)",
		name, app.MemoryLimitBytes>>20, float64(app.CPULimitMillicores)/1000, len(env), withDB)
	containerID, err := d.engine.Run(ctx, d.runSpec(app, dep.ID, tag, env, networks))
	if err != nil {
		return err
	}
	upstream := name + ":" + strconv.Itoa(app.Port)

	// Check it through Caddy on a temporary hostname. Whatever happens, the
	// probe route goes away afterwards.
	out.Printf("Waiting up to %s for GET %s to succeed", d.cfg.HealthTimeout, app.HealthCheckPath)
	err = d.checkHealth(ctx, dep.ID, upstream, app.HealthCheckPath)
	if err != nil {
		if rmErr := d.engine.Remove(context.WithoutCancel(ctx), containerID); rmErr != nil {
			log.Error("removing the unhealthy container", "container", name, "err", rmErr)
		}
		return err
	}

	// Healthy: make it the live deployment (superseding the old one, or
	// marking it rolled back, in the same transaction), then point the app's
	// hostname at it.
	out.Printf("Healthy")
	if err := d.store.MarkHealthy(ctx, dep.ID); err != nil {
		return err
	}
	if err := d.ApplyRoutes(ctx); err != nil {
		// The database says the new deployment is live, but Caddy still
		// routes to the old container, which is still running, so the
		// site stays up on the old version. Keeping the old container is
		// what makes that safe; the reconciler corrects Caddy within
		// seconds, and only then removes the old container. Not retried:
		// the deploy itself has succeeded.
		log.Error("deployment is healthy but switching its route failed; keeping the previous container", "err", err)
		out.Printf("Couldn't update the route yet: the previous version keeps serving until the next route update")
		return nil
	}
	if err := d.retireOlder(ctx, log, out, app, dep.ID); err != nil {
		return err
	}
	d.pruneImages(ctx, log, out, app)
	log.Info("deploy finished: healthy")
	out.Printf("Live at http://%s/", app.Hostname)
	return nil
}

// build downloads the deployment's commit and builds its image (the code
// streams from GitHub straight into Docker's build), moving the deployment
// from building to starting. It returns the image's tag.
func (d *Deployer) build(ctx context.Context, out *deployLog, app store.App, dep store.Deployment, labels map[string]string) (string, error) {
	if err := d.store.MarkBuilding(ctx, dep.ID); err != nil {
		return "", err
	}
	tag := ImageTag(app.Name, dep.CommitSHA)
	where := "the repo's root"
	if app.SourceDir != "" {
		where = app.SourceDir + "/"
	}
	out.Printf("Building image %s from %s", tag, where)
	src, err := d.source.BuildContext(ctx, app.RepoURL, dep.CommitSHA, app.SourceDir)
	if err != nil {
		return "", err
	}
	stream := &streamReader{r: src}
	err = d.engine.Build(ctx, stream, tag, labels, out.Build)
	src.Close()
	if err != nil {
		// Problems with the code itself (too large, source directory
		// missing) are found while Docker reads the stream, so Build fails
		// with whatever Docker made of the cut-off upload. The stream's own
		// error is the real cause, and its message is one users can act on.
		if streamErr := stream.Err(); streamErr != nil {
			return "", streamErr
		}
		return "", err
	}
	if err := d.store.MarkStarting(ctx, dep.ID, tag); err != nil {
		return "", err
	}
	return tag, nil
}

// ErrImageGone means the image a deployment runs no longer exists in Docker,
// so it can't be rolled back to or recreated. The platform keeps the images
// of an app's recent live versions, so this is an image removed by hand (e.g.
// docker image prune -a) since.
var ErrImageGone = errors.New("the deployment's image no longer exists; deploy its commit instead")

// reuseImage is a rollback's stand-in for build: the image was built by an
// earlier deployment, so it's only checked to still exist. The deployment
// moves from queued (or starting, on a retry) to starting.
func (d *Deployer) reuseImage(ctx context.Context, out *deployLog, dep store.Deployment) (string, error) {
	ok, err := d.engine.ImageExists(ctx, dep.ImageTag)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrImageGone
	}
	if err := d.store.StartRollback(ctx, dep.ID); err != nil {
		return "", err
	}
	out.Printf("Using image %s (no build)", dep.ImageTag)
	return dep.ImageTag, nil
}

// pruneImages removes the app's images that are no longer kept: the platform
// keeps the images of its store.ImagesKept most recent live versions (what a
// rollback can go back to) and removes the rest, including images that were
// built but never went live. Called once a deploy has switched over and
// removed the old containers. Best effort: failures are logged, and the next
// deploy tries again.
//
// It goes by what Docker has (the images labelled with the app's name), not
// by the database's deployments, so an image that failed to be removed once
// is simply seen again next time; and it only ever touches the app's own
// images, under the app's own tag prefix. Removal is never forced, so an
// image a container still uses stays.
func (d *Deployer) pruneImages(ctx context.Context, log *slog.Logger, out *deployLog, app store.App) {
	kept, err := d.store.KeptImages(ctx, app.ID)
	if err != nil {
		log.Warn("reading the images to keep", "err", err)
		return
	}
	keep := map[string]bool{}
	for _, t := range kept {
		keep[t] = true
	}
	// Once this deploy is live it's no longer in progress, so the app's next
	// one may have started already: its image (built, or about to be) stays.
	recent, err := d.store.Deployments(ctx, app.ID, 1)
	if err != nil {
		log.Warn("reading the app's latest deployment", "err", err)
		return
	}
	if len(recent) == 1 && inProgress(recent[0].Status) {
		keep[ImageTag(app.Name, recent[0].CommitSHA)] = true
		if recent[0].ImageTag != "" {
			keep[recent[0].ImageTag] = true
		}
	}

	images, err := d.engine.AppImages(ctx, app.Name)
	if err != nil {
		log.Warn("listing the app's images", "err", err)
		return
	}
	prefix := ImageTag(app.Name, "")
	removed := 0
	for _, img := range images {
		var refs []string
		for _, t := range img.Tags {
			if strings.HasPrefix(t, prefix) && !keep[t] {
				refs = append(refs, t)
			}
		}
		if len(img.Tags) == 0 {
			// Left untagged when a redeploy of the same commit moved its tag
			// to a newer build.
			refs = []string{img.ID}
		}
		for _, ref := range refs {
			switch err := d.engine.RemoveImage(ctx, ref); {
			case errors.Is(err, engine.ErrImageInUse):
				// A container still uses it; tried again next time.
			case err != nil:
				log.Warn("removing an old image", "image", ref, "err", err)
			default:
				removed++
			}
		}
	}
	if removed > 0 {
		out.Printf("Removed %d old images; the last %d live versions are kept for rollbacks", removed, store.ImagesKept)
	}
}

// inProgress reports whether a deployment hasn't finished yet.
func inProgress(s store.DeploymentStatus) bool {
	return s == store.StatusQueued || s == store.StatusBuilding || s == store.StatusStarting
}

func shortSHA(sha string) string { return sha[:min(7, len(sha))] }

// checkHealth adds a probe route to the new container, waits for it to answer
// healthily through Caddy, and removes the probe route again.
func (d *Deployer) checkHealth(ctx context.Context, deploymentID int64, upstream, path string) error {
	host := probeHost(deploymentID)
	d.setProbe(deploymentID, &router.Route{Host: host, Upstream: upstream})
	defer func() {
		d.setProbe(deploymentID, nil)
		// Best effort: a leftover probe route only points at a container
		// that's being removed or is now live, and the next update drops it.
		_ = d.ApplyRoutes(context.WithoutCancel(ctx))
	}()
	if err := d.ApplyRoutes(ctx); err != nil {
		return err
	}
	return d.waitHealthy(ctx, host, path)
}

// HealthError means a new container never answered its health check in time.
type HealthError struct {
	Path    string
	Timeout time.Duration
	Last    string // the last probe's result, e.g. "502" or "connection refused"
}

func (e *HealthError) Error() string {
	return fmt.Sprintf("health check failed: GET %s didn't succeed within %s (last result: %s)", e.Path, e.Timeout, e.Last)
}

// waitHealthy probes until the app answers 2xx or 3xx, or HealthTimeout
// passes. Caddy answers 502 while the app isn't listening yet, so that
// counts as "not yet", like a connection error.
func (d *Deployer) waitHealthy(parent context.Context, host, path string) error {
	ctx, cancel := context.WithTimeout(parent, d.cfg.HealthTimeout)
	defer cancel()
	last := "no answer"
	for {
		status, err := d.router.Probe(ctx, host, path)
		switch {
		case err == nil && status >= 200 && status < 400:
			return nil
		case err == nil:
			last = strconv.Itoa(status)
		case ctx.Err() == nil:
			last = err.Error()
		}

		select {
		case <-ctx.Done():
			// If the deploy as a whole was cancelled or ran out of time,
			// report that; only the health check's own timeout is a
			// health failure.
			if err := parent.Err(); err != nil {
				return err
			}
			return &HealthError{Path: path, Timeout: d.cfg.HealthTimeout, Last: last}
		case <-time.After(d.cfg.HealthInterval):
		}
	}
}

func (d *Deployer) setProbe(deploymentID int64, r *router.Route) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r == nil {
		delete(d.probes, deploymentID)
	} else {
		d.probes[deploymentID] = *r
	}
}

// ApplyRoutes sends Caddy the complete routing table (routeTable). It's built
// fresh from the database each time, so it's always safe to call.
func (d *Deployer) ApplyRoutes(ctx context.Context) error {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	routes, err := d.routeTable(ctx)
	if err != nil {
		return err
	}
	return d.router.Apply(ctx, routes)
}

// SyncRoutes is the reconciler's ApplyRoutes: it reads Caddy's routes first
// and applies the table only if they differ (changed reports whether it did).
// Every apply makes Caddy rebuild its config, so a check every few seconds
// mustn't apply when nothing is wrong. Under the same lock as ApplyRoutes, so
// a deploy's own route change can't land in between the read and the apply.
func (d *Deployer) SyncRoutes(ctx context.Context) (changed bool, err error) {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	want, err := d.routeTable(ctx)
	if err != nil {
		return false, err
	}
	have, err := d.router.Routes(ctx)
	if err != nil {
		return false, err
	}
	if sameRoutes(want, have) {
		return false, nil
	}
	return true, d.router.Apply(ctx, want)
}

// SyncMetrics makes sure Caddy counts requests per host (for the dashboard's
// request metrics), for the reconciler. Under the same lock as route
// updates: both change Caddy's config.
func (d *Deployer) SyncMetrics(ctx context.Context) (changed bool, err error) {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	return d.router.SyncMetrics(ctx)
}

// routeTable is what Caddy should route: every app's hostname to its live
// deployment's container, plus any probe routes for deployments being
// checked.
func (d *Deployer) routeTable(ctx context.Context) ([]router.Route, error) {
	live, err := d.store.LiveApps(ctx)
	if err != nil {
		return nil, err
	}
	routes := make([]router.Route, 0, len(live))
	for _, a := range live {
		routes = append(routes, router.Route{
			Host:     a.Hostname,
			Upstream: ContainerName(a.AppName, a.DeploymentID) + ":" + strconv.Itoa(a.Port),
		})
	}
	d.mu.Lock()
	for _, id := range slices.Sorted(maps.Keys(d.probes)) {
		routes = append(routes, d.probes[id])
	}
	d.mu.Unlock()
	return routes, nil
}

// sameRoutes reports whether two tables route the same, in any order (each
// host appears once, so order doesn't change where a request goes).
func sameRoutes(a, b []router.Route) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[router.Route]int, len(a))
	for _, r := range a {
		seen[r]++
	}
	for _, r := range b {
		if seen[r] == 0 {
			return false
		}
		seen[r]--
	}
	return true
}

// deploymentLabels are the labels on a deployment's image and container:
// how the platform recognises its own, and which app and deployment each is.
func deploymentLabels(app store.App, deploymentID int64) map[string]string {
	return map[string]string{
		engine.LabelApp:        app.Name,
		engine.LabelAppID:      strconv.FormatInt(app.ID, 10),
		engine.LabelDeployment: strconv.FormatInt(deploymentID, 10),
	}
}

// runSpec is the container a deployment runs as, the same whether a deploy
// starts it or the reconciler recreates it.
func (d *Deployer) runSpec(app store.App, deploymentID int64, image string, env map[string]string, networks []string) engine.RunSpec {
	return engine.RunSpec{
		Name:          ContainerName(app.Name, deploymentID),
		Image:         image,
		Env:           env,
		Labels:        deploymentLabels(app, deploymentID),
		Network:       d.cfg.AppsNetwork,
		ExtraNetworks: networks,
		MemoryBytes:   app.MemoryLimitBytes,
		NanoCPUs:      int64(app.CPULimitMillicores) * 1_000_000,
	}
}

// Recreate creates and starts a live deployment's container again, after it
// was deleted, for the reconciler (which holds the app's lock). It has the
// same name as before, so the app's route reaches it with no change, and
// the app's current env vars, as a rollback does. There's no health check:
// the app was down already, and the reconciler looks again on its next pass.
// ErrImageGone if the deployment's image was removed meanwhile.
func (d *Deployer) Recreate(ctx context.Context, app store.App, dep store.Deployment) error {
	if dep.ImageTag == "" {
		return ErrImageGone
	}
	ok, err := d.engine.ImageExists(ctx, dep.ImageTag)
	if err != nil {
		return err
	}
	if !ok {
		return ErrImageGone
	}
	env, networks, err := d.appEnv(ctx, app)
	if err != nil {
		return err
	}
	_, err = d.engine.Run(ctx, d.runSpec(app, dep.ID, dep.ImageTag, env, networks))
	return err
}

// TryLockApp takes the app's lock if no deploy of it is running, for the
// reconciler: ok is false (and nothing is held) if one is. unlock releases
// it.
func (d *Deployer) TryLockApp(appID int64) (unlock func(), ok bool) {
	return d.locks.tryLock(appID)
}

// appLocks is a lock per app, made on first use. Apps are few and never
// many, so they're never cleaned up.
type appLocks struct {
	mu sync.Mutex
	m  map[int64]*sync.Mutex
}

func (l *appLocks) get(appID int64) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[int64]*sync.Mutex{}
	}
	if l.m[appID] == nil {
		l.m[appID] = &sync.Mutex{}
	}
	return l.m[appID]
}

func (l *appLocks) lock(appID int64) (unlock func()) {
	m := l.get(appID)
	m.Lock()
	return m.Unlock
}

func (l *appLocks) tryLock(appID int64) (unlock func(), ok bool) {
	m := l.get(appID)
	if !m.TryLock() {
		return nil, false
	}
	return m.Unlock, true
}

// retireOlder stops and removes the app's containers from deployments before
// live, once Caddy routes to live: the version it replaced, and any left over
// from earlier failures. It drains them first (see drain). Newer containers
// are left alone: once live is no longer in progress, the app's next deploy
// may already have started its container.
//
// The only error is being cut off (the platform shutting down, or the attempt
// running out of time), and then nothing is removed: a retry finds the
// deployment healthy and comes back here, and failing that, the reconciler
// removes them (without draining). Other failures are logged
// rather than failing the deploy, which has succeeded.
func (d *Deployer) retireOlder(ctx context.Context, log *slog.Logger, out *deployLog, app store.App, live int64) error {
	containers, err := d.engine.ManagedContainers(ctx, app.Name)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Error("listing old containers", "err", err)
		return nil
	}
	var older []engine.ManagedContainer
	for _, c := range containers {
		id, err := strconv.ParseInt(c.Labels[engine.LabelDeployment], 10, 64)
		if err == nil && id < live {
			older = append(older, c)
		}
	}
	if len(older) == 0 {
		return nil
	}
	if err := d.drain(ctx, log, out, app.Port, older); err != nil {
		return err
	}
	// Not cancelled part-way: once drained, each takes at most its stop
	// timeout, and stopping only some would leave the rest for a retry that
	// may not come.
	rmCtx := context.WithoutCancel(ctx)
	for _, c := range older {
		if err := d.engine.Remove(rmCtx, c.ID); err != nil {
			log.Error("removing old container", "container", c.Name, "err", err)
			continue
		}
		out.Printf("Removed the previous container %s", c.Name)
	}
	return nil
}

// drain waits until Caddy has no requests in flight to any of containers, or
// DrainTimeout passes.
//
// After a route switch, Caddy sends new requests to the new version but lets
// the ones already started finish against the old one; stopping the old
// container under them would cut them off. So the old version is stopped only
// once Caddy's in-flight count for it is zero. The first check comes one
// interval after the switch, by when any request the old config had just
// routed has been counted. Bounded, because a client holding a request open
// (a long poll, a stream) mustn't hold up the deploy forever; and if Caddy
// can't be asked, it waits the whole timeout, which is slower but safe.
//
// The alternative, a fixed wait, is either too short for a slow request or
// makes every deploy slower than it needs to be.
func (d *Deployer) drain(ctx context.Context, log *slog.Logger, out *deployLog, port int, containers []engine.ManagedContainer) error {
	addrs := make([]string, 0, len(containers))
	for _, c := range containers {
		addrs = append(addrs, c.Name+":"+strconv.Itoa(port))
	}
	timeout := time.NewTimer(d.cfg.DrainTimeout)
	defer timeout.Stop()
	last := -1 // in flight at the last check; -1 if Caddy couldn't be asked
	warned, waiting := false, false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			if last < 0 {
				out.Printf("Couldn't check the previous version's requests; stopping it after %s", d.cfg.DrainTimeout)
			} else {
				out.Printf("Stopping the previous version after %s with requests still in flight (%d)", d.cfg.DrainTimeout, last)
			}
			return nil
		case <-time.After(d.cfg.DrainInterval):
		}

		inFlight, err := d.router.InFlight(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !warned {
				log.Warn("reading in-flight requests from Caddy; waiting out the drain timeout", "err", err)
				warned = true
			}
			last = -1
			continue
		}
		n := 0
		for _, a := range addrs {
			n += inFlight[a]
		}
		if n == 0 {
			if waiting {
				out.Printf("The previous version's requests have finished")
			}
			return nil
		}
		if !waiting {
			out.Printf("Waiting up to %s for the previous version's requests to finish (%d in flight)", d.cfg.DrainTimeout, n)
			waiting = true
		}
		last = n
	}
}

// removeDeploymentContainers removes any container an earlier attempt at a
// deployment left, so this attempt can create it afresh under the same name.
func (d *Deployer) removeDeploymentContainers(ctx context.Context, out *deployLog, app string, deploymentID int64) error {
	containers, err := d.engine.ManagedContainers(ctx, app)
	if err != nil {
		return err
	}
	label := strconv.FormatInt(deploymentID, 10)
	for _, c := range containers {
		if c.Labels[engine.LabelDeployment] != label {
			continue
		}
		slog.Info("removing the container of an earlier attempt", "container", c.Name)
		if err := d.engine.Remove(ctx, c.ID); err != nil {
			return err
		}
		out.Printf("Removed container %s, left by an earlier attempt", c.Name)
	}
	return nil
}

// publicReason turns a deploy error into the reason shown in the dashboard:
// specific for the failures a user can act on (their build failed, their
// app didn't come up, the repo wasn't found), generic otherwise, since
// internal errors can contain details that belong in the log only.
func publicReason(err error, deployTimeout time.Duration) string {
	var buildErr *engine.BuildError
	var healthErr *HealthError
	switch {
	case errors.As(err, &buildErr):
		return buildErr.Error()
	case errors.As(err, &healthErr):
		return healthErr.Error()
	case errors.Is(err, source.ErrNotFound):
		return source.ErrNotFound.Error()
	case errors.Is(err, source.ErrTooLarge):
		return source.ErrTooLarge.Error()
	case errors.Is(err, source.ErrDirNotFound):
		return source.ErrDirNotFound.Error()
	case errors.Is(err, errTimedOut):
		return fmt.Sprintf("deploy timed out after %s", deployTimeout)
	case errors.Is(err, ErrImageGone):
		return ErrImageGone.Error()
	case errors.Is(err, jobs.ErrNoAttemptsLeft):
		return "gave up: " + jobs.ErrNoAttemptsLeft.Error()
	}
	return "internal error; see the platform's logs"
}

// streamReader passes reads through and remembers the first error other than
// EOF. Docker's client reads it on another goroutine, hence the lock.
type streamReader struct {
	r   io.Reader
	mu  sync.Mutex
	err error
}

func (s *streamReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		s.mu.Lock()
		if s.err == nil {
			s.err = err
		}
		s.mu.Unlock()
	}
	return n, err
}

func (s *streamReader) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// An app's database: its container, volume and network all share one name,
// derived from the app's, so none needs storing.

// DatabaseImage is what every app database runs: pinned, like the platform's
// own Postgres.
const DatabaseImage = "postgres:18.6"

// databaseMemoryBytes caps an app database's memory. Postgres's defaults
// (128 MiB of shared buffers) fit well inside it.
const databaseMemoryBytes = 256 << 20

// The database user and database name inside an app's Postgres. Each app has
// a Postgres of its own, so they needn't vary.
const (
	DatabaseUser = "app"
	DatabaseDB   = "app"
)

// DatabaseName is an app's database container, its data volume and its
// network, e.g. ops-db-hello. It's also the database's hostname.
func DatabaseName(app string) string { return "ops-db-" + app }

// DatabaseURL is how an app reaches its database, given as DATABASE_URL. No
// TLS: the connection never leaves the app's own internal network.
func DatabaseURL(app, password string) string {
	return "postgres://" + DatabaseUser + ":" + password + "@" + DatabaseName(app) + ":5432/" + DatabaseDB + "?sslmode=disable"
}

// appEnv is what an app's container starts with: its env vars and, if the app
// has a database, DATABASE_URL and the database's network to join (made sure
// to exist first, as a deploy can come before the reconciler has created the
// database). The platform's DATABASE_URL replaces an env var of that name.
func (d *Deployer) appEnv(ctx context.Context, app store.App) (env map[string]string, networks []string, err error) {
	env, err = d.store.EnvVars(ctx, app.ID)
	if err != nil {
		return nil, nil, err
	}
	db, err := d.store.Database(ctx, app.ID)
	if errors.Is(err, store.ErrNotFound) {
		return env, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	name := DatabaseName(app.Name)
	if err := d.engine.EnsureNetwork(ctx, name, databaseLabels(app)); err != nil {
		return nil, nil, err
	}
	env["DATABASE_URL"] = DatabaseURL(app.Name, db.Password)
	return env, []string{name}, nil
}

func databaseLabels(app store.App) map[string]string {
	return map[string]string{
		engine.LabelApp:   app.Name,
		engine.LabelAppID: strconv.FormatInt(app.ID, 10),
	}
}

// RunDatabase creates and starts an app's database container, for the
// reconciler (which holds the app's lock): its network first, then its
// image, pulled if Docker doesn't have it. Its data is on the volume of the
// same name, so a container created again finds the data of the one before.
func (d *Deployer) RunDatabase(ctx context.Context, app store.App, db store.Database) error {
	name := DatabaseName(app.Name)
	if err := d.engine.EnsureNetwork(ctx, name, databaseLabels(app)); err != nil {
		return err
	}
	if err := d.engine.Pull(ctx, DatabaseImage); err != nil {
		return err
	}
	_, err := d.engine.RunDatabase(ctx, engine.DatabaseSpec{
		Name:  name,
		Image: DatabaseImage,
		// Read by the image only when the volume is first initialised; a
		// recreated container keeps the data, and the password, it finds.
		Env: map[string]string{
			"POSTGRES_USER":     DatabaseUser,
			"POSTGRES_PASSWORD": db.Password,
			"POSTGRES_DB":       DatabaseDB,
		},
		Labels:      databaseLabels(app),
		Network:     name,
		Volume:      name,
		MemoryBytes: databaseMemoryBytes,
	})
	return err
}
