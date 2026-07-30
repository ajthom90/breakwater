package scheduler_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/scheduler"
)

// unserialisedStub grants every Acquire immediately with no mutual exclusion.
// Used for red-first evidence: overlap counter must go non-zero under this stub.
type unserialisedStub struct{}

type stubLease struct {
	repo string
	mode scheduler.LockMode
	job  string
}

func (s stubLease) RepoID() string           { return s.repo }
func (s stubLease) Mode() scheduler.LockMode { return s.mode }
func (s stubLease) JobID() string            { return s.job }
func (s stubLease) Release()                 {}

func (u *unserialisedStub) Acquire(_ context.Context, repoID string, mode scheduler.LockMode, jobID string) (scheduler.Lease, error) {
	return stubLease{repo: repoID, mode: mode, job: jobID}, nil
}

// probeOverlap runs backup (shared) and prune (exclusive) "jobs" concurrently,
// each holding the lease for hold time while a critical section increments an
// overlap counter if the peer is also in section. Returns peak overlap.
func probeOverlap(t *testing.T, acquire func(ctx context.Context, repo string, mode scheduler.LockMode, job string) (scheduler.Lease, error)) int32 {
	t.Helper()
	const hold = 80 * time.Millisecond
	var inBackup, inPrune atomic.Int32
	var peak atomic.Int32

	enter := func(which *atomic.Int32) {
		which.Store(1)
		o := inBackup.Load() + inPrune.Load()
		for {
			cur := peak.Load()
			if o <= cur || peak.CompareAndSwap(cur, o) {
				break
			}
		}
		time.Sleep(hold)
		which.Store(0)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		l, err := acquire(ctx, "repo-A", scheduler.Shared, "backup-1")
		if err != nil {
			t.Errorf("backup acquire: %v", err)
			return
		}
		enter(&inBackup)
		l.Release()
	}()
	// Slight delay so backup usually acquires first under real locks.
	time.Sleep(10 * time.Millisecond)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		l, err := acquire(ctx, "repo-A", scheduler.Exclusive, "prune-1")
		if err != nil {
			t.Errorf("prune acquire: %v", err)
			return
		}
		enter(&inPrune)
		l.Release()
	}()
	wg.Wait()
	return peak.Load()
}

// TestRepoLock_RedFirst_UnserialisedOverlaps documents that without serialization
// backup+prune critical sections overlap (peak ≥ 2). Captured for PROGRESS.md.
func TestRepoLock_RedFirst_UnserialisedOverlaps(t *testing.T) {
	stub := &unserialisedStub{}
	peak := probeOverlap(t, stub.Acquire)
	if peak < 2 {
		t.Fatalf("red-first: unserialised stub should allow overlap peak≥2; got %d", peak)
	}
	t.Logf("RED-FIRST evidence: unserialised stub peak overlap=%d (backup+prune concurrent)", peak)
}

// TestRepoLock_BackupPruneNeverOverlap is the structural guarantee (R2-2).
func TestRepoLock_BackupPruneNeverOverlap(t *testing.T) {
	locks := scheduler.NewRepoLocks()
	peak := probeOverlap(t, locks.Acquire)
	if peak > 1 {
		t.Fatalf("backup+prune overlapped (peak=%d); serialization broken", peak)
	}
	t.Logf("serialized backup+prune peak overlap=%d (want ≤1)", peak)
}

// TestRepoLock_TwoBackupsConcurrent — shared holders run together.
func TestRepoLock_TwoBackupsConcurrent(t *testing.T) {
	locks := scheduler.NewRepoLocks()
	var concurrent atomic.Int32
	var peak atomic.Int32
	const hold = 60 * time.Millisecond

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		job := "backup-" + string(rune('A'+i))
		go func(jobID string) {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			l, err := locks.AcquireShared(ctx, "repo-A", jobID)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			n := concurrent.Add(1)
			for {
				cur := peak.Load()
				if n <= cur || peak.CompareAndSwap(cur, n) {
					break
				}
			}
			time.Sleep(hold)
			concurrent.Add(-1)
			l.Release()
		}(job)
	}
	close(start)
	wg.Wait()
	if peak.Load() < 2 {
		t.Fatalf("expected two shared backups concurrent; peak=%d", peak.Load())
	}
	t.Logf("two backups peak concurrency=%d", peak.Load())
}

