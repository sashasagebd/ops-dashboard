Backend: 
    Go

Frontend: 
    React + Typescript

Reading docker container data: 
    Talk to docker through /var/run/docker.sock, and make sure to put a read only proxy in front of it, so the dashboard cant create or kill containers. 
    Note: mounting the socket with :ro does NOT make it read-only. Anything that can connect to the socket can still send write requests. The real protection is the proxy's filtering: only the endpoints we enable are allowed (CONTAINERS=1), all write requests are blocked (POST=0), and the proxy sits on an internal Compose network with no published port.

Talking to the Docker API: 
    Plain net/http client instead of the official Docker SDK. We only need a couple of endpoints (list containers, container stats), and the SDK brings in a large dependency tree. A small client is easy to read and easy to test with httptest. It sits behind a Go interface, so we can switch to the SDK later without touching anything else.

Serving the frontend: 
    One container. A multi-stage Dockerfile builds the frontend, then the Go binary, and Go serves both the API and the static files from one origin. That means one port to bind to 127.0.0.1, no CORS, and one target for tailscale serve. The static files are read from a folder set by an env var, not go:embed, so the backend CI job can run go build without the frontend having been built first.
Getting container data to the page: 
    A background poller asks Docker every POLL_INTERVAL (default 5s) and keeps the latest snapshot in memory; API requests only read that snapshot. The alternative, calling Docker on every page request, would make each request wait on several Docker calls per container, multiply load with every open tab, and leave nowhere to keep the previous sample that CPU % needs. The Discord alerts (M4) also need something watching continuously, even with no browser open, and they reuse the same poller. 
    Tests call Poll() directly instead of waiting on a timer, so they're fast and never flaky.

Measuring CPU: 
    Docker's stats endpoint normally waits about a second per container to take two CPU samples, which would make one poll of N containers take N seconds. We use one-shot mode instead (one instant sample) and compare it against the previous poll's sample. The cost is that CPU shows "—" for the first poll after startup or a restart. 
    CPU is shown as a share of the whole host (100% = every core busy), not per core like docker stats (where 4 busy cores is 400%). That keeps it on the same scale as the host CPU, so the numbers add up. 
    Memory matches docker stats: usage minus the file cache the kernel can reclaim. Otherwise cached files make containers look much bigger than they are.

When Docker is unreachable: 
    Stale over broken. If a poll fails, the API keeps serving the last good snapshot with "stale": true; it only returns an error if no poll has ever succeeded. The page keeps showing data and a banner says which link is broken (server can't reach Docker, browser can't reach server, or data stopped updating) and how old the data is. A monitoring page that goes blank exactly when something's wrong is the least useful kind.

Refreshing the page: 
    A small custom React hook (usePolling) instead of a library like TanStack Query. There's one endpoint, so the library's caching and deduplication wouldn't earn the dependency. The hook schedules the next fetch after each response (so requests never overlap), pauses while the tab is hidden, and keeps the old data when a refresh fails.
