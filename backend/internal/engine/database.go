package engine

import (
	"context"
	"fmt"
	"maps"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// An app's own Postgres runs as a container the platform manages, like the
// app's: labelled, hardened, restarted by the reconciler. These are the
// pieces it needs beyond what apps do: a network of its own, pulling a
// public image, and a container with a data volume.

// LabelRole tells an app's containers apart: RoleDatabase for its database,
// and no label (containers from before databases) or RoleApp for a
// deployment's.
const (
	LabelRole    = "ops.platform.role"
	RoleApp      = "app"
	RoleDatabase = "database"
)

// EnsureNetwork creates an internal bridge network called name, with labels
// (plus LabelManaged), unless one exists already. Internal: containers on it
// reach each other and nothing else, not even the internet.
func (c *Client) EnsureNetwork(ctx context.Context, name string, labels map[string]string) error {
	_, err := c.api.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err == nil {
		return nil
	}
	if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspecting network: %w", err)
	}
	l := maps.Clone(labels)
	if l == nil {
		l = map[string]string{}
	}
	l[LabelManaged] = "true"
	_, err = c.api.NetworkCreate(ctx, name, client.NetworkCreateOptions{Driver: "bridge", Internal: true, Labels: l})
	// Created meanwhile by someone else (a deploy and the reconciler at once):
	// it exists, which is all that was asked.
	if err != nil && !cerrdefs.IsConflict(err) {
		return fmt.Errorf("creating network: %w", err)
	}
	return nil
}

// Pull pulls ref (a public image, pinned by tag) unless Docker has it
// already, waiting for the pull to finish.
func (c *Client) Pull(ctx context.Context, ref string) error {
	if ok, err := c.ImageExists(ctx, ref); err != nil || ok {
		return err
	}
	res, err := c.api.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pulling %s: %w", ref, err)
	}
	if err := res.Wait(ctx); err != nil {
		return fmt.Errorf("pulling %s: %w", ref, err)
	}
	return nil
}

// DatabaseSpec is an app's database container.
type DatabaseSpec struct {
	Name        string // the container's name, also its hostname on Network
	Image       string // e.g. postgres:18.6
	Env         map[string]string
	Labels      map[string]string // LabelManaged and LabelRole are added
	Network     string            // the app's database network, the only one it joins
	Volume      string            // a named volume for its data; created by Docker if missing
	MemoryBytes int64
}

// dataDir is where the official Postgres 18 image wants its volume: it keeps
// data under /var/lib/postgresql/18/docker and refuses a volume at the older
// /var/lib/postgresql/data.
const dataDir = "/var/lib/postgresql"

// RunDatabase creates and starts a database container, returning its ID.
// As with Run, the config is fixed here apart from the spec. The one thing
// it allows that app containers never get is a mount, and only of a named
// volume (never a host path) at Postgres's data directory. Docker creates
// the volume on first use, so this needs no permission for volumes; and the
// platform never removes one, so an app's data outlives its container.
func (c *Client) RunDatabase(ctx context.Context, spec DatabaseSpec) (string, error) {
	labels := maps.Clone(spec.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[LabelManaged] = "true"
	labels[LabelRole] = RoleDatabase

	pids := int64(pidsLimit)
	// Postgres's fast shutdown can take a few seconds; a killed Postgres has
	// to recover on its next start.
	stopTimeout := 30
	created, err := c.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: spec.Name,
		Config: &container.Config{
			Image:       spec.Image,
			Env:         envList(spec.Env),
			Labels:      labels,
			StopTimeout: &stopTimeout,
		},
		HostConfig: &container.HostConfig{
			NetworkMode:   container.NetworkMode(spec.Network),
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled}, // the reconciler restarts it
			Mounts:        []mount.Mount{{Type: mount.TypeVolume, Source: spec.Volume, Target: dataDir}},
			Resources: container.Resources{
				Memory:    spec.MemoryBytes,
				PidsLimit: &pids,
			},
			SecurityOpt: []string{"no-new-privileges:true"},
			LogConfig: container.LogConfig{
				Type:   "json-file",
				Config: map[string]string{"max-size": "10m", "max-file": "3"},
			},
		},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{spec.Network: {}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("creating database container: %w", err)
	}
	if _, err := c.api.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		_ = c.Remove(context.WithoutCancel(ctx), created.ID)
		return "", fmt.Errorf("starting database container: %w", err)
	}
	return created.ID, nil
}
