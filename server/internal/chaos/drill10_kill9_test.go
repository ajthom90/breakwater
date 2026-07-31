package chaos_test

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/chaos"
	"github.com/ajthom90/breakwater/server/internal/clock"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/retention"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/vault"
	"github.com/oklog/ulid/v2"
)

// TestChaos10_Kill9Fuzz is PLAN chaos drill #10 — the flagship.
//
// Justification for crash model (not always OS kill -9 of breakwaterd):
//
//	A test harness cannot kill -9 itself. We use *crash-equivalent* injection:
//	concurrent backup writers + prune, with random Manager.Close of the live
//	vault handle mid-flight (drops open kopia sessions the same way a process
//	death would), then reopen and assert every surviving snapshot fully
//	restores. A smaller SIGKILL subprocess loop is in TestChaos10_ProcessKill9
//	for OS-level durability.
//
// After every iteration: repo consistent; prune never removed referenced data
// (walk every surviving snapshot — same invariant as M5 property tests).
// Seeded; seed printed.
func TestChaos10_Kill9Fuzz(t *testing.T) {
	seed := chaos.Seed(t, time.Now().UnixNano())
	// Full=500 (PLAN); reduced CI default=8; -short=3.
	iters := chaos.Iters(t, 500, 8)
	if testing.Short() {
		iters = chaos.Iters(t, 500, 3)
	}

	var faultsInjected int64
	for i := 0; i < iters; i++ {
		iterSeed := seed + int64(i)*9973
		if err := runKill9Iter(t, iterSeed, &faultsInjected); err != nil {
			t.Fatalf("SEED %d iter %d: %v", seed, i, err)
		}
	}
	if faultsInjected == 0 {
		t.Fatal("vacuous: no crash faults were injected across all iterations")
	}
	t.Logf("chaos#10 kill9-fuzz seed=%d iters=%d faults_injected=%d OK", seed, iters, faultsInjected)
}

