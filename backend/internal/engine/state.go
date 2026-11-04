package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/moby/moby/client"
)

// What the reconciler (internal/reconcile) needs beyond running and removing
// containers: why one stopped, and starting it again.

// ExitInfo is how a stopped container ended.
type ExitInfo struct {
	ExitCode   int
	OOMKilled  bool      // killed for going over its memory limit
	FinishedAt time.Time // zero if Docker didn't say
}

// Exit returns how a container ended (meaningful once it has stopped).
func (c *Client) Exit(ctx context.Context, id string) (ExitInfo, error) {
	res, err := c.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return ExitInfo{}, fmt.Errorf("inspecting container: %w", err)
	}
	st := res.Container.State
	if st == nil {
		return ExitInfo{}, nil
	}
	info := ExitInfo{ExitCode: st.ExitCode, OOMKilled: st.OOMKilled}
	// Docker reports "0001-01-01T00:00:00Z" for a container that never
	// stopped; time.Parse turns that into the zero time anyway.
	if t, err := time.Parse(time.RFC3339Nano, st.FinishedAt); err == nil && t.Year() > 1 {
		info.FinishedAt = t
	}
	return info, nil
}

// Start starts an existing, stopped container again, with the config it was
// created with.
func (c *Client) Start(ctx context.Context, id string) error {
	if _, err := c.api.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("starting container: %w", err)
	}
	return nil
}
