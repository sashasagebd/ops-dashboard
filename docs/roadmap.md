# v1 Roadmap

Milestones for the v1 scope in [spec.md](spec.md). Each numbered step is one
small, reviewed commit. Design reasoning lives in [decisions/](decisions/).

_Last updated: 2026-10-01_

| Milestone | Status |
|---|---|
| M1: Thinnest end-to-end slice | ✅ Done, deployed |
| M2: Full container status | ✅ Done, deployed |
| M3: Host stats | 🚧 In progress |
| M4: Discord alerts | Not started |
| M5: Portfolio polish | Not started |

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

## M3: Host stats 🚧

**Goal:** CPU, memory and disk for the whole server, above the container
table, refreshing with it.

| Step | What | Status |
|---|---|---|
| 3.1 | `internal/host` package: parse `/proc/stat` (CPU, from two samples like containers) and `/proc/meminfo` (used = `MemTotal` − `MemAvailable`); disk usage via `statfs`. Parsers tested against real fixture text; `statfs` is Linux-only, so it sits behind a build tag with a stub for Windows dev. | ✅ `7d88367`; CI (Ubuntu) passed, including the real disk read |
| 3.2 | Monitor reads host stats on each poll; API gains a `host` object (additive: `{updatedAt, stale, host, containers}`). `host` is `null` if the read fails; containers are unaffected. | ✅ Done, not yet committed |
| 3.3 | Frontend: three summary tiles (CPU %, memory used / total, disk used / total) with a usage bar. Page is now "Ops Dashboard" with Server and Containers sections; bars amber at 80%, red at 90%; "Server stats unavailable." when `host` is null. | ✅ Done, not yet committed |
| 3.4 | Deploy; check the numbers against `top`, `free -h` and `df -h /` on the server; record decisions. | |

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

## M4: Discord alerts

Detect up↔down transitions from the poller's snapshots and post to
`DISCORD_WEBHOOK_URL`. No alerts on startup (first poll is a baseline); a
change only counts after N consecutive polls, so a quick restart doesn't
alert twice. Known limitation: if the whole host is down, nothing alerts.

## M5: Portfolio polish

README with architecture diagram and screenshots, decision records, a Docker
`HEALTHCHECK` (the distroless image has no curl, so the binary needs a
`-healthcheck` mode), and a CI job that builds the Docker image.

## Open questions

None right now.

Resolved:
- **M2 plan** approved 2026-10-01.
- **M3 plan** approved 2026-10-01.
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
  README. A sample-data mode would still help for demos/screenshots (M5).
- **Minecraft is published on `0.0.0.0:25565`**, so it's reachable from the LAN
  (and the internet, if the router forwards it) regardless of UFW. Fine if
  intended; not a dashboard issue.
- **Local `-race` on Windows** needs gcc; WinLibs is installed via winget and
  on the user PATH (a fully restarted VS Code picks it up).
