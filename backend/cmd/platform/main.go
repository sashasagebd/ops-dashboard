// Command platform runs the deploy platform's control plane: the API for
// registering and deploying apps, the job workers that run the deploys, and
// Caddy's routes. The reconciler comes in a later phase (see
// docs/platform-plan.md and docs/roadmap.md). It's a separate service from the
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
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sashasagebd/ops-dashboard/backend/internal/api"
	"github.com/sashasagebd/ops-dashboard/backend/internal/backup"
	"github.com/sashasagebd/ops-dashboard/backend/internal/deploy"
	"github.com/sashasagebd/ops-dashboard/backend/internal/engine"
	"github.com/sashasagebd/ops-dashboard/backend/internal/healthcheck"
	"github.com/sashasagebd/ops-dashboard/backend/internal/jobs"
	"github.com/sashasagebd/ops-dashboard/backend/internal/metrics"
	"github.com/sashasagebd/ops-dashboard/backend/internal/reconcile"
	"github.com/sashasagebd/ops-dashboard/backend/internal/router"
	"github.com/sashasagebd/ops-dashboard/backend/internal/secrets"
	"github.com/sashasagebd/ops-dashboard/backend/internal/source"
	"github.com/sashasagebd/ops-dashboard/backend/internal/store"
	"github.com/sashasagebd/ops-dashboard/backend/internal/webhook"
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
	// Apps get the hostname <name>.<APPS_DOMAIN>: for now a sslip.io name
	// for the server's Tailscale IP, later a real domain.
	appsDomain := os.Getenv("APPS_DOMAIN")
	if appsDomain == "" {
		return errors.New("APPS_DOMAIN is required (e.g. 100-101-102-103.sslip.io)")
	}
	// Push-to-deploy is off unless the GitHub webhook's secret is set: then
	// a second listener serves the webhook (see below). The secret is all
	// that stands between that public endpoint and deploying, so a short
	// one is refused rather than used.
	webhookSecret := os.Getenv("GITHUB_WEBHOOK_SECRET")
	if webhookSecret != "" && len(webhookSecret) < 32 {
		return errors.New("GITHUB_WEBHOOK_SECRET: use at least 32 characters (e.g. openssl rand -hex 32)")
	}
	// Public hostnames, through Cloudflare's tunnel: apps made public are
	// served at <public name>.<PUBLIC_DOMAIN>, and the webhook at
	// hooks.<PUBLIC_DOMAIN>. Off unless set.
	publicDomain := strings.ToLower(strings.TrimSuffix(os.Getenv("PUBLIC_DOMAIN"), "."))
	if publicDomain != "" && publicDomain == strings.ToLower(appsDomain) {
		// A public and a tailnet hostname could then be the same name, on
		// two Caddy servers that must never share one.
		return errors.New("PUBLIC_DOMAIN must differ from APPS_DOMAIN")
	}
	// The webhook's listener as Caddy reaches it, for hooks.<PUBLIC_DOMAIN>.
	var webhookUpstream string
	if webhookSecret != "" {
		webhookUpstream = envOr("WEBHOOK_UPSTREAM", "platform:8082")
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

	github := source.NewGitHub()
	github.Token = os.Getenv("GITHUB_TOKEN") // optional
	// Caddy's admin API and proxy port, on the internal network only the
	// platform shares with it (see compose.yaml).
	caddy := router.New(
		envOr("CADDY_ADMIN_URL", "http://10.203.0.2:2019"),
		envOr("CADDY_PROXY_URL", "http://10.203.0.2:80"),
	)
	// The tunnel network's subnet (compose's tunnel network): Caddy's
	// tailnet server refuses requests from it.
	caddy.TunnelSubnet = os.Getenv("TUNNEL_SUBNET")
	deployer := deploy.New(deploy.Config{
		AppsNetwork:    envOr("APPS_NETWORK", "platform-apps"),
		DeployTimeout:  20 * time.Minute,
		HealthTimeout:  time.Minute,
		HealthInterval: time.Second,
		DrainTimeout:   15 * time.Second,
		DrainInterval:  250 * time.Millisecond,

		PublicDomain:    publicDomain,
		WebhookUpstream: webhookUpstream,
	}, st, github, dockerClient, caddy)
	if publicDomain == "" {
		slog.Info("public hostnames are off: PUBLIC_DOMAIN isn't set")
	}
	// Backups of app databases, on when compose gives the platform the backup
	// volume (BACKUP_VOLUME; the host directory behind it is BACKUP_DIR). It's
	// mounted read-only at /backups here too, to check each dump landed.
	var backuper *backup.Backuper
	if volume := os.Getenv("BACKUP_VOLUME"); volume != "" {
		backuper = backup.New(backup.Config{
			Volume: volume,
			Dir:    envOr("BACKUP_MOUNT", "/backups"),
			// The owner's uid:gid, so the dumps in ~/backups are theirs.
			User:       envOr("BACKUP_USER", "1000:1000"),
			Keep:       7,
			Every:      24 * time.Hour,
			CheckEvery: 10 * time.Minute,
			Timeout:    30 * time.Minute,
		}, st, dockerClient)
	} else {
		slog.Info("backups are off: BACKUP_VOLUME isn't set")
	}
	// Apps' request numbers, read from Caddy every minute, 24h kept in
	// memory.
	requests := metrics.New(caddy, time.Minute, 24*time.Hour)
	// Counted per app: its tailnet and public hostnames together.
	requests.Group = func(ctx context.Context) (map[string]string, error) {
		apps, err := st.Apps(ctx)
		if err != nil {
			return nil, err
		}
		group := make(map[string]string, 2*len(apps))
		for _, a := range apps {
			group[a.Hostname] = a.Name
			if host := deploy.PublicHost(a.PublicName, publicDomain); host != "" {
				group[host] = a.Name
			}
		}
		return group, nil
	}

	handlers := jobs.Kinds{"deploy": deployer}
	if backuper != nil {
		handlers["backup"] = backuper
	}

	runner := jobs.New(jobs.Config{
		// Two deploys (of different apps) at once: builds are heavy, and
		// the game servers share these 4 cores.
		Workers:     2,
		MaxAttempts: 3,
		// A crashed platform's deploy is picked up again within 30s of it
		// coming back.
		Lease:        30 * time.Second,
		PollInterval: 5 * time.Second,
		RetryDelay:   15 * time.Second,
	}, st, handlers)
	deployer.Notify = runner.Wake
	// The API's view of backups: nil, not a nil *Backuper, when they're off.
	var backups api.Backups
	if backuper != nil {
		backuper.Notify = runner.Wake
		backups = backuper
	}
	reconciler := reconcile.New(reconcile.Config{
		Interval:    10 * time.Second,
		MinBackoff:  10 * time.Second,
		MaxBackoff:  5 * time.Minute,
		StableAfter: 5 * time.Minute,
		KeepEvents:  30 * 24 * time.Hour,
		OutputLines: 20,
	}, st, dockerClient, deployer)

	// The reconciler's first pass brings things back in line after the
	// platform was down (or the whole server restarted): it starts stopped
	// apps, re-applies Caddy's routes if they were lost, and removes
	// containers left by deploys that were cut off. Then the job workers
	// start, resuming any deploy the platform was in the middle of: only
	// afterwards, so the cleanup can't remove a container a resumed deploy
	// has just started. In the background, retrying until Docker and Caddy
	// answer: neither should stop the API from starting, and deploys queue
	// up meanwhile. After that, a pass every 10s.
	workersDone := make(chan struct{})
	go func() {
		defer close(workersDone)
		firstPass(ctx, reconciler)
		// Both stop when ctx is done; waited for, so neither is still
		// using the database when it closes.
		var wg sync.WaitGroup
		wg.Go(func() { reconciler.Run(ctx) })
		if backuper != nil {
			wg.Go(func() { backuper.RunScheduler(ctx) })
		}
		wg.Go(func() { requests.Run(ctx) })
		runner.Run(ctx) // returns at once if ctx is already done
		wg.Wait()
	}()

	// Only listen once the database is ready, so /healthz passing means the
	// platform started up fully.
	srv := &http.Server{
		Addr:    addr,
		Handler: newHandler(api.New(st, deployer, dockerClient, backups, requests, appsDomain, publicDomain)),
		// Without this, a client that sends headers very slowly can hold a
		// connection open forever (Slowloris).
		ReadHeaderTimeout: 5 * time.Second,
		// Requests end when shutdown starts. Shutdown waits for handlers to
		// return, and a log stream (Server-Sent Events) otherwise only
		// returns when the browser leaves, so an open log viewer would
		// hold up every restart until the shutdown timeout.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	servers := []*http.Server{srv}

	// The GitHub webhook, on a listener of its own: it's the one endpoint
	// reachable from the internet (compose publishes this port on 127.0.0.1
	// for Tailscale Funnel), so nothing else is served on it, and it gets
	// tighter limits than the API: a whole delivery must arrive within 15s.
	if webhookSecret != "" {
		servers = append(servers, &http.Server{
			Addr:              envOr("WEBHOOK_LISTEN_ADDR", ":8082"),
			Handler:           webhook.New(webhookSecret, st, deployer),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      15 * time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    16 << 10,
			BaseContext:       func(net.Listener) context.Context { return ctx },
		})
	} else {
		slog.Info("push-to-deploy is off: GITHUB_WEBHOOK_SECRET isn't set")
	}

	errCh := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			slog.Info("listening", "addr", s.Addr)
			errCh <- s.ListenAndServe()
		}()
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		if serr := s.Shutdown(shutdownCtx); serr != nil && err == nil {
			err = serr
		}
	}
	// ctx is cancelled, so deploys in flight are stopping; wait for their
	// jobs to be handed back before the database closes.
	<-workersDone
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// firstPass makes the reconciler's first pass, retrying every 5s until it
// succeeds or the platform shuts down.
func firstPass(ctx context.Context, r *reconcile.Reconciler) {
	for {
		err := r.Pass(ctx)
		if err == nil {
			slog.Info("first reconcile pass done: apps started, routes applied, orphans removed")
			return
		}
		slog.Warn("first reconcile pass failed; retrying in 5s", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
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

// newHandler returns the platform's HTTP routes: its health check, and the
// API (internal/api) under /api/platform/.
func newHandler(apiHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.Handle("/api/platform/", apiHandler)
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
