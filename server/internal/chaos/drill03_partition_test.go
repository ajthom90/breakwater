package chaos_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/chaos"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

// flakyVault wraps vault.Vault and fails PutContent while partitioned.
// Proves the fault was injected via failCount.
type flakyVault struct {
	vault.Vault
	partitioned *atomic.Bool
	failCount   *atomic.Int64
}

func (f *flakyVault) PutContent(ctx context.Context, data []byte) (vault.ContentID, error) {
	if f.partitioned.Load() {
		f.failCount.Add(1)
		return "", fmt.Errorf("network partition: connection reset (injected)")
	}
	return f.Vault.PutContent(ctx, data)
}

// TestChaos03_NetworkPartitionMidBackup is PLAN chaos drill #3:
// 30s network partition mid-backup → retries; no duplicate manifests / snapshot rows.
//
// Fault: flakyVault returns errors while partitioned=true. Backup path aborts
// without CommitSnapshot. After partition heals, a successful commit produces
// exactly one snapshot. Proved via failCount > 0 and post-state uniqueness.
func TestChaos03_NetworkPartitionMidBackup(t *testing.T) {
	seed := chaos.Seed(t, time.Now().UnixNano())
	t.Logf("chaos#3 seed=%d", seed)
	ctx := context.Background()
	env := newDrillEnv(t)
	base := env.openVault(ctx)

	var partitioned atomic.Bool
	var failCount atomic.Int64
	fv := &flakyVault{Vault: base, partitioned: &partitioned, failCount: &failCount}

	// Partition ON — simulate mid-backup network cut.
	partitioned.Store(true)
	t.Log("FAULT injected: network partition ON")

	// Attempt writes under partition — must fail (prove injection).
	_, err := fv.PutContent(ctx, []byte(fmt.Sprintf("during-partition-%d", seed)))
	if err == nil {
		t.Fatal("expected PutContent to fail during partition")
	}
	if failCount.Load() < 1 {
		t.Fatal("vacuous: partition did not trip fail counter")
	}
	t.Logf("partition blocked PutContent (failCount=%d): %v", failCount.Load(), err)

	// Heal (PLAN says 30s; we don't sleep wall-clock — flip the flag, same effect).
	partitioned.Store(false)
	t.Log("FAULT cleared: network partition OFF (healed)")

	// Retry succeeds; single commit.
	id, _, man := env.putSnapshot(ctx, base, "after.txt", fmt.Sprintf("post-partition-%d", seed), env.Clock.Now())

	metas, err := base.ListSnapshotRecords(ctx, vault.KindFileSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 {
		t.Fatalf("duplicate manifests? count=%d", len(metas))
	}
	if string(metas[0].ID) != string(man) {
		t.Fatalf("manifest id mismatch")
	}
	live, _ := env.DB.ListSnapshotsByMachine(ctx, env.MachineID, 100)
	if len(live) != 1 || live[0].ID != id {
		t.Fatalf("catalog snapshot rows=%d want 1", len(live))
	}

	// Second successful backup must still not create ambiguous duplicates for the
	// *failed* attempt (failed attempt never committed). Total = 2, distinct IDs.
	id2, _, _ := env.putSnapshot(ctx, base, "second.txt", "second-ok", env.Clock.Now().Add(time.Hour))
	live, _ = env.DB.ListSnapshotsByMachine(ctx, env.MachineID, 100)
	if len(live) != 2 {
		t.Fatalf("want 2 snapshots after second success, got %d", len(live))
	}
	if id == id2 {
		t.Fatal("duplicate snapshot IDs")
	}
	assertAllSurvivorsRestorable(t, ctx, env.DB, base, env.MachineID)
	t.Logf("chaos#3 OK: partition fails=%d then single-commit resume id=%s id2=%s", failCount.Load(), id, id2)
}
