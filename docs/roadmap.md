# v1 Roadmap

Milestones for the v1 scope in [spec.md](spec.md). Each numbered step is one
small, reviewed commit. Design reasoning lives in [decisions/](decisions/).

_Last updated: 2026-10-06_

| Milestone | Status |
|---|---|
| M1: Thinnest end-to-end slice | ✅ Done, deployed |
| M2: Full container status | ✅ Done, deployed |
| M3: Host stats | ✅ Done, deployed |
| M4: Discord alerts | ⏸️ Deferred (not in v1; plan kept below) |
| M5: Portfolio polish | 🚧 In progress (5.1–5.3 done, 5.4 dropped) |
| M6: History sparklines | 🚧 Code done (6.1–6.4, docs for 6.5); not committed or deployed |

**Where we left off (2026-10-06):** 5.1–5.3 are committed but not deployed.
M6 was started ahead of finishing v1 (owner's call); 5.5 and 5.6 are still
to do. Next:
1. Check the new **Docker image** job in GitHub Actions is green (it's the
   only test of the image itself).
2. Deploy (`cd ~/apps/dashboard && git pull && docker compose up -d --build`),
   then check `docker ps` shows the dashboard `(healthy)` after ~30s and the
   page shows `healthy` badges on `mc` and the dashboard.
3. Review M6 (6.1–6.4), commit, push, then deploy and run the 6.5 checks
   (they cover the 5.1–5.3 checks above too). Then back to 5.5 and 5.6.

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

## M5: Portfolio polish 🚧

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

## M6: History sparklines 🚧

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

## Open questions

None right now.

Resolved:
- **M2 plan** approved 2026-10-01.
- **M3 plan** approved 2026-10-01.
- **M5 plan** approved 2026-10-01; 5.4 (demo mode) dropped 2026-10-06.
- **M6 plan** approved 2026-10-06.
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
