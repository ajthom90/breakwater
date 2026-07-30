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
// Criterion: write chunked data → restore → verify → retention + GC with real
// reclamation (forgotten contents gone, stats shrink, live object intact).
// Default size is 10 GiB. Use BW_GATE_BYTES to override. Skip with -short.
func TestEngineGate_Kopia(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping engine gate in -short mode")
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

	t.Logf("engine gate: writing %d bytes (%.2f GiB) into kopia vault", totalBytes, float64(totalBytes)/(1<<30))

	mgr := vault.NewManager(reposDir)
	defer mgr.CloseAll(ctx)

	v, err := mgr.Create(ctx, repoID, password)
	if err != nil {
		t.Fatalf("Create vault: %v", err)
	}

	// Hashing key must come from repo format (not random).
	hk, algo, err := v.HashingKey(ctx)
	if err != nil || len(hk) == 0 || algo == "" {
		t.Fatalf("HashingKey: keyLen=%d algo=%q err=%v", len(hk), algo, err)
	}
	t.Logf("hashing key: algo=%s secretLen=%d", algo, len(hk))

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
	// Unique payload so content is not shared with the live object.
	small := bytes.Repeat([]byte("orphan-payload-for-gc-UNIQUE-"), 64<<10)
	smallOID, err := v.WriteObject(ctx, vault.SplitterFixed4M, bytes.NewReader(small))
	if err != nil {
		t.Fatalf("WriteObject small: %v", err)
	}
	forgetContentIDs, err := v.VerifyObject(ctx, smallOID)
	if err != nil || len(forgetContentIDs) == 0 {
		t.Fatalf("VerifyObject small: ids=%v err=%v", forgetContentIDs, err)
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
	// M8: Timestamp populated
	for _, m := range listed {
		if m.Timestamp.IsZero() {
			t.Fatalf("ListSnapshotRecords Timestamp empty for %s", m.ID)
		}
	}

	// --- Retention (forget) + GC with reclamation assertions ---
	if err := v.DeleteSnapshotRecord(ctx, forgetID); err != nil {
		t.Fatalf("DeleteSnapshotRecord: %v", err)
	}
	if _, err := v.GetSnapshotRecord(ctx, recID); err != nil {
		t.Fatalf("live snapshot missing after forget: %v", err)
	}

	before, err := v.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats before prune: %v", err)
	}
	t.Logf("stats before prune: user_contents=%d user_size=%d all=%d",
		before.UserContentCount, before.UserSizeBytes, before.ContentCount)

	if err := v.Prune(ctx); err != nil {
		t.Fatalf("Prune/GC: %v", err)
	}
	t.Logf("Prune completed")

	// Forgotten content IDs must be absent from have/want.
	hasForget, err := v.HasContents(ctx, forgetContentIDs)
	if err != nil {
		t.Fatalf("HasContents forgotten: %v", err)
	}
	for i, present := range hasForget {
		if present {
			t.Fatalf("forgotten content %s still present after prune", forgetContentIDs[i])
		}
	}
	// Forgotten object must be unreadable (contents dropped from index).
	if rBad, err := v.OpenObject(ctx, smallOID); err == nil {
		_, _ = io.Copy(io.Discard, rBad)
		_ = rBad.Close()
		t.Fatalf("forgotten object %s still readable after prune", smallOID)
	} else {
		t.Logf("forgotten object unreadable as expected: %v", err)
	}

	// Live object still readable with correct size + checksum after GC
	r2, err := v.OpenObject(ctx, objID)
	if err != nil {
		t.Fatalf("OpenObject after prune: %v", err)
	}
	h2 := sha256.New()
	n2, err := io.Copy(h2, r2)
	_ = r2.Close()
	if err != nil {
		t.Fatalf("read after prune: %v", err)
	}
	if n2 != totalBytes {
		t.Fatalf("size after prune %d, want %d", n2, totalBytes)
	}
	if !bytes.Equal(h2.Sum(nil), wantSum) {
		t.Fatalf("checksum mismatch after prune")
	}

	after, err := v.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats after prune: %v", err)
	}
	t.Logf("stats after prune: user_contents=%d user_size=%d all=%d",
		after.UserContentCount, after.UserSizeBytes, after.ContentCount)
	if after.UserContentCount >= before.UserContentCount {
		t.Fatalf("reclamation failed: user content count did not shrink (%d → %d)",
			before.UserContentCount, after.UserContentCount)
	}
	if after.UserSizeBytes >= before.UserSizeBytes {
		t.Fatalf("reclamation failed: user size did not shrink (%d → %d)",
			before.UserSizeBytes, after.UserSizeBytes)
	}
	t.Logf("reclamation OK: user contents %d→%d user size %d→%d",
		before.UserContentCount, after.UserContentCount, before.UserSizeBytes, after.UserSizeBytes)

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

	// M3: methods after Close return error, not panic
	if err := v2.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := v2.Stats(ctx); err == nil {
		t.Fatal("expected error on Stats after Close")
	}

	t.Log("ENGINE GATE PASSED: write/restore/verify + mark-sweep reclamation + re-open")
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

// TestPutContent_RejectsOversize is H2: payloads >4MiB must error.
func TestPutContent_RejectsOversize(t *testing.T) {
	ctx := context.Background()
	mgr := vault.NewManager(t.TempDir())
	defer mgr.CloseAll(ctx)

	v, err := mgr.Create(ctx, "oversize", "pw")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	big := make([]byte, vault.MaxPutContentBytes+1)
	_, err = v.PutContent(ctx, big)
	if err == nil {
		t.Fatal("expected PutContent to reject >4MiB payload")
	}
	t.Logf("oversize rejected: %v", err)

	// Exactly 4MiB is allowed.
	ok := make([]byte, vault.MaxPutContentBytes)
	for i := range ok {
		ok[i] = byte(i)
	}
	cid, err := v.PutContent(ctx, ok)
	if err != nil {
		t.Fatalf("PutContent 4MiB: %v", err)
	}
	if cid == "" {
		t.Fatal("empty content id")
	}
}

func fillDeterministic(buf []byte, seq uint64) {
	var state uint64 = 0x9e3779b97f4a7c15 ^ seq
	for i := 0; i < len(buf); {
		state = state*6364136223846793005 + 1
		if i+8 <= len(buf) {
			binary.LittleEndian.PutUint64(buf[i:], state^seq)
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
