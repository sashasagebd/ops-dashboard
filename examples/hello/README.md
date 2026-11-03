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

CI deploys it the same way on every push (`.github/workflows/ci.yml`).
