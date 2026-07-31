package chaos_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/chaos"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

// TestChaos01_AgentKilledMidUpload is PLAN chaos drill #1 (Linux half):
// kill agent mid-upload → resume works, no duplicate/partial data.
//
// Windows half (no leaked VSS snapshot across kill) is M3-gated — see matrix.
//
// Fault: abandon an in-progress backup after writing contents but before
// CommitSnapshot (simulates agent process death). Then complete a fresh backup
// and assert no partial catalog/vault snapshot and no duplicates.
func TestChaos01_AgentKilledMidUpload(t *testing.T) {
	seed := chaos.Seed(t, time.Now().UnixNano())
	t.Logf("chaos#1 seed=%d", seed)
	ctx := context.Background()
	env := newDrillEnv(t)
	v := env.openVault(ctx)

	// Simulate agent mid-upload: PutContent + tree materialization, NO commit.
	root, err := writeTreeNoT(ctx, v, map[string]string{
		"partial.txt": fmt.Sprintf("abandoned-%d", seed),
	})
	if err != nil {
		t.Fatal(err)
	}
	// FAULT PROOF: no snapshot record yet; root exists as orphan content.
	metas, err := v.ListSnapshotRecords(ctx, vault.KindFileSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 0 {
		t.Fatalf("pre-commit should have 0 manifests, got %d", len(metas))
	}
	t.Logf("FAULT injected: agent abandoned mid-upload root=%s (no CommitSnapshot)", root)

	// Resume: full backup from "agent restart".
	id, _, man := env.putSnapshot(ctx, v, "full.txt", fmt.Sprintf("complete-%d", seed), env.Clock.Now())

	metas, err = v.ListSnapshotRecords(ctx, vault.KindFileSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 {
		t.Fatalf("after resume want 1 snapshot, got %d (duplicates?)", len(metas))
	}
	if string(metas[0].ID) != string(man) {
		t.Fatalf("manifest mismatch")
	}
	live, _ := env.DB.ListSnapshotsByMachine(ctx, env.MachineID, 100)
	if len(live) != 1 || live[0].ID != id {
		t.Fatalf("catalog rows=%d want 1 id=%s", len(live), id)
	}
	assertAllSurvivorsRestorable(t, ctx, env.DB, v, env.MachineID)
	t.Logf("chaos#1 OK (Linux half): abandoned mid-upload, resume single snapshot %s", id)
	t.Log("WINDOWS GATED: no-leaked-VSS-snapshot half requires M3 Windows VM")
}
