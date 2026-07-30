package vault_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
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

// TestS3F8_SplitterBoundaryIdentityWithWriteObject (S3-F8): pkg splitter + hash
// must match content IDs from vault WriteObject(DYNAMIC) on the same payload.
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

	payload := make([]byte, 10<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	_, pkgIDs, err := contentid.ChunkAndID(h, sp, payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgIDs) <= 1 {
		t.Fatalf("need multi-chunk, got %d", len(pkgIDs))
	}

	oid, err := v.WriteObject(ctx, vault.SplitterDynamic, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	serverIDs, err := v.VerifyObject(ctx, oid)
	if err != nil {
		t.Fatal(err)
	}
	// Filter out indirect-index contents (prefixed x/g-z).
	var dataIDs []string
	for _, id := range serverIDs {
		s := string(id)
		if len(s) > 0 && s[0] >= 'g' && s[0] <= 'z' {
			continue // metadata/index prefix
		}
		dataIDs = append(dataIDs, s)
	}
	if len(dataIDs) != len(pkgIDs) {
		t.Fatalf("S3-F8: content count pkg=%d server=%d (all=%v)", len(pkgIDs), len(dataIDs), serverIDs)
	}
	// Order and values must match (same splitter + same keyed hash).
	for i := range pkgIDs {
		if pkgIDs[i] != dataIDs[i] {
			t.Fatalf("S3-F8: id[%d] pkg=%s server=%s", i, pkgIDs[i], dataIDs[i])
		}
	}
	t.Logf("S3-F8 PASS: %d content IDs match WriteObject(DYNAMIC)", len(pkgIDs))
}
