package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to the Docker Engine API over HTTP, through the read-only
// socket proxy. It never touches /var/run/docker.sock itself.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient returns a Client for host, in DOCKER_HOST form
// ("tcp://docker-proxy:2375").
//
// Only tcp:// is accepted. unix:// would mean talking to the raw socket,
// which this project never does; failing loudly beats quietly bypassing the
// proxy.
func NewClient(host string) (*Client, error) {
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("parsing docker host %q: %w", host, err)
	}
	if u.Scheme != "tcp" || u.Host == "" {
		return nil, fmt.Errorf("docker host %q: want tcp://host:port (the socket proxy)", host)
	}
	return &Client{
		baseURL: "http://" + u.Host,
		// A safety net in case a caller forgets a deadline; callers' contexts
		// can still cancel sooner.
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// apiContainer is the subset of Docker's container list response we use.
// No API version is pinned in the path; these fields are the same in every
// version.
type apiContainer struct {
	ID     string   `json:"Id"`
	Names  []string `json:"Names"`
	Image  string   `json:"Image"`
	State  string   `json:"State"`
	Status string   `json:"Status"`
}

// ListContainers returns the running containers.
func (c *Client) ListContainers(ctx context.Context) ([]Container, error) {
	var raw []apiContainer
	if err := c.get(ctx, "/containers/json", &raw); err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	containers := make([]Container, 0, len(raw))
	for _, rc := range raw {
		containers = append(containers, Container{
			ID:     rc.ID,
			Name:   containerName(rc),
			Image:  rc.Image,
			State:  rc.State,
			Status: rc.Status,
		})
	}
	return containers, nil
}

// containerName returns the container's name without Docker's leading "/".
func containerName(rc apiContainer) string {
	if len(rc.Names) == 0 {
		return rc.ID
	}
	return strings.TrimPrefix(rc.Names[0], "/")
}

// get sends a GET request to path and decodes the JSON response into v.
func (c *Client) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Docker errors look like {"message": "..."}. The proxy answers 403
		// for endpoints it doesn't allow, which is worth seeing in the log.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("GET %s: decoding response: %w", path, err)
	}
	return nil
}
