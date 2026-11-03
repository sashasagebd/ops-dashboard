# Ops Dashboard

A self-hosted monitoring dashboard for my home server: which containers are
running, what they're using, how the host is doing, and how that has changed
over the last day.

See [docs/spec.md](docs/spec.md) for scope and [docs/decisions/](docs/decisions/)
for the reasoning behind the main design choices.

**In progress:** turning this into a small self-hosted deploy platform, with
the dashboard as its UI; see [docs/platform-plan.md](docs/platform-plan.md).
So far that's a separate `platform` service with its own Postgres; it doesn't
deploy anything yet.

## Layout

| Path | What |
|---|---|
| `backend/` | Go: `cmd/dashboard` (API server; also serves the built frontend in production) and `cmd/platform` (the deploy platform). |
| `frontend/` | React + TypeScript + Vite single-page app. |
| `Dockerfile` | Builds the frontend and both Go binaries into one small image. |
| `compose.yaml` | The dashboard and its read-only Docker socket proxy; the platform, its Postgres, its write proxy, and Caddy. |
| `caddy/caddy.json` | Caddy's bootstrap config: its admin address and an empty app server, whose routes the platform manages. |

## How it reaches Docker

```
browser ──tailscale serve──▶ 127.0.0.1:8080 ──▶ dashboard ──docker-api network──▶ docker-proxy ────▶ /var/run/docker.sock
                                                 platform ──docker-rw network───▶ docker-proxy-rw ─▶ /var/run/docker.sock
```

The dashboard container never mounts the Docker socket. It talks HTTP to
[tecnativa/docker-socket-proxy](https://github.com/Tecnativa/docker-socket-proxy),
which only allows read requests to the container endpoints. The proxy has no
published port and sits on an internal network only the dashboard can reach.

The platform needs to build images and run containers, so it has a second
proxy of its own that allows writes, limited to the container, image, build
and network endpoints (no exec, volumes, secrets and so on). It's on a
separate internal network that only the platform joins. See
[docs/decisions/platform.md](docs/decisions/platform.md) for why the two are
kept apart.

## Local development

You need Go and Node 24 (LTS).

```sh
# Terminal 1: the API on :8080. Without a socket proxy running,
# /api/containers returns 502, which is expected.
cd backend
go run ./cmd/dashboard

# Terminal 2: the frontend on :5173, forwarding /api to :8080.
cd frontend
npm install
npm run dev
```

To work on the UI with real data instead, skip the Go server and point the
frontend at the deployed dashboard (your PC must be on the tailnet). The API is
read-only, so this can't change anything on the server.

```sh
cd frontend
API_TARGET=https://<server-name>.<tailnet>.ts.net npm run dev    # bash, incl. Git Bash
# PowerShell: $env:API_TARGET="https://<server-name>.<tailnet>.ts.net"; npm run dev
```

Use the same URL you open the dashboard at (`tailscale serve status` on the
server prints it).

Checks (the same ones CI runs):

```sh
cd backend && gofmt -l . && go vet ./... && go test -race ./...
cd frontend && npm run lint && npm test && npm run build
```

## Configuration

| Variable | Default | Used by |
|---|---|---|
| `LISTEN_ADDR` | `:8080` (dashboard), `:8081` (platform) | backend: address to listen on |
| `DOCKER_HOST` | `tcp://docker-proxy:2375` (dashboard), `tcp://docker-proxy-rw:2375` (platform) | backend: socket proxy address (`tcp://` only) |
| `POLL_INTERVAL` | `5s` | dashboard: how often to poll Docker (Go duration, minimum `1s`) |
| `STATIC_DIR` | unset | dashboard: built frontend to serve (set to `/static` in the image) |
| `DATABASE_URL` | required | platform: Postgres connection URL (compose builds it from `POSTGRES_PASSWORD`). Database tests in `internal/store` also use it, and skip without it. |
| `DASHBOARD_PORT` | `8080` | compose: host port, always bound to `127.0.0.1` |
| `POSTGRES_PASSWORD` | required | compose: password for the platform's Postgres; `openssl rand -hex 32`. Only read when the database volume is first created. |
| `PLATFORM_SECRET_KEY` | required | platform: key that encrypts app env vars (AES-256-GCM); `openssl rand -hex 32`. Back it up: without it, stored env var values are unrecoverable. |
| `APPS_DOMAIN` | required | platform: apps are served at `<name>.<APPS_DOMAIN>`; for now `<tailscale-ip-with-dashes>.sslip.io` |
| `GITHUB_TOKEN` | unset | platform: optional token for downloading app code (higher rate limit, private repos); read-only Contents access is enough |
| `APPS_NETWORK`, `CADDY_ADMIN_URL`, `CADDY_PROXY_URL` | `platform-apps`, `http://10.203.0.2:2019`, `http://10.203.0.2:80` | platform: set in `compose.yaml`; only change them together with it |
| `API_TARGET` | `http://localhost:8080` | frontend dev server only: where `npm run dev` forwards `/api` |

## Deploying to the server

On the server (Ubuntu, with Docker, the Compose plugin and Tailscale installed):

```sh
mkdir -p ~/apps
git clone https://github.com/sashasagebd/ops-dashboard.git ~/apps/dashboard
cd ~/apps/dashboard
cp .env.example .env        # adjust DASHBOARD_PORT if 8080 is taken
sed -i "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=$(openssl rand -hex 32)/" .env
sed -i "s/^PLATFORM_SECRET_KEY=.*/PLATFORM_SECRET_KEY=$(openssl rand -hex 32)/" .env
grep PLATFORM_SECRET_KEY .env   # copy this into a password manager
docker compose up -d --build
```

Check it's up, from the server:

```sh
docker compose ps                              # dashboard shows "(healthy)" ~30s after start; platform and postgres within seconds
curl -s http://127.0.0.1:8080/healthz          # {"status":"ok"}
curl -s http://127.0.0.1:8080/api/containers   # JSON: host stats plus every container, including stopped ones
curl -s http://127.0.0.1:8080/api/history      # JSON: the last hour per minute; starts empty after every restart
```

Expose it to your tailnet over HTTPS (needs MagicDNS and HTTPS certificates
enabled in the Tailscale admin console). This keeps running across reboots:

```sh
sudo tailscale serve --https=443 --bg 127.0.0.1:8080
tailscale serve status
```

The dashboard is then at `https://<server-name>.<tailnet>.ts.net/`.

### Updating

```sh
cd ~/apps/dashboard
git pull
docker compose up -d --build
```

The platform's database lives in the `dashboard_platform-db` volume, which
survives `docker compose down` and rebuilds. `docker compose down -v` deletes
it.
