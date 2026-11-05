// Package router configures Caddy, which receives all traffic for deployed
// apps, through Caddy's admin API.
//
// It's declarative: Apply takes the complete list of routes that should
// exist and replaces Caddy's list with it in one request. There's no
// add/remove bookkeeping that could drift out of step with Caddy (after a
// failed request, a Caddy restart, or a platform crash), and applying the
// same list twice is harmless, so the platform can simply re-apply whenever
// it isn't sure.
package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Route sends requests for Host to Upstream (a container name and port on
// the apps network, e.g. "app-hello-7:8080").
type Route struct {
	Host     string
	Upstream string
}

// Caddy talks to Caddy's admin API and, for health checks, to its proxy port.
type Caddy struct {
	AdminURL   string // e.g. http://10.203.0.2:2019
	ProxyURL   string // e.g. http://10.203.0.2:80
	HTTPClient *http.Client
}

// New returns a Caddy client. Requests time out after 10s: admin calls are
// small, and a health probe that takes longer counts as unhealthy.
func New(adminURL, proxyURL string) *Caddy {
	return &Caddy{AdminURL: adminURL, ProxyURL: proxyURL, HTTPClient: &http.Client{Timeout: 10 * time.Second}}
}

// The server in Caddy's config (caddy/caddy.json) that the platform owns
// outright: Apply replaces the whole object, not just its routes, so it
// doesn't depend on a routes array already existing (Caddy may drop an empty
// one when it saves its config). Nothing else edits it.
const serverPath = "/config/apps/http/servers/apps"

// listenAddr is where Caddy accepts app traffic inside its container.
const listenAddr = ":80"

// Apply makes routes Caddy's complete routing table, replacing whatever was
// there.
func (c *Caddy) Apply(ctx context.Context, routes []Route) error {
	body, err := json.Marshal(caddyServer{
		Listen: []string{listenAddr},
		Routes: caddyRoutes(routes),
		// Plain HTTP: inside the tailnet now, behind Cloudflare later, which
		// terminates HTTPS itself.
		AutomaticHTTPS: caddyAutoHTTPS{Disable: true},
	})
	if err != nil {
		return err
	}
	// PATCH replaces the value at the path (the server) in one operation:
	// Caddy swaps to the new config only if it's valid, so a request never
	// leaves routing half-updated.
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.AdminURL+serverPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("applying routes: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("applying routes: Caddy answered %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

// Probe sends one GET for path to host through Caddy, as a visitor would, and
// returns the status code. Following redirects is disabled: a 3xx is an
// answer from the app, which is enough to call it up.
func (c *Caddy) Probe(ctx context.Context, host, path string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ProxyURL+path, nil)
	if err != nil {
		return 0, err
	}
	req.Host = host
	client := *c.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}

// Routes returns the routes Caddy has now, in the platform's terms, for the
// reconciler to compare with what it should have. A route in any shape Apply
// doesn't produce (edited by hand, say) comes back with an empty Upstream, so
// it never matches a wanted route. If the platform's server is missing
// altogether (Caddy started without its saved config), that's no routes.
func (c *Caddy) Routes(ctx context.Context) ([]Route, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.AdminURL+serverPath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reading routes: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// Caddy answers 4xx for a path into config that doesn't exist.
		return []Route{}, nil
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("reading routes: Caddy answered %s", resp.Status)
	}
	var server *caddyServer // "null" if the server was removed
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&server); err != nil {
		return nil, fmt.Errorf("reading routes: %w", err)
	}
	out := []Route{}
	if server == nil {
		return out, nil
	}
	for _, r := range server.Routes {
		var route Route
		if len(r.Match) == 1 && len(r.Match[0].Host) == 1 {
			route.Host = r.Match[0].Host[0]
		}
		if len(r.Handle) == 1 && r.Handle[0].Handler == "reverse_proxy" && len(r.Handle[0].Upstreams) == 1 && r.Terminal {
			route.Upstream = r.Handle[0].Upstreams[0].Dial
		}
		out = append(out, route)
	}
	return out, nil
}

