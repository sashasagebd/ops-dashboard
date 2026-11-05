# The platform builds this with Docker's legacy builder (see
# docs/decisions/platform.md), which handles multi-stage builds like this
# one; BuildKit-only features such as RUN --mount aren't available.
FROM golang:1.27-alpine AS build
WORKDIR /src
# Dependencies first (pgx, for /visits), so a code change doesn't download
# them again: this layer is cached until go.mod or go.sum changes.
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /hello .

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /hello /hello
EXPOSE 8080
ENTRYPOINT ["/hello"]
