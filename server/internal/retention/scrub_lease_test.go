package retention_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/clock"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/retention"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/vault"
	"github.com/oklog/ulid/v2"
)

// TestM5F3_ScrubCompletesWhileSharedBackupHeld is the behavioral guard for
// M5-F2/M5-F3: Scrub must take a *shared* lease so it can run while a backup
// holds shared. If Scrub regresses to Exclusive, Acquire blocks until the
// backup releases — and this test fails with context deadline exceeded.
//
// Self-check (mutation): revert scrub.go to Exclusive → this test FAILS;
// restore Shared → PASS. Empirically confirmed before commit.
func TestM5F3_ScrubCompletesWhileSharedBackupHeld(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := catalog.Open(filepath.Join(dir, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ks, err := keystore.OpenOrCreate(db, filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	vm := vault.NewManager(filepath.Join(dir, "repos"), dir)
	t.Cleanup(func() { _ = vm.CloseAll(ctx) })

	locks := scheduler.NewRepoLocks()
	machineID := ulid.Make().String()
	if err := db.InsertMachine(ctx, catalog.Machine{
		ID: machineID, CertFP: "fp-" + machineID, Hostname: "scrub-lease",
		RepoID: machineID, Status: "enrolled",
	}); err != nil {
		t.Fatal(err)
	}
	pw, err := ks.CreateRepoPassword(ctx, machineID)
	if err != nil {
		t.Fatal(err)
	}
	v, err := vm.Create(ctx, machineID, pw)
	if err != nil {
		t.Fatal(err)
	}
	secret, algo, err := v.HashingKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.SetHashingKey(ctx, machineID, secret, algo); err != nil {
		t.Fatal(err)
	}

	// Small live snapshot so Scrub has real work.
	root := putTree(t, ctx, v, map[string]string{"f.txt": "scrub-lease-payload"})
	ts := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	rec, err := v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind: vault.KindFileSnapshot, MachineID: machineID,
		Timestamp: ts, RootObjectID: root, Source: "/data",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InsertSnapshot(ctx, catalog.Snapshot{
		ID: ulid.Make().String(), MachineID: machineID, Kind: "file", Source: "/data",
		ManifestRef: string(rec), RootObjectID: string(root), CreatedAt: ts,
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate in-flight backup: hold shared on the machine repo.
	backupLease, err := locks.Acquire(ctx, machineID, scheduler.Shared, "backup-in-flight")
	if err != nil {
		t.Fatal(err)
	}
	defer backupLease.Release()

	svc := &retention.Service{
		DB: db, Vaults: vm, Keystore: ks, Locks: locks,
		Clock: clock.NewFake(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)),
	}

	// Short timeout: under Exclusive, Scrub blocks on backup's shared lease and
	// must not complete. Under Shared, Scrub acquires shared and finishes.
	scrubCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res, err := svc.Scrub(scrubCtx, machineID, retention.ScrubFull, 1)
	if err != nil {
		t.Fatalf("Scrub must complete while backup holds shared (lease mode Shared); "+
			"if this times out, Scrub likely regressed to Exclusive: %v", err)
	}
	if res == nil {
		t.Fatal("nil scrub result")
	}
	if res.ManifestsChecked < 1 {
		t.Fatalf("expected at least one manifest checked, got %+v", res)
	}
}

// TestM5F3_PruneBlockedWhileRealScrubRuns: while Scrub holds its shared lease,
// exclusive prune cannot start. Uses a long-lived shared held by Scrub's path
// by starting Scrub after we verify exclusive is free, then holding scrub's
// progress via a concurrent exclusive wait — simpler: hold the same locks table
// and call Scrub which takes shared; TryAcquire exclusive must fail mid-scrub.
//
// We pin exclusive-blocked by holding a second shared that Scrub also needs…
// Actually Scrub releases lease on return. So: run Scrub in a goroutine after
// acquiring a "pause" is hard without hooks.
//
// Instead: after Scrub starts it holds shared briefly. We use the lock table
// that Scrub uses and assert: while we hold shared (as Scrub would), exclusive
// fails — covered by TestRepoLocks_ExclusiveBlockedWhileSharedHeld.
// The real Scrub mode is guarded solely by TestM5F3_ScrubCompletesWhileSharedBackupHeld.
//
// Additional guard: after Scrub completes, exclusive must be free again.
func TestM5F3_AfterScrubExclusiveAvailable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := catalog.Open(filepath.Join(dir, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ks, err := keystore.OpenOrCreate(db, filepath.Join(dir, "mk"))
	if err != nil {
		t.Fatal(err)
	}
	vm := vault.NewManager(filepath.Join(dir, "repos"), dir)
	t.Cleanup(func() { _ = vm.CloseAll(ctx) })
	locks := scheduler.NewRepoLocks()
	machineID := ulid.Make().String()
	if err := db.InsertMachine(ctx, catalog.Machine{
		ID: machineID, CertFP: "fp-" + machineID, Hostname: "h", RepoID: machineID,
	}); err != nil {
		t.Fatal(err)
	}
	pw, err := ks.CreateRepoPassword(ctx, machineID)
	if err != nil {
		t.Fatal(err)
	}
	v, err := vm.Create(ctx, machineID, pw)
	if err != nil {
		t.Fatal(err)
	}
	secret, algo, err := v.HashingKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = ks.SetHashingKey(ctx, machineID, secret, algo)
	root := putTree(t, ctx, v, map[string]string{"a.txt": "x"})
	rec, err := v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind: vault.KindFileSnapshot, MachineID: machineID,
		Timestamp: time.Now().UTC(), RootObjectID: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = db.InsertSnapshot(ctx, catalog.Snapshot{
		ID: ulid.Make().String(), MachineID: machineID, Kind: "file",
		ManifestRef: string(rec), RootObjectID: string(root),
		CreatedAt: time.Now().UTC(),
	})

	svc := &retention.Service{
		DB: db, Vaults: vm, Keystore: ks, Locks: locks,
		Clock: clock.NewFake(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)),
	}
	if _, err := svc.Scrub(ctx, machineID, retention.ScrubFull, 1); err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	// Lease released: exclusive must succeed.
	l, ok := locks.TryAcquire(machineID, scheduler.Exclusive, "post-scrub-prune")
	if !ok {
		t.Fatal("after Scrub returns, exclusive must be available (lease leak?)")
	}
	l.Release()
}

// --- RepoLocks primitive tests (do NOT claim to cover Scrub lease mode) ---

// TestRepoLocks_SharedDoesNotBlockShared documents the lock primitive only.
func TestRepoLocks_SharedDoesNotBlockShared(t *testing.T) {
	locks := scheduler.NewRepoLocks()
	repo := "rl-shared"
	a, err := locks.Acquire(context.Background(), repo, scheduler.Shared, "a")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	b, err := locks.Acquire(ctx, repo, scheduler.Shared, "b")
	if err != nil {
		t.Fatalf("second shared blocked: %v", err)
	}
	b.Release()
}

// TestRepoLocks_ExclusiveBlockedWhileSharedHeld documents the lock primitive only.
func TestRepoLocks_ExclusiveBlockedWhileSharedHeld(t *testing.T) {
	locks := scheduler.NewRepoLocks()
	repo := "rl-ex"
	shared, err := locks.Acquire(context.Background(), repo, scheduler.Shared, "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := locks.TryAcquire(repo, scheduler.Exclusive, "e"); ok {
		t.Fatal("exclusive must fail while shared held")
	}
	shared.Release()
	l, ok := locks.TryAcquire(repo, scheduler.Exclusive, "e2")
	if !ok {
		t.Fatal("exclusive free after shared release")
	}
	l.Release()
}
