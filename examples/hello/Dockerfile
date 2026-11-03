# The platform builds this with Docker's legacy builder (see
# docs/decisions/platform.md), which handles multi-stage builds like this
# one; BuildKit-only features such as RUN --mount aren't available.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /hello .

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /hello /hello
EXPOSE 8080
ENTRYPOINT ["/hello"]
