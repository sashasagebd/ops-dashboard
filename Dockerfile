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
# go.sum* so the build doesn't fail while there are no dependencies yet.
COPY backend/go.mod backend/go.sum* ./
RUN go mod download
COPY backend/ ./
# CGO_ENABLED=0 gives a fully static binary, which the distroless/static
# image below needs (it has no C library).
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/dashboard ./cmd/dashboard

# ── 3. Runtime image ─────────────────────────────────────────────────────
# distroless/static: no shell, no package manager, runs as a non-root user.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=backend /out/dashboard /dashboard
COPY --from=frontend /src/dist /static
ENV STATIC_DIR=/static
EXPOSE 8080
# The image has no curl or shell, so the binary checks itself (exec form, no
# shell needed). Docker marks the container unhealthy after 3 failed checks;
# start-period gives it time to start listening before failures count.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/dashboard", "-healthcheck"]
ENTRYPOINT ["/dashboard"]
