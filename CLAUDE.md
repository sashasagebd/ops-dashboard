# CLAUDE.md

Self-hosted monitoring dashboard for a home server, built as a portfolio
project. Go API + React/TS frontend, deployed with Docker Compose and reached
over Tailscale.

## Read first

- `docs/roadmap.md`: milestone plan, current status, pending decisions and open questions. Start here when resuming.
- `docs/spec.md`: v1 scope, out-of-scope list, constraints, server context.
- `docs/decisions/stack.md`: reasoning behind the main design choices.

## How to work on this repo

- Work in small, reviewable steps: one roadmap step at a time, then stop for review.
- Propose a plan and get approval before writing code for a new milestone.
- Explain non-obvious decisions, in code comments where they'd help a reader, and in the end-of-step summary.
- End each step with: files changed, decisions, what was verified, what wasn't.
- Don't commit unless asked; the owner commits. Update `docs/roadmap.md` status when a step lands.
- Re-read a file right before editing it; the owner edits docs between turns.

## Layout

- `backend/`: Go module `github.com/sashasagebd/ops-dashboard/backend`
  - `cmd/dashboard/`: entrypoint, config from env vars; `-healthcheck` flag for the Dockerfile's `HEALTHCHECK` (GETs its own `/healthz`, exits 0/1)
  - `internal/server/`: HTTP routes; reads snapshots through its `SnapshotSource` interface, never Docker directly
  - `internal/monitor/`: background poller (`Run` on a ticker, `Poll` for one round, called directly in tests); holds the latest snapshot and the previous stats samples for CPU %; feeds `internal/history` after each successful poll
  - `internal/history/`: in-memory trend data (1-minute buckets of avg/max, 24h ring per series, keyed by container name; gaps are `OK: false`, never 0). Not persisted.
  - `internal/docker/`: Docker Engine API client (plain `net/http`, no SDK)
  - `internal/host/`: host CPU (`/proc/stat`), memory (`/proc/meminfo`) and disk (`statfs`, Linux-only via build tag; stub elsewhere so Windows dev still builds)
- `frontend/`: Vite + React + TypeScript, Oxlint, Vitest + Testing Library
- `Dockerfile`, `compose.yaml`: one image (Go serves the built frontend from `STATIC_DIR`) plus the socket proxy

## API

- `GET /healthz`: `{"status":"ok"}` whenever the process is serving; deliberately doesn't check Docker.
- `GET /api/containers`: `{updatedAt, stale, host, containers}` from the monitor's latest snapshot; 502 only if no poll has ever succeeded. `host` is null if it couldn't be read. Per container: `id, name, image, state, health, status, startedAt, finishedAt, cpuPercent, memoryBytes, memoryLimitBytes`. Values that don't apply are `null`, never 0. CPU is percent of the whole host (0–100), for containers and host alike.
- `GET /api/history?window=1h|24h` (default 1h, else 400): `{start, stepSeconds, host, containers}` from the in-memory history. Each series is an array with one value per step (60s) from `start`, oldest first, `null` for gaps; all series in a response have the same length. Containers are keyed by name.
- Types: `containersResponse` and `historyResponse` in `backend/internal/server/server.go`, mirrored in `frontend/src/api.ts`; keep them in sync.

## Commands

```sh
# Backend (from backend/)
gofmt -l . && go vet ./... && go test -race ./... && go build ./...
GOOS=linux go vet ./...           # dev PC is Windows; this type-checks Linux-only code (disk_linux.go)
go run ./cmd/dashboard            # :8080; /api/containers is 502 without a proxy

# Frontend (from frontend/)
npm run lint && npm test && npm run build
npm run dev                       # :5173, forwards /api to :8080 (or $API_TARGET, e.g. the server's ts.net URL for real data)
```

CI (`.github/workflows/ci.yml`) runs exactly these checks, plus a job that builds the Docker image and runs `/dashboard -healthcheck` inside it (the only place the image is tested, since the dev PC has no Docker); keep them green.

Deploying is done by the owner on the server (`homelab`), which only pulls: `cd ~/apps/dashboard && git pull && docker compose up -d --build`. The server's only local file is its git-ignored `.env`. After a change that needs checking on the real server, end the step with exact commands for the owner to run and what output to expect.

## Constraints (do not break)

- Never mount `/var/run/docker.sock` into the dashboard container. Docker is reached only through `tecnativa/docker-socket-proxy` (`POST=0`, only needed endpoints enabled) on the internal `docker-api` network. The Go client accepts only `tcp://` hosts.
- Publish the dashboard only on `127.0.0.1` (Docker bypasses UFW); remote access is via `tailscale serve`.
- Keep Docker access behind Go interfaces so logic is tested against fakes; tests use `httptest`, never a real Docker daemon.
- Secrets come from env vars; document every variable in `.env.example` and the README's configuration table.
- API errors to clients are generic; details go to the server log.

## Conventions

- Go: standard library first; table-driven tests; interfaces declared where they're used.
- API JSON uses dedicated response types in `internal/server`, separate from internal types. Empty lists encode as `[]`, never `null`.
- Frontend: single quotes, no semicolons (template style); one discriminated-union state per async view; abort fetches on unmount.
- Pin image versions in `Dockerfile` and `compose.yaml`.
