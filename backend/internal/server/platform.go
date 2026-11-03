package server

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// NewPlatformProxy forwards requests to the platform's API at target (e.g.
// http://platform:8081), unchanged: the platform serves the same
// /api/platform/ paths.
//
// The browser only ever talks to the dashboard, so the platform needs no
// published port, no CORS and no address of its own on the tailnet. The
// dashboard adds nothing but forwarding (and, in New, the cross-origin
// check); the platform does all the validating.
func NewPlatformProxy(target *url.URL) http.Handler {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Every platform request answers quickly (a deploy returns 202 and runs
	// in the background), so a slow answer means it's stuck.
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &httputil.ReverseProxy{
		// Rewrite rather than Director: it drops any X-Forwarded-* headers
		// the client sent, so they can't be spoofed through to the platform.
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Error("forwarding to the platform", "method", r.Method, "path", r.URL.Path, "err", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the platform isn't reachable"})
		},
	}
}
