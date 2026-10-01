// Package server wires up the dashboard's HTTP routes.
package server

import "net/http"

// New returns the dashboard's HTTP handler.
func New() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	return mux
}

// handleHealthz reports that the process is up and serving requests.
// It deliberately doesn't check Docker: a liveness check that fails when a
// dependency is down would get the dashboard restarted for no reason.
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
}
