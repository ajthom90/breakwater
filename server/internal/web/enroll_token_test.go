package web_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
	"github.com/ajthom90/breakwater/server/internal/agentgw"
	"github.com/ajthom90/breakwater/server/internal/audit"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/enroll"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/mtls"
	"github.com/ajthom90/breakwater/server/internal/vault"
	"github.com/ajthom90/breakwater/server/internal/web"
)

// enrollTokenEnv is a full stack for mint-via-REST → enroll-via-mTLS.
type enrollTokenEnv struct {
	t        *testing.T
	DB       *catalog.DB
	Auditor  *audit.Writer
	Handler  http.Handler
	APIToken string
	ServerFP string
	GWAddr   string
	LogBuf   *bytes.Buffer
	VM       *vault.Manager
	KS       *keystore.Store
}

func startEnrollTokenEnv(t *testing.T) *enrollTokenEnv {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	db, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ks, err := keystore.OpenOrCreate(db, filepath.Join(tmp, "keys", "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	serverID, err := mtls.GenerateServerIdentity("bw-mint", []string{"127.0.0.1", "localhost"}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	serverFP := serverID.Fingerprint()
	vm := vault.NewManager(filepath.Join(tmp, "repos"), filepath.Join(tmp, "data"))
	t.Cleanup(func() { _ = vm.CloseAll(ctx) })

	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(io.MultiWriter(&logBuf, io.Discard), &slog.HandlerOptions{Level: slog.LevelDebug}))
	auditor := audit.NewWriter(db)
	enrollSvc := &enroll.Service{
		DB: db, Keystore: ks, Vaults: &mintVault{m: vm}, ServerFP: serverFP,
		DefaultPolicy: "01DEFAULTPOLICY000000000000", Log: log,
	}
	gw := agentgw.New(serverID, enrollSvc, log)
	gw.Auditor = auditor
	addr, err := gw.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.GracefulStop)

	apiToken := "test-api-token-mint-aaaaaaaaaaaaaaaa"
	h := web.NewHandler(web.Config{
		DB: db, Auditor: auditor, APIToken: apiToken, ServerFP: serverFP, Log: log,
	})
	return &enrollTokenEnv{
		t: t, DB: db, Auditor: auditor, Handler: h, APIToken: apiToken,
		ServerFP: serverFP, GWAddr: addr, LogBuf: &logBuf, VM: vm, KS: ks,
	}
}

type mintVault struct{ m *vault.Manager }

func (v *mintVault) Create(ctx context.Context, repoID, password string) ([]byte, string, error) {
	vv, err := v.m.Create(ctx, repoID, password)
	if err != nil {
		return nil, "", err
	}
	return vv.HashingKey(ctx)
}

func mintViaREST(t *testing.T, env *enrollTokenEnv, advertise string, ttlSec int, withAuth bool) (status int, body map[string]any, rawBody string) {
	t.Helper()
	payload := map[string]any{
		"advertise_addr": advertise,
		"ttl_seconds":    ttlSec,
		"note":           "vm-test",
		"created_by":     "test",
	}
	if ttlSec <= 0 {
		delete(payload, "ttl_seconds")
	}
	b, _ := json.Marshal(payload)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/enroll-tokens", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if withAuth {
		req.Header.Set("Authorization", "Bearer "+env.APIToken)
	}
	env.Handler.ServeHTTP(rr, req)
	rawBody = rr.Body.String()
	_ = json.Unmarshal([]byte(rawBody), &body)
	return rr.Code, body, rawBody
}

// TestMintEnrollToken_UnauthenticatedRejected: no API token → 401.
func TestMintEnrollToken_UnauthenticatedRejected(t *testing.T) {
	env := startEnrollTokenEnv(t)
	code, _, _ := mintViaREST(t, env, "10.0.0.5:9443", 0, false)
	if code != http.StatusUnauthorized {
		t.Fatalf("want 401 got %d", code)
	}
}

