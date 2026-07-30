package vault_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/vault"
)

// TestEngineGate_Kopia is the M1 storage-engine decision gate (PLAN.md).
//
// Criterion: write chunked data → restore → verify → retention + GC.
// Default size is 10 GiB on Linux (full gate). Use BW_GATE_BYTES to override
// (e.g. smaller for quick local iteration). Skip with -short.
func TestEngineGate_Kopia(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 10GB engine gate in -short mode")
	}

	ctx := context.Background()
	reposDir := t.TempDir()
	password := "breakwater-engine-gate-test-password"
	repoID := "gate-machine-001"

	totalBytes := int64(10 << 30) // 10 GiB
	if s := os.Getenv("BW_GATE_BYTES"); s != "" {
		var n int64
		if _, err := fmt.Sscan(s, &n); err == nil && n > 0 {
			totalBytes = n
		}
	}
	// On non-CI local runs without override, still honor full size if disk allows;
	// shrink only when explicitly requested via BW_GATE_BYTES.

	t.Logf("engine gate: writing %d bytes (%.2f GiB) into kopia vault", totalBytes, float64(totalBytes)/(1<<30))

	mgr := vault.NewManager(reposDir)
	defer mgr.CloseAll(ctx)

	v, err := mgr.Create(ctx, repoID, password)
	if err != nil {
		t.Fatalf("Create vault: %v", err)
	}

	// --- Write chunked data (CDC) ---
	const chunk = 4 << 20 // 4 MiB generation blocks
	h := sha256.New()
	pr, pw := io.Pipe()
	writeErr := make(chan error, 1)
	go func() {
		defer pw.Close()
		var written int64
		buf := make([]byte, chunk)
		seq := uint64(0)
		for written < totalBytes {
			n := chunk
			if remain := totalBytes - written; remain < int64(n) {
				n = int(remain)
			}
			// Deterministic pseudo-random-looking data (compressible + unique).
			fillDeterministic(buf[:n], seq)
			seq++
			if _, err := pw.Write(buf[:n]); err != nil {
				writeErr <- err
				return
			}
			h.Write(buf[:n])
			written += int64(n)
		}
		writeErr <- nil
	}()

	startWrite := time.Now()
	objID, err := v.WriteObject(ctx, vault.SplitterDynamic, pr)
	if err != nil {
		t.Fatalf("WriteObject: %v", err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("writer: %v", err)
	}
	wantSum := h.Sum(nil)
	t.Logf("WriteObject done in %s, object=%s", time.Since(startWrite), objID)

	// Snapshot record (manifest)
	recID, err := v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind:         vault.KindFileSnapshot,
		MachineID:    repoID,
		Timestamp:    time.Now().UTC(),
		RootObjectID: objID,
		Source:       "engine-gate",
		JobID:        "job-gate-1",
	})
	if err != nil {
		t.Fatalf("PutSnapshotRecord: %v", err)
	}
	t.Logf("snapshot record: %s", recID)

	// Second smaller snapshot that we will forget (retention exercise).
	small := bytes.Repeat([]byte("orphan-payload-for-gc-"), 64<<10)
	smallOID, err := v.WriteObject(ctx, vault.SplitterFixed4M, bytes.NewReader(small))
	if err != nil {
		t.Fatalf("WriteObject small: %v", err)
	}
	forgetID, err := v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind:         vault.KindFileSnapshot,
		MachineID:    repoID,
		Timestamp:    time.Now().UTC().Add(-time.Hour),
		RootObjectID: smallOID,
		Source:       "engine-gate-forget",
	})
	if err != nil {
		t.Fatalf("PutSnapshotRecord forget: %v", err)
	}

	// PutContent + HasContents smoke
	cid, err := v.PutContent(ctx, []byte("have-want-probe"))
	if err != nil {
		t.Fatalf("PutContent: %v", err)
	}
	has, err := v.HasContents(ctx, []vault.ContentID{cid, vault.ContentID("deadbeefdeadbeefdeadbeefdeadbeef")})
	if err != nil {
		t.Fatalf("HasContents: %v", err)
	}
	if !has[0] {
		t.Fatalf("expected content %s present", cid)
	}
	if has[1] {
		t.Fatalf("expected missing content to be false")
	}
	gotProbe, err := v.GetContent(ctx, cid)
	if err != nil {
		t.Fatalf("GetContent: %v", err)
	}
	if string(gotProbe) != "have-want-probe" {
		t.Fatalf("GetContent mismatch: %q", gotProbe)
	}

	// --- Restore + verify ---
	startRead := time.Now()
	r, err := v.OpenObject(ctx, objID)
	if err != nil {
		t.Fatalf("OpenObject: %v", err)
	}
	rh := sha256.New()
	n, err := io.Copy(rh, r)
	_ = r.Close()
	if err != nil {
		t.Fatalf("restore read: %v", err)
	}
	if n != totalBytes {
		t.Fatalf("restored size %d, want %d", n, totalBytes)
	}
	if !bytes.Equal(rh.Sum(nil), wantSum) {
		t.Fatalf("checksum mismatch after restore")
	}
	t.Logf("restore verified in %s", time.Since(startRead))

	cids, err := v.VerifyObject(ctx, objID)
	if err != nil {
		t.Fatalf("VerifyObject: %v", err)
	}
	if len(cids) == 0 {
		t.Fatalf("VerifyObject returned no content IDs")
	}
	t.Logf("VerifyObject: %d content pieces", len(cids))

	// Load snapshot record
	gotRec, err := v.GetSnapshotRecord(ctx, recID)
	if err != nil {
		t.Fatalf("GetSnapshotRecord: %v", err)
	}
	if gotRec.RootObjectID != objID || gotRec.MachineID != repoID {
		t.Fatalf("snapshot record mismatch: %+v", gotRec)
	}

	listed, err := v.ListSnapshotRecords(ctx, vault.KindFileSnapshot)
	if err != nil {
		t.Fatalf("ListSnapshotRecords: %v", err)
	}
	if len(listed) < 2 {
		t.Fatalf("expected ≥2 snapshots, got %d", len(listed))
	}

	// --- Retention (forget) + GC ---
	if err := v.DeleteSnapshotRecord(ctx, forgetID); err != nil {
		t.Fatalf("DeleteSnapshotRecord: %v", err)
	}
	// Live snapshot must remain
	if _, err := v.GetSnapshotRecord(ctx, recID); err != nil {
		t.Fatalf("live snapshot missing after forget: %v", err)
	}

	before, err := v.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats before prune: %v", err)
	}
	t.Logf("stats before prune: contents=%d size=%d", before.ContentCount, before.TotalSizeBytes)

	if err := v.Prune(ctx); err != nil {
		t.Fatalf("Prune/GC: %v", err)
	}
	t.Logf("Prune completed")

	// Primary object still readable after GC
	r2, err := v.OpenObject(ctx, objID)
	if err != nil {
		t.Fatalf("OpenObject after prune: %v", err)
	}
	n2, err := io.Copy(io.Discard, r2)
	_ = r2.Close()
	if err != nil {
		t.Fatalf("read after prune: %v", err)
	}
	if n2 != totalBytes {
		t.Fatalf("size after prune %d, want %d", n2, totalBytes)
	}

	after, err := v.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats after prune: %v", err)
	}
	t.Logf("stats after prune: contents=%d size=%d", after.ContentCount, after.TotalSizeBytes)

	// Re-open from disk (crash recovery path)
	if err := mgr.CloseAll(ctx); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	v2, err := mgr.Open(ctx, repoID, password)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	r3, err := v2.OpenObject(ctx, objID)
	if err != nil {
		t.Fatalf("OpenObject after re-open: %v", err)
	}
	h3 := sha256.New()
	if _, err := io.Copy(h3, r3); err != nil {
		t.Fatalf("read after re-open: %v", err)
	}
	_ = r3.Close()
	if !bytes.Equal(h3.Sum(nil), wantSum) {
		t.Fatalf("checksum mismatch after re-open")
	}

	// Config file should exist under repo
	cfg := filepath.Join(reposDir, repoID, "breakwater.config")
	if _, err := os.Stat(cfg); err != nil {
		t.Fatalf("missing config %s: %v", cfg, err)
	}

	t.Log("ENGINE GATE PASSED: kopia repo/content/object/manifest/maintenance implement vault interface")
}

