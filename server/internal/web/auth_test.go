package web_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/web"
)

func TestAPIToken_UnauthenticatedRejected(t *testing.T) {
	h, token, _ := testHandler(t)
	// healthz open
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rr.Code)
	}

	paths := []string{
		"/api/v1/summary",
		"/api/v1/machines",
		"/api/v1/jobs",
		"/api/v1/snapshots",
		"/api/v1/audit",
		"/api/v1/events",
	}
	for _, p := range paths {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s: want 401 got %d body=%s", p, rr.Code, rr.Body.String())
		}
	}

	// Wrong token
	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/summary", nil)
	req.Header.Set("Authorization", "Bearer wrong-token-value")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", rr.Code)
	}

	// Good token
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/summary", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("good token: %d %s", rr.Code, rr.Body.String())
	}
	_ = token
}

func TestAPIToken_NotLoggedInFull(t *testing.T) {
	dir := t.TempDir()
	var buf strings.Builder
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	tok, err := web.LoadOrCreateAPIToken(dir, log)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) < 32 {
		t.Fatalf("token too short: %d", len(tok))
	}
	out := buf.String()
	if strings.Contains(out, tok) {
		t.Fatalf("full API token must not appear in logs; log=%s", out)
	}
	preview := web.TokenPreview(tok)
	if !strings.Contains(out, preview) {
		t.Fatalf("expected preview %q in log; got %s", preview, out)
	}
	// File exists with 0600
	info, err := os.Stat(filepath.Join(dir, web.APITokenFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("api-token perms too open: %o", info.Mode().Perm())
	}
	// Reload same token
	tok2, err := web.LoadOrCreateAPIToken(dir, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if tok2 != tok {
		t.Fatal("token should be stable across reloads")
	}
}

func TestAPIToken_XHeaderAndQuery(t *testing.T) {
	h, token, _ := testHandler(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/summary", nil)
	req.Header.Set("X-API-Token", token)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("X-API-Token: %d", rr.Code)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/summary?token="+token, nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("query token: %d", rr.Code)
	}
}

func testHandler(t *testing.T) (http.Handler, string, *catalog.DB) {
	t.Helper()
	db, err := catalog.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	token := "test-dev-token-aaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hub := scheduler.NewEventHub()
	h := web.NewHandler(web.Config{
		DB:       db,
		Events:   hub,
		APIToken: token,
		Version:  "test",
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return h, token, db
}

func TestMachinesAndSummary_RealCatalog(t *testing.T) {
	h, token, db := testHandler(t)
	ctx := context.Background()
	if err := db.InsertMachine(ctx, catalog.Machine{
		ID: "m1", CertFP: "fp1", Hostname: "host-a", OSInfo: "linux",
		AgentVersion: "0.0.1", Status: catalog.MachineStatusActive, RepoID: "m1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertMachine(ctx, catalog.Machine{
		ID: "m2", CertFP: "fp2", Hostname: "host-b", OSInfo: "windows",
		AgentVersion: "0.0.1", Status: catalog.MachineStatusEnrolled, RepoID: "m2",
	}); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/summary", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	var sum map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	if int(sum["machines_total"].(float64)) != 2 {
		t.Fatalf("machines_total=%v", sum["machines_total"])
	}
	if int(sum["machines_online"].(float64)) != 1 {
		t.Fatalf("machines_online=%v", sum["machines_online"])
	}
	if sum["capacity_bytes"] != nil {
		t.Fatal("capacity_bytes must be null in M2")
	}
	if sum["capacity_note"] == nil || sum["capacity_note"] == "" {
		t.Fatal("capacity_note must label the placeholder")
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/machines", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(rr, req)
	var list struct {
		Machines []struct {
			ID, Hostname, Status string
		} `json:"machines"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Machines) != 2 {
		t.Fatalf("machines=%d", len(list.Machines))
	}
}
