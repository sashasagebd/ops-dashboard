# Roadmap

Milestones for the v1 scope in [spec.md](spec.md) (M1–M6), then the deploy
platform in [platform-plan.md](platform-plan.md) (P0 onwards, numbered after
the plan's phases). Each numbered step is one small, reviewed commit. Design
reasoning lives in [decisions/](decisions/).

_Last updated: 2026-11-04_

| Milestone | Status |
|---|---|
| M1: Thinnest end-to-end slice | ✅ Done, deployed |
| M2: Full container status | ✅ Done, deployed |
| M3: Host stats | ✅ Done, deployed |
| M4: Discord alerts | ⏸️ Deferred (not in v1; plan kept below) |
| M5: Portfolio polish | ⏸️ Set aside (5.1–5.3 done and deployed, 5.4 dropped, 5.5–5.6 set aside for the platform) |
| M6: History sparklines | ✅ Done, deployed (with P0, 2026-11-02) |
| P0: Platform foundations | ✅ Done, deployed 2026-11-02 (PR #1) |
| P1: Manual deploy of one app | ✅ Done, deployed 2026-11-03 |
| P2: Push-to-deploy, job queue, live logs | ✅ Done, deployed 2026-11-03 (PR #7) |
| P3: Zero-downtime deploys and rollbacks | ✅ Done, deployed 2026-11-04 (PR #9) |
| P4: Reconciliation and crash recovery | ✅ Done, deployed 2026-11-04 (PR #11) |
| P5: Databases for apps, observability, public hostnames | ✅ Done, deployed 2026-11-05 |

**Where we left off (2026-11-05):** P0 to P5 are done and deployed.
Pushing to `main` deploys `examples/hello` with no clicks, the build log
streams live, and a platform killed mid-build finishes the same deploy after
it restarts. Deploys have no downtime (the old version drains before it's
stopped), a broken deploy never goes live, and any of an app's last 5
previous versions can be rolled back to from the dashboard, with no build.
A reconciler keeps what runs in line with the database every 10s: a killed
or deleted app container, a lost route, or a whole-server reboot all come
back on their own, each fix shown as an event in the dashboard.
Apps can have their own Postgres (backed up daily to `~/backups`), show CPU,
memory and request metrics, and be made public on `sashasagebd.com` through
Cloudflare Tunnel with one setting; `hello` is live at
`https://hello.sashasagebd.com`. The webhook comes in at
`hooks.sashasagebd.com`; Tailscale Funnel is off.
**Next:** the frontend redesign (UI below), written and awaiting review and CI. The pokewordle
backend is a separate project; once it exists, deploying it is registering
it, adding a database and the public name `api`. Phase 6 (preview
environments, a stretch goal) is paused at the owner's call (2026-11-05).

Notes for resuming:
- **Workflow:** the owner wants fewer, larger steps (P1 was regrouped from 9
  to 6 steps). Within a step, keep going without stopping unless owner
  action is needed. Only the owner commits.
- **Undecided:** whether to let Claude commit and push to feature branches
  (never `main`), with GitHub branch protection on `main` requiring a PR
  and green CI. Suggested, not decided; leave commits to the owner until
  they say so.
- **CI vs the server:** CI's Docker is 28.0.4 and the server's is 29.8.1.
  Anything that depends on Docker's behaviour (start order, networking,
  builds) gets a real check on the server too; the first reboot found a bug
  CI couldn't.
- **Footprint** (idle, `docker stats`): after P1, platform 7.7 MiB,
  Postgres 35 MiB, Caddy 10 MiB. After P2's server checks: platform
  8.3 MiB, Postgres 67 MiB (warm cache), Caddy 52 MiB. After P3's server
  checks: platform 8.7 MiB, Postgres 69 MiB, Caddy 56 MiB, hello 1.7 MiB.
  After P4's reboot (measured 30s after boot): platform 18.7 MiB, Postgres
  65 MiB, Caddy 51 MiB, hello 7.2 MiB. After P5's server checks: platform
  9.7 MiB, Postgres 72 MiB, Caddy 16 MiB, cloudflared 17 MiB, hello
  2.2 MiB, hello's database 46 MiB. **Caddy:** each route update rebuilds
  its config, and Go holds freed memory until it needs to return it; it caps
  itself near its 128 MiB limit (`GOMEMLIMIT`, ~115 MiB). Since P4 the
  reconciler only applies routes when Caddy's differ, so an idle platform
  no longer causes rebuilds. If Caddy still climbs over days, look again.
- M5's 5.5 (README) and 5.6 (`v1.0.0` tag) are set aside; the README gets
  rewritten for the platform later.

## M1: Thinnest end-to-end slice ✅

One Go endpoint returning running containers as JSON, one React page showing
them, both running via Docker Compose on the server and reachable through
`tailscale serve`.

| Step | What | Commit |
|---|---|---|
| 1.1 | Go server skeleton, `GET /healthz`, CI cache fix (`go.mod` instead of missing `go.sum`) | `05b9086`, `4326c99` |
| 1.2 | `ContainerLister` interface, fake, `GET /api/containers` | `25b533c` |
| 1.3 | Real Docker client over HTTP to the socket proxy (`tcp://` only) | `ad153ed` |
| 1.4 | Vite + React + TS page with loading/empty/error states, Vitest tests | `63ece15` |
| 1.5 | Dockerfile, `compose.yaml` with socket proxy, `.env.example`, README; Go serves the built frontend | `ceb1070` |

Verified on the server: the Tailscale URL shows the real containers, and the
dashboard port is not reachable from another LAN device.

## M2: Full container status ✅

**Goal:** every container (including stopped), with up/down status, uptime,
CPU % and RAM, refreshing automatically.

| Step | What | Status |
|---|---|---|
| 2.1 | List all containers (`all=true`) and inspect each for `startedAt` / `finishedAt`. Additive API fields (`null` when Docker has no time); page has an Uptime column ("up 3h 12m" / "down 20m", Docker's status text on hover). | ✅ `3ef9fd0`, deployed |
| 2.2 | `ContainerStats` (one-shot stats sample, RAM minus inactive file cache) and pure `CPUPercent(prev, cur)` in the Docker client. Not wired up yet, so no behaviour change. | ✅ `92b3b30` (CPU switched to percent of host in 2.3's commit) |
| 2.3 | `internal/monitor` poller (`POLL_INTERVAL`, default 5s, min 1s) holding the latest snapshot in memory, including stats for running containers (CPU from the previous poll's sample). API is now `{updatedAt, stale, containers: [...]}` with `cpuPercent`, `memoryBytes`, `memoryLimitBytes` (null when not available); frontend fetch updated. | ✅ `bf339c3`, deployed; server API shows CPU/RAM for all running containers, so the proxy allows stats |
| 2.4 | `usePolling` hook (5s, next fetch scheduled after each response, paused while the tab is hidden, keeps data when a refresh fails); CPU/Memory columns; stale banner (server can't reach Docker / browser can't reach server / data older than 30s); `API_TARGET` for `npm run dev` against the real server. | ✅ `d232e42`, deployed; `API_TARGET` verified against the server from the PC |
| 2.5 | Redeploy; record the poller, CPU/memory, staleness and polling-hook decisions in `decisions/stack.md`. | ✅ Deployed; live page verified on the server |

Decisions (approved 2026-10-01):
- **CPU % from two polls.** Use the stats endpoint's `one-shot` mode (instant,
  single sample) and compute CPU % from the previous poll's sample, instead of
  Docker's default ~1s wait per container. CPU shows "—" for the first poll.
  Scale is percent of the whole host (100% = every core busy), the same
  scale as M3's host CPU; chosen over `docker stats`' per-core scale.
- **RAM like `docker stats`:** usage minus reclaimable file cache.
- **Stale over broken:** if Docker stops answering, keep serving the last
  snapshot marked stale; 502 only if there has never been a successful poll.
- **Poller tests trigger polls manually**, so they don't depend on real time.
- **Small custom polling hook** in React, not TanStack Query, until we need more.

`docker ps -a` on the server shows no leftover containers, so "all containers"
needs no filtering.

## M3: Host stats ✅

**Goal:** CPU, memory and disk for the whole server, above the container
table, refreshing with it.

| Step | What | Status |
|---|---|---|
| 3.1 | `internal/host` package: parse `/proc/stat` (CPU, from two samples like containers) and `/proc/meminfo` (used = `MemTotal` − `MemAvailable`); disk usage via `statfs`. Parsers tested against real fixture text; `statfs` is Linux-only, so it sits behind a build tag with a stub for Windows dev. | ✅ `7d88367`; CI (Ubuntu) passed, including the real disk read |
| 3.2 | Monitor reads host stats on each poll; API gains a `host` object (additive: `{updatedAt, stale, host, containers}`). `host` is `null` if the read fails; containers are unaffected. | ✅ `965afce`, deployed |
| 3.3 | Frontend: three summary tiles (CPU %, memory used / total, disk used / total) with a usage bar. Page is now "Ops Dashboard" with Server and Containers sections; bars amber at 80%, red at 90%; "Server stats unavailable." when `host` is null. | ✅ `965afce` (with 3.2), deployed |
| 3.4 | Deploy; check the numbers against `top`, `free -h` and `df -h /` on the server; record decisions. | ✅ Deployed, tiles working on the server; decisions recorded |

Decisions (approved 2026-10-01):
- **No extra mounts.** The original idea was mounting the host's `/proc` and
  `/` read-only. But `/proc/stat` and `/proc/meminfo` inside a container
  already report the whole host (they aren't namespaced), and `statfs` on the
  container's `/` reports the filesystem Docker stores everything on, which is
  the host's root disk. Mounting the host's `/` would expose every
  world-readable host file to the dashboard for no gain.
- **Memory "used" = total − available**, like `free`'s "available" column, so
  reclaimable cache doesn't count, consistent with container memory.
- **One snapshot, one request:** host stats ride along in `/api/containers`
  instead of a second endpoint, so the page's numbers are always from the same
  moment and there's one poll loop, one stale flag.
- **Disk is the root filesystem only.** Fine while everything lives on one
  disk; a `DISK_PATHS` list can come later if a second disk is added.

## M4: Discord alerts ⏸️ Deferred

Deferred on 2026-10-01: not needed right now, so it's out of v1 (see
[spec.md](spec.md)). The plan below was drafted but never approved; review it
again before starting. The number is kept so the milestone history stays
readable, and M5 comes next.

**Goal:** a Discord message when a container goes down or comes back up,
without false alarms from redeploys or brief blips.

| Step | What |
|---|---|
| 4.1 | `internal/alert` detector: a pure function of successive snapshots → events (down / up / removed). Baseline on first poll, debounce, keyed by container name. Health is already read (5.3), so "unhealthy" can count as down. Tests only, no sending. |
| 4.2 | `internal/discord` webhook client: one embed per event (red down, green up), 10s timeout, honours Discord's 429 `retry_after`, never logs the URL. Tested with `httptest`. |
| 4.3 | Wiring: detector runs after each successful poll; a sender goroutine with a small queue so a slow Discord never delays polling. Config `DISCORD_WEBHOOK_URL` (unset = alerts off, logged once) and `ALERT_AFTER_POLLS` (default 3). A "dashboard started" message on boot. Docs: `.env.example`, README, compose. |
| 4.4 | Deploy; `docker stop` the bot → down alert, start it → up alert; record decisions. |

Draft decisions (not approved):
- **"Down" = not running, or running but unhealthy.** Containers with a
  healthcheck (Minecraft has one) can be up but hung; that's the failure most
  worth knowing about. Containers without a healthcheck only go by state.
- **Debounce: a change counts after 3 polls in a row (~15s).** A redeploy's
  container recreate, or a single failed poll, doesn't alert. A Minecraft
  restart that takes a minute *does* alert down then up, which is correct.
- **Keyed by name, not ID.** `docker compose up --build` replaces the
  container (new ID, same name); by ID that would look like one container
  removed and another added.
- **First poll is a baseline; new containers join silently.** Starting the
  dashboard, or adding a service, doesn't send a burst of alerts.
- **A container that disappears alerts once as "removed"**, so a crashed
  `--rm` container isn't silent. A deliberate removal costs one message.
- **No alerts while Docker is unreachable.** Failed polls don't feed the
  detector, so a proxy blip can't mark everything down. The banner already
  covers it on the page.
- **"Dashboard started" message on boot.** Partly covers the known limitation
  (nothing alerts while the whole host is down): after a reboot or power cut,
  this message tells you it happened.
- **Alerts are best-effort.** Up to 3 attempts with backoff, then logged and
  dropped. No persistence: a restart forgets pending alerts, which is fine for
  a home server.

## M5: Portfolio polish ⏸️

**Goal:** someone landing on the GitHub repo understands what it is, how it's
built and why, in a couple of minutes, and the deploy is a bit more robust.

| Step | What | Status |
|---|---|---|
| 5.1 | `dashboard -healthcheck`: GETs its own `/healthz` and exits 0/1. Dockerfile `HEALTHCHECK` uses it (distroless has no curl or shell). The dashboard's own row then gets a `healthy` badge (5.3). | ✅ `5ca4416`; not yet deployed |
| 5.2 | CI job that builds the Docker image (no push), so a broken Dockerfile fails the PR instead of the deploy. Also starts the image and runs the healthcheck inside it, since the dev PC has no Docker. | ✅ `5ca4416`; first CI run of the Docker job not yet confirmed green |
| 5.3 | Health badges: Docker client reads `State.Health.Status` from the inspect it already does; API `health` (null if no healthcheck or not running, since Docker keeps a stale value after stop); green `healthy` / amber `starting` / red `unhealthy` badge next to the state. Replaces relying on the Uptime tooltip, which is easy to miss and invisible on phones. | ✅ `3f5bd6d`; not yet deployed |
| 5.4 | ~~Demo mode (`DEMO=1`): fake Docker and host readers with generated data.~~ Dropped 2026-10-06: the screenshot can come from the real server (container and image names aren't sensitive), `API_TARGET` already covers local dev, and the existing fake-based tests already show the interface design. Kept under Ideas / notes for if end-to-end tests are added. | ❌ Dropped |
| 5.5 | README rewrite: screenshot (from the real server; crop out the URL bar, optionally `docker stop` one container briefly for a "down" row), architecture diagram, security model, key decisions (linking `decisions/stack.md`), how it was built with AI, run/deploy. | |
| 5.6 | Deploy, check the healthcheck on the server, tag `v1.0.0`. | |

Decisions (approved 2026-10-01):
- **Healthcheck in the binary itself**, not a separate tool: the image stays
  distroless with nothing extra in it. Checks the process only (`/healthz`),
  not Docker, for the reason in `handleHealthz`.
- **Diagram in Mermaid**: GitHub renders it natively, and it lives as text in
  the repo, so it's diffable and stays in sync with the code.
- ~~**Demo mode reuses the interfaces**~~ (moot: 5.4 dropped 2026-10-06).
- **A "How this was built" README section**: the workflow (spec → milestone
  plan → small reviewed steps, `CLAUDE.md`, tests as guardrails, decisions
  written down), since showing AI used well is part of the project's point.
- **No image publishing** (e.g. to GHCR) for now: the server builds from
  source, so a registry would add moving parts for no gain.

## M6: History sparklines ✅

Post-v1 scope (see [spec.md](spec.md)), but started before `v1.0.0` was
tagged, at the owner's request. Plan approved 2026-10-06.

**Goal:** a small trend line next to each container's CPU and memory, and
under each host tile, so you can see whether "now" is normal, a spike or a
slow climb (e.g. a memory leak, or the disk filling up).

| Step | What |
|---|---|
| 6.1 | `internal/history`: a fixed-size ring buffer per series, fed by `Poll` after each successful poll. Raw 5s samples are folded into 1-minute buckets (CPU avg + max, memory avg; host disk used), 24h kept (1,440 buckets). Keyed by container name; a missing value (stopped, no stats, failed poll) is a gap, not 0. A container's history is dropped once it has been gone for 24h. Tests only, driven with a fake clock; no API change. **In review, not committed.** Window returns a `View` (`Start`, `Step`, series) since 6.2. |
| 6.2 | `GET /api/history?window=1h\|24h` (default 1h; anything else 400): `{start, stepSeconds, host: {cpuPercent, cpuPercentMax, memoryBytes, diskUsedBytes}, containers: {name: {cpuPercent, cpuPercentMax, memoryBytes}}}`. Each series is a plain array, one value per step from `start`, `null` for gaps; CPU rounded to 2 decimals, bytes to whole numbers. Changed from the plan's `{t, v}` points: a timestamp per value would be most of a 24h body (~1,440 values × ~18 series). Host also gets `cpuPercentMax`. No 502 before the first poll: an empty history is a valid answer. `HistorySource` interface in `internal/server`; types mirrored in `frontend/src/api.ts` (`fetchHistory`, not used by the page yet). **In review, not committed.** |
| 6.3 | `Sparkline` component: hand-drawn inline SVG, one `<path>` with a subpath per run of non-null values (a lone value is a dot), stretched to its box with a non-scaling stroke. Fixed y-scale (0–100 for CPU, the limit for memory). Average as the line, peaks faint behind it. `role="img"` with the average and peak in its name. Pure helpers (`sparklinePath`, `seriesStats`, `seriesMax`) in `series.ts` so the component file only exports components (fast refresh). Tests: `series.test.ts`, `Sparkline.test.tsx`. **Not committed.** |
| 6.4 | Wired in: sparklines in the CPU and Memory cells (by container name) and the three host tiles (disk scaled to usable space, like its bar); a 1h / 24h toggle in the header (`aria-pressed` buttons); history polled every 60s with `usePolling`, separate from the 5s live poll, and paused with it while the tab is hidden. If history fails the page simply has no lines (no error, no banner). App tests: the fetch mock now answers by URL. **Not committed.** |
| 6.5 | Decisions recorded in `decisions/stack.md`, README mentions `/api/history`, spec updated (approval). **Still to do (owner):** deploy, check the lines against `docker stats` / `top` over a few minutes. |

Decisions (approved 2026-10-06):
- **In memory first, no database.** Covers "what happened in the last hour or
  day" with no new dependencies, volumes or migrations. The known cost: every
  `docker compose up --build` wipes history. If that gets annoying, move it
  behind a `HistoryStore` interface backed by SQLite on a volume
  (`modernc.org/sqlite`, pure Go, because the static distroless build has no
  cgo).
- **1-minute buckets keeping avg and max CPU.** 720 raw points an hour is more
  than a sparkline can draw; an average alone would hide a 10-second spike,
  so the max is kept alongside it. Memory is ~1,440 buckets × a few numbers ×
  a handful of series: well under 1 MB.
- **Keyed by name, not ID**, as in the M4 plan: a Compose recreate gets a new
  ID, and by ID every deploy would reset the container's history.
- **Gaps are null**, matching the API's "null, never 0" rule: a stopped
  container's line breaks instead of dropping to zero.
- **Separate endpoint, slower poll.** Resending 24h of points every 5s would
  be wasteful. This is a deliberate exception to "one snapshot, one request"
  (M3): the trend and the live number can be a few seconds apart, which
  doesn't matter for a trend line.
- **No chart library.** A sparkline is ~40 lines of SVG; Recharts or similar
  would add far more bundle than it saves.

## P0: Platform foundations ✅

Phase 0 of [platform-plan.md](platform-plan.md). Plan approved 2026-10-28;
decisions in [decisions/platform.md](decisions/platform.md).

**Goal:** a platform service with its own Postgres and migrations, running in
CI and on the server, with the dashboard working exactly as before. Changed
from the plan's "locally and on the server": the dev PC has no Postgres for
now, so database tests skip locally and run in CI.

| Step | What | Status |
|---|---|---|
| 0.1 | Docs: decision log ([decisions/platform.md](decisions/platform.md)), this milestone, spec note. No code. | ✅ `8595b2a` |
| 0.2 | `cmd/platform` skeleton (`/healthz`, `-healthcheck`, `DATABASE_URL` config) and `internal/store`: `pgx` pool and the embedded migration runner (advisory lock, one transaction per file, `schema_migrations`). First migration creates `apps`. Unit tests for anything that doesn't need a database; database tests skip without `DATABASE_URL`. Dockerfile builds both binaries. | ✅ `8595b2a`. Also: the healthcheck moved to `internal/healthcheck` for both binaries; the platform waits for Postgres with backoff before migrating and listening; a database newer than the binary is refused. Database tests first ran in 0.3's CI, green. |
| 0.3 | CI: a Postgres service container so the database tests run (migrations apply, re-running does nothing, concurrent runs are serialized by the lock). The image job also checks the platform binary is in the image. | ✅ Green in CI (draft PR, `8595b2a`). `postgres:18.6` (newest; there's no 19 image yet), the same tag 0.4's compose will pin. Database tests fail instead of skipping if `CI` is set without `DATABASE_URL`. `cache-dependency-path` is now `go.sum`. The image job goes beyond checking the binary exists: it starts `/platform` before its Postgres, so the smoke test also covers waiting for the database, then migrating and passing its healthcheck. |
| 0.4 | Compose: `postgres` (pinned, named volume, memory limit, internal `platform-db` network, no published port) and `platform` services; `.env.example` and README config table. The platform has no Docker access yet (that arrives with its write proxy in P1). Deploy; owner checks the platform is healthy, migrations ran, and the dashboard is unchanged. | ✅ `2666fae`, deployed 2026-11-02; owner confirmed the server checks. Memory limits 128 MB (platform) and 512 MB (Postgres); volume at `/var/lib/postgresql` (Postgres 18's layout); `start_interval` so services report healthy within seconds; a 1-minute stop grace period for Postgres. CI gains a "Compose stack" step: `docker compose up --wait` of the real file, checks the dashboard lists the new containers, restarts the platform and checks it finds nothing left to migrate. |

Decisions (approved 2026-10-28): see
[decisions/platform.md](decisions/platform.md). In short: a separate service
in the same module and image; the dashboard stays read-only and forwards
`/api/platform/*` (wired up in P1, when there's an API to forward to); the
Docker SDK for the platform only; Postgres reachable only from the platform;
`pgx`; an embedded migration runner; real-Postgres tests in CI.

## P1: Manual deploy of one app ✅

Phase 1 of [platform-plan.md](platform-plan.md). Plan approved 2026-11-02
(owner: "get started", taking the recommended option for each choice below).

**Goal (definition of done):** register a sample app in the dashboard, click
Deploy, and reach it at its own hostname from any device on the tailnet, with
the dashboard still working as before and nothing new reachable from the LAN
or the internet.

Changed 2026-11-02 (owner's call): no domain yet, so P1 apps get
`<app>.<tailscale-ip-with-dashes>.sslip.io` hostnames. sslip.io is a free
public DNS service that resolves any name containing an IP to that IP, so
every app has its own hostname with no domain or DNS setup, reachable only
over Tailscale. Caddy routes by hostname either way, so going public later
(a domain on Cloudflare plus cloudflared, likely by P5 for pokewordle) is a
config change, not a code change.

**How it fits together:**

```
you ──tailscale──► dashboard ──/api/platform/*──► platform ──► Postgres
                                                     ├──► docker-proxy-rw ──► Docker
                                                     ├──► Caddy admin API (:2019, internal)
                                                     └──► GitHub (code tarball at a commit)
tailnet ──► <app>.<ip>.sslip.io ──► Caddy :80 (bound to the Tailscale IP) ──► app containers (apps network)
(later)  public ──► Cloudflare ──tunnel──► cloudflared ──► Caddy :80
```

Deploy flow (one `deployments` row per attempt): `queued` → download the
repo at the exact commit → build its Dockerfile, tagged `<app>:<sha>`
(`building`) → start a container with platform labels, decrypted env vars and
the app's limits, on the `apps` network only (`starting`) → point the app's
Caddy route at it and stop the previous one (`healthy`), or `failed` with an
error.

| Step | What | Status |
|---|---|---|
| 1.1 | This plan; decision entries. No code. | ✅ `3763550`; amended 2026-11-02 for the no-domain approach |
| 1.2 | Docker access for the platform: `docker-proxy-rw` (its own internal network; `POST=1` with only `CONTAINERS`, `IMAGES`, `BUILD`, `NETWORKS`), the official SDK (`github.com/moby/moby/client`) behind `internal/engine`, `tcp://` only. The platform logs the Docker version at startup (best effort, non-fatal). No container operations yet. | ✅ `3763550`, green in CI (incl. the proxy probes). The SDK adds ~18 indirect modules (mostly OpenTelemetry and OCI types) but only ~0.5 MB to the binary. CI probes the proxy's rules from a container on its network (containers/images 200; exec, volumes, secrets 403). Also: `.gitattributes` keeps `.go` files LF, since Git for Windows' `autocrlf` checked P0's files out with CRLF and broke the local `gofmt` check. |
| 1.3 | Migrations: `deployments` (status, commit, image, timestamps, error) and `app_env_vars` (AES-256-GCM encrypted values; key `PLATFORM_SECRET_KEY` from `.env`); `internal/secrets` for encryption, the app ID bound in as associated data so a value can't be moved to another app. Store methods for apps, env vars and deployments. | ✅ Green in CI. The key name is bound in as well as the app ID. The two rules "one deploy in progress per app" and "one live deployment per app" are partial unique indexes; status changes are conditional `UPDATE`s, so they only move forward. `PLATFORM_SECRET_KEY` is required (the platform won't start without it). At startup the platform marks deployments left in progress as failed ("interrupted"), which stops a lost deploy from blocking the app forever. |
| 1.4 | **Build and run** (was 1.4 + 1.5). `internal/source`: download GitHub's tarball for a commit (download and unpacked size caps, public repos only for now) and stream it, re-packed, as a build context. `internal/engine`: build with Docker's legacy builder through `/build` (`version=1`, context in the request body; BuildKit needs a session, see risks), image `<app>:<sha>`, the build's error kept as a `BuildError`; run an app container (platform labels, limits, decrypted env, one network, `unless-stopped` until P4) and stop/remove it; list the platform's containers by label. Health-checking moved to 1.5 (it goes through Caddy). Tested against fakes and `httptest`. | ✅ Green in CI, including a real build through the proxy. Every app container also gets fixed hardening the caller can't override: no privileged mode, mounts or published ports; `no-new-privileges`; a 512-process limit; rotated logs (3 × 10 MB). The platform joins the compose default network for internet access (GitHub). CI does a real build through the write proxy. The first CI run proved BuildKit can't build there without a session, so builds use the legacy builder (see risks). |
| 1.5 | **Route and deploy** (was 1.6 + 1.7). Caddy: a pinned service, its admin API only on an internal network apps can't reach, `internal/router` to add/replace/remove an app's route, all routes re-applied at platform startup. Health checks go through Caddy: a temporary probe route per new deployment, so the platform never joins the apps network. The deploy orchestration (one goroutine per deploy, status written at each stage) and the platform API: create/list/get apps, set/delete env vars (values never returned), trigger a deploy of a commit, list deployments. | ✅ `047ed91`, `8542ca3` (CI fix), green in CI. Caddy's admin API binds a fixed IP on an internal network only the platform shares (apps can't reach it). `Apply` replaces Caddy's whole app server from the database each time (declarative). Caddy runs with `--resume`. Startup re-applies routes and removes orphaned app containers. API bodies must be JSON (blocks simple cross-site requests; the dashboard adds an origin check in 1.6). Optional `GITHUB_TOKEN`, sent to the API only (stripped on redirects, by an explicit check). CI now deploys this very commit end to end through the API and Caddy: redeploy, failed deploy (site stays up), isolation and restarts. |
| 1.6 | **Dashboard and go live** (was 1.8 + 1.9). The dashboard forwards `/api/platform/*` to the platform; frontend routing; Apps list and App detail pages (current deployment and status, Deploy button, env var names). Caddy published on the server's Tailscale IP only (`APPS_DOMAIN` and the IP in `.env`), an `examples/hello` sample app. Merge, deploy, and the owner checks the definition of done from a tailnet device, including a reboot. cloudflared waits for a domain. || 🚧 In review; CI not yet run. The dashboard forwards with `httputil.ReverseProxy` behind `http.CrossOriginProtection`. Hash routing, no library. Apps can be built from a folder of their repo (`sourceDir`, migration 0004), which `examples/hello` needs. Caddy is published on `APPS_LISTEN_IP` (required, no default). A deploy reports the code stream's own error (missing folder, too large) rather than Docker's. CI adds: the published port, forwarding and the CSRF check through the dashboard, deploying `examples/hello` with an env var, and a missing `sourceDir`. Merged and deployed 2026-11-03. On the server, `examples/hello` was registered from the Apps page on a tailnet laptop, given `GREETING`, deployed, and served at `hello.<tailscale-ip>.sslip.io`. Idle footprint (`docker stats`): platform 7.7 MiB / 128, Postgres 35 MiB / 512, Caddy 10 MiB / 128, about 53 MiB in all. First reboot: Caddy didn't come back ("Address already in use": the platform had taken Caddy's fixed `10.203.0.2` by starting first). Fixed with an `ip_range` on `caddy-admin`, plus a CI check that starts the platform first. Second reboot, with the fix: every service, Caddy included, came back by itself. The Tailscale IP was up in time for Caddy's port, so `ip_nonlocal_bind` wasn't needed. |

Decisions (approved 2026-11-02), with entries in
[decisions/platform.md](decisions/platform.md):
- **Code via GitHub's tarball API**, not `go-git` or letting Docker clone:
  no git dependency, pinned to an exact SHA, and the platform controls the
  build context (size limit).
- **Caddy via its admin API**, not Traefik's label discovery: the platform
  owns routing as code, and Traefik would need Docker access of its own.
- **Deploys run in a goroutine in P1.** Known gap: a platform restart
  mid-deploy leaves the deployment stuck in `building`/`starting`. P2's
  Postgres job queue fixes it; the before/after is part of the story.
- **Docker's `unless-stopped` restarts crashed apps until P4.** Without it a
  crashed app would stay down until the reconciler exists.
- **Network isolation:** apps are only on `apps` (shared with Caddy). They
  can't reach the platform, Postgres, the proxies or Caddy's admin API.
- **Frontend routing:** small and dependency-free if it stays at two or three
  pages; revisited in 1.6.

Risks to check early:
- **Building through the API** (resolved 2026-11-03 by CI, after a wrong
  first call). The plan was BuildKit (`version=2`): Docker's source showed it
  reads the build context from the request body with no session. True, but
  CI's real build then failed with `no active sessions`: BuildKit also needs
  a client-side session to look up the `FROM` image in the registry, even a
  public one. Decision: the legacy builder (`version=1`), which pulls base
  images itself through `/build` alone. It's deprecated (Docker 23, no
  removal date), lacks newer Dockerfile features such as `RUN --mount`, and
  its progress is plain text. Upgrade path, if an app needs BuildKit: a
  session in Go (`moby/buildkit`'s session package) and the proxy's
  `SESSION` group, changing only `engine.Build`. CI's compose step does a
  real build through the write proxy.
- **Publishing Caddy on the Tailscale IP.** If Docker starts before
  Tailscale has its IP after a reboot, binding to that IP fails and Caddy
  won't start. 1.6 checks this with a real reboot and picks a fix if needed
  (e.g. a systemd ordering override, or `tailscale serve` in front instead).

Steps regrouped 2026-11-02 at the owner's request (fewer push/review
rounds): the original 1.4–1.9 became 1.4–1.6, each pairing two steps that
touch the same part of the system and are checked the same way.

## P2: Push-to-deploy, job queue, live logs ✅

Phase 2 of [platform-plan.md](platform-plan.md). Plan approved 2026-11-03.

**Done when** (from the plan): pushing to an app's repo deploys it
automatically, the build log can be watched live in the dashboard, and
killing the platform mid-build gets the deploy retried, not lost.

| Step | What | Status |
|---|---|---|
| 2.1 | **Durable job queue.** A `jobs` table; workers inside the platform claim with `SELECT … FOR UPDATE SKIP LOCKED` and hold a renewed lease, so a crashed platform's jobs are picked up again after a restart (replacing "interrupted deploys are marked failed"). Retries with backoff only for transient errors (GitHub/Docker unreachable), at most 3 attempts; build or health-check failures fail at once. Retries are idempotent (leftover containers from the attempt removed first; the build cache makes rebuilding cheap). Still one deploy per app at a time, enforced by the database. Database tests for concurrent claims; CI kills the platform mid-build and requires the deploy to finish after it restarts. | ✅ `53867d5`, deployed with P2. Migration 0005 (`jobs`; also fails pre-queue deployments left in progress, which have no job). `internal/jobs` runner (2 workers, 30s lease renewed every 10s, retries after 15s then 30s); the deployer is its handler. Beyond the plan: attempts are counted at claim, so a job whose worker keeps crashing gives up after 3; a clean shutdown hands the job back without counting the attempt; a retry of a deployment already healthy only finishes the route switch; cleanup after a switch now removes only *older* deployments' containers. Details in [decisions/platform.md](decisions/platform.md) ("How the job queue works"). |
| 2.2 | **Live logs.** Deploy logs (Docker's build output and the platform's own stage messages) stored per deployment, capped per deployment, last few deployments per app kept. An SSE endpoint replays stored lines then streams new ones; the dashboard's proxy flushes immediately. App (runtime) logs tailed live from Docker, not stored. UI: a log viewer per deployment and an "App logs" tab. | ✅ Deployed with P2 (CI needed one fix, to its own end-of-stream check). Migration 0006 (`deployment_logs`). Build output capped at 5,000 lines per deployment; the last 10 deployments per app keep logs. The deploy log stream polls Postgres every 500ms and ends once the deployment's *job* has finished (its status turns healthy before the last messages are written). App output streams use line timestamps as event IDs, so reconnects resume. Beyond the plan: both servers end open streams when shutdown starts (otherwise an open viewer holds every restart for the shutdown timeout); lines are cleaned up (colours, `\r` progress bars, NULs, 4 KiB cap); the app page shows the new deploy's log as soon as Deploy is clicked. CI reads both streams through the dashboard. Details in [decisions/platform.md](decisions/platform.md) ("How deploy logs are stored and streamed"). |
| 2.3 | **Push-to-deploy.** A webhook endpoint on its own listener (see decisions): HMAC signature check (`GITHUB_WEBHOOK_SECRET`, constant-time), body cap, GitHub's redelivered events ignored (delivery ID), a deploy queued for each app whose repo and branch match. A per-app auto-deploy switch (on by default). A "Deploy latest" button that looks up the branch's newest commit. Server work: Tailscale Funnel for the webhook port, the GitHub webhook, and checking the three done-when points on the real server. | ✅ `1a99873`, merged in PR #7 and deployed 2026-11-03. Migration 0007 (`apps.auto_deploy`, `apps.next_commit_sha`, `webhook_deliveries`). `internal/webhook` on its own listener (`:8082`, published on `127.0.0.1:${WEBHOOK_PORT}`), only when `GITHUB_WEBHOOK_SECRET` is set (at least 32 characters). Beyond the plan: a push that arrives mid-deploy waits as the app's next commit (newest wins) and deploys when the current deploy finishes, rather than being refused and lost; a delivery that failed before doing anything can be redelivered; the webhook's response lists what happened per app. API: `PATCH /apps/{id}` (`autoDeploy`), `POST /apps/{id}/deployments/latest`. README has the setup (Funnel on 8443, the GitHub webhook). CI signs deliveries like GitHub: ping, two pushes (the second waits or deploys), a redelivery, a forged one (401), the API absent from the webhook port, and "Deploy latest". Details in [decisions/platform.md](decisions/platform.md) ("How push-to-deploy works"). |

Verified on the server (2026-11-03, Docker 29.8.1): the three done-when
points, and the setup around them.
- **Setup:** migrations 5–7 applied on deploy. The webhook listener came
  up once `GITHUB_WEBHOOK_SECRET` was added to `.env` (an unsigned POST got
  401). `tailscale funnel status` shows only `:8443` public, forwarding to
  `127.0.0.1:8082`; the dashboard on `:443` stays tailnet-only. GitHub's
  ping was answered 200 with `pong`.
- **Push-to-deploy:** empty commits pushed to `main` deployed `hello` with
  no clicks, each superseding the last; the deliveries' responses showed
  `hello: deploying`. Two pushes a few seconds apart: the second's response
  was "deploys next, after the deploy in progress", and both deployed, one
  after the other.
- **Live build log:** lines arrived in the dashboard during the build, so
  `tailscale serve` passes the event stream through without buffering (the
  one part CI couldn't check).
- **Crash recovery:** `hello`'s cached build takes about a second, too fast
  to kill by hand, so a loop on the server polled the API and killed the
  platform (SIGKILL) the moment the deployment read `building`. That same
  deployment (#4) then finished healthy: its log has `(attempt 2)`, no new
  deployment was created, and nothing was marked failed.
- Not checked on the server: a reboot with Funnel on (`--bg` config persists
  across Tailscale restarts, so it should come back on its own).

Decisions (approved 2026-11-03), with entries in
[decisions/platform.md](decisions/platform.md):
- **Tailscale Funnel for the webhook, until there's a domain.** The plan
  says Cloudflare Tunnel, which needs a domain. The owner will buy one (see
  Open questions); until then Funnel gives a public HTTPS URL on the
  existing `homelab.<tailnet>.ts.net` name at no cost. Funnel runs on a
  separate port (8443) mapped to the webhook listener only; the dashboard on
  443 stays tailnet-only. When the domain arrives, the webhook moves to
  Cloudflare Tunnel and Funnel is turned off.
- **A separate webhook listener, published on 127.0.0.1 only.** A second
  HTTP server inside the platform that serves only the webhook route, with
  its own timeouts and limits, so the public surface is that one handler.
  This relaxes the constraint "the platform publishes no ports" (approved by
  the owner; CLAUDE.md updated). Chosen over sending the webhook through the
  dashboard's port, which would put public traffic on the process that
  serves the admin pages.
- **App logs aren't stored.** Docker keeps them (3 × 10 MB per container),
  which covers watching live and recent history. Storing them would grow the
  database with app output and keep any secrets apps print. The one gap, a
  crashed container's last lines, is for P4: the reconciler saves them in
  its event when it replaces the container. Fuller observability is P5.
- **SSE, not WebSockets,** for streaming logs: one-way, works through the
  dashboard's plain HTTP proxy, and browsers reconnect on their own.

## P3: Zero-downtime deploys and rollbacks ✅

Phase 3 of [platform-plan.md](platform-plan.md). Plan approved 2026-11-04.

**Done when** (from the plan): deploying a deliberately broken commit leaves
the site up, and rollback works from the dashboard.

Already in place from P1/P2: the new container starts next to the old one,
is health-checked through Caddy on a probe hostname, and the route switches
only once it's healthy; a failed health check marks the deploy failed and
the old version keeps serving. What's left: the old container is removed the
moment the route switches (in-flight requests can be cut off), there's no
rollback, and built images are never removed.

| Step | What | Status |
|---|---|---|
| 3.1 | **Drain the old container, and prove there's no downtime.** First, confirm what Caddy does with in-flight proxied requests on a config change (and how that interacts with the router's 10s client timeout); set an explicit `grace_period` on the `apps` server. After the route switch, wait until Caddy's `GET /reverse_proxy/upstreams` shows no requests in flight to the old upstream (bounded, ~15s), then stop it (SIGTERM, 10s stop timeout). `examples/hello` shuts down gracefully on SIGTERM, and `BROKEN=1` makes its `/healthz` answer 500 (a broken deploy on purpose). CI: requests in a loop against hello during a redeploy, zero failures; a slow request started just before the switch completes; a broken deploy leaves the old version serving. | ✅ `3799b91`, CI green. From Caddy's source: a config change returns once the old config *starts* shutting down (no clash with the 10s timeout), and in-flight counts per upstream survive config changes, so the drain works as planned. `grace_period` **not** set (it's the whole `http` app's setting, not the platform's server, and `--resume` ignores `caddy.json`); the 15s cap plus stopping the container bounds it instead. Drain checks every 250ms. Beyond the plan: a live deployment whose job is given up isn't marked failed (its log says the switch was cut off); `hello` gained `/slow?seconds=N`. Details in [decisions/platform.md](decisions/platform.md) ("Draining the old version"). |
| 3.2 | **One-click rollback, end to end.** Migration 0008 (`deployments.rollback_of`). A rollback is a new deployment reusing the target's image: queued → starting (no build) → probe → switch → drain; the version it replaces becomes `rolled_back`. Same job kind and one-in-progress rule (409 while a deploy runs). `POST /api/platform/apps/{id}/deployments/{deploymentId}/rollback` → 202; the target must have been live once and still have its image; deployments get `rollbackable`. Engine: image inspect and remove. Image retention: after a successful deploy, keep the live image plus the last 5 previously live per app, remove the rest. UI: a Rollback button (with confirm) on eligible rows, "rollback of #N", a notice that auto-deploy was turned off. CI rolls back through the dashboard (no build lines in its log) and checks the old version is served. Decision entries; server checks of the done-when. | ✅ `0e2cd25`, merged in PR #9 and deployed 2026-11-04. Migration 0008 (`rollback_of`, a foreign key on `(app_id, rollback_of)` so the database refuses another app's deployment). `store.CheckRollback` decides what's rollbackable, for both the API flag and `CreateRollback`. Image cleanup goes by what Docker lists for the app (tags not kept, and untagged images left by a redeploy of the same commit); never forced. CI deploys this commit's parent, then this commit, rolls back to the parent while the visitor keeps requesting, and checks the image, the `rolled_back` status, no build, auto-deploy off, and a 409 for rolling back to what's live. Details in [decisions/platform.md](decisions/platform.md) ("How rollbacks and image cleanup work"). |

Verified on the server (2026-11-04, Docker 29.8.1): both done-when points.
- **Setup:** migration 8 applied on deploy (`applied=[8]`).
- **No downtime:** a visitor loop on the server requested hello every 0.5s
  through a broken deploy, redeploys and a rollback: 648 requests, all 200.
- **Broken deploy:** with `BROKEN=1`, the deploy failed its health check and
  never went live; the previous version served throughout.
- **Rollback from the dashboard:** the rollback went live with no build;
  afterwards the only hello container was `app-hello-13`, running an earlier
  deployment's image (`ops-platform/hello:d7f7c51`).
- **Image cleanup:** 6 hello images left, exactly the cap.
- Not reported: the slow-request drain check (3b; CI covers it on its own
  Docker), and whether hello's auto-deploy was turned back on afterwards (if
  not, pushes to `main` don't deploy hello).

Decisions (approved 2026-11-04), with entries in
[decisions/platform.md](decisions/platform.md):
- **A rollback is a new deployment,** not the old row made healthy again:
  statuses stay forward-only and the history reads truthfully ("#12:
  rollback of #9").
- **A rollback uses the app's current env vars.** Values aren't versioned;
  versioning them is real extra scope. The UI says so.
- **A rollback turns auto-deploy off and clears a waiting pushed commit,**
  so a push already queued doesn't undo it as soon as it finishes. The owner
  turns auto-deploy back on once a fix is pushed.
- **Five previously live images are kept per app** (plus the live one),
  which is how far back a rollback can go.

## P4: Reconciliation and crash recovery ✅

Phase 4 of [platform-plan.md](platform-plan.md). Plan approved 2026-11-04.

**Done when** (from the plan): `docker kill`-ing a managed container,
deleting its route, or restarting the whole server all result in apps coming
back on their own, without touching unmanaged containers.

Already in place: at startup, the platform re-applies routes from the
database and removes orphaned app containers, before the job workers start;
Docker's `unless-stopped` restart policy brings a crashed app back, silently.
What's left: nothing runs continuously (a container or route deleted, or an
orphan left, while the platform runs stays that way until its next restart),
and nothing records or shows what was fixed.

| Step | What | Status |
|---|---|---|
| 4.1 | **The reconciler loop.** `internal/reconcile`, every 10s: desired state from Postgres (each app's live deployment and route) against actual state from Docker (managed containers only) and Caddy (its current routes). Fixes: a live container that's stopped is started again; one that's gone is recreated from its deployment's image (same name, so its route reaches it); orphans are removed; Caddy's routes are re-applied only if they differ from the database's (also fewer config reloads, see the Caddy note). Skips any app with a deploy attempt running (a per-app lock shared with the deployer, covering the drain too). Crash loops back off (10s, doubling, up to 5 min) with a "crash-looping" event. The startup cleanup becomes its first pass, still before the workers start. New app containers get restart policy `no`. Each fix is a row in `reconciler_events` (app, kind, message; a crash's last 20 output lines), kept 30 days. CI: `docker kill` and `docker rm -f` on hello, back within ~20s; its route deleted through Caddy's admin API, back; an unlabelled container and a stopped one, untouched. | ✅ `dcd1e50`, CI green. Migration 0009 (`reconciler_events`). Beyond the plan: a pass does **routes first**, and removes orphans only once Caddy's routes are known to match the database (a deploy whose route switch failed keeps the old container because Caddy still routes to it; removing it first would take the site down). A `dead` live container is removed and recreated. A fix that keeps failing is recorded once, not every pass. Containers of apps that no longer exist are removed (after re-listing apps, so a just-registered app isn't mistaken for a deleted one). CI also stops hello while the platform is down (a reboot, in miniature) and checks the first pass starts it; its teardown now stops the platform first, or the reconciler recreates what it removes. Details in [decisions/platform.md](decisions/platform.md) ("How the reconciler works"). |
| 4.2 | **Events in the dashboard, and the server checks.** `GET /api/platform/events` (all apps) and events in the app detail. "Recent events" on the Apps page and each app's page ("restarted hello: container exited (code 137)"), a crash's last lines expandable. CI reads events through the dashboard. Decision entries. Server checks: `docker kill`, deleting the route, and a real `sudo reboot`. | ✅ `acc52ef`, merged in PR #11 and deployed 2026-11-04. API: `events` in the app detail (last 20) and `GET /api/platform/events` (last 50). UI: "Recent events" on the Apps page (polled every 10s, like the apps) and "Events" on each app's page, with a badge per kind and a crash's output folding out. CI reads hello's events and the routes fix through the dashboard. |

Verified on the server (2026-11-04, Docker 29.8.1): all three done-when
points.
- **Setup:** migration 9 applied on deploy, and the first reconcile pass
  ran. hello's container from before the deploy still had `unless-stopped`
  (the merge's push had deployed it through the old platform); a fresh
  "Deploy latest" gave it restart policy `no`, so the checks below were the
  reconciler's work, not Docker's.
- **`docker kill`:** the same container (`app-hello-16`) was started again
  within a pass ("had exited with code 137"); its `StartedAt` matched the
  log line to the second.
- **`docker rm -f`:** recreated under the same name (a new container ID),
  serving straight away through the unchanged route.
- **Route deleted** through Caddy's admin API: re-applied within seconds
  ("re-applied Caddy's routes"), before the next request even saw it
  missing.
- **`sudo reboot`:** Docker brought back the compose services, `mc` and the
  Discord bot by their own policies; the platform's first pass started
  `app-hello-16` a second after it came up ("had exited with code 0":
  Docker had stopped it cleanly at shutdown), and hello answered 38s into
  the boot. Tailscale Funnel came back on `:8443` by itself (the reboot
  check left open since P2).
- Not reported: the uptime check of `mc` and the bot before the reboot
  (step 6); CI checks unmanaged containers are untouched, and the reconciler
  only lists containers with the platform's label.

Decisions (approved 2026-11-04), with entries in
[decisions/platform.md](decisions/platform.md):
- **The reconciler restarts crashed apps, not Docker:** new app containers
  get restart policy `no`. One actor, backoff under the platform's control,
  and every crash becomes an event with its last lines (with Docker
  restarting silently, the reconciler would only see a restart count go
  up). The cost: while the platform itself is down, a crashed app stays
  down, and after a reboot apps come back with the platform (seconds after
  Postgres), not with Docker. Existing containers keep `unless-stopped`
  until their next deploy.
- **Polling every 10s, not Docker's event stream:** events would need the
  `EVENTS` proxy group (a new Docker permission) to react seconds sooner. A
  pass costs one container list and one Caddy GET.
- **An in-process per-app lock** coordinates the reconciler with deploys,
  not Postgres advisory locks: there's only ever one platform process (the
  startup cleanup already relies on it), and several instances are a
  non-goal.
- **A deleted live container is recreated directly,** with no new
  deployment and no health-check gate: the site is down already, and the
  next pass checks again. A redeploy would rebuild, take minutes and clutter
  the history.

## P5: Databases for apps, observability, public hostnames ✅

Phase 5 of [platform-plan.md](platform-plan.md), adjusted. Plan approved
2026-11-04.

**Done when** (adjusted from the plan, see below): `examples/hello` runs
with its own database, backed up daily to `~/backups` (and restorable),
shows up with CPU, memory and request metrics in the dashboard, and is
reachable at a public hostname on `sashasagebd.com`.

**Adjusted from the plan:** its done-when is the pokewordle backend running
with its own database and metrics. That backend doesn't exist yet; the owner
will write it after P5, so `hello` stands in for it, and deploying
pokewordle afterwards is registering it, adding a database and giving it
`api.sashasagebd.com` (or `pokewordle-api.`). The domain was bought for this
(2026-11-04), so public hostnames through Cloudflare Tunnel, deferred since
P2, are this phase's last step.

What exists: the dashboard samples every container's CPU and memory and
keeps 24h of trends, but keyed by container name, which changes with each
deploy (`app-hello-16`); nothing counts requests; the reconciler already
makes desired state real, which a database fits.

| Step | What | Status |
|---|---|---|
| 5.1 | **Databases for apps.** "Add a database" on an app's page: one per app, created once; its password encrypted like env vars. The reconciler creates and keeps it running: a `postgres:18.6` container (its own memory limit) with its data on a named volume, on an internal Docker network of the app's own that only its database and its containers join (never `platform-apps`, so other apps can't reach it). A recreated container finds its data on the volume. App containers get `DATABASE_URL` from the next deploy. No new Docker permissions (a container create makes its volume; `IMAGES` pulls, `NETWORKS` creates the network); database containers are the one exception to "no mounts", a named volume only, never a host path. Deleting a database isn't in P5, and the platform never removes a volume. `examples/hello` gets a `/visits` counter in its database; CI checks the count survives a redeploy, a `docker rm -f` of the database container and a rollback, and that another app can't reach it. | ✅ `ca49e4a`, CI green. Migration 0010 (`app_databases`, and a `database_created` event kind). Beyond the plan: `provisioned_at` records the database's first creation, so the reconciler tells "not created yet" (event `database_created`) from "its container was deleted" (`recreated`, data kept) without the Docker volumes permission; the dashboard shows "being created" until then, and "Deploy the app to start using it" while the live version predates the database. The platform's `DATABASE_URL` replaces a user env var of that name. Names derived: `ops-db-<app>` for the container, volume, network and hostname. **Not checked in CI: a rollback with the database** (CI's only other image of hello is the parent commit's, without `/visits`); rollbacks start containers through the same code as deploys, which unit tests cover. CI also checks the password appears in neither the API nor a deploy log. Details in [decisions/platform.md](decisions/platform.md) ("How app databases work"). |
| 5.2 | **Backups.** A daily `pg_dump` per database, as a job in the queue (retried, survives restarts), plus "Back up now". Run in a short-lived `postgres:18.6` container on the app's database network, writing into a volume compose defines as a bind of `BACKUP_DIR` (`~/backups`), so the platform never names a host path; files owned by the owner's user, not root. Keep 7 daily per app. The last backup's time and size on the app page. Restore is a documented manual `pg_restore`; CI backs up, drops hello's table, restores, and checks the count is back. | ✅ `402b27d`, CI green. Migration 0011 (`backups`; jobs get `backup_id` and a check that each kind has exactly its own input). `jobs.Kinds` dispatches jobs by kind. Beyond the plan: compose only creates a volume a service uses, so the platform mounts `platform-backups` read-only and checks each dump's size there (proof it landed); dumps go to a hidden partial file renamed when complete; the backup container is always removed (it holds the password). **Needs on the server before deploying:** `mkdir -p ~/backups` and `BACKUP_DIR=/home/sasha/backups` in `.env` (compose refuses to start without it). Details in [decisions/platform.md](decisions/platform.md) ("How backups work"). |
| 5.3 | **Per-app metrics.** CPU and memory: the dashboard's history also keeps a series per platform app (`app:<name>`, by the container's app label), surviving deploys, shown as sparklines on the app page. Requests: the platform turns on Caddy's per-host metrics, reads them from the admin API every minute, and keeps 24h per app in memory (requests per minute, the 5xx share, p95 latency), on the app page and as last-hour requests on the Apps list. CI sends hello known requests and checks the counts. | ✅ `2e24bf6`, CI green. CPU and memory: the dashboard's Docker client now keeps labels; `/api/history` has `apps` (each summing the app's own containers, not its database or backups). Requests: `internal/metrics` with a small parser for Caddy's Prometheus text (no new dependency), p95 estimated like `histogram_quantile`, a Caddy restart's reset handled, minutes without a reading are gaps. The reconciler keeps per-host metrics on (logged, not an event). API: `GET /apps/{id}/metrics?window=1h|24h`, `requestsLastHour` on apps. UI: a Metrics section (CPU, memory, requests, 5xx, p95) on the app page, requests in the last hour on the Apps list. Details in [decisions/platform.md](decisions/platform.md) ("How per-app metrics work"). |
| 5.4 | **Public hostnames through Cloudflare Tunnel.** A `cloudflared` service in compose that reaches Caddy over a network of its own (no published port, no port forwarding). Each app gets an optional public hostname (e.g. `api.sashasagebd.com`) alongside its tailnet one; apps without one stay tailnet-only, and the dashboard and the platform's API stay tailnet-only. The GitHub webhook moves to `hooks.sashasagebd.com` and Tailscale Funnel is turned off. The owner does the Cloudflare side (the tunnel and its token) from exact steps. Server checks of the done-when. | ✅ `179dc14`, CI green, deployed 2026-11-05. Migration 0012 (`apps.public_name`). Caddy gets a second server, `public` on :8080, the only one the tunnel reaches (`cloudflared` shares the `tunnel` network with Caddy alone): public apps' routes and `hooks.`, then a 404; both servers in one PATCH. Beyond the plan: the tailnet server refuses the tunnel's subnet (403), since `cloudflared` can connect to any port Caddy listens on and a route to `caddy:80` would otherwise expose tailnet-only apps. Public on the app page ("Public access": a name, confirmed; routes change at once). `cloudflared` 2026.10.7 behind the `tunnel` compose profile, from a token. The webhook's published port is gone. Request metrics are now per app (tailnet and public hostnames summed). CI checks through Caddy's public server from the tunnel network: the webhook, an app made public and private again, 404 for tailnet hostnames, 403 on the tailnet server, nothing else reachable. Details in [decisions/platform.md](decisions/platform.md) ("How public hostnames work"). |

Verified on the server (2026-11-05, Docker 29.8.1): every done-when point.
- **Setup:** migrations 10–12 applied on deploy. `.env` gained
  `BACKUP_DIR` (and the owner's uid/gid), `PUBLIC_DOMAIN`, the tunnel token
  and `COMPOSE_PROFILES=tunnel`. `cloudflared` registered four connections
  (Seattle) running read-only with no capabilities. Funnel turned off;
  the dashboard stays tailnet-only on 443. hello redeployed with "Deploy
  latest" first (the merge had deployed it on the old platform).
- **Cloudflare:** the tunnel `homelab` (remotely managed), one route:
  `*.sashasagebd.com` → `http://caddy:8080`. The dashboard accepted the
  wildcard and made its DNS record, so new public names need no Cloudflare
  change. Tunnels are now under Networking → Tunnels (moved from Zero Trust
  in 2026), and the setup wizard can't continue until a connector connects,
  so the route was added after the deploy. An unknown name
  (`nothing-here.`) gets Caddy's 404.
- **Public hostname:** hello made public as `hello`; it answered at
  `https://hello.sashasagebd.com` from the server and from a phone off Wi-Fi
  and Tailscale. A request to it with a tailnet `Host` header is refused by
  Cloudflare itself (403) before it reaches the tunnel.
- **Webhook:** GitHub's URL changed to
  `https://hooks.sashasagebd.com/webhooks/github`; a redelivery got 200.
- **Database:** added to hello and deployed; `/visits` counted up (3, 4)
  through the public hostname.
- **Backup:** "Back up now" wrote `~/backups/hello/hello-20261105-021455.dump`
  (1.6 KB), owned by `sasha`; `pg_restore --list` reads it (7 entries,
  gzip). The full drop-and-restore is CI's.
- **Metrics:** CPU, memory and requests show on hello's page.
- Not checked on the server: a reboot with the tunnel (it has
  `restart: unless-stopped` like the rest, and reconnects outbound on its
  own), and the daily scheduled backup (it runs within 24h of the last).

Decisions (approved 2026-11-04), written up in
[decisions/platform.md](decisions/platform.md) as they land:
- **One Postgres container per app,** not one shared instance with a
  database per app: isolation (one app can't exhaust another's connections
  or memory), per-app backups, independent upgrades later. About 30-60 MiB
  per database, fine for a few apps.
- **The platform owns Caddy's `apps.http.metrics`** as well as its `apps`
  server: it sets per-host metrics on, and the reconciler keeps it so.
  Caddy's per-server `metrics` would stay inside the platform's server, but
  it's deprecated (a warning on every config load). It's the one path
  outside the platform's own server; `grace_period` (P3) was skipped for
  that reason, metrics are worth it.
- **Backups daily, 7 kept, in `~/backups`,** through a compose-defined bind;
  restore tested in CI.
- **Request metrics in memory, 24h,** like the dashboard's history: reset by
  a platform restart, no table or pruning for numbers that matter mostly
  now.
- **The domain is `sashasagebd.com`** (Cloudflare Registrar, 2026-11-04).
  Public names stay one level deep (`api.sashasagebd.com`, not
  `api.pokewordle.sashasagebd.com`): Cloudflare's free certificate covers
  the bare domain and one level of subdomains only.

## UI: Frontend redesign 🔍

Plan approved 2026-11-05 (owner: "ops console" direction, tabs on the app
page, plain CSS; mocked up first, then done as one step at the owner's
request, since it's frontend only).

**Goal:** a dashboard that looks finished at first sight (portfolio), and an
app page that's easy to find your way around (it was one long scroll of
nine sections).

What changed:
- **A shell:** a sidebar with Overview, Containers, Apps, the apps with a
  status dot each, a System / Light / Dark theme switch and whether the
  platform answers; on phones a top bar with a menu button.
- **An Overview home** (`#/`): server tiles, every app as a card, recent
  events and the busiest containers. Containers moved to `#/containers`.
- **Apps as cards**, with status, public and tailnet addresses, live commit,
  last-hour requests and auto-deploy.
- **The app page in tabs** (`#/apps/{id}/{tab}`, kept in the URL): a header
  that stays (status, addresses, auto-deploy, a waiting push, Deploy
  latest), then Overview (metric cards, live version, events, about),
  Deployments (deploy a commit, the table, the chosen deploy's log), Logs
  (live output), Database (database and backups), Settings (public access,
  push to deploy, env vars). Arrow keys move between tabs (ARIA tabs).
- **Look:** cool greys, one blue accent, green/amber/red status, Inter and
  JetBrains Mono (self-hosted, OFL), light and dark from one set of tokens.
- **The theme switch** is remembered per browser and applied before the
  page draws (an inline script in `index.html`), so dark doesn't flash light.
- No backend change; no new dependencies. Tests were moved to the new
  layout, plus tests for the URL's tab, keyboard tabs and the theme switch.

## Open questions

- **pokewordle's Worker and its backend** (for after P5): worth checking
  whether the Worker could reach the backend through the tunnel without a
  public hostname (Cloudflare "Workers VPC"). Until then, the backend gets a
  public hostname and handles CORS itself.

Resolved:
- **M2 plan** approved 2026-10-01.
- **M3 plan** approved 2026-10-01.
- **M5 plan** approved 2026-10-01; 5.4 (demo mode) dropped 2026-10-06.
- **M6 plan** approved 2026-10-06.
- **P0 plan** approved 2026-10-28.
- **P1 plan** approved 2026-11-02.
- **P2 plan** approved 2026-11-03.
- **P3 plan** approved 2026-11-04.
- **P4 plan** approved 2026-11-04.
- **P5 plan** approved 2026-11-04 (adjusted: hello stands in for pokewordle, public hostnames added).
- **Domain for public apps** (2026-11-04): `sashasagebd.com`, bought on Cloudflare Registrar; set up in 5.4.
- **Docker version on the server** (2026-11-02): 29.8.1, API 1.56; see P1's
  risks for what it means for building.
- **Discord bot** is already Dockerized (`~/apps/discordbot`: Node/TS with a
  `Dockerfile` and `compose.yaml`). It wasn't started; it was brought up with
  `docker compose up -d` on 2026-10-01 and the dashboard shows it with no code
  changes. No non-Docker checks needed.
- **Stardew Valley server** isn't set up yet (answered 2026-10-01). If it's run
  in Docker when set up, the dashboard picks it up with no code changes.

## Ideas / notes

- **Local dev has no Docker data.** Local `go run` can't reach a socket proxy,
  so the page shows "could not list containers". Solved in 2.4 with
  `API_TARGET` (Vite forwards `/api` to the server's Tailscale URL); see the
  README.
- **Demo mode (dropped from M5 as 5.4).** Worth revisiting if Playwright
  end-to-end tests are added: they need a backend with stable fake data. It
  would be an `internal/demo` package implementing `monitor.Docker` and
  `monitor.HostReader` (generating cumulative CPU counters, not percentages,
  so the real CPU % code runs), switched on by `DEMO=1` in `main.go`.
- **Minecraft is published on `0.0.0.0:25565`**, so it's reachable from the LAN
  (and the internet, if the router forwards it) regardless of UFW. Fine if
  intended; not a dashboard issue.
- **Unhealthy doesn't restart anything.** Plain Docker only records health;
  `restart: unless-stopped` acts when a container exits, not when it's
  unhealthy. The badge is information, not recovery. (Autoheal-style tools
  exist if that's ever wanted, but they need write access to Docker.)
- **Local `-race` on Windows** needs gcc; WinLibs is installed via winget and
  on the user PATH (a fully restarted VS Code picks it up).
