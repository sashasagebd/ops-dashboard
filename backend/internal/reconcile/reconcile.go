// Package reconcile keeps what's running in line with what should be: the
// platform's central loop (see docs/decisions/platform.md).
//
// Postgres says what should run: each app's live deployment, and the route
// to it. Every few seconds the reconciler compares that with what Docker
// (the platform's own containers only) and Caddy actually have, and fixes the
// difference: it starts a live container that has stopped (crashed, killed,
// or not started after a reboot), recreates one that was deleted, removes
// containers no live deployment owns, and re-applies Caddy's routes if they
// differ. Each fix is recorded as an event for the dashboard.
//
// It never touches an app while that app's deploy is running (the deployer's
// per-app lock), so the two can't undo each other's work; and it only ever
// sees containers carrying the platform's label, so nothing else on the host
// (the game servers) is touched.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/deploy"
	"github.com/sashasagebd/ops-dashboard/backend/internal/engine"
	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

// The interfaces below are what the reconciler needs; in production they're
// *store.Store, *engine.Client and *deploy.Deployer.

type Store interface {
	Apps(ctx context.Context) ([]store.App, error)
	CurrentDeployment(ctx context.Context, appID int64) (store.Deployment, error)
	Databases(ctx context.Context) ([]store.Database, error)
	MarkDatabaseProvisioned(ctx context.Context, appID int64) error
	AddEvent(ctx context.Context, e store.NewEvent) error
	PruneEvents(ctx context.Context, before time.Time) (int64, error)
}

type Engine interface {
	ManagedContainers(ctx context.Context, app string) ([]engine.ManagedContainer, error)
	Exit(ctx context.Context, id string) (engine.ExitInfo, error)
	TailLogs(ctx context.Context, id string, n int) ([]engine.LogLine, error)
	Start(ctx context.Context, id string) error
	Remove(ctx context.Context, id string) error
}

type Deployer interface {
	TryLockApp(appID int64) (unlock func(), ok bool)
	Recreate(ctx context.Context, app store.App, dep store.Deployment) error
	RunDatabase(ctx context.Context, app store.App, db store.Database) error
	SyncRoutes(ctx context.Context) (changed bool, err error)
}

// Config holds the reconciler's settings.
type Config struct {
	Interval    time.Duration // between passes
	MinBackoff  time.Duration // after a crash restart, how long before the next is allowed; doubles each time
	MaxBackoff  time.Duration // the most it doubles to
	StableAfter time.Duration // running this long since its last restart forgives an app's past crashes
	KeepEvents  time.Duration // events older than this are pruned
	OutputLines int           // a crashed container's last lines kept in its event
}

// crashLoopAfter is the restart count from which an app counts as
// crash-looping, and its events say so.
const crashLoopAfter = 3

// Reconciler runs reconcile passes. Pass and Run are called from one
// goroutine at a time (startup, then the loop), so its own state needs no
// lock.
type Reconciler struct {
	cfg      Config
	store    Store
	engine   Engine
	deployer Deployer
	now      func() time.Time

	crashes   map[string]*crashes // by container: "app:<app ID>" or "db:<app ID>"
	failing   map[string]bool     // fixes failing pass after pass, by key: recorded once
	lastPrune time.Time
	lastErr   string // the last failed pass's error, to log only changes
}

// crashes is a container's recent restarts, for backing off.
type crashes struct {
	deployment int64     // whose container (0 for a database); a new live deployment starts afresh
	restarts   int       // since the app was last stable
	lastStart  time.Time // when the reconciler last started it
	next       time.Time // no restart before this
}

// New returns a Reconciler.
func New(cfg Config, st Store, eng Engine, dep Deployer) *Reconciler {
	return &Reconciler{
		cfg: cfg, store: st, engine: eng, deployer: dep, now: time.Now,
		crashes: map[string]*crashes{}, failing: map[string]bool{},
	}
}

// Run makes a pass every Interval until ctx is done. A failed pass (Docker,
// Caddy or the database unreachable) is logged, once until it changes, and
// the next pass tries again.
func (r *Reconciler) Run(ctx context.Context) {
	t := time.NewTicker(r.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := r.Pass(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil && err.Error() != r.lastErr:
			slog.Warn("reconcile pass failed; retrying every pass", "err", err)
			r.lastErr = err.Error()
		case err == nil && r.lastErr != "":
			slog.Info("reconcile passes are succeeding again")
			r.lastErr = ""
		}
	}
}

