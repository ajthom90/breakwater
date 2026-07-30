# Breakwater Web UI

React 18 + TypeScript + Vite static shell embedded into `breakwaterd` via `go:embed`.

## Stack

- React 18, TypeScript, Vite
- Tailwind CSS
- TanStack Router + TanStack Query
- Recharts (available; M2 dashboard uses simple tiles)

## Build

From repo root:

```bash
make web
# → builds into server/internal/web/dist (go:embed path)
```

Or:

```bash
cd web
npm ci
npx tsc --noEmit -p tsconfig.app.json
npm run build
```

### Backend-only contributors

`server/internal/web/dist/` always contains at least a minimal `index.html` so
`go build ./...` works without Node. Run `make web` before packaging a binary
you intend to use in a browser.

## Dev server

```bash
# Terminal 1: breakwaterd on :8443
# Terminal 2:
cd web && npm run dev
```

Vite proxies `/api` and `/healthz` to `https://127.0.0.1:8443` (self-signed ok).

## Auth (M2)

Paste the contents of `<dataDir>/api-token` into **Settings** (stored in
`localStorage`). All `/api/v1/*` calls send `Authorization: Bearer …`. Real
sessions replace this in M6.
