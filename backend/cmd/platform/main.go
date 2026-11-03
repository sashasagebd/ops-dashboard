// Command platform runs the deploy platform's control plane: for now, its
// database, its Docker client and a health endpoint; deploys, the API, job
// workers and the reconciler come in later steps (see docs/platform-plan.md
// and docs/roadmap.md). It's a separate service from the
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

	"github.com/sashasagebd/ops-dashboard/backend/internal/engine"
	"github.com/sashasagebd/ops-dashboard/backend/internal/healthcheck"
	"github.com/sashasagebd/ops-dashboard/backend/internal/secrets"
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

	// Encrypts app env vars at rest. Required, and never defaulted: a
	// generated key would be lost on restart, along with every value
	// encrypted under it.
	key, err := secrets.ParseKey(os.Getenv("PLATFORM_SECRET_KEY"))
	if err != nil {
		return fmt.Errorf("PLATFORM_SECRET_KEY: %w", err)
	}
	box, err := secrets.New(key)
	if err != nil {
		return err
	}

	st, err := store.Open(databaseURL, box)
	if err != nil {
		return err
	}
	defer st.Close()

	// The platform's own proxy, which allows writes; never the dashboard's
	// read-only one. The default matches the service name in compose.yaml.
	dockerClient, err := engine.New(envOr("DOCKER_HOST", "tcp://docker-proxy-rw:2375"))
	if err != nil {
		return err
	}
	defer dockerClient.Close()
	// Best effort, in the background: a log line confirming Docker is
	// reachable (and which version), without delaying startup or failing it.
	// Docker being down shouldn't stop the API from serving what's in the
	// database.
	go logDockerVersion(ctx, dockerClient)

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

	// Nothing is deploying yet at this point, so any deployment still marked
	// in progress was cut off by the platform stopping. In P1 nothing can
	// resume it, so it's failed (which also unblocks the app's next deploy).
	// P2's job queue retries it instead.
	interrupted, err := st.FailInterruptedDeployments(ctx, "interrupted: the platform restarted during this deploy")
	if err != nil {
		return fmt.Errorf("clearing interrupted deployments: %w", err)
	}
	if interrupted > 0 {
		slog.Warn("marked interrupted deployments failed", "count", interrupted)
	}

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

func logDockerVersion(ctx context.Context, c *engine.Client) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	daemon, api, err := c.Version(ctx)
	if err != nil {
		slog.Warn("docker not reachable", "err", err)
		return
	}
	slog.Info("docker reachable", "version", daemon, "apiVersion", api)
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
