package engine

import (
	"context"
	"fmt"
	"maps"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// RoleBackup labels a backup's container: a one-off task, run until it
// exits and then removed, which the reconciler leaves to its job.
const RoleBackup = "backup"

// TaskSpec is a one-off container: it runs a command to completion. Backups
// are the only one so far.
type TaskSpec struct {
	Name    string
	Image   string
	Cmd     []string
	Env     map[string]string
	Labels  map[string]string // LabelManaged is added
	Network string            // the only network it joins
	User    string            // "uid:gid" to run as; the image's own user if ""
	// A named volume to mount (never a host path), at VolumeTarget.
	Volume       string
	VolumeTarget string
	MemoryBytes  int64
}

// RunTask creates and starts a one-off container, returning its ID; Wait
// then waits for it to exit. Hardened like an app's, with no restart policy
// at all: it's meant to stop.
func (c *Client) RunTask(ctx context.Context, spec TaskSpec) (string, error) {
	labels := maps.Clone(spec.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[LabelManaged] = "true"

	pids := int64(pidsLimit)
	hc := &container.HostConfig{
		NetworkMode: container.NetworkMode(spec.Network),
		Resources: container.Resources{
			Memory:    spec.MemoryBytes,
			PidsLimit: &pids,
		},
		SecurityOpt: []string{"no-new-privileges:true"},
		LogConfig: container.LogConfig{
			Type:   "json-file",
			Config: map[string]string{"max-size": "10m", "max-file": "3"},
		},
	}
	if spec.Volume != "" {
		hc.Mounts = []mount.Mount{{Type: mount.TypeVolume, Source: spec.Volume, Target: spec.VolumeTarget}}
	}
	created, err := c.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: spec.Name,
		Config: &container.Config{
			Image:  spec.Image,
			Cmd:    spec.Cmd,
			Env:    envList(spec.Env),
			Labels: labels,
			User:   spec.User,
		},
		HostConfig: hc,
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{spec.Network: {}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("creating task container: %w", err)
	}
	if _, err := c.api.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		_ = c.Remove(context.WithoutCancel(ctx), created.ID)
		return "", fmt.Errorf("starting task container: %w", err)
	}
	return created.ID, nil
}

// Wait waits for a container to stop and returns its exit code. One that has
// stopped already returns at once.
func (c *Client) Wait(ctx context.Context, id string) (int, error) {
	res := c.api.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case r := <-res.Result:
		if r.Error != nil && r.Error.Message != "" {
			return 0, fmt.Errorf("waiting for container: %s", r.Error.Message)
		}
		return int(r.StatusCode), nil
	case err := <-res.Error:
		return 0, fmt.Errorf("waiting for container: %w", err)
	}
}
