package mtls_test

import (
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/mtls"
)

func TestIdentityRoundTripAndFingerprint(t *testing.T) {
	id, err := mtls.GenerateServerIdentity("bw-test", []string{"127.0.0.1", "localhost"}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if id.Cert.IsCA {
		t.Fatal("server leaf must not be CA")
	}
	fp1 := id.Fingerprint()
	if len(fp1) != 64 {
		t.Fatalf("fp length %d", len(fp1))
	}

	loaded, err := mtls.LoadIdentityFromPEM(id.CertPEM, id.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	fp2 := loaded.Fingerprint()
	if fp1 != fp2 {
		t.Fatalf("fingerprint unstable: %s vs %s", fp1, fp2)
	}

	agent, err := mtls.GenerateAgentIdentity("agent-1", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if agent.Fingerprint() == fp1 {
		t.Fatal("agent and server fingerprints collided")
	}
	parsed, err := mtls.ParseCertPEM(agent.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	if mtls.CertFingerprintFromTLS(parsed) != agent.Fingerprint() {
		t.Fatal("ParseCertPEM fingerprint mismatch")
	}
}
