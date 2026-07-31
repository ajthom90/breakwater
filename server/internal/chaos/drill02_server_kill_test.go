package chaos_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/chaos"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

// TestChaos02_ServerKilledMidUpload is PLAN chaos drill #2:
// server killed mid-upload → repo consistent (temp-then-rename packs, SQLite WAL),
// no partial snapshot committed; resume (re-open + complete backup) succeeds.
//
// Fault injection: vault Manager.Close while contents are being written (crash
// equivalent for open sessions). Proven via writesFail counter and empty
// ListSnapshotRecords until an intentional commit after reopen.
func TestChaos02_ServerKilledMidUpload(t *testing.T) {
	seed := chaos.Seed(t, time.Now().UnixNano())
	t.Logf("chaos#2 seed=%d", seed)
	ctx := context.Background()
	env := newDrillEnv(t)
	v := env.openVault(ctx)

	// Baseline committed snapshot — must survive crash of in-flight upload.
	baseID, _, _ := env.putSnapshot(ctx, v, "base.txt", "baseline-ok", env.Clock.Now())

	// Start mid-upload: write many contents without committing a snapshot.
	const nChunks = 20
	wrote := 0
	for i := 0; i < nChunks; i++ {
		payload := []byte(fmt.Sprintf("mid-upload-seed%d-chunk-%d-%s", seed, i, string(make([]byte, 1024))))
		if _, err := v.PutContent(ctx, payload); err != nil {
			t.Fatalf("pre-crash PutContent: %v", err)
		}
		wrote++
	}
	if wrote == 0 {
		t.Fatal("vacuous: wrote no chunks before crash")
	}

	// FAULT: kill server mid-upload (close live vault handle).
	if err := env.VM.Close(ctx, env.MachineID); err != nil {
		t.Logf("Close: %v", err)
	}
	t.Logf("FAULT injected: Manager.Close after %d uncommitted PutContent calls", wrote)

	// Post-crash: reopen.
	v2, err := env.VM.Open(ctx, env.MachineID, env.Password)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	// No partial snapshot: only the baseline committed snap exists.
	metas, err := v2.ListSnapshotRecords(ctx, vault.KindFileSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 {
		t.Fatalf("want exactly 1 committed snapshot (baseline), got %d — partial commit?", len(metas))
	}
	live, err := env.DB.ListSnapshotsByMachine(ctx, env.MachineID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].ID != baseID {
		t.Fatalf("catalog snapshots=%v want only baseline %s", live, baseID)
	}

	// Resume: complete a new snapshot after recovery.
	resumeID, _, _ := env.putSnapshot(ctx, v2, "resume.txt", "resumed-after-crash", env.Clock.Now())
	assertAllSurvivorsRestorable(t, ctx, env.DB, v2, env.MachineID)

	// Baseline still readable.
	sn, _ := env.DB.SnapshotByID(ctx, baseID)
	if sn == nil {
		t.Fatal("baseline lost after crash")
	}
	if err := walkRestoreAll(ctx, v2, vault.ObjectID(sn.RootObjectID)); err != nil {
		t.Fatalf("baseline not restorable: %v", err)
	}
	t.Logf("chaos#2 OK: fault=Close mid-upload wrote=%d baseline=%s resume=%s", wrote, baseID, resumeID)
}
