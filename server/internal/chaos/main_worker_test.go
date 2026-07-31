package chaos_test

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/clock"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/retention"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

// TestMain intercepts CHAOS_KILL9_WORKER=1 to run the kill -9 worker process
// instead of the test suite. Parent TestChaos10_ProcessKill9 re-execs this binary.
func TestMain(m *testing.M) {
	if os.Getenv("CHAOS_KILL9_WORKER") == "1" {
		os.Exit(runKill9Worker())
	}
	os.Exit(m.Run())
}

func runKill9Worker() int {
	dir := os.Getenv("CHAOS_DIR")
	if dir == "" {
		fmt.Fprintln(os.Stderr, "CHAOS_DIR required")
		return 2
	}
	seed := time.Now().UnixNano()
	if v := os.Getenv("CHAOS_WORKER_SEED"); v != "" {
		fmt.Sscanf(v, "%d", &seed)
	}
	rng := rand.New(rand.NewSource(seed))
	ctx := context.Background()

	db, err := catalog.Open(filepath.Join(dir, "catalog.db"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer db.Close()
	ks, err := keystore.OpenOrCreate(db, filepath.Join(dir, "mk"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	vm := vault.NewManager(filepath.Join(dir, "repos"), dir)
	defer vm.CloseAll(ctx)
	machineID := "machine-kill9"
	pw, err := ks.GetRepoPassword(ctx, machineID)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	v, err := vm.Open(ctx, machineID, pw)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	zero := time.Duration(0)
	locks := scheduler.NewRepoLocks()
	svc := &retention.Service{
		DB: db, Vaults: vm, Keystore: ks, Locks: locks,
		Clock: clock.System(), MinContentAge: &zero,
	}

	// Signal parent we are mid-work.
	_ = os.WriteFile(filepath.Join(dir, "worker-started"), []byte("1"), 0o600)

	// Loop concurrent-ish backup + prune until killed.
	for n := 0; ; n++ {
		payload := fmt.Sprintf("worker-%d-%d", seed, n)
		// Use a testing.T-less put — inline tree write.
		root, err := writeTreeNoT(ctx, v, map[string]string{"w.txt": payload})
		if err == nil {
			rec, err2 := v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
				Kind: vault.KindFileSnapshot, MachineID: machineID,
				Timestamp: time.Now().UTC(), RootObjectID: root, Source: "/w",
			})
			if err2 == nil {
				id := fmt.Sprintf("snap-%d-%d", seed, n)
				_ = db.InsertSnapshot(ctx, catalog.Snapshot{
					ID: id, MachineID: machineID, Kind: "file", Source: "/w",
					ManifestRef: string(rec), RootObjectID: string(root),
					CreatedAt: time.Now().UTC(),
				})
			}
		}
		if n%3 == 0 && n > 0 {
			// Soft-delete older and prune.
			live, _ := db.ListSnapshotsByMachine(ctx, machineID, 100)
			if len(live) > 2 {
				_ = db.SoftDeleteSnapshot(ctx, live[len(live)-1].ID, time.Now().UTC().Add(-10*24*time.Hour))
			}
			_, _ = svc.Prune(ctx, machineID, "worker", "system")
		}
		// Busy enough that kill mid-loop is likely mid-IO.
		if rng.Intn(3) == 0 {
			time.Sleep(time.Millisecond)
		}
	}
}
