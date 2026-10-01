# Ops Dashboard — Spec

A self-hosted monitoring dashboard for my home server.

## Context
- Server: Lenovo ThinkCentre M700 Tiny, Ubuntu Server, Docker + Docker Compose
- Services running on it: a Minecraft server (itzg/minecraft-server container), a Stardew Valley server, and a Discord bot, with personal websites coming later
- Remote access is through Tailscale

## Stack and layout
- Monorepo: `backend/` (Go) and `frontend/` (React + TypeScript + Vite)
- Deployed with Docker Compose, under `~/apps/dashboard` on the server
- GitHub Actions CI (`.github/workflows/ci.yml`) runs gofmt, go vet, go test -race, and the frontend lint/test/build

## v1 scope
1. A page listing each container with up/down status, uptime, and CPU/RAM usage
   - Shows **all** containers on the host (no label-based opt-in for now)
2. Host stats: CPU, memory, disk usage
3. A Discord webhook alert when a service goes down or comes back up

## Out of scope for v1
- Logs
- Restart buttons (or any other action that changes container state)
- History graphs
- Auth beyond Tailscale

## Constraints
- Container data is read through a read-only Docker socket proxy (e.g. tecnativa/docker-socket-proxy). `/var/run/docker.sock` is never mounted directly into the dashboard container.
- The dashboard is published only on `127.0.0.1` (Docker bypasses UFW) and exposed via `tailscale serve`.
- Docker access sits behind a Go interface so business logic can be tested against a fake.
- Secrets (like the Discord webhook URL) come from environment variables, with a committed `.env.example`.
