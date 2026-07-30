package web

import (
	"net/http"
	"strings"
)

// RequireAPIToken gates /api/v1/* with the dev local API token.
// Accepts Authorization: Bearer <token> or X-API-Token: <token>.
// This middleware is the single future attachment point for M6 sessions.
func RequireAPIToken(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := extractAPIToken(r)
			if !ConstantTimeTokenEqual(got, token) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="breakwater"`)
				writeJSONError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func extractAPIToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		const p = "Bearer "
		if strings.HasPrefix(h, p) {
			return strings.TrimSpace(h[len(p):])
		}
		// Case-insensitive Bearer
		if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
			return strings.TrimSpace(h[7:])
		}
	}
	if t := r.Header.Get("X-API-Token"); t != "" {
		return strings.TrimSpace(t)
	}
	// Query param for EventSource (cannot set Authorization headers).
	// Documented as dev-only; M6 sessions will use cookies.
	if t := r.URL.Query().Get("token"); t != "" {
		return strings.TrimSpace(t)
	}
	return ""
}
