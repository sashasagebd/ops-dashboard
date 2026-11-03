// Package engine is the platform's client for the Docker Engine API: building
// images and running app containers, through the platform's own socket proxy
// (docker-proxy-rw). It uses the official SDK, unlike the dashboard's small
// read-only client in internal/docker, because builds, container configs and
// log streams are a lot to hand-write (see docs/decisions/platform.md).
//
// Callers declare the interfaces they need where they use them, so platform
// logic is tested against fakes; this package itself is tested against
// httptest servers, never a real daemon.
package engine

import (
	"context"
	"fmt"
	"net/url"

	"github.com/moby/moby/client"
)

// Client wraps the SDK's client. Methods are added as the deploy flow needs
// them, so the platform's use of Docker stays a short, reviewable list.
type Client struct {
	api *client.Client
}

// New returns a Client for host, in DOCKER_HOST form
// ("tcp://docker-proxy-rw:2375").
//
// Only tcp:// is accepted, as in internal/docker: unix:// would mean the raw
// socket, which this project never mounts, and failing loudly beats quietly
// bypassing the proxy. There's no client-wide timeout either, because builds
// legitimately stream for minutes; every call takes a context with its own
// deadline instead.
func New(host string) (*Client, error) {
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("parsing docker host %q: %w", host, err)
	}
	if u.Scheme != "tcp" || u.Host == "" {
		return nil, fmt.Errorf("docker host %q: want tcp://host:port (the socket proxy)", host)
	}
	// The SDK negotiates the API version with the daemon on first use, so
	// this works across Docker upgrades on the server.
	api, err := client.New(client.WithHost(host))
	if err != nil {
		return nil, err
	}
	return &Client{api: api}, nil
}

// Version returns the Docker daemon's version and the API version the client
// negotiated with it, e.g. "29.0.1" and "1.52".
func (c *Client) Version(ctx context.Context) (daemon, api string, err error) {
	v, err := c.api.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return "", "", err
	}
	return v.Version, c.api.ClientVersion(), nil
}

// Close releases the client's idle connections.
func (c *Client) Close() error {
	return c.api.Close()
}
