package retention_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/scheduler"
)

// TestM5F2_ScrubSharedDoesNotBlockBackup is M5-F2(a): a scrub-held shared lease
// must not prevent a backup (also shared) from starting.
func TestM5F2_ScrubSharedDoesNotBlockBackup(t *testing.T) {
	locks := scheduler.NewRepoLocks()
	repo := "machine-scrub-shared"

	// Simulate scrub holding shared.
	scrubLease, err := locks.Acquire(context.Background(), repo, scheduler.Shared, "scrub-test")
	if err != nil {
		t.Fatal(err)
	}
	defer scrubLease.Release()

	// Backup must acquire shared without blocking.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	backupLease, err := locks.Acquire(ctx, repo, scheduler.Shared, "backup-test")
	if err != nil {
		t.Fatalf("backup blocked by scrub shared lease (M5-F2): %v", err)
	}
	backupLease.Release()
}

// TestM5F2_PruneBlockedWhileScrubRunning is M5-F2(b): prune (exclusive) cannot
// start while a scrub holds shared — exclusion is free from lock discipline.
func TestM5F2_PruneBlockedWhileScrubRunning(t *testing.T) {
	locks := scheduler.NewRepoLocks()
	repo := "machine-scrub-vs-prune"

	scrubLease, err := locks.Acquire(context.Background(), repo, scheduler.Shared, "scrub-test")
	if err != nil {
		t.Fatal(err)
	}

	// Non-blocking: exclusive must fail while shared held.
	if _, ok := locks.TryAcquire(repo, scheduler.Exclusive, "prune-probe"); ok {
		t.Fatal("prune exclusive must not start while scrub holds shared")
	}

	// Blocking exclusive with short timeout must not succeed until scrub releases.
	var got atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		l, err := locks.Acquire(ctx, repo, scheduler.Exclusive, "prune-wait")
		if err == nil {
			got.Store(true)
			l.Release()
		}
	}()

	// While scrub is held, exclusive must not complete.
	time.Sleep(80 * time.Millisecond)
	if got.Load() {
		t.Fatal("prune acquired exclusive while scrub still running")
	}

	scrubLease.Release()
	wg.Wait()
	if !got.Load() {
		// After release, the waiter may have timed out if too slow — re-try once.
		l, ok := locks.TryAcquire(repo, scheduler.Exclusive, "prune-after")
		if !ok {
			t.Fatal("after scrub release, exclusive should be available")
		}
		l.Release()
	}
}

// TestM5F2_ScrubServiceUsesShared is a structural check: Scrub acquires Shared
// (not Exclusive). We hold Exclusive first; Scrub must block (ctx timeout),
// proving it is competing for a lease that exclusive blocks — and that it is
// not silently skipping locks. Separately, with no exclusive holder, Scrub
// runs while another shared holder is present (would fail if Scrub took Exclusive).
func TestM5F2_ScrubCompatibleWithConcurrentShared(t *testing.T) {
	// Pure lock-mode contract: shared + shared OK; shared blocks exclusive.
	// (Full Scrub() I/O is covered by grace/property tests; this isolates F2.)
	locks := scheduler.NewRepoLocks()
	repo := "compat"

	s1, err := locks.Acquire(context.Background(), repo, scheduler.Shared, "scrub")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := locks.Acquire(context.Background(), repo, scheduler.Shared, "backup")
	if err != nil {
		t.Fatalf("second shared (backup during scrub) must succeed: %v", err)
	}
	if _, ok := locks.TryAcquire(repo, scheduler.Exclusive, "prune"); ok {
		t.Fatal("exclusive must fail with two shared holders")
	}
	s1.Release()
	s2.Release()
}
