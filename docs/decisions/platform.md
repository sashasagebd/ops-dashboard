# Platform design decisions

Design log for the deploy platform described in
[../platform-plan.md](../platform-plan.md). One entry per significant choice:
what, why, and what was considered instead. The monitoring dashboard's own
decisions are in [stack.md](stack.md).

## Phase 0 (approved 2026-10-28)

### The control plane is a separate service

**What:** the platform runs as its own Go binary (`backend/cmd/platform`) and
its own Compose service. It lives in the same Go module and is built into the
same image as the dashboard; the two services just run different commands.

**Why:**
- *Failure isolation.* The risky code lives in the platform: builds streaming
  large tar files, workers, migrations, the reconciler. If any of it crashes or
  runs out of memory, the monitoring page keeps working. That's the same
  reasoning as "stale over broken" in [stack.md](stack.md): a monitoring page
  shouldn't go dark exactly when something is wrong. It's also what makes the
  chaos demo convincing, because the thing being killed isn't the thing you're
  watching.
- *Less privilege for the web-facing process.* See the next entry.

**Alternatives:** one binary doing both monitoring and the platform. It's
simpler (one process, one config, no proxy hop), and plenty of real tools work
that way. It was turned down because monitoring would then share a process with
Docker write access and go down with every platform bug. The cost of
separating is a second service, a second socket proxy, and a "platform
unreachable" state the platform pages have to handle. The extra process uses
about 15–20 MB of RAM. The package boundary (`internal/store`, later
`internal/jobs` and so on, with no dependency on `internal/monitor`) matters
more than the process boundary, so the choice could be reversed.

### The dashboard stays read-only; only the platform can write to Docker

**What:** the dashboard keeps its existing read-only socket proxy (`POST=0`,
GET on `/containers` only). The platform gets a second proxy instance on its
own internal network, with only the write endpoints it needs.

**Why:** being able to create containers is root-equivalent on the host. A
privileged container with `/` mounted is one API call away, and a socket proxy
filters by endpoint, not by request body, so it can't prevent that. Keeping
this ability out of the web-facing dashboard process means a bug in the
dashboard can, at worst, ask the platform for one of its specific operations
(deploy app X, roll back). It can never send an arbitrary container spec,
because the platform builds every container config itself, with resource
limits and no privileged options or host mounts. Precisely stated: the
dashboard holds no Docker write access; it can only request specific platform
operations. This extends the read-only design in [stack.md](stack.md) instead
of reversing it.

**Alternatives:** giving the dashboard's proxy `POST=1` and the image, build,
network and volume endpoints, i.e. the one-binary design. It would drop the
existing guarantee and require rewriting the constraint everywhere it's
documented.

### The dashboard forwards `/api/platform/*` to the platform

**What:** the dashboard's Go server reverse-proxies `/api/platform/*` to the
platform service (`net/http/httputil.ReverseProxy`). The browser only ever
talks to the dashboard.

**Why:** it keeps one origin (no CORS), one port bound to `127.0.0.1`, and one
`tailscale serve` target, which preserves the existing access model. If the
platform is down, the dashboard answers those routes with a generic 502 and
the monitoring pages carry on.

**Alternatives:** publishing the platform on its own port with its own
`tailscale serve` entry. That means two origins, CORS, and a second
admin-facing surface to keep off the LAN. (The GitHub webhook in Phase 2 is a
separate concern: it needs a public route through Cloudflare Tunnel, and will
get its own listener so the tunnel never reaches the admin API.)

### The official Docker SDK, for the platform only

**What:** the platform uses the official Go SDK for the Docker Engine API. The
dashboard keeps its small `net/http` client.

**Why:** the dashboard only needs two read endpoints, which is why
[stack.md](stack.md) skipped the SDK. The platform needs much more: builds
from a streamed tar context with streamed JSON progress, container create
with detailed config, and Docker's multiplexed log streams. Hand-writing all
of that would be a lot of code with subtle bugs. The SDK still sits behind
interfaces declared by its callers, so platform logic is tested against fakes,
and the SDK client can point at an `httptest` server. The exact module is
pinned when it's first added (Phase 1).

**Alternatives:** extending the hand-written client (no dependency, but a lot
of work for builds and logs), or shelling out to the `docker` CLI (ruled out
by the plan: harder to test, parsing text output, and the CLI would need to be
in the image).

### Postgres is the source of truth, reachable only by the platform

**What:** a pinned `postgres` image with a named volume and a memory limit, on
an internal Compose network that only the platform joins. It has no published
port.

**Why:** apps, deployments and jobs must survive restarts, and the job queue
depends on row locks (`FOR UPDATE SKIP LOCKED`) and transactions. Keeping the
database off every other network means the platform is the only way in.

**Alternatives:** SQLite. It's smaller, but its locking is per database
rather than per row, so it can't do `SKIP LOCKED` queue claims, and Postgres
is also what the hosted apps will use (Phase 5). Publishing the port for
development access was also rejected; development runs against CI instead
(see the last entry).

### `pgx` as the driver

**What:** `github.com/jackc/pgx/v5`, using its native pool API (`pgxpool`).

**Why:** the standard library has no Postgres driver, and `pgx` is the
maintained default (`lib/pq` is in maintenance mode). The native API maps
Postgres types more precisely than `database/sql`, and supports
`LISTEN`/`NOTIFY`, which can wake workers when a job is enqueued instead of
leaving them to poll (Phase 2).

**Alternatives:** `pgx` through its `database/sql` adapter (more familiar
interface, fewer Postgres features), or an ORM or query builder (more magic,
less to show in an interview).

### Migrations: a small embedded runner, run on platform startup

**What:** numbered `.sql` files embedded in the binary with `go:embed`, plus
about 60 lines of runner. On startup, the platform:
1. takes a Postgres advisory lock;
2. reads the applied versions from `schema_migrations`;
3. applies each missing file in its own transaction, recording its version in
   the same transaction;
4. releases the lock.

**Why:** Postgres runs DDL inside transactions, so a failed migration rolls
back completely and is never half-applied. That leaves very little for a
library to do. The advisory lock makes two platform processes starting at once
take turns instead of racing. Running on startup means a deploy can't forget
to migrate. Embedding the files means the binary and its schema can't drift
apart. This also fits the repo's "standard library first" rule.