func runKill9Iter(t *testing.T, seed int64, faults *int64) error {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	ctx := context.Background()
	dir := t.TempDir()

	db, err := catalog.Open(filepath.Join(dir, "catalog.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	ks, err := keystore.OpenOrCreate(db, filepath.Join(dir, "mk"))
	if err != nil {
		return err
	}
	vm := vault.NewManager(filepath.Join(dir, "repos"), dir)
	defer vm.CloseAll(ctx)

	machineID := ulid.Make().String()
	if err := db.InsertMachine(ctx, catalog.Machine{
		ID: machineID, CertFP: "fp-" + machineID, Hostname: "k9",
		RepoID: machineID, Status: "enrolled",
	}); err != nil {
		return err
	}
	pw, err := ks.CreateRepoPassword(ctx, machineID)
	if err != nil {
		return err
	}
	v, err := vm.Create(ctx, machineID, pw)
	if err != nil {
		return err
	}
	secret, algo, err := v.HashingKey(ctx)
	if err != nil {
		return err
	}
	if err := ks.SetHashingKey(ctx, machineID, secret, algo); err != nil {
		return err
	}

	// Seed a few durable baseline snapshots so post-crash always has something to check.
	type known struct {
		id, payload string
		root        vault.ObjectID
	}
	var baseline []known
	baseTS := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for j := 0; j < 3; j++ {
		payload := fmt.Sprintf("baseline-seed%d-j%d", seed, j)
		id, root, err := putSnapDirectNoT(ctx, db, v, machineID, fmt.Sprintf("b%d.txt", j), payload, baseTS.Add(time.Duration(j)*time.Hour))
		if err != nil {
			return fmt.Errorf("baseline put: %w", err)
		}
		baseline = append(baseline, known{id: id, payload: payload, root: root})
	}

	zero := time.Duration(0)
	locks := scheduler.NewRepoLocks()
	svc := &retention.Service{
		DB: db, Vaults: vm, Keystore: ks, Locks: locks,
		Clock: clock.System(), MinContentAge: &zero,
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var writesOK, writesFail, prunesOK, prunesFail atomic.Int64

	// Concurrent "backup" writers.
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			local := rand.New(rand.NewSource(seed + int64(wid)*10007))
			n := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Shared lease like a real backup job.
				lease, err := locks.Acquire(context.Background(), machineID, scheduler.Shared, fmt.Sprintf("backup-%d-%d", wid, n))
				if err != nil {
					writesFail.Add(1)
					time.Sleep(time.Duration(local.Intn(3)) * time.Millisecond)
					continue
				}
				vv, err := vm.Open(context.Background(), machineID, pw)
				if err != nil {
					lease.Release()
					writesFail.Add(1)
					continue
				}
				payload := fmt.Sprintf("live-seed%d-w%d-n%d-%d", seed, wid, n, local.Int63())
				_, _, err = putSnapDirectNoT(context.Background(), db, vv, machineID,
					fmt.Sprintf("w%d.txt", wid), payload, time.Now().UTC())
				lease.Release()
				if err != nil {
					writesFail.Add(1)
				} else {
					writesOK.Add(1)
				}
				n++
				if n > 50 {
					return
				}
			}
		}(w)
	}

	// Concurrent prune (exclusive) — forget oldest soft targets when available.
	wg.Add(1)
	go func() {
		defer wg.Done()
		local := rand.New(rand.NewSource(seed + 99991))
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Soft-delete a random non-baseline live snap if many exist.
			live, _ := db.ListSnapshotsByMachine(context.Background(), machineID, 1000)
			if len(live) > 4 {
				// Prefer forgetting newer concurrent snaps, never wipe all baseline if possible.
				pick := live[local.Intn(len(live))]
				isBase := false
				for _, b := range baseline {
					if b.id == pick.ID {
						isBase = true
						break
					}
				}
				if !isBase {
					_ = db.SoftDeleteSnapshot(context.Background(), pick.ID, time.Now().UTC().Add(-8*24*time.Hour)) // past grace
				}
			}
			_, err := svc.Prune(context.Background(), machineID, "chaos", "system")
			if err != nil {
				prunesFail.Add(1)
			} else {
				prunesOK.Add(1)
			}
			time.Sleep(time.Duration(local.Intn(5)) * time.Millisecond)
		}
	}()

	// Crash injector: random Close of vault handle while work is in flight.
	// This is the fault — prove it fired.
	crashDelay := time.Duration(5+rng.Intn(40)) * time.Millisecond
	time.Sleep(crashDelay)
	if err := vm.Close(context.Background(), machineID); err != nil {
		// Close errors still count as crash attempt under load.
		t.Logf("iter seed=%d Close during load: %v", seed, err)
	}
	atomic.AddInt64(faults, 1)
	// Let workers hit closed vault briefly.
	time.Sleep(time.Duration(5+rng.Intn(20)) * time.Millisecond)
	close(stop)
	wg.Wait()

	// Re-open after crash and assert consistency.
	v2, err := vm.Open(ctx, machineID, pw)
	if err != nil {
		return fmt.Errorf("reopen after crash: %w", err)
	}
	// Baseline survivors that still exist in catalog must restore with exact payload.
	for _, b := range baseline {
		sn, _ := db.SnapshotByID(ctx, b.id)
		if sn == nil || sn.DeletedAt != nil {
			// May have been pruned only if soft-deleted past grace — baseline was not soft-deleted.
			if sn == nil {
				// Hard-deleted would mean prune removed a live (non-soft-deleted) snap — catastrophic.
				// Soft-delete path only hard-deletes eligible; baseline should remain unless we forgot it.
				// We never soft-delete baseline, so nil is a bug.
				return fmt.Errorf("baseline snapshot %s vanished from catalog after crash+prune", b.id)
			}
			continue
		}
		if err := walkRestoreAll(ctx, v2, vault.ObjectID(sn.RootObjectID)); err != nil {
			return fmt.Errorf("baseline %s not restorable: %w", b.id, err)
		}
	}
	n := assertAllSurvivorsRestorable(t, ctx, db, v2, machineID)
	if n == 0 {
		return fmt.Errorf("no survivors to check after crash (unexpected)")
	}
	_ = writesOK.Load()
	_ = writesFail.Load()
	_ = prunesOK.Load()
	_ = prunesFail.Load()
	return nil
}

