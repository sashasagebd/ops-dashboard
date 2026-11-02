// Command platform runs the deploy platform's control plane: for now, its
// database and a health endpoint; the API, job workers and reconciler come in
// later phases (see docs/platform-plan.md). It's a separate service from the
// dashboard so that monitoring keeps working when the platform doesn't, and
// so the dashboard never needs write access to Docker (see
// docs/decisions/platform.md).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/healthcheck"
	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
)

func main() {
	checkHealth := flag.Bool("healthcheck", false, "check that a running platform is serving, then exit (for Docker's HEALTHCHECK)")
	flag.Parse()

	// 8081, not the dashboard's 8080, so both can run side by side in
	// development. In Compose it's only reachable on internal networks.
	addr := envOr("LISTEN_ADDR", ":8081")

	if *checkHealth {
		if err := healthcheck.Check(addr); err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			os.Exit(1)
		}
		return
	}

	if err := run(addr); err != nil {
		slog.Error("platform exited", "err", err)
		os.Exit(1)
	}
}

func run(addr string) error {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}

	// docker stop sends SIGTERM; Ctrl+C sends SIGINT.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(databaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := waitForDatabase(ctx, st.Ping, time.Second, 30*time.Second); err != nil {
		return err
	}
	// A failed migration is a bug, not a blip, so it isn't retried here: the
	// process exits and the error is in the log. (The restart policy will
	// try again, which is harmless, since a failed migration leaves nothing
	// behind.)
	applied, err := st.Migrate(ctx)
	if err != nil {
		return fmt.Errorf("migrating the database: %w", err)
	}
	slog.Info("database schema up to date", "applied", applied)

	// Only listen once the database is ready, so /healthz passing means the
	// platform started up fully.
	srv := &http.Server{
		Addr:    addr,
		Handler: newHandler(),
		// Without this, a client that sends headers very slowly can hold a
		// connection open forever (Slowloris).
		ReadHeaderTimeout: 5 * time.Second,
	}

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

// waitForDatabase calls ping until it succeeds, waiting first, then twice as
// long after each failure, up to maxDelay, between attempts.
//
// After a reboot, Docker starts containers in no particular order (Compose's
// depends_on only applies to `docker compose up`), so the platform often
// starts before Postgres is accepting connections. Waiting here, rather than
// exiting and leaving it to the restart policy, keeps the container from
// crash-looping and the log to one line per attempt.
func waitForDatabase(ctx context.Context, ping func(context.Context) error, first, maxDelay time.Duration) error {
	delay := first
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := ping(pingCtx)
		cancel()
		if err == nil {
			return nil
		}
		slog.Warn("database not reachable yet", "err", err, "retryIn", delay)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, maxDelay)
	}
}

// newHandler returns the platform's HTTP routes. P1 adds the API; it will
// move to its own package then.
func newHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	return mux
}

// handleHealthz reports that the platform is up. The server only starts
// listening once Postgres has answered and the schema is migrated, so this
// doesn't ping the database on every check: like the dashboard's, it's a
// liveness check, and a database blip shouldn't mark the platform unhealthy.
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