**Alternatives:**
- `goose` or `golang-migrate`. They're proven and handle down-migrations and
  rarer cases, but they add a dependency for something this small.
- A separate migrate command or init container. It decouples migrating from
  starting, but it's one more step to forget.

**Known limit:** statements that can't run inside a transaction (e.g.
`CREATE INDEX CONCURRENTLY`) aren't supported. That's fine at this scale; a
per-file opt-out can be added if one is ever needed.

### Database tests use real Postgres, in CI only

**What:** tests that need Postgres skip themselves when `DATABASE_URL` is
unset, and CI runs them against a Postgres service container. The dev PC has
no Postgres for now (owner's call, 2026-10-28).

**Why:** the important behaviour (row locks, `SKIP LOCKED`, advisory locks,
transaction rollback) is behaviour of Postgres itself. A Go fake would pass
tests that real Postgres fails. Skipping instead of failing keeps
`go test ./...` green on a machine without a database. CI provides the
guarantee, the same way it's the only place the Docker image is tested. In
CI, a missing `DATABASE_URL` fails the tests instead of skipping them:
otherwise a config slip would leave them never running anywhere while the
job still showed green.

**Alternatives:** installing Postgres natively on the dev PC (faster feedback,
which is likely worth doing before Phase 2's queue work), Docker Desktop or
WSL2 locally, or connecting to the server's database over Tailscale (rejected:
it publishes the database beyond its network, and tests would write to real
data).

### The platform waits for Postgres on startup, then listens

**What:** on startup the platform pings Postgres until it answers (waiting
1s, then 2s, 4s… up to 30s between attempts), runs migrations, and only then
starts listening. `/healthz` doesn't ping the database on each check. A
failed migration isn't retried; the process exits.

**Why:** after a reboot, Docker starts containers in no particular order
(Compose's `depends_on` only applies to `docker compose up`), so the platform
often starts before Postgres does. Waiting in-process avoids a crash loop and
gives one clear log line per attempt. Not listening until ready means a
passing healthcheck means "fully started". Like the dashboard's, `/healthz`
is a liveness check: a database blip shouldn't mark the platform unhealthy.
A broken migration is a bug, not a blip, so retrying it in a loop would only
hide it.

**Alternatives:** exiting on connection failure and leaving it to the restart
policy (works, but crash-loops with growing restart delays and noisy logs);
listening straight away with a `/healthz` that reports "starting" until ready
(more states for little gain while nothing else is served).

### A database newer than the binary is refused

**What:** if `schema_migrations` has a version the binary doesn't know, the
platform refuses to start.

**Why:** that happens when an older platform image runs against a newer
schema, e.g. after rolling the platform itself back. Its code may write rows
the newer schema's assumptions don't allow. Refusing makes the problem loud.
Schema changes only go forward; undoing one is a new migration.

**Alternatives:** ignoring unknown versions (what many tools do, and handy for
quick rollbacks, but it silently runs old code against a schema it wasn't
written for); down-migrations (more code, and rarely trustworthy once real
data exists).

### The `apps` table

**What:** `bigint` identity primary key; `name` restricted to a DNS-label
shape (lowercase letters, digits, inner hyphens, at most 32 characters);
`hostname` unique and lowercase; memory and CPU limits `NOT NULL` with
defaults (256 MiB, half a core). CPU is stored in millicores (500 = half a
core) and converted to Docker's NanoCPUs at container start.

**Why:** the name ends up in container names, image tags and Docker labels,
so constraining its shape at the source avoids escaping problems everywhere
else. Limits that can't be null mean nothing deployed can run unbounded next
to the game servers. Millicores are readable and integer (no floats in the
schema). Constraints in the database are the last line of defence; the API
validates first, with friendlier errors.

**Alternatives:** UUID keys (useful across systems or to hide row counts;
neither matters for a single-user, single-host platform); nullable limits
meaning "unlimited" (exactly what the limits are there to prevent).

### Shared healthcheck package

**What:** the dashboard's self-check (`-healthcheck`, which GETs its own
`/healthz`) moved from `cmd/dashboard` to `internal/healthcheck`, used by
both binaries.

**Why:** both images are distroless, so both need the binary to check itself.
It's about 40 lines, and sharing it means one implementation and one set of
tests. Each binary decides what its own `/healthz` means.

**Alternatives:** copying the code into `cmd/platform` (two copies to keep in
sync).

### Running Postgres and the platform in Compose

**What:** both services live in the dashboard's `compose.yaml`. Postgres
(`postgres:18.6`) has a named volume, a 512 MB memory ceiling, a 1-minute
stop grace period, and no published port. The platform has a 128 MB ceiling
and the same hardening as the dashboard (read-only filesystem, no Linux
capabilities). The password is a hex string from `.env`, and the connection
doesn't use TLS.

**Why:**
- *One compose file:* one `docker compose up -d --build` deploys everything,
  the way the server is already run, and the two services are only useful
  together.
- *Memory ceilings:* they protect the game servers if something misbehaves.
  They're limits, not reservations; Postgres idles far below its limit.
- *The stop grace period:* the image stops Postgres with a fast shutdown,
  which can outlast Compose's default 10s. Being killed instead means crash
  recovery on the next start.
- *Hex password:* it can go into the connection URL without escaping.
- *No TLS:* the connection never leaves an internal Docker network on one
  host, so TLS would add certificate handling for no real protection.
- *Volume path:* Postgres 18 images expect the volume at
  `/var/lib/postgresql`, not the `/var/lib/postgresql/data` older guides use.

**Alternatives:** a separate compose project for the platform (independent
deploys, but two places to configure and deploy for services that go
together); Postgres installed directly on the host (survives Docker problems,
but goes against "everything in Docker" and is harder to pin and reproduce);
the `-alpine` image (smaller download, but musl's locale behaviour differs
from the glibc that most Postgres guides assume, and disk space isn't the
constraint here).

## Phase 1 (approved 2026-11-02)

### The platform's write proxy: few endpoint groups, nothing finer

**What:** a second `tecnativa/docker-socket-proxy` for the platform only, on
its own internal network, with `POST=1` and only the `CONTAINERS`, `IMAGES`,
`BUILD` and `NETWORKS` groups enabled. The dashboard keeps its read-only
proxy.

**Why:** the proxy's rules are per endpoint group, and with `POST=1` every
write method in an enabled group is allowed (its config is one
`deny unless GET or POST=1` line, then an allow line per group). So the only
control is which groups are on. These four are what building and running
apps need. Exec, volumes, swarm, secrets, plugins and the rest stay off,
which blocks e.g. running commands inside the game servers' containers.
Being able to create containers is still root-equivalent, as the Phase 0
entry says. The proxy narrows the API surface; it doesn't make that safe.
The real guarantees are that the platform builds every container config
itself, and that the proxy is reachable only from the platform.

**Alternatives:** the proxy's `ALLOW_START`/`ALLOW_STOP` switches (they allow
start and stop without `POST=1`, but not create, build or remove, which
deploys need); a proxy that filters request bodies (e.g. refusing privileged
containers), which would be stronger but is a custom component to write and
maintain; mounting the socket into the platform (no narrowing at all).

