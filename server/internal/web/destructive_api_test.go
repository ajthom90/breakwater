package web_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/web"
)

// TestM5F1_DestructiveAPIDisabledByDefault: with EnableDestructiveAPI false
// (production default), retention-mutating POSTs are 403 while GETs still work
// with a valid token.
func TestM5F1_DestructiveAPIDisabledByDefault(t *testing.T) {
	h, token, _ := testHandlerDestructive(t, false)

	// Read still works.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/summary", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET summary want 200 got %d %s", rr.Code, rr.Body.String())
	}

	destructive := []string{
		"/api/v1/snapshots/snap1/forget",
		"/api/v1/snapshots/snap1/undelete",
		"/api/v1/machines/m1/prune",
		"/api/v1/machines/m1/retention",
		"/api/v1/machines/m1/scrub",
	}
	for _, p := range destructive {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, p, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s: want 403 got %d body=%s", p, rr.Code, rr.Body.String())
		}
	}
}

// TestM5F1_DestructiveAPIEnabledPassesGate: when opted in, the gate allows the
// request through (may 503 if retention not wired — gate itself must not 403).
func TestM5F1_DestructiveAPIEnabledPassesGate(t *testing.T) {
	h, token, _ := testHandlerDestructive(t, true)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/machines/m1/prune", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(rr, req)
	if rr.Code == http.StatusForbidden {
		t.Fatalf("enabled: prune must not be 403, got %d %s", rr.Code, rr.Body.String())
	}
	// Retention service nil → 503 is fine; proves we passed the opt-in gate.
	if rr.Code != http.StatusServiceUnavailable && rr.Code != http.StatusOK && rr.Code != http.StatusInternalServerError {
		// Accept any non-403 after gate.
		t.Logf("enabled prune status=%d body=%s (gate open)", rr.Code, rr.Body.String())
	}
}

func testHandlerDestructive(t *testing.T, enableDestructive bool) (http.Handler, string, *catalog.DB) {
	t.Helper()
	db, err := catalog.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	token := "test-dev-token-bbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	hub := scheduler.NewEventHub()
	h := web.NewHandler(web.Config{
		DB:                   db,
		Events:               hub,
		APIToken:             token,
		EnableDestructiveAPI: enableDestructive,
		Version:              "test",
		Log:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return h, token, db
}
