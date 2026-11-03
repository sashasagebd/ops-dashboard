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
