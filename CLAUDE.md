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
  - `cmd/dashboard/`: entrypoint, config from env vars
  - `internal/server/`: HTTP routes; declares the interfaces it consumes (e.g. `ContainerLister`)
  - `internal/docker/`: Docker Engine API client (plain `net/http`, no SDK)
- `frontend/`: Vite + React + TypeScript, Oxlint, Vitest + Testing Library
- `Dockerfile`, `compose.yaml`: one image (Go serves the built frontend from `STATIC_DIR`) plus the socket proxy

## Commands

```sh
# Backend (from backend/)
gofmt -l . && go vet ./... && go test -race ./... && go build ./...
go run ./cmd/dashboard            # :8080; /api/containers is 502 without a proxy

# Frontend (from frontend/)
npm run lint && npm test && npm run build
npm run dev                       # :5173, forwards /api to :8080
```

CI (`.github/workflows/ci.yml`) runs exactly these checks; keep them green.

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
