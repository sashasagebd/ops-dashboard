// Command dashboard serves the ops dashboard API.
package main

import (
	"context"
	"errors"
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

	srv := &http.Server{
		Addr:    addr,
		Handler: server.New(notImplementedLister{}),
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

// notImplementedLister stands in until the real Docker client lands in
// milestone step 1.3. Until then /api/containers returns 502.
type notImplementedLister struct{}

func (notImplementedLister) ListContainers(context.Context) ([]docker.Container, error) {
	return nil, errors.New("docker client not implemented yet")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
