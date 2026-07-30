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
// pkg/contentid chunks + IDs must match vault PutContent / ObjectFromContents /
// WriteObject-equivalent content IDs for the same payload.
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

	// Random multi-MiB payload so CDC may produce multiple chunks.
	payload := make([]byte, 3<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	// Also cover empty + small.
	cases := []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"small", []byte("roundtrip-small-payload")},
		{"3MiB-random", payload},
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
