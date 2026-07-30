// Package web serves the HTTPS :8443 surface: REST API, SSE live events, and the
// embedded React UI (M2 stage 5).
//
// # Auth (M2 decision — deliberate placeholder for M6 sessions)
//
// Every /api/v1/* route is gated by RequireAPIToken middleware. Today it enforces
// a single dev-only local API token stored at <dataDir>/api-token (generated on
// first boot, 0600, printed once to the log as a truncated form — never logged
// in full). This is the obvious single middleware where real sessions + argon2id
// + TOTP land in M6; do not scatter auth checks into individual handlers.
//
// /healthz and /version remain open (probes / ops). Mutating admin surfaces are
// NOT in M2 scope — M2 adds no write RPCs on :8443.
//
// # Audit (read-only GETs)
//
// Read-only REST GETs are intentionally NOT audited (noise). See the audit
// package policy comment. Any future mutating endpoint on :8443 MUST be audited.
//
// # UI embed
//
// The Vite build output is embedded from dist/. A committed placeholder
// (dist/index.html) keeps go build working when the real UI has not been built.
// Run `make web` to produce the production bundle into this package's dist/.
package web
