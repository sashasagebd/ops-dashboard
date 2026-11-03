// Package deploy runs a deployment from start to finish: download the code,
// build it, start a container, check it's healthy through Caddy, switch the
// app's route to it, and remove the previous version.
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
}

type Router interface {
	Apply(ctx context.Context, routes []router.Route) error
	Probe(ctx context.Context, host, path string) (int, error)
}

// Config holds the deployer's settings.
type Config struct {
	AppsNetwork    string        // the Docker network app containers join
	DeployTimeout  time.Duration // one attempt at a deploy, download to switch
	HealthTimeout  time.Duration // how long a new container gets to answer healthily
	HealthInterval time.Duration // time between health probes
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

	switch dep.Status {
	case store.StatusQueued, store.StatusBuilding, store.StatusStarting:
		// Older deployments' logs go as each new one starts, so an app
		// keeps only its recent ones.
		if _, err := d.store.PruneDeploymentLogs(ctx, app.ID, keepLogsFor); err != nil {
			log.Warn("pruning old deploy logs", "err", err)
		}
		out.Printf("Deploying %s at %s (attempt %d)", app.RepoURL, shortSHA(dep.CommitSHA), job.Attempts)
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
		if err := d.switchTo(ctx, log, out, app.Name, dep.ID); err != nil {
			return err
		}
		out.Printf("Live at http://%s/", app.Hostname)
		return nil
	default:
		// Finished some other way (superseded by a later deploy, or failed
		// and recorded just before the platform died): nothing to do.
		log.Info("deploy already finished", "status", dep.Status)
		return nil
	}

	if err := d.store.MarkBuilding(ctx, dep.ID); err != nil {
		return err
	}

	// Download and build. The code streams from GitHub straight into
	// Docker's build.
	tag := ImageTag(app.Name, dep.CommitSHA)
	where := "the repo's root"
	if app.SourceDir != "" {
		where = app.SourceDir + "/"
	}
	out.Printf("Building image %s from %s", tag, where)
	src, err := d.source.BuildContext(ctx, app.RepoURL, dep.CommitSHA, app.SourceDir)
	if err != nil {
		return err
	}
	labels := map[string]string{
		engine.LabelApp:        app.Name,
		engine.LabelAppID:      strconv.FormatInt(app.ID, 10),
		engine.LabelDeployment: strconv.FormatInt(dep.ID, 10),
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
			return streamErr
		}
		return err
	}
	if err := d.store.MarkStarting(ctx, dep.ID, tag); err != nil {
		return err
	}

	// Start the new container next to the old one, which keeps serving.
	env, err := d.store.EnvVars(ctx, app.ID)
	if err != nil {
		return err
	}
	name := ContainerName(app.Name, dep.ID)
	out.Printf("Starting container %s (%d MiB memory, %g CPU, %d env vars)",
		name, app.MemoryLimitBytes>>20, float64(app.CPULimitMillicores)/1000, len(env))
	containerID, err := d.engine.Run(ctx, engine.RunSpec{
		Name:        name,
		Image:       tag,
		Env:         env,
		Labels:      labels,
		Network:     d.cfg.AppsNetwork,
		MemoryBytes: app.MemoryLimitBytes,
		NanoCPUs:    int64(app.CPULimitMillicores) * 1_000_000,
	})
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

	// Healthy: make it the live deployment (superseding the old one in the
	// same transaction), then point the app's hostname at it.
	out.Printf("Healthy")
	if err := d.store.MarkHealthy(ctx, dep.ID); err != nil {
		return err
	}
	if err := d.switchTo(ctx, log, out, app.Name, dep.ID); err != nil {
		// The database says the new deployment is live, but Caddy still
		// routes to the old container, which is still running, so the
		// site stays up on the old version. Keeping the old container is
		// what makes that safe; the next route update (another deploy, or
		// the platform's startup) corrects Caddy. Not retried: the deploy
		// itself has succeeded.
		log.Error("deployment is healthy but switching its route failed; keeping the previous container", "err", err)
		out.Printf("Couldn't update the route yet: the previous version keeps serving until the next route update")
		return nil
	}
	log.Info("deploy finished: healthy")
	out.Printf("Live at http://%s/", app.Hostname)
	return nil
}

// switchTo points Caddy at a deployment that's now live, then removes the
// app's older containers. If the route update fails, nothing is removed.
func (d *Deployer) switchTo(ctx context.Context, log *slog.Logger, out *deployLog, app string, deploymentID int64) error {
	if err := d.ApplyRoutes(ctx); err != nil {
		return err
	}
	d.removeOlderContainers(context.WithoutCancel(ctx), log, out, app, deploymentID)
	return nil
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

// ApplyRoutes sends Caddy the complete routing table: every app's hostname to
// its live deployment's container, plus any probe routes for deployments
// being checked. It's built fresh from the database each time, so it's always
// safe to call; the platform also calls it at startup, as Caddy may have
// restarted or missed an update.
func (d *Deployer) ApplyRoutes(ctx context.Context) error {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()

	live, err := d.store.LiveApps(ctx)
	if err != nil {
		return err
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
	return d.router.Apply(ctx, routes)
}

// removeOlderContainers removes the app's containers from deployments before
// live: the version it replaced, and any left over from earlier failures.
// Newer ones are left alone: once live is no longer in progress, the app's
// next deploy may already have started its container. Failures are logged
// rather than failing the deploy, which has succeeded.
func (d *Deployer) removeOlderContainers(ctx context.Context, log *slog.Logger, out *deployLog, app string, live int64) {
	containers, err := d.engine.ManagedContainers(ctx, app)
	if err != nil {
		log.Error("listing old containers", "err", err)
		return
	}
	for _, c := range containers {
		id, err := strconv.ParseInt(c.Labels[engine.LabelDeployment], 10, 64)
		if err != nil || id >= live {
			continue
		}
		if err := d.engine.Remove(ctx, c.ID); err != nil {
			log.Error("removing old container", "container", c.Name, "err", err)
			continue
		}
		out.Printf("Removed the previous container %s", c.Name)
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

// RemoveOrphans removes every platform container that isn't an app's live
// deployment. It's for startup only, before the job workers start, so no
// deploy is in flight: any such container was left by a deploy cut off by a
// crash or restart (its restart policy would otherwise keep it running
// forever, unrouted; if that deploy's job is retried, it starts a new one). It only
// ever sees containers with the platform's label, so nothing else on the
// host is touched. P4's reconciler does this continuously.
func (d *Deployer) RemoveOrphans(ctx context.Context) error {
	live, err := d.store.LiveApps(ctx)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, a := range live {
		keep[strconv.FormatInt(a.DeploymentID, 10)] = true
	}
	containers, err := d.engine.ManagedContainers(ctx, "")
	if err != nil {
		return err
	}
	for _, c := range containers {
		if keep[c.Labels[engine.LabelDeployment]] {
			continue
		}
		slog.Info("removing orphaned app container", "container", c.Name)
		if err := d.engine.Remove(ctx, c.ID); err != nil {
			return err
		}
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