// Pass makes one reconcile pass: Caddy's routes first, then every app's
// containers, then the containers of apps that no longer exist. It carries
// on past one part's failure, and returns every error it met.
//
// Routes come first because removing an orphan is only safe once Caddy is
// known to route to live containers: a deploy whose route switch failed
// keeps the previous version's container running because Caddy still sends
// it traffic. So if the routes couldn't be checked or fixed, this pass
// starts and recreates containers but removes none.
func (r *Reconciler) Pass(ctx context.Context) error {
	var errs []error
	routesOK := true
	changed, err := r.deployer.SyncRoutes(ctx)
	switch {
	case err != nil && changed:
		r.fixFailed(ctx, "routes", 0, "Caddy's routes didn't match the database, and re-applying them failed", err)
		errs = append(errs, err)
		routesOK = false
	case err != nil:
		errs = append(errs, err)
		routesOK = false
	case changed:
		r.fixed("routes")
		slog.Info("re-applied Caddy's routes: they didn't match the database")
		r.event(ctx, store.NewEvent{Kind: store.EventRoutesReapplied,
			Message: "Caddy's routes didn't match the database (a route was missing or changed); re-applied them"})
	}

	apps, err := r.store.Apps(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	dbs, err := r.store.Databases(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	dbByApp := map[int64]store.Database{}
	for _, db := range dbs {
		dbByApp[db.AppID] = db
	}
	for _, app := range apps {
		var db *store.Database
		if d, ok := dbByApp[app.ID]; ok {
			db = &d
		}
		if err := r.app(ctx, app, db, routesOK); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", app.Name, err))
		}
	}
	if routesOK {
		if err := r.unknownApps(ctx, apps); err != nil {
			errs = append(errs, err)
		}
	}

	r.prune(ctx)
	return errors.Join(errs...)
}

// app reconciles one app's containers, its database's (if db isn't nil)
// first, unless a deploy of it is running: the deploy owns them until it's
// done. Orphans are only removed if removeOrphans (Caddy's routes are known
// to be right; see Pass).
func (r *Reconciler) app(ctx context.Context, app store.App, db *store.Database, removeOrphans bool) error {
	unlock, ok := r.deployer.TryLockApp(app.ID)
	if !ok {
		return nil
	}
	defer unlock()

	// Read after taking the lock: a deploy that finished just before has
	// changed which deployment is live.
	live, err := r.store.CurrentDeployment(ctx, app.ID)
	hasLive := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	containers, err := r.engine.ManagedContainers(ctx, app.Name)
	if err != nil {
		return err
	}

	var liveContainer, dbContainer *engine.ManagedContainer
	for _, c := range containers {
		if c.Labels[engine.LabelRole] == engine.RoleBackup {
			// A backup running: its job owns it, and removes it when done.
			continue
		}
		if c.Labels[engine.LabelRole] == engine.RoleDatabase {
			if db != nil {
				dbContainer = &c
				continue
			}
			// The app has no database (any more): its container is an
			// orphan like any other. Its volume, with the data, stays.
		} else if hasLive && c.Labels[engine.LabelDeployment] == strconv.FormatInt(live.ID, 10) {
			liveContainer = &c
			continue
		}
		// No live deployment owns it: left by a deploy that was cut off, a
		// removal that failed, or a switch whose route update failed.
		if removeOrphans {
			r.removeOrphan(ctx, app.ID, app.Name, c)
		}
	}
	if db != nil {
		r.database(ctx, app, *db, dbContainer)
	}
	appKey := "app:" + strconv.FormatInt(app.ID, 10)
	if !hasLive {
		delete(r.crashes, appKey)
		return nil
	}

	name := deploy.ContainerName(app.Name, live.ID)
	switch {
	case liveContainer == nil:
		r.recreate(ctx, app, live, name, "it had been removed")
	case liveContainer.State == "running":
		r.maybeStable(appKey)
	case liveContainer.State == "dead":
		// Docker can't start a dead container: replace it.
		key := "remove:" + liveContainer.ID
		if err := r.engine.Remove(ctx, liveContainer.ID); err != nil {
			r.fixFailed(ctx, key, app.ID, fmt.Sprintf("%s's container %s is dead, and removing it failed", app.Name, name), err)
			return nil
		}
		r.fixed(key)
		r.recreate(ctx, app, live, name, "Docker had marked it dead")
	case liveContainer.State == "exited" || liveContainer.State == "created":
		r.restart(ctx, app, appKey, live.ID, *liveContainer, "its container")
	default:
		// "restarting" (Docker's own restart policy, on containers from
		// before the reconciler), "paused" (someone paused it on purpose)
		// or "removing": left alone.
	}
	return nil
}

// restart starts one of the app's stopped containers again (key and
// deployment say which, what says it in words: "its container"), unless
// it's backing off after repeated crashes, recording why it stopped.
func (r *Reconciler) restart(ctx context.Context, app store.App, key string, deployment int64, c engine.ManagedContainer, what string) {
	now := r.now()
	st := r.crashes[key]
	if st == nil || st.deployment != deployment {
		st = &crashes{deployment: deployment}
		r.crashes[key] = st
	}
	if now.Before(st.next) {
		return
	}

	// Why it stopped, and its last words, before starting it again. Both
	// best effort: the restart matters more than the record.
	exit, exitErr := r.engine.Exit(ctx, c.ID)
	stopped := "had stopped"
	if exitErr == nil {
		stopped = describeExit(exit)
	}
	var output []string
	if lines, err := r.engine.TailLogs(ctx, c.ID, r.cfg.OutputLines); err == nil {
		for _, l := range lines {
			output = append(output, l.Line)
		}
	}

	st.restarts++
	st.lastStart = now
	backoff := r.backoff(st.restarts)
	st.next = now.Add(backoff)
	fixKey := "start:" + c.ID
	if err := r.engine.Start(ctx, c.ID); err != nil {
		r.fixFailed(ctx, fixKey, app.ID, fmt.Sprintf("%s's %s %s %s, and starting it again failed",
			app.Name, strings.TrimPrefix(what, "its "), c.Name, stopped), err)
		return
	}
	r.fixed(fixKey)

	e := store.NewEvent{AppID: app.ID, Kind: store.EventRestarted, Output: output,
		Message: fmt.Sprintf("Restarted %s: %s %s %s", app.Name, what, c.Name, stopped)}
	if st.restarts >= crashLoopAfter {
		e.Kind = store.EventCrashLooping
		e.Message = fmt.Sprintf("%s keeps stopping: restarted %d times (%s %s %s); the next restart waits at least %s",
			app.Name, st.restarts, what, c.Name, stopped, backoff)
	}
	slog.Info("restarted an app's container", "app", app.Name, "container", c.Name, "restarts", st.restarts, "stopped", stopped)
	r.event(ctx, e)
}

// backoff is how long after the nth restart the next is allowed: MinBackoff,
// doubling, at most MaxBackoff.
func (r *Reconciler) backoff(n int) time.Duration {
	d := r.cfg.MinBackoff
	for i := 1; i < n && d < r.cfg.MaxBackoff; i++ {
		d *= 2
	}
	return min(d, r.cfg.MaxBackoff)
}

// maybeStable forgets a container's crashes once it has run for StableAfter
// since the reconciler last started it.
func (r *Reconciler) maybeStable(key string) {
	if st := r.crashes[key]; st != nil && r.now().Sub(st.lastStart) >= r.cfg.StableAfter {
		delete(r.crashes, key)
	}
}

func describeExit(e engine.ExitInfo) string {
	if e.OOMKilled {
		return fmt.Sprintf("was killed for using more than its memory limit (exit code %d)", e.ExitCode)
	}
	return fmt.Sprintf("had exited with code %d", e.ExitCode)
}

// database keeps an app's database container running: created the first
// time (or again, if it was deleted: its data is on its volume), started if
// it stopped, replaced if Docker marked it dead. c is its container, nil if
// there's none.
func (r *Reconciler) database(ctx context.Context, app store.App, db store.Database, c *engine.ManagedContainer) {
	name := deploy.DatabaseName(app.Name)
	key := "db:" + strconv.FormatInt(app.ID, 10)
	switch {
	case c == nil:
		r.runDatabase(ctx, app, db)
	case c.State == "running":
		r.maybeStable(key)
		if db.ProvisionedAt == nil {
			// Created, but recording that failed last time.
			r.markProvisioned(ctx, app)
		}
	case c.State == "dead":
		fixKey := "remove:" + c.ID
		if err := r.engine.Remove(ctx, c.ID); err != nil {
			r.fixFailed(ctx, fixKey, app.ID, fmt.Sprintf("%s's database %s is dead, and removing it failed", app.Name, name), err)
			return
		}
		r.fixed(fixKey)
		r.runDatabase(ctx, app, db)
	case c.State == "exited" || c.State == "created":
		r.restart(ctx, app, key, 0, *c, "its database")
	}
}

// runDatabase creates the app's database container: for the first time, or
// again after it was deleted (its data is on the volume of the same name).
func (r *Reconciler) runDatabase(ctx context.Context, app store.App, db store.Database) {
	name := deploy.DatabaseName(app.Name)
	first := db.ProvisionedAt == nil
	fixKey := "database:" + strconv.FormatInt(app.ID, 10)
	if err := r.deployer.RunDatabase(ctx, app, db); err != nil {
		msg := fmt.Sprintf("Creating %s's database %s failed", app.Name, name)
		if !first {
			msg = fmt.Sprintf("%s's database %s is missing, and creating it again failed", app.Name, name)
		}
		r.fixFailed(ctx, fixKey, app.ID, msg, err)
		return
	}
	r.fixed(fixKey)
	if first {
		r.markProvisioned(ctx, app)
		slog.Info("created an app's database", "app", app.Name, "container", name)
		r.event(ctx, store.NewEvent{AppID: app.ID, Kind: store.EventDatabaseCreated,
			Message: fmt.Sprintf("Created %s's database %s (Postgres 18); the app gets DATABASE_URL from its next deploy", app.Name, name)})
		return
	}
	slog.Info("recreated an app's database", "app", app.Name, "container", name)
	r.event(ctx, store.NewEvent{AppID: app.ID, Kind: store.EventRecreated,
		Message: fmt.Sprintf("Recreated %s's database %s: its container had been removed. Its data was kept on the volume %s", app.Name, name, name)})
}

func (r *Reconciler) markProvisioned(ctx context.Context, app store.App) {
	if err := r.store.MarkDatabaseProvisioned(ctx, app.ID); err != nil {
		slog.Error("recording a database as created", "app", app.Name, "err", err)
	}
}

// recreate creates the live deployment's container again.
func (r *Reconciler) recreate(ctx context.Context, app store.App, live store.Deployment, name, why string) {
	key := "recreate:" + strconv.FormatInt(live.ID, 10)
	if err := r.deployer.Recreate(ctx, app, live); err != nil {
		msg := fmt.Sprintf("%s's container %s is missing (%s), and recreating it failed", app.Name, name, why)
		if errors.Is(err, deploy.ErrImageGone) {
			msg += ": its image no longer exists; deploy the app again"
		}
		r.fixFailed(ctx, key, app.ID, msg, err)
		return
	}
	r.fixed(key)
	slog.Info("recreated an app's container", "app", app.Name, "container", name)
	r.event(ctx, store.NewEvent{AppID: app.ID, Kind: store.EventRecreated,
		Message: fmt.Sprintf("Recreated %s's container %s: %s", app.Name, name, why)})
}

func (r *Reconciler) removeOrphan(ctx context.Context, appID int64, app string, c engine.ManagedContainer) {
	key := "remove:" + c.ID
	if err := r.engine.Remove(ctx, c.ID); err != nil {
		r.fixFailed(ctx, key, appID, fmt.Sprintf("Removing %s's unused container %s failed", app, c.Name), err)
		return
	}
	r.fixed(key)
	slog.Info("removed an orphaned app container", "app", app, "container", c.Name)
	r.event(ctx, store.NewEvent{AppID: appID, Kind: store.EventRemovedOrphan,
		Message: fmt.Sprintf("Removed %s's container %s: no live deployment uses it", app, c.Name)})
}

// unknownApps removes the platform's containers whose app isn't registered
// (any more). The apps are listed again after the containers: an app created
// meanwhile, whose first deploy has just started its container, is then in
// the list, since it existed before its container did.
func (r *Reconciler) unknownApps(ctx context.Context, apps []store.App) error {
	containers, err := r.engine.ManagedContainers(ctx, "")
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, a := range apps {
		known[a.Name] = true
	}
	var unknown []engine.ManagedContainer
	for _, c := range containers {
		if !known[c.Labels[engine.LabelApp]] {
			unknown = append(unknown, c)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	again, err := r.store.Apps(ctx)
	if err != nil {
		return err
	}
	for _, a := range again {
		known[a.Name] = true
	}
	for _, c := range unknown {
		if !known[c.Labels[engine.LabelApp]] {
			r.removeOrphan(ctx, 0, c.Labels[engine.LabelApp], c)
		}
	}
	return nil
}

// fixFailed records that a fix failed, once while it keeps failing pass
// after pass (key names the fix); the error itself goes to the log only.
func (r *Reconciler) fixFailed(ctx context.Context, key string, appID int64, msg string, err error) {
	slog.Error("reconcile fix failed", "fix", key, "err", err)
	if r.failing[key] {
		return
	}
	r.failing[key] = true
	r.event(ctx, store.NewEvent{AppID: appID, Kind: store.EventFixFailed, Message: msg})
}

// fixed clears a failing fix, so a later failure is recorded again.
func (r *Reconciler) fixed(key string) { delete(r.failing, key) }

func (r *Reconciler) event(ctx context.Context, e store.NewEvent) {
	if err := r.store.AddEvent(ctx, e); err != nil {
		slog.Error("recording a reconcile event", "kind", e.Kind, "err", err)
	}
}

// prune deletes old events, at most once an hour.
func (r *Reconciler) prune(ctx context.Context) {
	now := r.now()
	if now.Sub(r.lastPrune) < time.Hour {
		return
	}
	if _, err := r.store.PruneEvents(ctx, now.Add(-r.cfg.KeepEvents)); err != nil {
		slog.Warn("pruning reconcile events", "err", err)
		return
	}
	r.lastPrune = now
}
