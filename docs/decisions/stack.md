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