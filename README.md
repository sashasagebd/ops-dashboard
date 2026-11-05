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
| `examples/hello/` | A minimal app to deploy on the platform (its own Go module). |
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
| `APPS_LISTEN_IP` | required | compose: the IP Caddy serves apps on (port 80); the server's Tailscale IP. Never `0.0.0.0`: Docker bypasses UFW. |
| `PLATFORM_URL` | `http://platform:8081` | dashboard: where it forwards `/api/platform/` (set in `compose.yaml`) |
| `GITHUB_TOKEN` | unset | platform: optional token for downloading app code (higher rate limit, private repos); read-only Contents access is enough |
| `GITHUB_WEBHOOK_SECRET` | unset | platform: turns on push-to-deploy; the GitHub webhook's secret, at least 32 characters (`openssl rand -hex 32`). Unset, the webhook listener doesn't run. |
| `WEBHOOK_LISTEN_ADDR` | `:8082` | platform: the webhook listener's address (only `POST /webhooks/github` is served there) |
| `WEBHOOK_PORT` | `8082` | compose: host port for the webhook listener, always bound to `127.0.0.1` (Tailscale Funnel forwards to it) |
| `BACKUP_DIR` | required | compose: where app database backups are written (an existing directory, e.g. `/home/sasha/backups`) |
| `BACKUP_UID`, `BACKUP_GID` | `1000`, `1000` | compose: who backups are written as, so the files are yours (`id -u`, `id -g`) |
| `BACKUP_VOLUME`, `BACKUP_USER`, `BACKUP_MOUNT` | `platform-backups`, from `BACKUP_UID:BACKUP_GID`, `/backups` | platform: set in `compose.yaml`. Without `BACKUP_VOLUME`, backups are off. |
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

### Push-to-deploy

A push to an app's branch deploys it (apps can turn this off on their page).
GitHub sends the push to a webhook listener the platform runs on its own
port, published on `127.0.0.1:8082` only, and Tailscale Funnel gives that
one port a public HTTPS address. The dashboard on 443 stays tailnet-only.
Every delivery is signed with a shared secret, and the platform refuses any
that isn't.

1. Set the secret in `.env` and restart the platform:
   ```sh
   cd ~/apps/dashboard
   sed -i "s/^GITHUB_WEBHOOK_SECRET=.*/GITHUB_WEBHOOK_SECRET=$(openssl rand -hex 32)/" .env
   grep GITHUB_WEBHOOK_SECRET .env   # you'll paste this into GitHub
   docker compose up -d
   docker compose logs platform | grep 'listening addr=:8082'
   ```
2. Allow Funnel for the server in the Tailscale admin console (Access
   controls: the `funnel` node attribute), then publish the webhook port.
   This keeps running across reboots:
   ```sh
   sudo tailscale funnel --bg --https=8443 http://127.0.0.1:8082
   tailscale funnel status
   ```
3. In each app's GitHub repo: Settings → Webhooks → Add webhook.
   - Payload URL: `https://<server-name>.<tailnet>.ts.net:8443/webhooks/github`
   - Content type: `application/json`
   - Secret: the value from step 1
   - Events: just the push event

   GitHub sends a ping straight away; the webhook's "Recent deliveries"
   should show it answered 200 with `{"result":"pong"}`. After a push, the
   delivery's response lists what happened to each app.

One webhook per repo is enough: a push deploys every app registered with
that repo and branch.

### Deploys and rollbacks

A deploy starts the new version next to the old one and switches traffic
only once the new one passes its health check; a version that fails it never
goes live. After the switch, the old version gets up to 15s to finish the
requests it's serving before it's stopped.

To undo a bad version that did go live, press **Roll back** on an earlier
deployment on the app's page. That version's image runs again (no build),
with the app's current env vars, through the same health check and switch.
Rolling back turns auto-deploy off, so that a push doesn't replace it; turn
it back on once the fix is pushed. The platform keeps the images of each
app's last 6 live versions, so a rollback can go back 5 versions.

### Databases

An app can have a Postgres database of its own: press **Add a database** on
its page. The platform runs it as a container next to the app, with its
data on a Docker volume, reachable only by that app's containers. From the
app's next deploy, it gets the connection string as `DATABASE_URL`; the
password is generated by the platform and never shown. If the database's
container stops or is deleted, the platform brings it back with its data.
The platform never deletes a database's data: the volume (`ops-db-<app>`)
stays until it's removed by hand.

**Backups.** Every database is backed up once a day (and on **Back up now**)
with `pg_dump`, to `BACKUP_DIR/<app>/` (`~/backups/hello/` for `hello`). The
newest 7 are kept; the app's page shows each backup's time, size and file.
Files are written as `BACKUP_UID`, so they're yours to copy or delete.

**Restoring a backup** replaces the database's contents with the dump's, so
stop writes first if you can (deploying a version that doesn't use the
database, or accepting a minute of errors). On the server, for `hello`:

```sh
ls -lt ~/backups/hello/                     # pick a dump
docker exec -i ops-db-hello pg_restore --clean --if-exists -U app -d app \
  < ~/backups/hello/hello-20261105-031500.dump
```

It runs inside the database's container (`docker exec`), where Postgres
trusts its local user, so the password, which is never shown, isn't needed.

### Crash recovery

Every 10 seconds the platform compares what should be running (in its
database) with what Docker and Caddy actually have, and fixes the
difference. A crashed or killed app is started again, and if it keeps
crashing, the restarts back off up to 5 minutes apart. A deleted app
container is recreated, a lost route is re-applied, and leftover containers
are removed. The same check runs when the platform starts, so after a
reboot, apps come back with it. It only ever touches containers the
platform created, never the game servers or anything else on the host.
Docker itself doesn't restart platform apps: the platform does, so it can
record each crash.

### Updating

```sh
cd ~/apps/dashboard
git pull
docker compose up -d --build
```

The platform's database lives in the `dashboard_platform-db` volume, which
survives `docker compose down` and rebuilds. `docker compose down -v` deletes
it.