### Code comes from GitHub's tarball API

**What:** to deploy commit `abc123`, the platform downloads
`/repos/{owner}/{repo}/tarball/abc123`, strips GitHub's top-level folder,
and streams the result to Docker as the build context, with a size cap.
Public repos only in Phase 1.

**Why:** a Docker build context is a tar file anyway, so this is one
streaming download and one streaming upload, with no git anywhere. It's
pinned to an exact commit, and private repos later only need a token header.
Because the platform handles the bytes, it can cap the size and record
exactly what was built.

**Alternatives:** cloning with `go-git` (works with any git host, but a large
dependency, and a full clone where a snapshot is enough); passing the repo
URL to Docker and letting the daemon clone it (least code, but the platform
can't limit or inspect what's fetched, and Docker's daemon would need to be
able to reach GitHub on the platform's behalf).

### Caddy, configured through its admin API

**What:** a Caddy container receives all app traffic. The platform adds,
replaces and removes one route per app through Caddy's admin API, which is
only reachable on an internal network that apps aren't on. Because routes
set this way live in Caddy's memory, the platform re-applies them all when it
starts.

**Why:** routing is part of the deploy. Phase 3's zero-downtime switch is
"start the new container, then repoint the route", and that's easiest to
reason about when the platform does it explicitly. Caddy's admin API takes
small JSON changes with no reload. The admin API has no authentication, so
keeping it off any network an app can reach is what protects it.

**Alternatives:** Traefik with Docker label discovery (very little code, but
Traefik would need Docker access of its own, and the routing logic would
live in Traefik rather than in code you can show and test); generating a
Caddyfile and reloading (simpler to inspect, but rewrites everything for one
route, and is file-based state to keep in sync).

### Deploys run in a goroutine (Phase 1 only)

**What:** the "deploy" API call records a `queued` deployment and starts a
goroutine that runs the whole flow, allowing one deploy per app at a time.

**Why:** it gets an end-to-end deploy working without the job queue, so
Phase 2 can focus on the queue itself. The known gap is deliberate: if the
platform restarts mid-deploy, that deploy is lost. At startup the platform
marks any deployment still in progress as failed ("interrupted"), since
nothing can resume it and it would otherwise block the app's next deploy
forever. Phase 2 fixes this properly with jobs in Postgres that survive
restarts and are retried. "Here's what broke, here's the queue that fixed it" is a better
story than starting with the queue.

**Alternatives:** building the queue first (more correct from day one, but
two hard things at once in the same phase).

_Superseded in P2 step 2.1 (2026-11-03) by the job queue: see "How the job
queue works" below. Migration 0005 fails any deployment still in progress
from before it, since it has no job; the startup code that did this is
gone._

### Docker's restart policy keeps apps up until the reconciler exists

**What:** app containers get `restart: unless-stopped`.

**Why:** without a restart policy, a crashed app would stay down until
Phase 4. Docker's policy restarts a container that exits, which covers the
common case. Phase 4 decides whether the reconciler takes this over
completely or works alongside it.

**Alternatives:** no restart policy until Phase 4 (crashes stay down for
weeks of development).

### Apps can only reach Caddy

**What:** app containers are on one network, `apps`, shared with Caddy's
proxy side and nothing else. The platform, Postgres, both socket proxies and
Caddy's admin API are on other internal networks.

**Why:** an app is someone's code (even if that someone is you) exposed to
the internet. If it's compromised, the attacker should land in a container
that can talk to Caddy and the internet, not to the database holding every
app's encrypted secrets or the proxy that can create containers.

**Alternatives:** one shared network (simpler, but every app could reach the
control plane).

### No domain yet: sslip.io hostnames over Tailscale (2026-11-02)

**What:** until there's a domain, apps get hostnames like
`hello.100-101-102-103.sslip.io`. Caddy is published on the server's
Tailscale IP only, so apps are reachable from tailnet devices and nowhere
else. cloudflared and public hostnames wait until a domain is bought.

**Why:** sslip.io is a free public DNS service that resolves any name
containing an IP address to that IP. It gives each app a real, distinct
hostname with no domain and no DNS setup, so Caddy's hostname routing
(the part that matters for the platform) is built and tested exactly as it
will run publicly. Swapping in a real domain later is configuration: a
different `APPS_DOMAIN` plus the cloudflared service. It also keeps the
platform off the internet until it has proven itself. Traffic inside the
tailnet is plain HTTP, which is acceptable because Tailscale already
encrypts it.

**Alternatives:** buying a domain now (about $10–12/year; the owner will
likely do this later anyway); Cloudflare quick tunnels (free, but a random
URL that changes on every restart, and meant for testing); Tailscale Funnel
(free and public, but one hostname per machine, so apps would share it by
path, which many apps don't handle); free registries such as `eu.org` (slow,
uncertain approval).

### Env vars are encrypted with AES-256-GCM, bound to their row

**What:** `internal/secrets` seals each env var value with AES-256-GCM from
Go's standard library, under `PLATFORM_SECRET_KEY` (32 random bytes, in
`.env`, never in the database or repo). A stored value is a format byte,
a random 12-byte nonce, then the ciphertext and tag. The app ID and key name
are passed as associated data. Only the store encrypts and decrypts, and the
API will only ever return key names.

**Why:**
- *GCM is authenticated:* a tampered value, or the wrong key, fails loudly
  instead of decrypting to garbage that would be injected into an app.
