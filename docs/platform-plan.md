# Platform plan: turning the ops dashboard into a self-hosted PaaS

## What we're building

This repo is my working ops dashboard for my home server. I want to evolve it
into a self-hosted deployment platform (a small Heroku/Render) with the
dashboard as its UI.

The end state: I register an app (GitHub repo + env vars) in the dashboard.
When I push to the repo, the platform pulls the code, builds a Docker image,
starts the new version, health-checks it, routes traffic to it, and keeps the
old version running if the new one fails. If a running app crashes, the
platform notices and restarts it. The dashboard shows all of this live.

This is a resume project. The goal is a backend that demonstrates real
engineering: background jobs, failure handling, crash recovery, and state
reconciliation. Correctness and robustness matter more than feature count.

## Ground rules for working on this

- **Read the existing codebase first.** Before proposing changes, figure out
  the current stack, structure, data sources, and how the dashboard gets its
  data. Summarize it for me and propose how the platform fits in. Adapt this
  plan to what exists; don't assume.
- **The dashboard must keep working at every step.** No big-bang rewrite. Each
  phase ends with something that runs and that I can demo.
- **Explain the why.** I want to understand and defend every design decision
  in an interview. When you make a non-obvious choice (data model,
  concurrency, failure handling), explain it briefly and mention the
  alternative you didn't pick.
- **Small, reviewable changes.** One phase at a time, broken into steps I can
  review and commit separately. Ask before large refactors of existing code.
- **Match the existing code style of this repo.**
- **Keep a decisions log in `docs/decisions/platform.md`:** one short entry per
  significant design choice (what, why, alternatives). This becomes the design
  doc for my portfolio.

## Environment and constraints

- Server: Lenovo ThinkCentre M700 Tiny, i5-6400T, 16GB RAM, 256GB SSD, Ubuntu
  Server 26.04 LTS, hostname `homelab`, user `sasha`.
- Docker is installed. Existing services (Minecraft via
  `itzg/minecraft-server`, Stardew, the Discord bot) live in
  `~/apps/<service>` with code from `git clone` and secrets in `.env` files.
- Security posture: UFW allows only OpenSSH. Nothing is port-forwarded except
  game servers that friends need. Never expose SSH. The platform's admin
  UI/API must only be reachable over Tailscale.
- Public traffic for deployed apps should go through Cloudflare Tunnel (no new
  port forwarding).
- Existing services stay as they are. The dashboard should keep monitoring
  containers the platform does not manage (game servers, etc.). Migrating them
  onto the platform is optional and later.
- Resources are limited. Keep the platform's own footprint small and enforce
  memory/CPU limits on deployed apps so nothing starves the game servers.

## Target architecture

- **Control plane (backend):** Go preferred for the new platform backend (good
  concurrency story, matches real infra tooling). If the existing dashboard
  backend is in another language, propose whether to extend it or run the
  control plane as a separate Go service the dashboard calls. Explain the
  tradeoff.
- **State:** Postgres. It is the source of truth for apps, deployments, and
  jobs.
- **Container management:** Docker Engine API via the official Go SDK. Do not
  shell out to the docker CLI.
- **Routing:** Caddy (or Traefik) configured dynamically from the control
  plane, e.g. Caddy's admin API. Each app gets a hostname like `app.<domain>`.
- **Dashboard (frontend):** the existing UI, extended with platform pages.

## Core design principles

- **Desired state vs. actual state (reconciliation).** Postgres stores what
  should be running (app X, deployment abc123, healthy). A reconciler loop
  periodically compares that with what Docker reports and fixes drift: restart
  crashed containers, remove orphans, re-apply routes. Deploys update the
  desired state, and the reconciler makes it real. This is the central idea of
  the project.
- **Durable job queue in Postgres.** Builds and deploys are jobs in a table,
  claimed with `SELECT ... FOR UPDATE SKIP LOCKED`. Jobs survive crashes,
  retry with backoff, and are idempotent. At most one deploy per app runs at a
  time.
- **Everything is labeled.** Containers the platform creates carry Docker
  labels (app id, deployment id) so the reconciler can tell managed containers
  from everything else and never touches unmanaged ones.
- **Secrets are encrypted at rest.** App env vars are stored encrypted (key
  from the environment, never committed) and injected at container start. No
  plaintext `.env` files for platform apps.

