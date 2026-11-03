package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// Labels on every container the platform creates. They're how the platform
// (and P4's reconciler) tells its own containers apart from everything else
// on the host, such as the game servers, which it must never touch.
const (
	LabelManaged    = "ops.platform.managed" // always "true"
	LabelApp        = "ops.platform.app"     // the app's name
	LabelAppID      = "ops.platform.app-id"
	LabelDeployment = "ops.platform.deployment" // the deployment's ID
)

// RunSpec is what the platform runs for one deployment of an app.
type RunSpec struct {
	Name        string // container name, unique per deployment
	Image       string
	Env         map[string]string
	Labels      map[string]string // LabelManaged is added automatically
	Network     string            // the only network it joins
	MemoryBytes int64
	NanoCPUs    int64 // 1e9 = one core
}

// Limits every app container gets regardless of its spec.
const (
	// pidsLimit caps processes, so a fork bomb (or a leak) in one app can't
	// exhaust the host's PIDs.
	pidsLimit = 512
	// stopTimeoutSeconds is how long an app gets to shut down after SIGTERM.
	stopTimeoutSeconds = 10
)

// Run creates and starts an app container, returning its ID. If it was
// created but couldn't start, it's removed again, so a failed Run leaves
// nothing behind.
//
// Apart from the spec, the config is fixed here, so no caller can ask for
// anything the platform shouldn't run (privileged mode, host mounts,
// published ports). This is the main protection that the socket proxy
// can't give.
func (c *Client) Run(ctx context.Context, spec RunSpec) (string, error) {
	labels := maps.Clone(spec.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[LabelManaged] = "true"

	pids := int64(pidsLimit)
	stopTimeout := stopTimeoutSeconds
	created, err := c.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: spec.Name,
		Config: &container.Config{
			Image:       spec.Image,
			Env:         envList(spec.Env),
			Labels:      labels,
			StopTimeout: &stopTimeout,
		},
		HostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(spec.Network),
			// Docker restarts a crashed app until P4's reconciler exists.
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			Resources: container.Resources{
				Memory:    spec.MemoryBytes,
				NanoCPUs:  spec.NanoCPUs,
				PidsLimit: &pids,
			},
			// setuid binaries can't gain privileges inside the container.
			SecurityOpt: []string{"no-new-privileges:true"},
			// Docker's default json-file log grows without limit; an app
			// that logs heavily could otherwise fill the server's disk.
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
		return "", fmt.Errorf("creating container: %w", err)
	}

	if _, err := c.api.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		// Best effort: the start error is the one worth reporting.
		_ = c.Remove(context.WithoutCancel(ctx), created.ID)
		return "", fmt.Errorf("starting container: %w", err)
	}
	return created.ID, nil
}

// Remove stops a container (SIGTERM, then SIGKILL after its stop timeout) and
// removes it. A container that's already gone isn't an error, so Remove is
// safe to repeat, e.g. when a deploy is retried after a crash.
func (c *Client) Remove(ctx context.Context, id string) error {
	if _, err := c.api.ContainerStop(ctx, id, client.ContainerStopOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("stopping container: %w", err)
	}
	if _, err := c.api.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("removing container: %w", err)
	}
	return nil
}

// ManagedContainer is one of the platform's containers, as Docker reports it.
type ManagedContainer struct {
	ID     string
	Name   string
	State  string // e.g. "running", "exited"
	Labels map[string]string
}

// ManagedContainers lists the platform's containers (running or not) for
// app, or for every app if app is "". It filters by the platform's label on
// Docker's side, so other containers on the host never even reach the
// platform.
func (c *Client) ManagedContainers(ctx context.Context, app string) ([]ManagedContainer, error) {
	filters := client.Filters{}.Add("label", LabelManaged+"=true")
	if app != "" {
		filters = filters.Add("label", LabelApp+"="+app)
	}
	res, err := c.api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	out := make([]ManagedContainer, 0, len(res.Items))
	for _, s := range res.Items {
		name := s.ID
		if len(s.Names) > 0 {
			name = s.Names[0][1:] // Docker prefixes names with "/"
		}
		out = append(out, ManagedContainer{ID: s.ID, Name: name, State: string(s.State), Labels: s.Labels})
	}
	return out, nil
}

// envList turns env vars into Docker's KEY=value form, sorted so the same
// env always gives the same container config.
func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, k+"="+env[k])
	}
	return out
}
