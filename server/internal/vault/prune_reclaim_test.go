package vault_test

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/vault"
)

func TestPruneReclaimsForgottenContent(t *testing.T) {
	ctx := context.Background()
	reposDir := t.TempDir()
	mgr := vault.NewManager(reposDir, reposDir)
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
	liveRoot := wrapFilePayloadRoot(t, ctx, v, "live.bin", liveOID, int64(len(livePayload)))
	_, err = v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind: vault.KindFileSnapshot, MachineID: "d1", RootObjectID: liveRoot, Timestamp: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Large incompressible dead payload so on-disk pack reclamation is measurable (R2-13).
	// Repeating strings compress to nearly nothing under zstd and hide real GC.
	deadPayload := make([]byte, 1<<20) // 1 MiB
	fillDeterministic(deadPayload, 0xdead)
	deadOID, err := v.WriteObject(ctx, vault.SplitterFixed4M, bytes.NewReader(deadPayload))
	if err != nil {
		t.Fatal(err)
	}
	deadRoot := wrapFilePayloadRoot(t, ctx, v, "dead.bin", deadOID, int64(len(deadPayload)))
	deadCIDs, err := v.VerifyObject(ctx, deadOID)
	if err != nil {
		t.Fatal(err)
	}
	// Include tree root contents among things that must be reclaimed.
	rootCIDs, err := v.VerifyObject(ctx, deadRoot)
	if err != nil {
		t.Fatal(err)
	}
	deadCIDs = append(deadCIDs, rootCIDs...)
	deadSnap, err := v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind: vault.KindFileSnapshot, MachineID: "d1", RootObjectID: deadRoot, Timestamp: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	repoPath := filepath.Join(reposDir, "d1")
	beforeDisk := diskBytes(t, repoPath)
	before, _ := v.Stats(ctx)
	if err := v.DeleteSnapshotRecord(ctx, deadSnap); err != nil {
		t.Fatal(err)
	}
	// Reclamation tests opt into zero min-age so young test data is eligible.
	if err := pruneForTest(ctx, v); err != nil {
		t.Fatal(err)
	}
	after, _ := v.Stats(ctx)
	afterDisk := diskBytes(t, repoPath)
	t.Logf("user contents %d/%d -> %d/%d (all contents %d -> %d); disk %d -> %d",
		before.UserContentCount, before.UserSizeBytes,
		after.UserContentCount, after.UserSizeBytes,
		before.ContentCount, after.ContentCount,
		beforeDisk, afterDisk)

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
	// R2-13: assert actual on-disk bytes shrink, not only index stats.
	if afterDisk >= beforeDisk {
		t.Fatalf("on-disk bytes did not shrink after prune: %d -> %d", beforeDisk, afterDisk)
	}
	// Material shrinkage: at least a quarter of the dead payload should leave disk
	// (packs/indexes have overhead; require a meaningful decrease).
	if saved := beforeDisk - afterDisk; saved < int64(len(deadPayload))/4 {
		t.Fatalf("on-disk reclamation too small: saved %d bytes, dead payload %d", saved, len(deadPayload))
	}
}