// TestRepoLock_CrossRepoIsolation — prune A does not block backup B.
func TestRepoLock_CrossRepoIsolation(t *testing.T) {
	locks := scheduler.NewRepoLocks()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	prune, err := locks.AcquireExclusive(ctx, "repo-A", "prune-A")
	if err != nil {
		t.Fatal(err)
	}
	defer prune.Release()

	// Backup on B must not block.
	done := make(chan struct{})
	go func() {
		defer close(done)
		l, err := locks.AcquireShared(ctx, "repo-B", "backup-B")
		if err != nil {
			t.Errorf("backup B: %v", err)
			return
		}
		l.Release()
	}()
	select {
	case <-done:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("backup on repo-B blocked by prune on repo-A")
	}
}

// TestRepoLock_ReleaseOnCancel — cancelled wait does not leak; exclusive can proceed.
func TestRepoLock_ReleaseOnCancel(t *testing.T) {
	locks := scheduler.NewRepoLocks()
	ctx := context.Background()
	shared, err := locks.AcquireShared(ctx, "repo-X", "job-shared")
	if err != nil {
		t.Fatal(err)
	}

	// Exclusive waiter cancelled before grant.
	wctx, wcancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := locks.AcquireExclusive(wctx, "repo-X", "job-ex")
		errCh <- err
	}()
	time.Sleep(30 * time.Millisecond)
	wcancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected context cancel error for exclusive waiter")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("exclusive waiter did not return on cancel")
	}

	shared.Release()
	// After shared release, exclusive must succeed.
	l, err := locks.AcquireExclusive(ctx, "repo-X", "job-ex-2")
	if err != nil {
		t.Fatalf("exclusive after release: %v", err)
	}
	l.Release()
	s, ex := locks.Held("repo-X")
	if s != 0 || ex != 0 {
		t.Fatalf("leak after release: shared=%d exclusive=%d", s, ex)
	}
}

// TestRepoLock_DoubleReleaseIdempotent — no panic / underflow wedge.
func TestRepoLock_DoubleReleaseIdempotent(t *testing.T) {
	locks := scheduler.NewRepoLocks()
	l, err := locks.AcquireShared(context.Background(), "repo-Z", "j1")
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	l.Release() // must not panic or drive shared negative
	s, ex := locks.Held("repo-Z")
	if s != 0 || ex != 0 {
		t.Fatalf("after double release: shared=%d exclusive=%d", s, ex)
	}
	// Still acquirable.
	l2, err := locks.AcquireExclusive(context.Background(), "repo-Z", "j2")
	if err != nil {
		t.Fatal(err)
	}
	l2.Release()
}

// TestRepoLock_TryAcquireBusy
func TestRepoLock_TryAcquireBusy(t *testing.T) {
	locks := scheduler.NewRepoLocks()
	l, ok := locks.TryAcquire("r", scheduler.Shared, "a")
	if !ok {
		t.Fatal("expected shared try ok")
	}
	if _, ok := locks.TryAcquire("r", scheduler.Exclusive, "b"); ok {
		t.Fatal("exclusive must fail while shared held")
	}
	l.Release()
	if _, ok := locks.TryAcquire("r", scheduler.Exclusive, "c"); !ok {
		t.Fatal("exclusive after release")
	}
}

// TestLockModeForJobType mapping.
func TestLockModeForJobType(t *testing.T) {
	cases := []struct {
		typ    string
		mode   scheduler.LockMode
		wantOK bool
	}{
		{scheduler.TypeFileBackup, scheduler.Shared, true},
		{scheduler.TypeRestore, scheduler.Shared, true},
		{scheduler.TypePrune, scheduler.Exclusive, true},
		{scheduler.TypeVerify, scheduler.Exclusive, true},
		{scheduler.TypeInventory, 0, false},
		{scheduler.TypeNoop, 0, false},
	}
	for _, tc := range cases {
		m, ok := scheduler.LockModeForJobType(tc.typ)
		if ok != tc.wantOK || (ok && m != tc.mode) {
			t.Errorf("%s: got (%v,%v) want (%v,%v)", tc.typ, m, ok, tc.mode, tc.wantOK)
		}
	}
}
