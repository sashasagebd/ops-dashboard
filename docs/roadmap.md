# v1 Roadmap

Milestones for the v1 scope in [spec.md](spec.md). Each numbered step is one
small, reviewed commit. Design reasoning lives in [decisions/](decisions/).

_Last updated: 2026-10-01_

| Milestone | Status |
|---|---|
| M1: Thinnest end-to-end slice | ✅ Done, deployed |
| M2: Full container status | 📝 Planned, awaiting approval |
| M3: Host stats | Not started |
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

## M2: Full container status 📝

**Goal:** every container (including stopped), with up/down status, uptime,
CPU % and RAM, refreshing automatically.

| Step | What |
|---|---|
| 2.1 | List all containers (`all=true`) and inspect each for `startedAt` / `finishedAt`. Additive API fields; page shows "up 3h 12m" / "down for 20m". |
| 2.2 | CPU/RAM in the Docker client via the stats endpoint. |
| 2.3 | Background poller (`POLL_INTERVAL`, default 5s) holding the latest snapshot in memory. API becomes `{updatedAt, containers: [...]}`; frontend fetch updated in the same step. |
| 2.4 | Frontend: uptime/CPU/RAM columns, auto-refresh, pause when tab hidden, stale-data banner. Add a way to develop the UI with real data (see ideas below). |
| 2.5 | Redeploy; record the poller decision in `decisions/stack.md`. |

Proposed decisions:
- **CPU % from two polls.** Use the stats endpoint's `one-shot` mode (instant,
  single sample) and compute CPU % from the previous poll's sample, instead of
  Docker's default ~1s wait per container. CPU shows "—" for the first poll.
- **RAM like `docker stats`:** usage minus reclaimable file cache.
- **Stale over broken:** if Docker stops answering, keep serving the last
  snapshot marked stale; 502 only if there has never been a successful poll.
- **Poller tests trigger polls manually**, so they don't depend on real time.
- **Small custom polling hook** in React, not TanStack Query, until we need more.

`docker ps -a` on the server shows no leftover containers, so "all containers"
needs no filtering.

## M3: Host stats

CPU, memory and disk for the host, from the host's `/proc` and `/` mounted
read-only into the container, behind its own interface so it can be faked.

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

- **Stardew Valley server and Discord bot aren't Docker containers** (not in
  `docker ps -a`). Are they not set up yet, running outside Docker, or
  candidates to containerize? v1 only monitors containers, so this decides
  whether they need a separate check (e.g. systemd), probably as a milestone
  after M4.
- **M2 plan** needs approval before 2.1 starts.

## Ideas / notes

- **Local dev has no Docker data.** Local `go run` can't reach a socket proxy,
  so the page shows "could not list containers". Options: point Vite's `/api`
  forwarding at the server's Tailscale URL (read-only, tailnet-only), or a
  sample-data mode in the backend.
- **Minecraft is published on `0.0.0.0:25565`**, so it's reachable from the LAN
  (and the internet, if the router forwards it) regardless of UFW. Fine if
  intended; not a dashboard issue.
- **Local `-race` on Windows** needs gcc; WinLibs is installed via winget and
  on the user PATH (a fully restarted VS Code picks it up).
