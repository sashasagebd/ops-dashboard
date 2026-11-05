package deploy

import (
	"context"
	"errors"
	"log/slog"

	"github.com/sashasagebd/ops-dashboard/backend/internal/engine"
	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

// Stopping, starting and deleting apps. The store decides whether each is
// allowed (not mid-deploy, not mid-backup) and records it first, so the
// reconciler and deploys act on the new state from then on; the Docker and
// Caddy work here follows, under the app's lock. That part is best effort:
// whatever it leaves, the reconciler finishes (it removes a stopped app's
// container and a deleted app's containers on its own).

// StopApp stops an app: no routes, no container (its memory is freed), no
// deploys until it's started again. Its database keeps running, so backups
// carry on and Start is quick. store.ErrDeployInProgress mid-deploy.
func (d *Deployer) StopApp(ctx context.Context, appID int64) (store.App, error) {
	app, err := d.store.StopApp(ctx, appID)
	if err != nil {
		return store.App{}, err
	}
	unlock := d.locks.lock(app.ID)
	defer unlock()
	log := slog.With("app", app.Name)
	// Routes first, so nothing is sent to a container that's going away.
	if err := d.ApplyRoutes(ctx); err != nil {
		log.Warn("stopping: removing the app's routes; the reconciler will retry", "err", err)
	}
	d.removeContainers(ctx, log, app.Name, func(c engine.ManagedContainer) bool { return isAppContainer(c) })
	log.Info("app stopped")
	return app, nil
}

// StartApp starts a stopped app again: its live deployment's container is
// run again from its image (no build), and its routes come back. An app
// with nothing live only loses its stopped mark.
func (d *Deployer) StartApp(ctx context.Context, appID int64) (store.App, error) {
	app, err := d.store.StartApp(ctx, appID)
	if err != nil {
		return store.App{}, err
	}
	unlock := d.locks.lock(app.ID)
	defer unlock()
	log := slog.With("app", app.Name)
	live, err := d.store.CurrentDeployment(ctx, app.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		log.Info("app started; nothing is live to run")
		return app, nil
	case err != nil:
		return app, err
	}
	// A container left by a stop that couldn't remove it would clash with
	// the new one's name.
	d.removeContainers(ctx, log, app.Name, func(c engine.ManagedContainer) bool { return isAppContainer(c) })
	if err := d.Recreate(ctx, app, live); err != nil {
		// Marked started, so the reconciler keeps trying (and records it).
		return app, err
	}
	if err := d.ApplyRoutes(ctx); err != nil {
		log.Warn("starting: restoring the app's routes; the reconciler will retry", "err", err)
	}
	log.Info("app started", "deployment", live.ID)
	return app, nil
}

// DeleteApp deletes an app: its record and history (the store), then its
// routes, every container of it (its database's too), its images and its
// database's network. Its database's volume, with the data, is left: the
// platform can't remove volumes (it has no permission to), so a slip can't
// destroy data; the owner removes it by hand. Backups stay too.
// store.ErrDeployInProgress or store.ErrBackupInProgress while either runs.
func (d *Deployer) DeleteApp(ctx context.Context, appID int64) (store.DeletedApp, error) {
	deleted, err := d.store.DeleteApp(ctx, appID)
	if err != nil {
		return store.DeletedApp{}, err
	}
	app := deleted.App
	unlock := d.locks.lock(app.ID)
	defer unlock()
	log := slog.With("app", app.Name)
	if err := d.ApplyRoutes(ctx); err != nil {
		log.Warn("deleting: removing the app's routes; the reconciler will retry", "err", err)
	}
	d.removeContainers(ctx, log, app.Name, func(engine.ManagedContainer) bool { return true })
	images, err := d.engine.AppImages(ctx, app.Name)
	if err != nil {
		log.Warn("deleting: listing the app's images", "err", err)
	}
	for _, img := range images {
		refs := img.Tags
		if len(refs) == 0 {
			refs = []string{img.ID}
		}
		for _, ref := range refs {
			if err := d.engine.RemoveImage(ctx, ref); err != nil {
				log.Warn("deleting: removing an image", "image", ref, "err", err)
			}
		}
	}
	if deleted.HadDatabase {
		if err := d.engine.RemoveNetwork(ctx, DatabaseName(app.Name)); err != nil {
			log.Warn("deleting: removing the database's network", "err", err)
		}
	}
	log.Info("app deleted", "database_volume_kept", deleted.HadDatabase)
	return deleted, nil
}

// removeContainers removes the app's containers that match, logging any it
// can't (the reconciler removes them later).
func (d *Deployer) removeContainers(ctx context.Context, log *slog.Logger, app string, match func(engine.ManagedContainer) bool) {
	containers, err := d.engine.ManagedContainers(ctx, app)
	if err != nil {
		log.Warn("listing the app's containers", "err", err)
		return
	}
	for _, c := range containers {
		if !match(c) {
			continue
		}
		if err := d.engine.Remove(ctx, c.ID); err != nil {
			log.Warn("removing a container", "container", c.Name, "err", err)
		}
	}
}

// isAppContainer: a deployment's container, not its database's or a
// backup's (no role label: from before roles existed).
func isAppContainer(c engine.ManagedContainer) bool {
	role := c.Labels[engine.LabelRole]
	return role == "" || role == engine.RoleApp
}