// TestVault_SmallRoundTrip is a fast unit test always run in CI.
func TestVault_SmallRoundTrip(t *testing.T) {
	ctx := context.Background()
	mgr := vault.NewManager(t.TempDir())
	defer mgr.CloseAll(ctx)

	v, err := mgr.Create(ctx, "m1", "pw-test-small")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	payload := bytes.Repeat([]byte("breakwater-m1-"), 1000)
	oid, err := v.WriteObject(ctx, vault.SplitterDynamic, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("WriteObject: %v", err)
	}
	r, err := v.OpenObject(ctx, oid)
	if err != nil {
		t.Fatalf("OpenObject: %v", err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch")
	}

	sid, err := v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind:         vault.KindFileSnapshot,
		MachineID:    "m1",
		RootObjectID: oid,
		Source:       "/data",
	})
	if err != nil {
		t.Fatalf("PutSnapshotRecord: %v", err)
	}
	if err := v.DeleteSnapshotRecord(ctx, sid); err != nil {
		t.Fatalf("DeleteSnapshotRecord: %v", err)
	}
	if err := v.Prune(ctx); err != nil {
		t.Fatalf("Prune: %v", err)
	}
}

func fillDeterministic(buf []byte, seq uint64) {
	// Mix seq into a simple LCG-ish stream so blocks differ and compress modestly.
	var state uint64 = 0x9e3779b97f4a7c15 ^ seq
	for i := 0; i < len(buf); {
		state = state*6364136223846793005 + 1
		if i+8 <= len(buf) {
			binary.LittleEndian.PutUint64(buf[i:], state^seq)
			// Sprinkle zeros for zstd friendliness every 64 bytes.
			if i%64 == 0 {
				binary.LittleEndian.PutUint64(buf[i:], 0)
			}
			i += 8
			continue
		}
		buf[i] = byte(state)
		i++
	}
}
