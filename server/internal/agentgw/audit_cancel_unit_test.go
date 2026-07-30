package agentgw

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/ajthom90/breakwater/server/internal/audit"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/enroll"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/mtls"
)

// TestAuthFail_AuditDespiteCanceledContext is S1-F1 interceptor path: a
// pre-canceled request context on pin rejection must still leave an auth.fail
// row. White-box: invoke unaryInterceptor with a done ctx and synthetic TLS peer
// (client-side expired deadlines often never reach the server interceptor).
//
// Red-first on bc65f8a: interceptor Append(ctx) with canceled ctx → no row.
func TestAuthFail_AuditDespiteCanceledContext(t *testing.T) {
	tmp := t.TempDir()
	db, err := catalog.Open(filepath.Join(tmp, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ks, err := keystore.OpenOrCreate(db, filepath.Join(tmp, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	serverID, err := mtls.GenerateServerIdentity("bw", []string{"127.0.0.1"}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	attacker, err := mtls.GenerateAgentIdentity("attacker-cancel", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	auditor := audit.NewWriter(db)
	svc := &enroll.Service{
		DB: db, Keystore: ks, ServerFP: serverID.Fingerprint(),
		Log: slog.Default(),
	}
	g := New(serverID, svc, slog.Default())
	g.Auditor = auditor

	// Pre-canceled request context + peer presenting an unknown client cert.
	done, doneCancel := context.WithCancel(context.Background())
	doneCancel()
	done = peer.NewContext(done, &peer.Peer{
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{attacker.Cert},
			},
		},
	})

	info := &grpc.UnaryServerInfo{FullMethod: "/breakwater.v1.DataService/CheckContents"}
	_, err = g.unaryInterceptor(done, nil, info, func(ctx context.Context, req any) (any, error) {
		t.Fatal("handler must not run for unenrolled peer")
		return nil, nil
	})
	if err == nil {
		t.Fatal("expected PermissionDenied from pin rejection")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
	t.Logf("interceptor rejected as expected: %v", err)

	rows, err := auditor.ListByAction(context.Background(), audit.ActionAuthFail)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 1 {
		t.Fatal("expected auth.fail audit row despite pre-canceled ctx; got none (S1-F1)")
	}
	if err := auditor.VerifyChain(context.Background()); err != nil {
		t.Fatalf("audit chain: %v", err)
	}
	t.Logf("auth.fail audit landed despite canceled ctx: id=%s actor=%s…", rows[0].ID, rows[0].Actor[:16])
}
