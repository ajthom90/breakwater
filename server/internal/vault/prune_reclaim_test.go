package vault_test

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/vault"
)

func TestPruneReclaimsForgottenContent(t *testing.T) {
	ctx := context.Background()
	mgr := vault.NewManager(t.TempDir())
	defer mgr.CloseAll(ctx)
	v, err := mgr.Create(ctx, "d1", "pw")
	if err != nil {
		t.Fatal(err)
	}

	livePayload := bytes.Repeat([]byte("LIVE-DATA-AAAA"), 200)
	liveOID, err := v.WriteObject(ctx, vault.SplitterFixed4M, bytes.NewReader(livePayload))
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind: vault.KindFileSnapshot, MachineID: "d1", RootObjectID: liveOID, Timestamp: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	deadPayload := bytes.Repeat([]byte("DEAD-DATA-BBBB"), 200)
	deadOID, err := v.WriteObject(ctx, vault.SplitterFixed4M, bytes.NewReader(deadPayload))
	if err != nil {
		t.Fatal(err)
	}
	deadCIDs, err := v.VerifyObject(ctx, deadOID)
	if err != nil {
		t.Fatal(err)
	}
	deadSnap, err := v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind: vault.KindFileSnapshot, MachineID: "d1", RootObjectID: deadOID, Timestamp: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	before, _ := v.Stats(ctx)
	if err := v.DeleteSnapshotRecord(ctx, deadSnap); err != nil {
		t.Fatal(err)
	}
	if err := v.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := v.Stats(ctx)
	t.Logf("user contents %d/%d -> %d/%d (all contents %d -> %d)",
		before.UserContentCount, before.UserSizeBytes,
		after.UserContentCount, after.UserSizeBytes,
		before.ContentCount, after.ContentCount)

	has, err := v.HasContents(ctx, deadCIDs)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range has {
		if p {
			t.Fatalf("dead content still present: %s", deadCIDs[i])
		}
	}
	if r, err := v.OpenObject(ctx, deadOID); err == nil {
		n, _ := io.Copy(io.Discard, r)
		r.Close()
		t.Fatalf("dead object still readable n=%d", n)
	}
	r, err := v.OpenObject(ctx, liveOID)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(got, livePayload) {
		t.Fatal("live payload mismatch")
	}
	if after.UserContentCount >= before.UserContentCount {
		t.Fatalf("user content count did not shrink: %d -> %d", before.UserContentCount, after.UserContentCount)
	}
	if after.UserSizeBytes >= before.UserSizeBytes {
		t.Fatalf("user size did not shrink: %d -> %d", before.UserSizeBytes, after.UserSizeBytes)
	}
}
