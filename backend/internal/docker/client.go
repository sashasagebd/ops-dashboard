package docker

import (
	"context"
	"encoding/json"
	"errors"
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

// apiInspect is the subset of `GET /containers/{id}/json` we use. Docker
// reports times it doesn't have as "0001-01-01T00:00:00Z", which decodes to
// the zero time.Time.
type apiInspect struct {
	State struct {
		StartedAt  time.Time `json:"StartedAt"`
		FinishedAt time.Time `json:"FinishedAt"`
	} `json:"State"`
}

// ListContainers returns all containers, including stopped ones.
//
// The list endpoint has no exact start/stop times (only text like "Up 3
// hours"), so each container is also inspected. That's one extra request per
// container, which is fine for a home server's handful; they run one after
// another to keep this simple.
func (c *Client) ListContainers(ctx context.Context) ([]Container, error) {
	var raw []apiContainer
	if err := c.get(ctx, "/containers/json?all=true", &raw); err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	containers := make([]Container, 0, len(raw))
	for _, rc := range raw {
		var inspect apiInspect
		err := c.get(ctx, "/containers/"+url.PathEscape(rc.ID)+"/json", &inspect)
		if errors.Is(err, errNotFound) {
			// Removed between the list and the inspect; it's gone, so leave
			// it out rather than failing the whole list.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspecting container %s: %w", rc.ID, err)
		}

		containers = append(containers, Container{
			ID:         rc.ID,
			Name:       containerName(rc),
			Image:      rc.Image,
			State:      rc.State,
			Status:     rc.Status,
			StartedAt:  inspect.State.StartedAt,
			FinishedAt: inspect.State.FinishedAt,
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

// errNotFound is returned (wrapped) by get when Docker answers 404.
var errNotFound = errors.New("not found")

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

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("GET %s: %w", path, errNotFound)
	}
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
