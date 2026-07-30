package vault_test

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/hashing"
	"golang.org/x/crypto/blake2b"

	"github.com/ajthom90/breakwater/server/internal/vault"
)

// hashParams implements hashing.Parameters for CreateHashFunc (R2-14).
type hashParams struct {
	algo   string
	secret []byte
}

func (p hashParams) GetHashFunction() string { return p.algo }
func (p hashParams) GetHmacSecret() []byte   { return p.secret }

// agentSideContentID reproduces a kopia content ID using ONLY the enrollment-
// returned (algorithm, secret). kopia's HashFunc type references internal/gather
// which external modules cannot import, so for the default BLAKE2B-256-128 we
// mirror CreateHashFunc's keyed-blake2b + 16-byte truncate, then IDFromHash.
// CreateHashFunc is still invoked to prove the algorithm name is accepted.
func agentSideContentID(t *testing.T, algo string, secret, payload []byte) string {
	t.Helper()
	// Validate algorithm is known to kopia.
	if _, err := hashing.CreateHashFunc(hashParams{algo: algo, secret: secret}); err != nil {
		t.Fatalf("CreateHashFunc(%s): %v", algo, err)
	}
	switch algo {
	case hashing.DefaultAlgorithm: // BLAKE2B-256-128
		h, err := blake2b.New256(secret)
		if err != nil {
			t.Fatalf("blake2b.New256: %v", err)
		}
		if _, err := h.Write(payload); err != nil {
			t.Fatal(err)
		}
		sum := h.Sum(nil)[:16]
		id, err := content.IDFromHash("", sum)
		if err != nil {
			t.Fatalf("IDFromHash: %v", err)
		}
		return id.String()
	default:
		// Fallback: document unsupported for this test; fail clearly.
		t.Fatalf("test helper does not implement algorithm %q (default is %s)", algo, hashing.DefaultAlgorithm)
		return ""
	}
}

// TestHashingKeyReproducesContentIDs is R2-14: the agent, given only the
// enrollment-returned algorithm + secret, must compute the same content ID
// that PutContent returns for a known payload (have/want contract).
func TestHashingKeyReproducesContentIDs(t *testing.T) {
	ctx := context.Background()
	mgr := vault.NewManager(t.TempDir(), t.TempDir())
	defer mgr.CloseAll(ctx)

	v, err := mgr.Create(ctx, "hash-rt", "pw-hash-rt")
	if err != nil {
		t.Fatal(err)
	}

	secret, algo, err := v.HashingKey(ctx)
	if err != nil {
		t.Fatalf("HashingKey: %v", err)
	}
	if len(secret) == 0 || algo == "" {
		t.Fatalf("empty hashing material: secretLen=%d algo=%q", len(secret), algo)
	}
	t.Logf("algo=%s secretLen=%d secretPrefix=%s", algo, len(secret), hex.EncodeToString(secret[:min(4, len(secret))]))

	payload := []byte("breakwater-have-want-roundtrip-payload-v1")
	serverID, err := v.PutContent(ctx, payload)
	if err != nil {
		t.Fatalf("PutContent: %v", err)
	}

	agentID := agentSideContentID(t, algo, secret, payload)
	if agentID != string(serverID) {
		t.Fatalf("content ID mismatch: agent=%s server=%s (have/want broken)", agentID, serverID)
	}
	t.Logf("have/want OK: contentID=%s", serverID)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
