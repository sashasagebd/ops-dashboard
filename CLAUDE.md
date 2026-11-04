# CLAUDE.md

Self-hosted monitoring dashboard for a home server, built as a portfolio
project. Go API + React/TS frontend, deployed with Docker Compose and reached
over Tailscale.

## Read first

- `docs/roadmap.md`: milestone plan, current status, pending decisions and open questions. Start here when resuming.
- `docs/spec.md`: v1 scope, out-of-scope list, constraints, server context.
- `docs/decisions/stack.md`: reasoning behind the main design choices.
- `docs/decisions/platform.md`: the platform's design log (what, why, alternatives). Add an entry for each significant platform decision.
- `docs/platform-plan.md`: the next big goal, evolving the dashboard into a self-hosted deploy platform (phases, design principles, ground rules). Read it before any platform work; where it conflicts with the constraints below, ask before changing either.

## How to work on this repo

- Work in small, reviewable steps: one roadmap step at a time, then stop for review.
- Propose a plan and get approval before writing code for a new milestone.
- Explain non-obvious decisions, in code comments where they'd help a reader, and in the end-of-step summary.
- End each step with: files changed, decisions, what was verified, what wasn't.
- Don't commit unless asked; the owner commits. Update `docs/roadmap.md` status when a step lands.
- Re-read a file right before editing it; the owner edits docs between turns.

## Layout