- *Associated data binds a value to its row:* someone who can write to the
  database but doesn't have the key can't copy app A's secret into app B's
  row and have app B receive it. A test does exactly that and checks it
  fails.
- *Random nonces:* the same value sealed twice gives different bytes, so
  equal secrets aren't visible as such.
- *The format byte:* lets the scheme (or key, for a rotation) change later
  while old values stay readable.
- *Encryption in the store, not the API:* nothing else can write a value
  unencrypted by accident.
- *A missing or malformed key stops the platform at startup:* a default or
  generated key would silently lose every value on the next restart.

The cost: lose the key and every stored value is gone, so `.env.example`
and the README say to back it up.

**Alternatives:** Postgres's `pgcrypto` (encryption inside the database
means the key travels to it with every query, and may appear in its logs);
a secrets manager such as Vault (the "right" answer at scale, but a large
extra service for one person's server); envelope encryption with a key per
app (only worth it once keys need rotating per app).

### Invariants live in the database, not in Go checks

**What:** "at most one deploy in progress per app" and "at most one live
deployment per app" are partial unique indexes on `deployments`. Status
changes are single `UPDATE ... WHERE id = $1 AND status = ANY(allowed)`
statements, so a deployment only moves forward. Making a deployment live and
superseding the old one happen in one transaction.

**Why:** a "check, then write" in Go (e.g. "is a deploy running? if not,
start one") has a gap between the check and the write, where two requests,
or in Phase 2 two workers, can both pass the check. A unique index or a
conditional `UPDATE` makes the database decide atomically: exactly one wins,
and the other gets a clear error (`ErrDeployInProgress`, `ErrWrongStatus`).
This holds no matter how many platform processes or goroutines there are,
which is what Phase 2's queue will rely on.

**Alternatives:** a Go mutex per app (only works within one process, and is
lost on restart); `SELECT ... FOR UPDATE` then check then update (correct, but
more code and round trips for the same guarantee).

### Health checks go through Caddy (2026-11-02)

**What:** to check a new deployment is healthy, the platform adds a
temporary Caddy route for it (a probe hostname only the platform uses) and
sends requests to the app's health-check path through Caddy. Caddy is on the
apps network; the platform isn't.

**Why:** apps are isolated from the platform on purpose (see "Apps can only
reach Caddy"), so the platform can't reach a new container directly. Going
through Caddy keeps that isolation, and checks the same path real visitors
use, so a route or network mistake fails the check rather than going live.
It's also exactly Phase 3's zero-downtime switch: probe the new version
through Caddy, then move the real hostname to it.

**Alternatives:** putting the platform on the apps network (every app could
then call the platform's unauthenticated API); Docker's own `HEALTHCHECK`
(needs `curl` or similar inside every app image, which can't be assumed); a
short-lived helper container on the apps network per check (works, but adds
an image pull and a container per deploy).

### Fixed hardening on every app container

**What:** besides the app's own limits, `engine.Run` always sets: no
privileged mode, no host mounts, no published ports, `no-new-privileges`, a
512-process limit, log rotation (3 × 10 MB) and a 10 s stop timeout. Callers
can't change these; the spec only carries image, env, labels, network and
limits.

**Why:** the socket proxy can't stop a container being created privileged,
with the host's `/` mounted (it doesn't read request bodies), so the
platform's own code is the real boundary, and it's simplest to audit when
it's one function with no options. The process limit stops one app's fork
bomb or leak from exhausting the host. Log rotation matters on a 256 GB
disk: Docker's default log file grows forever.

**Alternatives:** a read-only root filesystem and `cap_drop: ALL` as well
(stronger, but breaks many ordinary images that write temp files or switch
users; could become an opt-in per app later).

### Builds use Docker's legacy builder, for now (2026-11-03)

**What:** `engine.Build` asks Docker for its legacy builder (`version=1`),
sending the code as the request body.

**Why:** the first plan was BuildKit, Docker's current builder. Its source
showed it accepts the build context in the request body without a session,
which is true. But CI's real build through the proxy failed with
`no active sessions`: BuildKit also uses a client-side session to look up
`FROM` images in the registry, even public ones. The legacy builder pulls
base images itself, so it works through `/build` alone, with no extra
dependency or proxy permission. It's deprecated (since Docker 23, with no
removal date) and lacks newer features such as `RUN --mount=type=cache` and
heredocs, which apps deployed here must avoid. A side benefit: its progress
is plain text, which makes Phase 2's live build logs simpler. The choice is
contained in one function, so it's cheap to change.

**Alternatives:**
- *BuildKit with a session implemented in Go* (what the `docker` CLI does).
  It's the upgrade path if an app needs BuildKit features. Cost: the large
  `moby/buildkit` library, the proxy's `SESSION` group, and a long-lived
  hijacked connection through a proxy with a 10-minute idle timeout.
- *BuildKit after pre-pulling every base image:* fragile, since `# syntax=`
  lines, `ARG`s in `FROM` and `COPY --from=<image>` all still need the
  registry.
- *A separate build service:* a standalone BuildKit daemon, or a builder
  container per build. More moving parts than this phase needs.

**Lesson:** reading the source got the first half right and missed the
second. The CI check that does a real build is what caught it, before any
deploy depended on it.

### Caddy's admin API on a fixed IP, on a network apps aren't on (2026-11-03)

**What:** Caddy's admin API listens only on `10.203.0.2:2019`, its address on
an internal `caddy-admin` network (fixed subnet `10.203.0.0/29`) that only
the platform shares. Caddy's proxy side is on `platform-apps`, the network
apps are on.

**Why:** the admin API has no authentication: anything that can reach it can
rewrite routing. Caddy has to be on the apps network to proxy to them, so
listening on all interfaces would hand every deployed app that power.
Binding to one known address on a separate network keeps it reachable from
the platform only, and CI checks an app can't connect. The subnet is outside
Docker's default address pools, so it can't clash with networks Docker
creates on its own.

**Lesson (2026-11-03, first reboot on the server):** Caddy didn't come back
up after a reboot: "failed to set up container networking: Address already
in use". The address in question was Caddy's fixed `.2`, not a port. Docker
gives containers without a fixed address the first free one in the subnet,
so whichever starts first gets `.2`. `docker compose up` happened to start
Caddy first, in CI and on the first deploy. A reboot starts containers in
any order, and this time the platform went first. Fix: `ip_range:
10.203.0.4/30`, so automatic addresses come only from `.4`–`.6`, and `.2`
is reserved in effect. CI now stops both and starts the platform first.
The same applies to any future fixed address: it must sit outside the
network's `ip_range`.

**Alternatives:** a Unix socket shared through a volume (no network at all,
but Caddy runs as root and the platform as a non-root user, so socket
permissions get fiddly); Caddy's admin API with authentication (only
available through an extra module and remote-admin certificates).

### Routes are applied declaratively, from the database (2026-11-03)

**What:** every routing change sends Caddy the complete table, built from
the database (each app's live deployment) plus probe routes for deployments
being checked, replacing Caddy's whole app server in one PATCH. Caddy runs
with `--resume`, so it restarts with its last config, and the platform
re-sends the table whenever it starts.

**Why:** incremental add/remove calls can drift from reality (a request
fails halfway, Caddy restarts, the platform crashes between steps), and then
need their own bookkeeping to repair. Sending the whole desired state makes
every update self-correcting and safe to repeat. This is the reconciliation
idea of Phase 4 in miniature, for routes. At a handful of apps the table is
tiny. PATCHing the whole server object, rather than only its routes array,
means it doesn't depend on Caddy keeping an empty array in its saved config.

**Alternatives:** per-route `@id`s with `PUT`/`DELETE` (less data per
request, but state to keep in step); regenerating a Caddyfile and reloading
(file-based state, and a reload per change).

### Startup cleanup of orphaned app containers (2026-11-03)

**What:** when the platform starts (no deploys in flight yet), it removes
every platform-labelled container that isn't an app's live deployment.

**Why:** a deploy cut off by a crash leaves its container behind. With the
`unless-stopped` restart policy it would run forever, unrouted, using
memory. Only containers carrying the platform's label are considered, so
nothing else on the host is touched. Phase 4's reconciler does this
continuously; this covers the restart case until then.

**Alternatives:** leaving cleanup to Phase 4 (orphans pile up for weeks of
development).

### Simple cross-site protection for an API without a login (2026-11-03)

**What:** the platform API only accepts request bodies with
`Content-Type: application/json` (otherwise 415), and rejects unknown JSON
fields.

**Why:** the API has no login, because only tailnet devices can reach it. But
a web page on any site, opened in a browser on one of those devices, could
still send requests to it (cross-site request forgery). A cross-site request
can only carry a JSON content type after a CORS preflight, which this API
never approves, so such a page can't trigger a deploy or change env vars.
Step 1.6 adds Go's `http.CrossOriginProtection` in the dashboard, which
forwards to the API, as a second check based on the browser's
`Sec-Fetch-Site`/`Origin` headers.

**Alternatives:** CSRF tokens (needs sessions, which a single-user API with
no login doesn't have); a login (worth adding if the API is ever reachable
from outside the tailnet).

### CI deploys this very commit end to end (2026-11-03)

**What:** CI's compose step uses the platform's API to register an app
pointing at this repo, then deploys the commit being tested. The root
Dockerfile builds the dashboard image, which serves `/healthz` without
needing anything else. CI then checks:
- a visitor reaches it through Caddy by hostname;
- a redeploy replaces the container;
- a failing deploy (a commit that doesn't exist) leaves the site up and
  says why;
- apps can reach neither Caddy's admin API nor the platform;
- routes survive restarts of Caddy and of the platform.

**Why:** unit tests use fakes, and the dev PC has no Docker, so this is the
first place the real pieces (GitHub, the proxy, the legacy builder, Docker
networks, Caddy) meet before anything goes to the server. It already earned
its keep at the build step: BuildKit's need for a session only showed up in a
real build. The cost is a few minutes of CI per run.

**Alternatives:** a separate sample repo (another repo to keep in step);
testing only on the server (a slow, manual feedback loop).

### The dashboard forwards the platform's API (2026-11-03)

**What:** the dashboard serves `/api/platform/*` by forwarding each request
to the platform (`httputil.ReverseProxy`, `PLATFORM_URL`), and wraps all its
routes in Go's `http.CrossOriginProtection`. The platform keeps no published
port.

**Why:** the browser only ever talks to one origin, the dashboard at its
`tailscale serve` URL. So there's no CORS to configure, and the platform needs
no address of its own on the tailnet. The cross-origin check is the second
CSRF layer promised in 1.5. Go 1.25+ has it built in: it refuses
state-changing requests (not GET/HEAD/OPTIONS) that the browser marks
cross-origin through `Sec-Fetch-Site` (or `Origin` vs `Host` on older
browsers), and passes requests with neither header (curl, scripts), which
CSRF can't involve. The proxy uses `Rewrite`, which drops any
`X-Forwarded-*` headers the client sent. If the platform is down, the
monitoring pages keep working and only `/api/platform/` answers 502.

**Alternatives:** publishing the platform behind its own `tailscale serve`
port (a second origin needs CORS, plus a second thing to keep tailnet-only);
merging the two binaries (rejected in P0, because monitoring should survive
a broken platform).

### Hash routing in the frontend (2026-11-03)

**What:** pages live in the URL hash (`#/`, `#/apps`, `#/apps/3`), parsed by
a 20-line `route.ts`, with no router library.

**Why:** the hash never reaches the server, so Go's file server needs no
"serve index.html for unknown paths" fallback, and reloads and bookmarks work
on every page. With three pages, a library would be more code to depend on
than the code it replaces.

**Alternatives:** path routing with a fallback in Go (nicer URLs, but every
unknown path would then answer 200 with the app instead of 404); React
Router (a dependency for three routes). Worth revisiting if the frontend
grows many pages.

### Apps can be built from a folder of their repo (2026-11-03)

**What:** apps have a `sourceDir` (migration 0004, default `''`, the repo's
root). The code fetcher keeps only that folder, re-rooted, as the build
context, so its Dockerfile is the one built.

**Why:** `examples/hello` lives in this repo, and a repo often holds more
than one deployable thing (pokewordle's backend is likely to be one folder of
its repo). The path is checked three times, because it selects files: the API
gives a clear 400, the database's CHECK refuses `.`/`..` parts, absolute
paths and trailing slashes, and the fetcher refuses anything that isn't a
clean relative path. A folder missing at that commit is only discovered
while the archive streams into Docker's build. So the deploy keeps the
stream's own error and reports it ("source directory not found…") rather
than whatever Docker's client made of the cut-off upload. The same applies
to a repo over the size limit. CI deploys `examples/hello` this way, and
checks the reason for a missing folder.

**Alternatives:** a separate repo for each example (more to keep in step, and
it doesn't help monorepos); a Dockerfile path option instead of a folder
(the build context would still be the whole repo, which is larger and
leaks more into each image's build).

### Caddy is published on the Tailscale IP (2026-11-03)

**What:** Caddy's port 80 is published on `APPS_LISTEN_IP:80` (required, no
default). On the server that's its Tailscale IP, the same one sslip.io
resolves app hostnames to.

**Why:** that address exists only on the `tailscale0` interface, so tailnet
devices can reach the apps and the LAN and internet can't. Docker bypasses
UFW, so a mistaken `0.0.0.0` would expose every app; making the variable
required, with no default, means it can't happen by omission. Apps go
public later through cloudflared, not by widening this.

**Known limits:**
- **Boot order.** Binding to the Tailscale IP fails if Docker starts the
  container before `tailscaled` has brought the address up. 1.6's reboot
  test checks this. The fix, if needed, is `net.ipv4.ip_nonlocal_bind=1`
  (lets a socket bind an address that isn't up yet), or ordering Docker
  after Tailscale in systemd. The first reboot failed for a different
  reason (Caddy's admin address, see "Caddy's admin API on a fixed IP").
  The second, after that fix, came up cleanly: the Tailscale IP was there
  in time, so neither fix was applied. It's a race, not a guarantee, so if
  Caddy is ever down after a boot with "cannot assign requested address",
  this is the fix.
- **Spoofed destinations.** A device on the same LAN segment could, in
  principle, send packets addressed to the Tailscale IP straight to the
  server's LAN interface, and Docker's forwarding rule matches on the
  destination address alone. That needs someone on the home LAN, and app
  sites aren't admin surfaces (the dashboard and platform stay on
  127.0.0.1 and internal networks). Not mitigated now; a firewall rule
  limiting the published port to `tailscale0` would close it.

**Alternatives:** `tailscale serve` in front of Caddy (it serves one name per
machine, not a wildcard, so it can't route `<app>.<domain>`); publishing on
127.0.0.1 and relaying (an extra moving part for no gain).

## Phase 2 (approved 2026-11-03)

### Deploys become jobs in a Postgres queue

**What:** a `jobs` table. Workers inside the platform claim jobs with
`SELECT … FOR UPDATE SKIP LOCKED` and hold a lease they keep renewing; a job
whose lease runs out (its worker died) is claimable again. Transient errors
(GitHub or Docker unreachable) are retried with backoff, up to 3 attempts.
Permanent ones (build failed, health check failed, commit not found) fail at
once.

**Why:** P1's goroutines lose a deploy when the platform restarts mid-build;
the best they can do is mark it failed at startup. A row in Postgres outlives
the process. `SKIP LOCKED` lets several workers claim jobs without blocking
each other or taking the same one, and the lease is what tells a running job
apart from one whose worker died. Retrying only transient errors avoids
rebuilding something that will fail the same way again.

**Alternatives:** a separate queue (Redis, NATS: another service to run and
back up, for a single-host system that already has Postgres); a plain
`locked = true` flag (a crashed worker's jobs stay locked forever without a
lease); retrying every error (wastes minutes on builds that can't succeed).

### How the job queue works (2.1, 2026-11-03)

**What:** the details behind the entry above.
- **The deployment and its job are created in one statement** (a CTE), so
  there's never a deployment nothing will run, or a job for a deployment
  that was refused. "One deploy per app at a time" needs no queue logic: a
  job belongs to one deployment, and the existing partial unique index
  allows one in progress per app.
- **A typed `deployment_id` column, not a JSON payload.** The database can
  check it (foreign key, deleted with the deployment), and there's only one
  kind of job. A later kind gets its own column.
- **Attempts are counted when a job is claimed**, not when it fails. A job
  whose worker keeps dying (say a build that takes the platform down with
  it) would otherwise be retried forever. A job claimed a fourth time gives
  up without running: "the platform stopped during every attempt".
- **A lease of 30s, renewed every 10s.** Every change to a job after the
  claim requires `locked_by` to still be this worker (fencing), so a worker
  that stalled past its lease can't overwrite the outcome of the worker
  that took over. If the worker can't renew (Postgres unreachable), it
  stops the job before the lease runs out, rather than risk two workers
  building the same deployment.
- **A clean shutdown hands the job back** (queued, attempt not counted), so
  `docker compose up -d` mid-deploy costs a rebuild, not an attempt and a
  30-second wait. Only a crash waits for the lease.
- **Each attempt is safe to repeat.** It removes any container an earlier
  attempt left for this deployment (same name), then rebuilds, mostly from
  Docker's cache. A deployment already marked healthy (the platform died
  between marking it live and switching the route) only finishes the
  switch. Cleanup after a switch removes only *older* deployments'
  containers, since the next deploy may already be starting one.
- **Which errors retry:** build failed, health check failed, repo, commit
  or folder not found, source too large, and the 20-minute attempt timeout
  are permanent. Anything else (Docker, GitHub or Postgres unreachable,
  unexpected errors) retries after 15s, then 30s, up to 3 attempts.
  Unknown errors retry because the transient ones come in too many shapes
  (network errors, 5xx, proxy errors) to list, and 3 attempts bound the
  cost.
- **Two workers, woken directly.** The API wakes a worker in-process when
  it queues a deploy; workers also poll every 5s for retries that are due
  and expired leases. Workers start only after the startup cleanup of
  orphaned containers, which would otherwise race a resumed deploy.

**Alternatives:** Postgres `LISTEN/NOTIFY` to wake workers (needs a
dedicated connection, and gains nothing while the API and the workers are
one process); a `max_attempts` column per job (one limit for every deploy
is enough); keeping the half-built image or container between attempts
(rebuilding from cache is cheap, and starting clean is easier to reason
about).

### The webhook arrives through Tailscale Funnel until there's a domain

**What:** GitHub's push webhook reaches the platform through Tailscale
Funnel, on port 8443 of the existing `homelab.<tailnet>.ts.net` name, mapped
to the webhook listener only. The owner will buy a domain (most likely for
pokewordle in P5); then the webhook moves to Cloudflare Tunnel, as the plan
intends, and Funnel is turned off.

**Why:** the plan's Cloudflare Tunnel needs a domain, and push-to-deploy
shouldn't wait for one. Funnel is free, gives a public HTTPS URL with a real
certificate, and is configured per port, so the dashboard on 443 stays
tailnet-only.

**Alternatives:** wait for the domain (P2's main feature blocked on a
purchase); polling GitHub for new commits (no public endpoint at all, but up
to a minute's delay and no webhook to show); a cloudflared quick tunnel (its
random URL changes on every restart, so the webhook would break).

### A separate listener for the webhook, on 127.0.0.1

**What:** the platform runs a second HTTP server that serves only the
webhook route, published on `127.0.0.1` only, for Funnel to forward to. The
main API stays unpublished. This relaxes the constraint "the platform
publishes no ports" (approved by the owner).

**Why:** the webhook is the platform's only public endpoint, so it gets its
own server: one route, a small body limit, short timeouts, and nothing else
reachable through it. 127.0.0.1 keeps it off the LAN (Docker bypasses UFW);
only tailscaled on the host connects to it.

**Alternatives:** routing the webhook path through the dashboard's port
(no new port, but public requests would reach the process that serves the
admin pages, and one Funnel mistake would expose them); publishing the main
API's port (everything one misconfiguration away from public).

### App logs are read live from Docker, not stored

**What:** deploy logs (build output, stage messages) are stored per
deployment. App (runtime) logs are streamed from Docker when someone is
watching, and not stored.

**Why:** Docker already keeps each container's logs (json-file, 3 × 10 MB),
enough for watching live and recent history. Storing them would grow the
database with app output on a server shared with game servers, and keep any
secrets an app prints. What's lost is a replaced container's logs; P4's
reconciler saves a crashed container's last lines in its event, which covers
the useful part. Fuller log storage belongs to P5's observability work.

**Alternatives:** storing everything in Postgres (growth, retention jobs,
secrets at rest); a log stack such as Loki (another service, too early).

### Logs stream over Server-Sent Events

**What:** log endpoints use SSE (`text/event-stream`): stored lines first,
then new ones as they're written.

**Why:** logs only go one way, server to browser. SSE is plain HTTP, so it
passes through the dashboard's reverse proxy (with flushing) unchanged, and
`EventSource` reconnects by itself, resuming from the last line ID.

**Alternatives:** WebSockets (two-way, which isn't needed, and an upgrade the
proxy must handle); polling (simpler, but laggy and wasteful for a build
that prints hundreds of lines).


### How deploy logs are stored and streamed (2.2, 2026-11-03)

**What:** the details behind the two entries above.
- **A `deployment_logs` table, one row per line,** with a global identity ID
  that doubles as the stream's resume point (`Last-Event-ID`). One worker
  writes a deployment's log at a time (the job's lease), so ID order is line
  order.
- **Written in batches every 500ms** (one `COPY` per batch), not per line: a
  build prints hundreds of lines in seconds. Logging is best effort: a batch
  that can't be written is dropped and noted in the platform's own log,
  rather than failing a deploy that's otherwise fine.
- **Bounded twice:** build output is capped at 5,000 lines per deployment
  (across attempts; the platform's own messages always get through), and
  only an app's 10 most recent deployments keep their logs (older ones are
  deleted as each new deploy starts). Lines are cut at 4 KiB and cleaned up
  (terminal colours, carriage-return progress bars, NUL bytes, invalid
  UTF-8). Per-layer download progress while pulling a base image is dropped
  as noise. Env var values never reach the log: they aren't build inputs,
  and the platform's messages only count them.
- **The stream polls Postgres every 500ms** while a deploy is running, and
  ends with an `end` event once the deployment's job has finished for good.
  "Finished" comes from the job, not the deployment's status: a deployment
  is marked healthy before its last messages (route switched, old version
  removed), but its job only finishes after every line is written. The
  stream checks that before reading, so the read that follows has the rest.
- **App output is followed live from Docker** with timestamps, which are the
  event IDs, so a dropped connection resumes from the last line's time
  (Docker's `since`, with already-seen lines skipped). When the container
  stops or is replaced, the stream ends and the page offers to reconnect.
- **Open streams end when shutdown starts** (the servers' `BaseContext` is
  the shutdown signal). Otherwise one open log viewer would hold every
  restart of the platform, and of the dashboard proxying it, for the full
  shutdown timeout.

**Alternatives:** waking streams in-process when lines are written, or
Postgres `LISTEN/NOTIFY` (immediate rather than up to 500ms late, but extra
moving parts for one viewer, and the writer batches every 500ms anyway); log
files on disk with a pointer in the database (another volume, and retention
done by hand); ending the deploy log on the deployment's status (would cut
off its last lines).

### How push-to-deploy works (2.3, 2026-11-03)

**What:** the details behind the Funnel and separate-listener entries above.
- **The signature is checked before anything else**, in constant time
  (`hmac.Equal`), on a body capped at 1 MiB. Until it checks out, the
  request touches nothing: no database, no logging of its contents. The
  secret must be at least 32 characters, and with none set the listener
  doesn't run at all.
- **Deliveries are deduped by GitHub's delivery ID** (kept 30 days), so a
  redelivery doesn't deploy again. If the platform fails before doing
  anything (it can't list apps), it forgets the delivery, so "Redeliver" on
  GitHub works.
- **A push that arrives mid-deploy waits as the app's next commit.** Only
  one deploy per app can be in progress (a database rule), so the push
  can't simply start one, and dropping it would break "a push deploys".
  It's stored on the app (`next_commit_sha`), and the newest push replaces
  an older waiting one, since deploying the one in between would only be
  replaced straight away. Every deploy that finishes, whether healthy or
  given up, starts the waiting commit. Both sides lock the app's row first,
  so a push can't see a deploy in progress, miss it finishing, and leave
  its commit with nothing to start it. A deploy that's still to be retried
  hasn't finished, so the push keeps waiting.
- **One webhook per repo deploys every matching app:** the same repo (owner
  and name compared case-insensitively, as GitHub does) and branch, with
  auto-deploy on. Tags and branch deletions are ignored. The response lists
  what happened to each app, which GitHub shows on the webhook's "Recent
  deliveries" page: the place to look when a push didn't deploy.
- **Auto-deploy is per app, on by default**; turning it off also drops a
  waiting push. "Deploy latest" looks up the branch's head with GitHub's
  commits API (a few bytes, not the whole commit) and deploys it like any
  commit.

**Alternatives:** a queue of every pushed commit per app (deploys commits
that are already out of date); refusing a mid-deploy push with a 409 (GitHub
doesn't retry, so the push is lost); a `jobs` row per push to serialize
deploys (the jobs table runs deploys, it doesn't decide which commit; and a
push isn't a deployment until it starts); checking the signature after
parsing (does work for unauthenticated requests).

## Phase 3 (approved 2026-11-04)

### A rollback is a new deployment of an old image

**What:** rolling back to deployment #9 creates a new deployment ("#12:
rollback of #9") that reuses #9's image: no download, no build, but the
same start, health check, switch and drain as any deploy. The version it
replaces is marked `rolled_back`.
**Why:** statuses only move forward (a database rule since P1), so #9
can't become healthy again; and the history then says what happened, in
order. It also reuses everything a deploy already guarantees (one deploy in
progress per app, retries, the old version kept if the new one fails).
**Alternatives:** making #9 live again (breaks forward-only statuses, and
the history would show #9 live twice with nothing in between); rebuilding
#9's commit (slow, and not the same image if a base image has moved).

### A rollback uses today's env vars, and turns auto-deploy off

**What:** a rollback runs the old image with the app's current env vars. It
also turns auto-deploy off and drops a waiting pushed commit.
**Why:** env var values aren't versioned, and versioning them is real
extra scope for little gain. Without turning auto-deploy off, a push
already waiting (or the next one, which may not be the fix) would undo the
rollback straight away. The owner turns it back on once the fix is pushed.
**Alternatives:** versioned config per deployment, as Heroku's releases do
(more storage and UI for a single user); leaving auto-deploy on (the
rollback might last seconds).

### Five previous images are kept per app

**What:** after a successful deploy, the platform keeps the live image and
those of the 5 most recent previously live deployments per app, and
removes the rest. That's how far back a rollback can go.
**Why:** every push builds a new image, and nothing removed them; the
server's disk is 256 GB. Five covers "the last few versions", which is what
rollbacks are for.
**Alternatives:** keeping every image (fills the disk); Docker's own prune
(not per app, and it would also see images the platform doesn't own).

### Draining the old version through Caddy's in-flight counts (3.1, 2026-11-04)

**What:** after the route switches, the deploy waits until Caddy reports no
requests in flight to the old container, at most 15s, then stops it
(SIGTERM, then SIGKILL after 10s). Before, it was removed straight after
the switch.
**Why:** read from Caddy's source (2.11.7): a config change returns as
soon as the old config *starts* shutting down, and the old config keeps
finishing the requests it had already started, against the old upstream,
however long they take. So removing the old container at once could cut
those requests off. Caddy counts in-flight requests per upstream address
in a global table that survives config changes, and
`GET /reverse_proxy/upstreams` reports it, so the old upstream's count is
still visible after its route has gone. The first check comes one interval
(250ms) after the switch, by when any request the old config had just
routed has been counted. Bounded at 15s, so a client holding a request open
(a long poll, a stream) can't hold up the deploy; if Caddy can't be asked,
it waits the whole 15s. A drain cut off by a shutdown hands the job back,
and the retry, finding the deployment healthy, drains again. If a live
deployment's job is given up (say it timed out while draining), it isn't
marked failed; its log says the switch was cut off, and the next deploy or
platform start removes the old container.
**Not done:** setting Caddy's `grace_period`. It belongs to the whole
`http` app, not the `apps` server the platform owns, and `--resume`
ignores `caddy/caddy.json` once Caddy has saved its config. It's not
needed: stopping the old container after the 15s cap also ends any request
still hanging on the old config.
**Alternatives:** a fixed wait (too short for a slow request, or slower
than needed for every deploy); relying on the app's SIGTERM handling alone
(that covers requests the app has, not ones Caddy is still forwarding).
`examples/hello` shuts down gracefully on SIGTERM, as apps should.

### How rollbacks and image cleanup work (3.2, 2026-11-04)

**What:** the details behind the three rollback entries above.
- **The database checks what it can.** A rollback's `rollback_of` is a
  foreign key on `(app_id, rollback_of)`, so it can only name one of the same
  app's deployments; a check requires a rollback to have its image tag from
  the start. The one-in-progress index covers rollbacks like any deploy.
  `CreateRollback` locks the app's row, checks the target, inserts the
  deployment and its job, and turns auto-deploy off, in one transaction: a
  refused rollback changes nothing.
- **What can be rolled back to** is decided in one place
  (`store.CheckRollback`), used both by the API's `rollbackable` flag and by
  `CreateRollback`: a deployment that was live before (superseded or rolled
  back), whose image is among the kept ones, and that isn't the same image as
  the live version.
- **A rollback skips the build, not the checks.** It goes queued → starting
  → healthy: the image is checked to still exist (someone may have pruned it
  by hand; then it fails at once, saying to deploy the commit instead), and
  then it's started, health-checked, switched to and drained like any deploy.
- **Kept images are counted per image, not per deployment:** redeploying a
  commit, or rolling back to it, reuses its tag. The six most recent distinct
  images of ever-live deployments are kept.
- **Cleanup goes by what Docker has.** After each successful deploy, the
  platform lists the images labelled with the app's name (Build labels every
  image) and removes the tags under `ops-platform/<app>:` that aren't kept,
  plus untagged ones (a redeploy of the same commit moves its tag to the new
  build, since each build's labels differ). The image of a deploy that has
  already started is kept too. Removal is never forced, so an image a
  container still uses stays until a later deploy. Going by Docker's list
  rather than the database's history means an image that failed to be
  removed is seen again next time, and nothing outside the app's own images
  is ever touched.

**Alternatives:** recording removed images in the database (another state
to keep in step with Docker); `docker image prune` (not per app, sees
images the platform doesn't own); a CHECK that the rollback target was live
(statuses change after insert; enforced by the locked transaction instead).
