// Command dashboard serves the ops dashboard API.
package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/docker"
	"github.com/sashasagebd/ops-dashboard/backend/internal/server"
)

func main() {
	if err := run(); err != nil {
		slog.Error("dashboard exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	// Listens on all interfaces inside the container. Restricting access to
	// 127.0.0.1 happens in the Compose port mapping, on the host side.
	addr := envOr("LISTEN_ADDR", ":8080")

	// The default matches the proxy's service name in compose.yaml.
	dockerClient, err := docker.NewClient(envOr("DOCKER_HOST", "tcp://docker-proxy:2375"))
	if err != nil {
		return err
	}

	// The built frontend. Unset in development, where Vite serves it instead.
	var static fs.FS
	if dir := os.Getenv("STATIC_DIR"); dir != "" {
		static = os.DirFS(dir)
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: server.New(dockerClient, static),
		// Without this, a client that sends headers very slowly can hold a
		// connection open forever (Slowloris).
		ReadHeaderTimeout: 5 * time.Second,
	}

	// docker stop sends SIGTERM; Ctrl+C sends SIGINT.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