## Rough data model (adjust as needed)

- `apps`: id, name, repo_url, branch, hostname, port, health_check_path,
  resource limits, created_at
- `app_env_vars`: app_id, key, encrypted_value
- `deployments`: id, app_id, commit_sha, image_tag, status (queued / building
  / starting / healthy / failed / superseded / rolled_back), started_at,
  finished_at, error
- `jobs`: id, type, payload, status, attempts, run_after, locked_by,
  locked_at, last_error
- `deployment_logs`: deployment_id, timestamp, stream, line (or log files on
  disk with a pointer)

## Phases

Each phase has a definition of done. Don't start the next phase until the
current one meets it.

### Phase 0: Understand and plan

- Inspect the repo and summarize the current architecture.
- Propose where the control plane lives, how the dashboard talks to it, and
  the folder layout.
- Set up Postgres (in Docker, with a named volume) and migrations.

**Done when:** I've approved the plan, and Postgres plus migrations run
locally and on the server.

### Phase 1: Manual deploy of one app

- API endpoints to create an app and trigger a deploy of a given commit.
- Deploy flow: clone repo → build image from its Dockerfile (tagged with
  commit SHA) → start container with labels, env vars, and resource limits →
  register route in Caddy.
- Dashboard: an apps list page and an app detail page showing current
  deployment and status.

**Done when:** I can deploy a sample app from the dashboard and reach it at
its hostname.

### Phase 2: Push-to-deploy, job queue, live logs

- GitHub webhook endpoint (verify the signature) that enqueues a deploy job.
  The webhook must be reachable through Cloudflare Tunnel, while the rest of
  the API stays Tailscale-only.
- Worker(s) pulling from the Postgres job queue with retries and per-app
  serialization.
- Build and runtime logs streamed live to the dashboard (SSE or WebSockets).

**Done when:** pushing to the repo deploys automatically, I can watch the
build log live, and killing the worker mid-build results in the job being
retried, not lost.

### Phase 3: Zero-downtime deploys and rollbacks

- Start the new container alongside the old one, poll its health check,
  switch the route only once it's healthy, then drain and stop the old one.
- Failed health check → mark deployment failed, keep the old one serving.
- One-click rollback to any previous successful deployment (reuse its image;
  no rebuild).

**Done when:** deploying a deliberately broken commit leaves the site up, and
rollback works from the dashboard.

### Phase 4: Reconciliation and crash recovery

- Reconciler loop comparing desired state (Postgres) with actual state
  (Docker, Caddy routes) and correcting drift.
- Dashboard shows reconciler events ("restarted app X: container exited").

**Done when:** `docker kill`-ing a managed container, deleting its route, or
restarting the whole server all result in apps coming back on their own
without touching unmanaged containers.

### Phase 5: Databases for apps and observability

- Let an app request a Postgres database: provisioned as a managed container
  with a persistent volume and injected `DATABASE_URL`. Scheduled backups to
  `~/backups`.
- Per-app CPU/memory metrics and request counts in the dashboard, alongside
  the existing monitoring of unmanaged services.

**Done when:** the first real app (below) runs with its own database and
shows up with metrics.

### Phase 6 (stretch): Preview environments

- Each open PR gets its own temporary deployment and URL, torn down when the
  PR closes.

## First real app: pokewordle backend

The first real workload is a backend for pokewordle (my
React/TypeScript/Tailwind Pokémon game; frontend is on Cloudflare). It will
need accounts, a leaderboard, and saved high scores, with server-authoritative
scoring: the server holds the answer and validates guesses so scores can't be
faked. That app lives in its own repo. This repo only needs to be able to
deploy it (including its database and a public `api.<domain>` hostname via
Cloudflare Tunnel, with CORS handled by the app).

## Non-goals (for now)

- Multi-server / clustering. Single host only.
- Supporting build systems other than a Dockerfile in the repo.
- Multi-user accounts or teams for the platform itself. It's just me, behind
  Tailscale.
- Replacing Docker with anything else.

## Portfolio deliverables (keep in mind throughout)

- README with an architecture diagram and a short explanation of the
  reconciliation model.
- `docs/decisions/platform.md` design log.
- A "chaos demo": a script or video that kills containers, pushes a broken
  build, and restarts the server, showing recovery.
- Some measured numbers (deploy time, time to recover from a crash, resource
  overhead of the platform itself).
