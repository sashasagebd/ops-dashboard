# syntax=docker/dockerfile:1

# ── 1. Build the frontend ────────────────────────────────────────────────
FROM node:24-alpine AS frontend
WORKDIR /src
# Dependencies first, so this layer is cached until package*.json changes.
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# ── 2. Build the backend ─────────────────────────────────────────────────
FROM golang:1.27-alpine AS backend
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
# CGO_ENABLED=0 gives fully static binaries, which the distroless/static
# image below needs (it has no C library). Both binaries go in one image;
# Compose picks which one each service runs.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/dashboard ./cmd/platform

# ── 3. Runtime image ─────────────────────────────────────────────────────
# distroless/static: no shell, no package manager, runs as a non-root user.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=backend /out/dashboard /out/platform /
COPY --from=frontend /src/dist /static
ENV STATIC_DIR=/static
EXPOSE 8080
# The image has no curl or shell, so the binary checks itself (exec form, no
# shell needed). Docker marks the container unhealthy after 3 failed checks;
# start-period gives it time to start listening before failures count.
# These defaults are the dashboard's; the platform's Compose service overrides
# the entrypoint and healthcheck.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/dashboard", "-healthcheck"]
ENTRYPOINT ["/dashboard"]
