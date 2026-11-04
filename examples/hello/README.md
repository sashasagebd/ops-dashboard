# hello

A minimal app for trying the platform: a Go web server on port 8080 with a
`/healthz` endpoint, which greets with the `GREETING` env var if it's set.

Register it from the dashboard's Apps page with:

| Field | Value |
|---|---|
| Name | `hello` |
| Repo URL | `https://github.com/sashasagebd/ops-dashboard` |
| Source directory | `examples/hello` |
| Port | `8080` |
| Health check path | `/healthz` |

Then add an env var `GREETING` and deploy a commit of this repo. The app is
served at `hello.<APPS_DOMAIN>`.

For trying out failure handling:

- **A broken deploy:** set the env var `BROKEN` to `1` and deploy. `/healthz`
  answers 500, so the deploy fails its health check and never goes live; the
  previous version keeps serving. Delete `BROKEN` to deploy normally again.
- **A slow request:** `/slow?seconds=N` (0 to 30) answers after N seconds,
  saying which container answered. One started just before a deploy's
  switch still finishes on the old version: the platform stops it only once
  its requests are done (up to 15s).
- **A database:** press "Add a database" on its page, then deploy. `/visits`
  counts visits in it ("Visits: 3"), and the count survives deploys, the
  database's container being recreated, and a backup restored. Without a
  database it answers 404.
- **Graceful shutdown:** on SIGTERM (how Docker stops a container), it stops
  taking new connections and finishes the requests it has, within 9s.

CI deploys it the same way on every push (`.github/workflows/ci.yml`).
