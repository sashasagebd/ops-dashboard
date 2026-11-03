// Package deploy runs a deployment from start to finish: download the code,
// build it, start a container, check it's healthy through Caddy, switch the
// app's route to it, and remove the previous version.
//
// In P1 each deploy runs in its own goroutine, started by the API (see
// docs/decisions/platform.md); P2 moves this onto a job queue in Postgres.
// The rules that must hold under concurrency (one deploy in progress and
// one live deployment per app) are enforced by the database, not here.
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
	MarkBuilding(ctx context.Context, id int64) error
	MarkStarting(ctx context.Context, id int64, imageTag string) error
	MarkHealthy(ctx context.Context, id int64) error
	MarkFailed(ctx context.Context, id int64, reason string) error
	EnvVars(ctx context.Context, appID int64) (map[string]string, error)
	LiveApps(ctx context.Context) ([]store.LiveApp, error)
}

type Source interface {
	BuildContext(ctx context.Context, repoURL, commitSHA, dir string) (io.ReadCloser, error)
}

type Engine interface {
	Build(ctx context.Context, buildContext io.Reader, tag string, labels map[string]string) error
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
	DeployTimeout  time.Duration // the whole deploy, download to switch
	HealthTimeout  time.Duration // how long a new container gets to answer healthily
	HealthInterval time.Duration // time between health probes
}

// Deployer starts and runs deployments.
type Deployer struct {
	store  Store
	source Source
	engine Engine
	router Router
	cfg    Config

	// baseCtx is cancelled when the platform shuts down, which cancels
	// every deploy in flight.
	baseCtx context.Context
	wg      sync.WaitGroup

	// applyMu makes route updates one at a time, so two deploys finishing
	// together can't send Caddy their tables in the wrong order.
	applyMu sync.Mutex
	mu      sync.Mutex
	probes  map[int64]router.Route // deployment ID -> its temporary probe route
}

// New returns a Deployer. Deploys it starts are cancelled when ctx is.
func New(ctx context.Context, cfg Config, st Store, src Source, eng Engine, rt Router) *Deployer {
	return &Deployer{
		store: st, source: src, engine: eng, router: rt, cfg: cfg,
		baseCtx: ctx,
		probes:  map[int64]router.Route{},
	}
}

// Start queues a deploy of commitSHA for an app and runs it in the
// background, returning the new deployment. If the app already has a deploy
// in progress, it returns store.ErrDeployInProgress and starts nothing.
func (d *Deployer) Start(ctx context.Context, appID int64, commitSHA string) (store.Deployment, error) {
	dep, err := d.store.CreateDeployment(ctx, appID, commitSHA)
	if err != nil {
		return store.Deployment{}, err
	}
	d.wg.Go(func() { d.run(dep) })
	return dep, nil
}

// Wait blocks until every deploy in flight has finished (or, after shutdown,
// given up), so the platform doesn't close the database under them.
func (d *Deployer) Wait() { d.wg.Wait() }

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

func (d *Deployer) run(dep store.Deployment) {
	ctx, cancel := context.WithTimeout(d.baseCtx, d.cfg.DeployTimeout)
	defer cancel()

	log := slog.With("deployment", dep.ID, "app", dep.AppID, "commit", dep.CommitSHA)
	log.Info("deploy started")
	if err := d.deploy(ctx, log, dep); err != nil {
		log.Error("deploy failed", "err", err)
		// Recorded even if the deploy's own context is done (timeout or
		// shutdown), so the deployment doesn't stay "in progress".
		if err := d.store.MarkFailed(context.WithoutCancel(ctx), dep.ID, publicReason(err, d.cfg.DeployTimeout)); err != nil {
			log.Error("recording the failure", "err", err)
		}
		return
	}
	log.Info("deploy finished: healthy")
}

func (d *Deployer) deploy(ctx context.Context, log *slog.Logger, dep store.Deployment) error {
	app, err := d.store.App(ctx, dep.AppID)
	if err != nil {
		return err
	}
	if err := d.store.MarkBuilding(ctx, dep.ID); err != nil {
		return err
	}

	// Download and build. The code streams from GitHub straight into
	// Docker's build.
	src, err := d.source.BuildContext(ctx, app.RepoURL, dep.CommitSHA, app.SourceDir)
	if err != nil {
		return err
	}
	tag := ImageTag(app.Name, dep.CommitSHA)
	labels := map[string]string{
		engine.LabelApp:        app.Name,
		engine.LabelAppID:      strconv.FormatInt(app.ID, 10),
		engine.LabelDeployment: strconv.FormatInt(dep.ID, 10),
	}
	stream := &streamReader{r: src}
	err = d.engine.Build(ctx, stream, tag, labels)
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
	err = d.checkHealth(ctx, dep.ID, upstream, app.HealthCheckPath)
	if err != nil {
		if rmErr := d.engine.Remove(context.WithoutCancel(ctx), containerID); rmErr != nil {
			log.Error("removing the unhealthy container", "container", name, "err", rmErr)
		}
		return err
	}

	// Healthy: make it the live deployment (superseding the old one in the
	// same transaction), then point the app's hostname at it.
	if err := d.store.MarkHealthy(ctx, dep.ID); err != nil {
		return err
	}
	if err := d.ApplyRoutes(ctx); err != nil {
		// The database says the new deployment is live, but Caddy still
		// routes to the old container, which is still running, so the
		// site stays up on the old version. Keeping the old container is
		// what makes that safe; the next route update (another deploy, or
		// the platform's startup) corrects Caddy.
		log.Error("deployment is healthy but switching its route failed; keeping the previous container", "err", err)
		return nil
	}

	// The route has switched: the previous version can go.
	d.removeOtherContainers(context.WithoutCancel(ctx), log, app.Name, dep.ID)
	return nil
}

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

// removeOtherContainers removes the app's containers other than deployment
// keep's: the version it replaced, and any left over from earlier failures.
// Failures are logged rather than failing the deploy, which has succeeded.
func (d *Deployer) removeOtherContainers(ctx context.Context, log *slog.Logger, app string, keep int64) {
	containers, err := d.engine.ManagedContainers(ctx, app)
	if err != nil {
		log.Error("listing old containers", "err", err)
		return
	}
	keepLabel := strconv.FormatInt(keep, 10)
	for _, c := range containers {
		if c.Labels[engine.LabelDeployment] == keepLabel {
			continue
		}
		if err := d.engine.Remove(ctx, c.ID); err != nil {
			log.Error("removing old container", "container", c.Name, "err", err)
		}
	}
}

// RemoveOrphans removes every platform container that isn't an app's live
// deployment. It's for startup only, when no deploy is in flight: then any
// such container was left by a deploy cut off by a crash or restart (its
// restart policy would otherwise keep it running forever, unrouted). It only
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
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("deploy timed out after %s", deployTimeout)
	case errors.Is(err, context.Canceled):
		return "interrupted: the platform shut down during this deploy"
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