// TestMintEnrollToken_EndToEnd: REST mint → agent enroll over mTLS → machine in catalog.
func TestMintEnrollToken_EndToEnd(t *testing.T) {
	env := startEnrollTokenEnv(t)
	ctx := context.Background()

	// Advertise the actual gateway address so the agent dials the test server.
	code, body, raw := mintViaREST(t, env, env.GWAddr, 0, true)
	if code != http.StatusCreated {
		t.Fatalf("mint: status=%d body=%s", code, raw)
	}
	tok, _ := body["token"].(string)
	id, _ := body["id"].(string)
	if tok == "" || id == "" {
		t.Fatalf("missing token/id: %v", body)
	}
	if !strings.HasPrefix(tok, "BW1:") {
		t.Fatalf("token prefix: %s", tok)
	}
	// Secret must never appear in server logs.
	logOut := env.LogBuf.String()
	// Extract secret (last field).
	parts := strings.Split(tok, ":")
	secret := parts[len(parts)-1]
	if secret == "" {
		t.Fatal("empty secret")
	}
	if strings.Contains(logOut, secret) {
		t.Fatal("secret leaked into logs")
	}
	if strings.Contains(logOut, tok) {
		t.Fatal("full token leaked into logs")
	}

	// Stored as hash only.
	row, err := env.DB.EnrollTokenByID(ctx, id)
	if err != nil || row == nil {
		t.Fatalf("token row: %v", err)
	}
	if row.SecretHash == secret || row.SecretHash == tok {
		t.Fatal("plaintext secret stored in catalog")
	}
	if row.SecretHash != catalog.HashSecret(secret) {
		t.Fatal("stored hash does not match secret")
	}

	// Audit machine.token_create + chain.
	rows, err := env.Auditor.ListByAction(ctx, audit.ActionMachineTokenCreate)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 1 {
		t.Fatal("expected machine.token_create audit row")
	}
	found := false
	for _, r := range rows {
		if r.Target == id {
			found = true
			if strings.Contains(r.Detail, secret) || strings.Contains(r.Detail, tok) {
				t.Fatal("secret in audit detail")
			}
		}
	}
	if !found {
		t.Fatal("no audit for minted token id")
	}
	if err := env.Auditor.VerifyChain(ctx); err != nil {
		t.Fatalf("audit chain: %v", err)
	}

	// Enroll using the returned token.
	agentID, err := mtls.GenerateAgentIdentity("minted-agent", 365*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(env.GWAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(mtls.ClientTLSConfig(agentID, env.ServerFP))),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	resp, err := breakwaterv1.NewEnrollmentServiceClient(conn).Enroll(ctx, &breakwaterv1.EnrollRequest{
		Token: tok,
		AgentInfo: &breakwaterv1.AgentInfo{
			Hostname: "minted-host", Os: "linux", AgentVersion: "0.0.1", Arch: "amd64",
		},
		ClientCertPem: agentID.CertPEM,
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if resp.MachineId == "" {
		t.Fatal("empty machine id")
	}
	m, err := env.DB.MachineByID(ctx, resp.MachineId)
	if err != nil || m == nil {
		t.Fatalf("machine missing: %v", err)
	}
	if m.Hostname != "minted-host" {
		t.Fatalf("hostname %s", m.Hostname)
	}

	// Reuse rejected.
	_, err = breakwaterv1.NewEnrollmentServiceClient(conn).Enroll(ctx, &breakwaterv1.EnrollRequest{
		Token: tok,
		AgentInfo: &breakwaterv1.AgentInfo{
			Hostname: "again", Os: "linux", AgentVersion: "0.0.1", Arch: "amd64",
		},
		ClientCertPem: agentID.CertPEM,
	})
	if err == nil {
		t.Fatal("token reuse must fail")
	}
	t.Logf("mint→enroll OK machine=%s reuse rejected", resp.MachineId)
}

// TestMintEnrollToken_ExpiredRejected: short TTL, wait, enroll fails.
func TestMintEnrollToken_ExpiredRejected(t *testing.T) {
	env := startEnrollTokenEnv(t)
	ctx := context.Background()
	code, body, raw := mintViaREST(t, env, env.GWAddr, 1, true) // 1 second
	if code != http.StatusCreated {
		t.Fatalf("mint: %d %s", code, raw)
	}
	tok, _ := body["token"].(string)
	time.Sleep(1100 * time.Millisecond)

	agentID, err := mtls.GenerateAgentIdentity("expired-agent", 365*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(env.GWAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(mtls.ClientTLSConfig(agentID, env.ServerFP))),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_, err = breakwaterv1.NewEnrollmentServiceClient(conn).Enroll(ctx, &breakwaterv1.EnrollRequest{
		Token: tok,
		AgentInfo: &breakwaterv1.AgentInfo{
			Hostname: "late", Os: "linux", AgentVersion: "0.0.1", Arch: "amd64",
		},
		ClientCertPem: agentID.CertPEM,
	})
	if err == nil {
		t.Fatal("expired token must be rejected")
	}
	t.Logf("expired rejected: %v", err)
}

// TestMintEnrollToken_MissingAdvertise: advertise_addr required.
func TestMintEnrollToken_MissingAdvertise(t *testing.T) {
	env := startEnrollTokenEnv(t)
	b, _ := json.Marshal(map[string]any{"ttl_seconds": 3600})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/enroll-tokens", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.APIToken)
	env.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d %s", rr.Code, rr.Body.String())
	}
}

// TestMintEnrollToken_NotBehindDestructiveGate: mint works with EnableDestructiveAPI=false.
func TestMintEnrollToken_NotBehindDestructiveGate(t *testing.T) {
	env := startEnrollTokenEnv(t)
	// Handler already has EnableDestructiveAPI=false (default).
	code, _, raw := mintViaREST(t, env, "10.0.0.5:9443", 3600, true)
	if code != http.StatusCreated {
		t.Fatalf("mint must work without --enable-destructive-api: %d %s", code, raw)
	}
}

// TestMintEnrollToken_ListNeverReturnsSecret.
func TestMintEnrollToken_ListNeverReturnsSecret(t *testing.T) {
	env := startEnrollTokenEnv(t)
	code, body, _ := mintViaREST(t, env, "10.0.0.5:9443", 3600, true)
	if code != http.StatusCreated {
		t.Fatal(code)
	}
	tok, _ := body["token"].(string)
	secret := strings.Split(tok, ":")[len(strings.Split(tok, ":"))-1]

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/enroll-tokens", nil)
	req.Header.Set("Authorization", "Bearer "+env.APIToken)
	env.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), secret) || strings.Contains(rr.Body.String(), tok) {
		t.Fatal("list response leaked secret/token")
	}
	if strings.Contains(rr.Body.String(), "secret_hash") {
		t.Fatal("list must not expose secret_hash")
	}
}