// metricsPath is Caddy's HTTP metrics setting. It belongs to Caddy's whole
// HTTP app, not the server the platform owns, but per-host metrics can only
// be turned on there (the per-server setting is deprecated): the one path
// outside its own server the platform writes (see docs/decisions/platform.md).
const metricsPath = "/config/apps/http/metrics"

// SyncMetrics makes sure Caddy counts requests per host, which is how the
// platform tells apps' traffic apart, changing the setting only if it's off
// (changed reports whether it did). Caddy only gives a host its own label if
// a route matches it, so a made-up Host header can't add new series.
func (c *Caddy) SyncMetrics(ctx context.Context) (changed bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.AdminURL+metricsPath, nil)
	if err != nil {
		return false, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("reading metrics config: %w", err)
	}
	var current *struct {
		PerHost bool `json:"per_host"`
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&current)
	case resp.StatusCode >= 500:
		err = fmt.Errorf("reading metrics config: Caddy answered %s", resp.Status)
	}
	resp.Body.Close()
	if err != nil {
		return false, err
	}
	if current != nil && current.PerHost {
		return false, nil
	}
	// POST sets the object whether or not it exists yet.
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, c.AdminURL+metricsPath,
		strings.NewReader(`{"per_host":true}`))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = c.HTTPClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("turning on per-host metrics: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return false, fmt.Errorf("turning on per-host metrics: Caddy answered %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	return true, nil
}

// Metrics returns Caddy's metrics, in Prometheus's text format, from its
// admin API.
func (c *Caddy) Metrics(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.AdminURL+"/metrics", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reading metrics: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reading metrics: Caddy answered %s", resp.Status)
	}
	// A few hosts' histograms are tens of KiB; the cap is for safety only.
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// InFlight returns how many requests Caddy is proxying to each upstream right
// now, by dial address (e.g. "app-hello-7:8080"); upstreams with none may be
// missing.
//
// It's what lets a deploy drain the old version: after a route change, Caddy
// lets requests the old config had already started finish against the old
// upstream (the PATCH returns as soon as the old config starts shutting down,
// without waiting for them), and its in-flight counts are kept per address
// across config changes, so they still show the old upstream after its route
// is gone.
func (c *Caddy) InFlight(ctx context.Context) (map[string]int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.AdminURL+"/reverse_proxy/upstreams", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reading upstreams: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reading upstreams: Caddy answered %s", resp.Status)
	}
	var upstreams []struct {
		Address     string `json:"address"`
		NumRequests int    `json:"num_requests"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&upstreams); err != nil {
		return nil, fmt.Errorf("reading upstreams: %w", err)
	}
	out := make(map[string]int, len(upstreams))
	for _, u := range upstreams {
		// Summed: Caddy can list one address from more than one pool.
		out[u.Address] += u.NumRequests
	}
	return out, nil
}

type caddyServer struct {
	Listen         []string       `json:"listen"`
	Routes         []caddyRoute   `json:"routes"`
	AutomaticHTTPS caddyAutoHTTPS `json:"automatic_https"`
}

type caddyAutoHTTPS struct {
	Disable bool `json:"disable"`
}

// caddyRoute is one entry in Caddy's JSON route list: match the Host header,
// reverse-proxy to the upstream, and stop there.
type caddyRoute struct {
	Match    []caddyMatch   `json:"match"`
	Handle   []caddyHandler `json:"handle"`
	Terminal bool           `json:"terminal"`
}

type caddyMatch struct {
	Host []string `json:"host"`
}

type caddyHandler struct {
	Handler   string          `json:"handler"`
	Upstreams []caddyUpstream `json:"upstreams"`
}

type caddyUpstream struct {
	Dial string `json:"dial"`
}

func caddyRoutes(routes []Route) []caddyRoute {
	out := make([]caddyRoute, 0, len(routes))
	for _, r := range routes {
		out = append(out, caddyRoute{
			Match:    []caddyMatch{{Host: []string{r.Host}}},
			Handle:   []caddyHandler{{Handler: "reverse_proxy", Upstreams: []caddyUpstream{{Dial: r.Upstream}}}},
			Terminal: true,
		})
	}
	return out
}
