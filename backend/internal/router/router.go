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
