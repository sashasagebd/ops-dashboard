// Command hello is the smallest app worth deploying on the platform: it
// answers on port 8080, has a health check, and shows an env var, so a deploy
// can be checked end to end (code fetched from a subdirectory, built, given
// its env vars, health-checked and routed).
package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"
)

func main() {
	greeting := os.Getenv("GREETING")
	if greeting == "" {
		greeting = "Hello"
	}
	started := time.Now()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s from the platform!\nServed by %s, up %s.\n",
			greeting, hostname(), time.Since(started).Round(time.Second))
	})
	// The platform's health check (healthCheckPath /healthz).
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	slog.Info("listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("hello exited", "err", err)
		os.Exit(1)
	}
}

// hostname is the container's name as Docker sets it (its short ID), which
// changes with each deploy.
func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
