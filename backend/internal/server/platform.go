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
	// Every platform request starts answering quickly (a deploy returns 202
	// and runs in the background; a log stream sends its headers at once),
	// so a slow start means it's stuck. It limits only the wait for the
	// headers, not how long a log stream's body can go on.
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &httputil.ReverseProxy{
		// Rewrite rather than Director: it drops any X-Forwarded-* headers
		// the client sent, so they can't be spoofed through to the platform.
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
		},
		Transport: transport,
		// Pass each write on to the browser at once, so log lines (Server-
		// Sent Events) arrive live instead of sitting in a buffer. The
		// proxy does this for text/event-stream anyway; set here so it
		// doesn't depend on that. The platform's other answers are small
		// single writes, so this costs nothing.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Error("forwarding to the platform", "method", r.Method, "path", r.URL.Path, "err", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the platform isn't reachable"})
		},
	}
}