- `backend/`: Go module `github.com/sashasagebd/ops-dashboard/backend`
  - `cmd/dashboard/`: entrypoint, config from env vars; `-healthcheck` flag for the Dockerfile's `HEALTHCHECK` (GETs its own `/healthz`, exits 0/1, via `internal/healthcheck`)
  - `cmd/platform/`: the deploy platform's control plane, a separate service (`docs/decisions/platform.md`). Needs `DATABASE_URL`; waits for Postgres, migrates, then listens on `:8081`; in the background, re-applies routes and removes orphans, then starts the job workers (2). With `GITHUB_WEBHOOK_SECRET` set (at least 32 characters), a second listener on `:8082` serves only the GitHub webhook. Same `-healthcheck` flag.
  - `internal/store/`: the platform's Postgres access (`pgx` pool). Schema in `migrations/NNNN_description.sql`, embedded and applied on startup under an advisory lock, one transaction per file. Migrations that have been deployed are never edited; changes go in a new file. Apps, env vars (encrypted/decrypted only here), deployments and jobs (`CreateDeployment` inserts the deployment and its job in one statement; `ClaimJob` uses `FOR UPDATE SKIP LOCKED` with a lease; every other job change is fenced on `locked_by` and returns `ErrLeaseLost` otherwise) and deploy logs (`deployment_logs`, appended with `COPY`; a log is complete once its job is done or failed). Push-to-deploy: `RequestDeployment` deploys now or stores the commit as the app's `next_commit_sha` (newest push wins), `StartNextDeployment` starts it once the deploy in progress finishes; both lock the app's row first. `webhook_deliveries` dedupes GitHub deliveries (30 days). Invariants (one deploy in progress and one live deployment per app, forward-only status) are enforced by the database: partial unique indexes and conditional `UPDATE`s, never check-then-write in Go.
  - `internal/secrets/`: AES-256-GCM sealing for values stored encrypted (env vars), key from `PLATFORM_SECRET_KEY`; associated data binds each value to its row
  - `internal/healthcheck/`: the self-check both binaries use for `-healthcheck`
  - `internal/server/`: HTTP routes; reads snapshots through its `SnapshotSource` interface, never Docker directly. Forwards `/api/platform/` to the platform (`PLATFORM_URL`, `NewPlatformProxy`, flushing every write so log streams arrive live); everything is wrapped in `http.CrossOriginProtection` (CSRF: the platform API has no login). Both binaries' servers use the shutdown signal as `BaseContext`, so open log streams end when shutdown starts.
  - `internal/monitor/`: background poller (`Run` on a ticker, `Poll` for one round, called directly in tests); holds the latest snapshot and the previous stats samples for CPU %; feeds `internal/history` after each successful poll
  - `internal/history/`: in-memory trend data (1-minute buckets of avg/max, 24h ring per series, keyed by container name; gaps are `OK: false`, never 0). Not persisted.
  - `internal/docker/`: the dashboard's read-only Docker Engine API client (plain `net/http`, no SDK)
  - `internal/engine/`: the platform's Docker client (official SDK, `github.com/moby/moby/client`), through `docker-proxy-rw`; `tcp://` only. Methods are added only as the deploy flow needs them: `Version`, `Build` (Docker's legacy builder, `version=1`, context in the body: BuildKit through the API needs a session to resolve base images), `Run` / `Remove` (app containers, with fixed hardening callers can't override), `ManagedContainers` (filtered by the `ops.platform.managed` label, so unmanaged containers like the game servers never reach the platform), `FollowLogs` (a container's output, demultiplexed, timestamped, followed). `Build` passes its output line by line to a callback. Output lines go through `cleanLine` (carriage-return redraws resolved, ANSI escapes and NULs removed, valid UTF-8, at most 4 KiB).
  - `internal/router/`: Caddy's admin API. Declarative: `Apply` replaces the whole `apps` server (all routes) in one PATCH, so re-applying is always safe. `Probe` sends a request through Caddy with a given Host (health checks). `InFlight` reads Caddy's in-flight request counts per upstream (`/reverse_proxy/upstreams`; kept across config changes), for draining.
  - `internal/jobs/`: the job runner. Workers claim jobs, renew the lease while running (and stop the job if it can't be renewed), then record the outcome: done, retried with doubling backoff (non-`Permanent` errors, up to `MaxAttempts`), failed (handler's `GiveUp` first, then the job), or released at shutdown (attempt not counted). `Wake` starts an idle worker at once.
  - `internal/deploy/`: runs a deploy (download → build → run → probe through Caddy → mark healthy → switch route → drain and remove older containers) as the job handler: `Run` is one attempt, safe to repeat (removes the deployment's own leftover container first; a deployment already healthy only finishes the route switch); `GiveUp` records the reason on the deployment (one already healthy isn't marked failed). Draining (`retireOlder`/`drain`): the old version is stopped only once Caddy has no requests in flight to it, at most `DrainTimeout` (15s); a drain cut off by shutdown hands the job back. Every deploy that finishes (done, or given up) starts the app's waiting pushed commit (`startNext`). `Start` (a commit), `StartLatest` (the branch's head, via GitHub) and `Push` (the webhook). Errors the user must fix are `jobs.Permanent`; the rest retry. Each attempt writes the deploy log (`log.go`: stage messages and build output, batched every 500ms, build output capped at 5,000 lines per deployment, only an app's last 10 deployments keep logs; never env var values). `ApplyRoutes` builds the full table from the database (live deployments) plus in-flight probe routes. `RemoveOrphans` at startup, before the workers start.
  - `internal/api/`: the platform's HTTP API under `/api/platform/` (apps, env vars, deployments, log streams in `logs.go`). Env var values are write-only. Bodies must be `application/json`.
  - `internal/source/`: fetches an app's code at a commit as GitHub's tarball and streams it, re-packed, as a build context; size-capped, rejects paths outside the context. An app's `sourceDir` selects one folder as the context. `LatestCommit` looks up a branch's head.
  - `internal/webhook/`: GitHub's push webhook, the platform's only public endpoint (`POST /webhooks/github`, nothing else on its listener). Checks the HMAC-SHA256 signature (constant time) before anything else; 1 MiB body cap; JSON only; `ping` answered, other events and tags ignored; deliveries deduped by `X-GitHub-Delivery`; pushes every matching app (repo case-insensitive, same branch, auto-deploy on) through `Deployer.Push`. The response lists what happened per app (GitHub shows it under "Recent deliveries").
  - `internal/host/`: host CPU (`/proc/stat`), memory (`/proc/meminfo`) and disk (`statfs`, Linux-only via build tag; stub elsewhere so Windows dev still builds)
- `frontend/`: Vite + React + TypeScript, Oxlint, Vitest + Testing Library. Hash routing (`src/route.ts`): `#/` containers (`App.tsx`), `#/apps` and `#/apps/{id}` (`Apps.tsx`). Logs: `useLogStream` (an `EventSource`, lines batched per 100ms, closed on the server's `end`) and `LogViewer` (follows the bottom; key it per URL); tests use `src/test/fakeEventSource.ts`, since jsdom has no `EventSource`.
- `examples/hello/`: a minimal app to deploy (its own Go module; CI deploys it from this repo with `sourceDir`). Shuts down gracefully on SIGTERM; `BROKEN=1` fails its health check (a broken deploy on purpose); `/slow?seconds=N` (≤30) answers late (CI's drain check)
- `Dockerfile`, `compose.yaml`: one image with both binaries (Go serves the built frontend from `STATIC_DIR`); compose runs the dashboard and its socket proxy, and the platform (entrypoint `/platform`; its webhook port published on `127.0.0.1:${WEBHOOK_PORT:-8082}`) and its `postgres` (volume `platform-db`, internal `platform-db` network)

## API

- `GET /healthz`: `{"status":"ok"}` whenever the process is serving; deliberately doesn't check Docker.
- `GET /api/containers`: `{updatedAt, stale, host, containers}` from the monitor's latest snapshot; 502 only if no poll has ever succeeded. `host` is null if it couldn't be read. Per container: `id, name, image, state, health, status, startedAt, finishedAt, cpuPercent, memoryBytes, memoryLimitBytes`. Values that don't apply are `null`, never 0. CPU is percent of the whole host (0–100), for containers and host alike.
- `GET /api/history?window=1h|24h` (default 1h, else 400): `{start, stepSeconds, host, containers}` from the in-memory history. Each series is an array with one value per step (60s) from `start`, oldest first, `null` for gaps; all series in a response have the same length. Containers are keyed by name.
- Types: `containersResponse` and `historyResponse` in `backend/internal/server/server.go`, mirrored in `frontend/src/api.ts`; keep them in sync.
- Platform API (served by `cmd/platform`, forwarded by the dashboard; apps include `sourceDir`, `""` for the repo's root, `autoDeploy` and `nextCommitSha`, null unless a push is waiting): `GET/POST /api/platform/apps`, `GET /api/platform/apps/{id}` (with `envKeys`, `deployments`, `currentDeployment`), `PATCH /api/platform/apps/{id}` (`{"autoDeploy"}`, the only changeable setting), `PUT/DELETE /api/platform/apps/{id}/env/{key}` (`{"value"}`; values are never returned), `GET/POST /api/platform/apps/{id}/deployments` (`{"commitSha"}` → 202, the deploy runs in the background), `POST /api/platform/apps/{id}/deployments/latest` (no body; the branch's newest commit, 400 if the branch isn't found). Log streams (Server-Sent Events; each message's data is `{at, stream, line}`, an `end` event when there's nothing more): `GET /api/platform/apps/{id}/deployments/{deploymentId}/logs` (stored lines then new ones; event IDs are line IDs, so `Last-Event-ID` resumes; ends once the deploy has finished for good) and `GET /api/platform/apps/{id}/logs` (the live container's last 200 lines then new ones, not stored; event IDs are timestamps; 404 if nothing is live; ends if the container stops). Errors are `{"error"}` with 400/404/409/415/500. Types in `backend/internal/api/api.go`, mirrored in `frontend/src/api.ts`.

## Commands

```sh
# Backend (from backend/)
gofmt -l . && go vet ./... && go test -race ./... && go build ./...
GOOS=linux go vet ./...           # dev PC is Windows; this type-checks Linux-only code (disk_linux.go)
go run ./cmd/dashboard            # :8080; /api/containers is 502 without a proxy
# Database tests in internal/store skip unless DATABASE_URL is set; CI sets it.
# The dev PC has no Postgres, so they (and `go run ./cmd/platform`) only run in CI and on the server.

# Frontend (from frontend/)
npm run lint && npm test && npm run build
npm run dev                       # :5173, forwards /api to :8080 (or $API_TARGET, e.g. the server's ts.net URL for real data)
```

CI (`.github/workflows/ci.yml`) runs exactly these checks, with a `postgres:18.6` service container so the database tests run (they fail rather than skip when `CI` is set without `DATABASE_URL`). A separate job builds the Docker image and runs `/dashboard -healthcheck` inside it, then starts `/platform` from the same image against a real Postgres and checks its healthcheck too, and finally runs the real `compose.yaml` with `docker compose up --wait`. That job is the only place the image is tested, since the dev PC has no Docker. Keep all of them green. CI runs on pushes to `main` and on pull requests, so a draft PR is the way to run it before merging.

Deploying is done by the owner on the server (`homelab`), which only pulls: `cd ~/apps/dashboard && git pull && docker compose up -d --build`. The server's only local file is its git-ignored `.env`. After a change that needs checking on the real server, end the step with exact commands for the owner to run and what output to expect.

## Constraints (do not break)

- Never mount `/var/run/docker.sock` into the dashboard or platform containers. The dashboard reaches Docker only through `docker-proxy` (`POST=0`, `CONTAINERS` only) on the internal `docker-api` network. The platform reaches it only through `docker-proxy-rw` (`POST=1` with only `CONTAINERS`, `IMAGES`, `BUILD`, `NETWORKS`) on the internal `docker-rw` network, which nothing else joins. Enable another proxy group only with a decision entry. Both Go clients accept only `tcp://` hosts.
- Publish the dashboard only on `127.0.0.1` (Docker bypasses UFW); remote access is via `tailscale serve`.
- The platform and Postgres publish no ports, with one exception from P2: the platform's webhook-only listener, published on `127.0.0.1` only, for Tailscale Funnel to forward GitHub's webhook to (`docs/decisions/platform.md`). The main API stays unpublished. Postgres is only on the internal `platform-db` network; nothing but the platform can reach it.
- Caddy's admin API (unauthenticated) listens only on `10.203.0.2:2019`, on the internal `caddy-admin` network shared with the platform alone. App containers are only on `platform-apps` (with Caddy's proxy side): never put the platform, Postgres or a proxy on that network. Caddy's port 80 is published only on `APPS_LISTEN_IP` (the server's Tailscale IP), never `0.0.0.0`.
- Keep Docker access behind Go interfaces so logic is tested against fakes; tests use `httptest`, never a real Docker daemon.
- Secrets come from env vars; document every variable in `.env.example` and the README's configuration table.
- API errors to clients are generic; details go to the server log.

## Conventions

- Go: standard library first; table-driven tests; interfaces declared where they're used.
- API JSON uses dedicated response types in `internal/server`, separate from internal types. Empty lists encode as `[]`, never `null`.
- Frontend: single quotes, no semicolons (template style); one discriminated-union state per async view; abort fetches on unmount.
- Pin image versions in `Dockerfile` and `compose.yaml`.