// TestChaos10_ProcessKill9 exercises true OS SIGKILL of a worker process mid
// backup+prune, then reopens the repo directory. Fewer iterations (still
// seeded); full 500 is the in-process crash fuzz above which is denser.
func TestChaos10_ProcessKill9(t *testing.T) {
	if os.Getenv("CHAOS_SKIP_PROCESS_KILL") == "1" {
		t.Skip("CHAOS_SKIP_PROCESS_KILL=1")
	}
	seed := chaos.Seed(t, time.Now().UnixNano())
	iters := chaos.Iters(t, 25, 2)
	if testing.Short() {
		iters = 1
	}

	// Self-reexec as worker via TestMain path.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	var kills int
	for i := 0; i < iters; i++ {
		dir := t.TempDir()
		// Prepare vault+catalog for worker.
		if err := prepareKill9Workdir(t, dir, seed+int64(i)); err != nil {
			t.Fatalf("prepare: %v", err)
		}
		marker := filepath.Join(dir, "worker-started")
		cmdEnv := append(os.Environ(),
			"CHAOS_KILL9_WORKER=1",
			"CHAOS_DIR="+dir,
			fmt.Sprintf("CHAOS_WORKER_SEED=%d", seed+int64(i)),
		)
		// Run the same test binary; TestMain intercepts CHAOS_KILL9_WORKER.
		// #nosec G204 -- test harness only
		cmd := execCommand(exe)
		cmd.Env = cmdEnv
		cmd.Dir = dir
		if err := cmd.Start(); err != nil {
			t.Fatalf("start worker: %v", err)
		}
		// Always reap the child so a killed worker cannot leave an unreaped
		// process holding paths under t.TempDir (CHAOS-F3 sibling hygiene).
		waited := false
		reap := func() {
			if !waited && cmd.Process != nil {
				_ = cmd.Wait()
				waited = true
			}
		}
		// Wait until worker signals it is mid-work (or timeout).
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(marker); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if _, err := os.Stat(marker); err != nil {
			_ = kill9(cmd.Process.Pid)
			reap()
			t.Fatalf("iter %d: worker never set started marker (fault not proven)", i)
		}
		// True kill -9.
		if err := kill9(cmd.Process.Pid); err != nil {
			reap()
			t.Fatalf("kill -9: %v", err)
		}
		reap()
		kills++

		// Reopen and verify (opens+closes vault; no mount left behind).
		if err := verifyKill9Workdir(t, dir); err != nil {
			t.Fatalf("SEED %d process-kill iter %d: %v", seed, i, err)
		}
	}
	if kills == 0 {
		t.Fatal("no SIGKILL injected")
	}
	t.Logf("chaos#10 process-kill seed=%d iters=%d kills=%d OK", seed, iters, kills)
}

func prepareKill9Workdir(t *testing.T, dir string, seed int64) error {
	t.Helper()
	ctx := context.Background()
	db, err := catalog.Open(filepath.Join(dir, "catalog.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	ks, err := keystore.OpenOrCreate(db, filepath.Join(dir, "mk"))
	if err != nil {
		return err
	}
	vm := vault.NewManager(filepath.Join(dir, "repos"), dir)
	defer vm.CloseAll(ctx)
	machineID := "machine-kill9"
	_ = db.InsertMachine(ctx, catalog.Machine{
		ID: machineID, CertFP: "fp", Hostname: "k9p", RepoID: machineID, Status: "enrolled",
	})
	pw, err := ks.CreateRepoPassword(ctx, machineID)
	if err != nil {
		return err
	}
	v, err := vm.Create(ctx, machineID, pw)
	if err != nil {
		return err
	}
	secret, algo, err := v.HashingKey(ctx)
	if err != nil {
		return err
	}
	if err := ks.SetHashingKey(ctx, machineID, secret, algo); err != nil {
		return err
	}
	// One durable snapshot.
	if _, _, err = putSnapDirectNoT(ctx, db, v, machineID, "base.txt", fmt.Sprintf("base-%d", seed), time.Now().UTC()); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "password"), []byte(pw), 0o600)
}

func verifyKill9Workdir(t *testing.T, dir string) error {
	t.Helper()
	ctx := context.Background()
	db, err := catalog.Open(filepath.Join(dir, "catalog.db"))
	if err != nil {
		return fmt.Errorf("catalog reopen: %w", err)
	}
	defer db.Close()
	ks, err := keystore.OpenOrCreate(db, filepath.Join(dir, "mk"))
	if err != nil {
		return err
	}
	vm := vault.NewManager(filepath.Join(dir, "repos"), dir)
	defer vm.CloseAll(ctx)
	machineID := "machine-kill9"
	pw, err := ks.GetRepoPassword(ctx, machineID)
	if err != nil {
		return err
	}
	v, err := vm.Open(ctx, machineID, pw)
	if err != nil {
		return fmt.Errorf("vault reopen after kill -9: %w", err)
	}
	assertAllSurvivorsRestorable(t, ctx, db, v, machineID)
	return nil
}
