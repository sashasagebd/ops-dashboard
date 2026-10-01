# Ops Dashboard

A self-hosted monitoring dashboard for my home server: which containers are
running, how the host is doing, and a Discord alert when a service goes down.

See [docs/spec.md](docs/spec.md) for scope and [docs/decisions/](docs/decisions/)
for the reasoning behind the main design choices.

## Layout

| Path | What |
|---|---|
| `backend/` | Go API server. Also serves the built frontend in production. |
| `frontend/` | React + TypeScript + Vite single-page app. |
| `Dockerfile` | Builds the frontend and backend into one small image. |
| `compose.yaml` | The dashboard plus a read-only Docker socket proxy. |

## How it reaches Docker

```
browser ──tailscale serve──▶ 127.0.0.1:8080 ──▶ dashboard ──docker-api network──▶ docker-proxy ──▶ /var/run/docker.sock
```

The dashboard container never mounts the Docker socket. It talks HTTP to
[tecnativa/docker-socket-proxy](https://github.com/Tecnativa/docker-socket-proxy),
which only allows read requests to the container endpoints. The proxy has no
published port and sits on an internal network only the dashboard can reach.

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

Checks (the same ones CI runs):

```sh
cd backend && gofmt -l . && go vet ./... && go test -race ./...
cd frontend && npm run lint && npm test && npm run build
```

## Configuration

| Variable | Default | Used by |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | backend: address to listen on |
| `DOCKER_HOST` | `tcp://docker-proxy:2375` | backend: socket proxy address (`tcp://` only) |
| `STATIC_DIR` | unset | backend: built frontend to serve (set to `/static` in the image) |
| `DASHBOARD_PORT` | `8080` | compose: host port, always bound to `127.0.0.1` |

## Deploying to the server

On the server (Ubuntu, with Docker, the Compose plugin and Tailscale installed):

```sh
mkdir -p ~/apps
git clone https://github.com/sashasagebd/ops-dashboard.git ~/apps/dashboard
cd ~/apps/dashboard
cp .env.example .env        # adjust DASHBOARD_PORT if 8080 is taken
docker compose up -d --build
```

Check it's up, from the server:

```sh
docker compose ps
curl -s http://127.0.0.1:8080/healthz          # {"status":"ok"}
curl -s http://127.0.0.1:8080/api/containers   # JSON list of running containers
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
