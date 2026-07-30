# Breakwater server — single static binary, distroless runtime.
# Build from repository root:
#   docker build -f packaging/docker/Dockerfile -t breakwater:dev .
#
# go.work members (keep this list in sync when adding modules):
#   agent, cli, pkg, restore, server, tools/golden
# Module go.mod (+ go.sum) for EVERY workspace member must be COPYed before
# `go mod download`, or the workspace load fails even if the binary does not
# import that package.

FROM node:22-bookworm AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
# Vite outDir is server/internal/web/dist (relative to web/)
RUN mkdir -p /src/server/internal/web && npm run build

FROM golang:1.23-bookworm AS build
WORKDIR /src
ENV GOTOOLCHAIN=local
ENV CGO_ENABLED=0

# --- go.work + per-module go.mod/go.sum (cache layer; one COPY per workspace member) ---
COPY go.work go.work.sum* ./
# pkg
COPY pkg/go.mod pkg/go.sum ./pkg/
# server
COPY server/go.mod server/go.sum ./server/
# agent
COPY agent/go.mod agent/go.sum* ./agent/
# restore
COPY restore/go.mod restore/go.sum* ./restore/
# cli
COPY cli/go.mod cli/go.sum* ./cli/
# tools/golden (stage 4 — required by go.work even though breakwaterd does not import it)
COPY tools/golden/go.mod tools/golden/go.sum ./tools/golden/

WORKDIR /src/server
RUN go mod download

# --- sources needed to build breakwaterd (server + shared pkg) ---
WORKDIR /src
COPY pkg/ ./pkg/
COPY server/ ./server/
# Embed real UI (overwrites placeholder if present)
COPY --from=web /src/server/internal/web/dist ./server/internal/web/dist

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
