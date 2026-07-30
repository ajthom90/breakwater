# Breakwater server — single static binary, distroless runtime.
# Build from repository root:
#   docker build -f packaging/docker/Dockerfile -t breakwater:dev .

FROM golang:1.23-bookworm AS build
WORKDIR /src
ENV GOTOOLCHAIN=local
ENV CGO_ENABLED=0

# Module files first for better layer caching
COPY go.work go.work.sum* ./
COPY pkg/go.mod pkg/go.sum ./pkg/
COPY server/go.mod server/go.sum ./server/
COPY agent/go.mod agent/go.sum* ./agent/
COPY restore/go.mod restore/go.sum* ./restore/
COPY cli/go.mod cli/go.sum* ./cli/

WORKDIR /src/server
RUN go mod download

WORKDIR /src
COPY pkg/ ./pkg/
COPY server/ ./server/

WORKDIR /src/server
RUN go build -trimpath -ldflags="-s -w -X main.version=0.0.1-dev" \
    -o /out/breakwaterd ./cmd/breakwaterd

FROM gcr.io/distroless/static-debian12:nonroot
USER nonroot:nonroot
COPY --from=build /out/breakwaterd /breakwaterd
EXPOSE 8443 9443
VOLUME ["/data", "/repos"]
# /healthz is HTTPS (M11); use a TLS-capable probe or disable HEALTHCHECK
# when the probe cannot skip cert verification of the self-signed server leaf.
ENTRYPOINT ["/breakwaterd"]
CMD ["-data", "/data", "-repos", "/repos"]
