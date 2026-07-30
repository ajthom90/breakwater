package vault_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	mathrand "math/rand"
	"testing"

	"github.com/ajthom90/breakwater/pkg/contentid"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

// TestPkgContentID_RoundTripWithVault locks the have/want contract (M2-S3):
// pkg/contentid chunks + IDs must match vault PutContent / ObjectFromContents
// content IDs for the same payload. S3-F6: multi-chunk case uses >8 MiB and
// asserts len(ids) > 1.
func TestPkgContentID_RoundTripWithVault(t *testing.T) {
	ctx := context.Background()
	mgr := vault.NewManager(t.TempDir(), t.TempDir())
	defer mgr.CloseAll(ctx)

	v, err := mgr.Create(ctx, "cid-rt", "pw-cid-rt")
	if err != nil {
		t.Fatal(err)
	}
	secret, algo, err := v.HashingKey(ctx)
	if err != nil {
		t.Fatalf("HashingKey: %v", err)
	}

	h, err := contentid.New(algo, secret)
	if err != nil {
		t.Fatalf("contentid.New: %v", err)
	}
	sp, err := contentid.NewSplitter(contentid.SplitterDynamic4M)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	// S3-F6: > maxSize (8 MiB) forces at least one split.
	payload := make([]byte, 10<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		data      []byte
		wantMulti bool
	}{
		{"empty", nil, false},
		{"small", []byte("roundtrip-small-payload"), false},
		{"10MiB-random", payload, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks, ids, err := contentid.ChunkAndID(h, sp, tc.data)
			if err != nil {
				t.Fatal(err)
			}
			if len(chunks) != len(ids) || len(ids) < 1 {
				t.Fatalf("chunks=%d ids=%d", len(chunks), len(ids))
			}
			if tc.wantMulti && len(ids) <= 1 {
				t.Fatalf("S3-F6: expected multi-chunk len(ids)>1, got %d", len(ids))
			}

			// Server PutContent per chunk; IDs must match pkg.
			for i, c := range chunks {
				serverID, err := v.PutContent(ctx, c)
				if err != nil {
					t.Fatalf("PutContent chunk %d (len=%d): %v", i, len(c), err)
				}
				if string(serverID) != ids[i] {
					t.Fatalf("chunk %d ID mismatch: pkg=%s server=%s", i, ids[i], serverID)
				}
			}

			// Have/want: all present.
			cidList := make([]vault.ContentID, len(ids))
			for i, id := range ids {
				cidList[i] = vault.ContentID(id)
			}
			present, err := v.HasContents(ctx, cidList)
			if err != nil {
				t.Fatal(err)
			}
			for i, p := range present {
				if !p {
					t.Fatalf("HasContents[%d]=false for %s", i, ids[i])
				}
			}

			// ObjectFromContents → OpenObject recovers full payload.
			oid, err := v.ObjectFromContents(ctx, cidList)
			if err != nil {
				t.Fatalf("ObjectFromContents: %v", err)
			}
			r, err := v.OpenObject(ctx, oid)
			if err != nil {
				t.Fatalf("OpenObject %s: %v", oid, err)
			}
			got, err := io.ReadAll(r)
			_ = r.Close()
			if err != nil {
				t.Fatal(err)
			}
			want := tc.data
			if want == nil {
				want = []byte{}
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("OpenObject bytes mismatch: got %d want %d", len(got), len(want))
			}
			t.Logf("OK chunks=%d object=%s bytes=%d", len(ids), oid, len(got))
		})
	}
}

// TestS3F8_SplitterBoundaryIdentityWithWriteObject (S3-F8 / S5-F1): pkg splitter
// + hash must match content IDs from vault WriteObject(DYNAMIC) on the same
// payload, in stream order.
//
// S5-F1: do NOT use VerifyObject for sequence — kopia returns map iteration
// order. Use ObjectDataContentIDs (indirect index walk). Deterministic seeds
// including the REVIEW-M2-S5 list live in TestS5F1_SeededSplitterSequenceIdentity.
func TestS3F8_SplitterBoundaryIdentityWithWriteObject(t *testing.T) {
	ctx := context.Background()
	mgr := vault.NewManager(t.TempDir(), t.TempDir())
	defer mgr.CloseAll(ctx)
	v, err := mgr.Create(ctx, "split-id", "pw")
	if err != nil {
		t.Fatal(err)
	}
	secret, algo, err := v.HashingKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h, err := contentid.New(algo, secret)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := contentid.NewSplitter(contentid.SplitterDynamic4M)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	// Deterministic payload (not crypto/rand) so this test is not flaky.
	payload := make([]byte, 10<<20)
	rng := mathrand.New(mathrand.NewSource(42))
	if _, err := rng.Read(payload); err != nil {
		t.Fatal(err)
	}
	chunks, pkgIDs, err := contentid.ChunkAndID(h, sp, payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgIDs) <= 1 {
		t.Fatalf("need multi-chunk, got %d", len(pkgIDs))
	}
	for i, c := range chunks {
		if len(c) > vault.MaxPutContentBytes {
			t.Fatalf("chunk[%d] len=%d > MaxPutContentBytes=%d", i, len(c), vault.MaxPutContentBytes)
		}
	}

	oid, err := v.WriteObject(ctx, vault.SplitterDynamic, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	serverIDs, err := v.ObjectDataContentIDs(ctx, oid)
	if err != nil {
		t.Fatalf("ObjectDataContentIDs: %v", err)
	}
	if len(serverIDs) != len(pkgIDs) {
		t.Fatalf("S3-F8: content count pkg=%d server=%d serverIDs=%v", len(pkgIDs), len(serverIDs), serverIDs)
	}
	for i := range pkgIDs {
		if pkgIDs[i] != string(serverIDs[i]) {
			t.Fatalf("S3-F8: id[%d] pkg=%s server=%s", i, pkgIDs[i], serverIDs[i])
		}
	}
	t.Logf("S3-F8 PASS: %d content IDs match WriteObject(DYNAMIC) in stream order", len(pkgIDs))
}
