// Command hello is the smallest app worth deploying on the platform: it
// answers on port 8080, has a health check, and shows an env var, so a deploy
// can be checked end to end (code fetched from a subdirectory, built, given
// its env vars, health-checked and routed).
//
// It also behaves the way the platform hopes apps will when it replaces
// them: on SIGTERM it stops taking new connections and finishes the requests
// it has before exiting. And it can be made to fail on purpose (BROKEN=1),
// or to answer slowly (/slow), to show a deploy that never goes live and a
// request that outlives the switch to a new version.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	greeting := os.Getenv("GREETING")
	if greeting == "" {
		greeting = "Hello"
	}
	// BROKEN=1 fails the health check, so a deploy of it never goes live and
	// the previous version keeps serving.
	broken := os.Getenv("BROKEN") == "1"
	started := time.Now()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s from the platform!\nServed by %s, up %s.\n",
			greeting, hostname(), time.Since(started).Round(time.Second))
	})
	// The platform's health check (healthCheckPath /healthz).
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if broken {
			http.Error(w, "broken on purpose (BROKEN=1)", http.StatusInternalServerError)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	// /slow?seconds=N answers after N seconds (at most 30), and says which
	// container answered, e.g. to check that a request started before a
	// deploy's switch still finishes on the old version.
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		seconds, err := strconv.Atoi(r.URL.Query().Get("seconds"))
		if err != nil || seconds < 0 || seconds > 30 {
			http.Error(w, "seconds must be 0 to 30", http.StatusBadRequest)
			return
		}
		select {
		case <-time.After(time.Duration(seconds) * time.Second):
			fmt.Fprintf(w, "Slow answer from %s after %ds.\n", hostname(), seconds)
		case <-r.Context().Done():
		}
	})

	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	// Docker stops a container with SIGTERM, then SIGKILL after the stop
	// timeout (10s on the platform). Shutting down within that lets requests
	// in progress finish instead of being cut off.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	shutDown := make(chan struct{})
	go func() {
		defer close(shutDown)
		<-ctx.Done()
		slog.Info("shutting down; finishing requests in progress")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown", "err", err)
		}
	}()

	slog.Info("listening", "addr", srv.Addr, "broken", broken)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		slog.Error("hello exited", "err", err)
		os.Exit(1)
	}
	// ListenAndServe returns as soon as Shutdown starts; the requests in
	// progress are finished once Shutdown returns.
	<-shutDown
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
