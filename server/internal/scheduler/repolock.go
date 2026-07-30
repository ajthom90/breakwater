// Package scheduler implements the job engine, per-repo vault serialization,
// and (later) cron windows / retry (M5).
//
// Per-repo RW discipline (PLAN Storage engine → GC/prune; R2-2 / R3-6 / M1-M2):
//
//	shared    — backup, replication, restore-stream jobs
//	exclusive — prune, verify; also Manager Close/Open for a repo
//
// Lock acquisition is job-scoped: held from job start to terminal state,
// INCLUDING open restore readers (a restore stream holds shared until closed).
// That makes the M1 "OpenObject vs prune" caveat structurally impossible.
//
// Vault Manager Close/Open for a repo must happen only under the exclusive
// lock — use WithExclusive (or AcquireExclusive + Release). ALL vault access
// for a repo after enrollment should go through a lease from this component.
//
// Enrollment exception: Create at enroll time is safe without a lease because
// the machine/repo ID is brand-new and no job can exist yet for that ID. Once
// the machine row is inserted, subsequent Open/Close/Prune/backup must lease.
package scheduler

import (
	"context"
	"fmt"
	"sync"
)

// LockMode is the per-repo lock discipline.
type LockMode int

const (
	// Shared allows concurrent holders (backup / restore / replicate).
	Shared LockMode = iota
	// Exclusive is sole holder (prune / verify / Manager Close-Open).
	Exclusive
)

func (m LockMode) String() string {
	switch m {
	case Shared:
		return "shared"
	case Exclusive:
		return "exclusive"
	default:
		return fmt.Sprintf("LockMode(%d)", int(m))
	}
}

// Lease is a held per-repo lock. Release must be called exactly once when the
// job reaches a terminal state (success/failed/cancelled) or on disconnect.
// Double-Release is a no-op; leaked leases wedge retention forever.
type Lease interface {
	RepoID() string
	Mode() LockMode
	// JobID is the owning job (empty for non-job exclusive ops like Close).
	JobID() string
	Release()
}

// RepoLocks is the structural enforcement point for backup-vs-prune and
// Manager Close-vs-Open (R2-2 / R3-6).
type RepoLocks struct {
	mu    sync.Mutex
	repos map[string]*repoState
}

type repoState struct {
	// cond is signaled whenever shared/exclusive counts change.
	cond *sync.Cond
	// shared is the count of shared holders.
	shared int
	// exclusive is true when an exclusive holder is present.
	exclusive bool
	// waiters is a diagnostic counter (not required for correctness).
	waiters int
}

// NewRepoLocks constructs an empty lock table.
func NewRepoLocks() *RepoLocks {
	return &RepoLocks{repos: make(map[string]*repoState)}
}

func (r *RepoLocks) state(repoID string) *repoState {
	st, ok := r.repos[repoID]
	if !ok {
		st = &repoState{}
		st.cond = sync.NewCond(&r.mu)
		r.repos[repoID] = st
	}
	return st
}

// Acquire obtains a job-scoped lease. Blocks until available or ctx is done.
// mode Shared vs Exclusive follows PLAN; jobID is recorded for diagnostics.
func (r *RepoLocks) Acquire(ctx context.Context, repoID string, mode LockMode, jobID string) (Lease, error) {
	if repoID == "" {
		return nil, fmt.Errorf("repolock: empty repoID")
	}
	if mode != Shared && mode != Exclusive {
		return nil, fmt.Errorf("repolock: invalid mode %v", mode)
	}

	// Fast path: try under lock; if must wait, use cond with ctx cancellation.
	r.mu.Lock()
	st := r.state(repoID)

	for {
		if err := ctx.Err(); err != nil {
			r.mu.Unlock()
			return nil, err
		}
		switch mode {
		case Shared:
			if !st.exclusive {
				st.shared++
				l := &lease{r: r, repoID: repoID, mode: Shared, jobID: jobID}
				r.mu.Unlock()
				return l, nil
			}
		case Exclusive:
			if !st.exclusive && st.shared == 0 {
				st.exclusive = true
				l := &lease{r: r, repoID: repoID, mode: Exclusive, jobID: jobID}
				r.mu.Unlock()
				return l, nil
			}
		}

		// Wait with context: spawn a waiter that broadcasts on cancel.
		st.waiters++
		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				r.mu.Lock()
				st.cond.Broadcast()
				r.mu.Unlock()
			case <-done:
			}
		}()
		st.cond.Wait()
		close(done)
		st.waiters--
		// loop re-checks ctx.Err and availability
	}
}

// AcquireShared is Acquire(..., Shared, jobID).
func (r *RepoLocks) AcquireShared(ctx context.Context, repoID, jobID string) (Lease, error) {
	return r.Acquire(ctx, repoID, Shared, jobID)
}

// AcquireExclusive is Acquire(..., Exclusive, jobID).
func (r *RepoLocks) AcquireExclusive(ctx context.Context, repoID, jobID string) (Lease, error) {
	return r.Acquire(ctx, repoID, Exclusive, jobID)
}

// WithExclusive runs fn while holding the exclusive lock for repoID.
// Preferred path for Manager.Close / Manager.Open serialization (R3-6).
func (r *RepoLocks) WithExclusive(ctx context.Context, repoID, jobID string, fn func() error) error {
	l, err := r.AcquireExclusive(ctx, repoID, jobID)
	if err != nil {
		return err
	}
	defer l.Release()
	return fn()
}

// Held reports current holders for tests/diagnostics. exclusive is 0 or 1.
func (r *RepoLocks) Held(repoID string) (shared, exclusive int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.repos[repoID]
	if !ok {
		return 0, 0
	}
	ex := 0
	if st.exclusive {
		ex = 1
	}
	return st.shared, ex
}

// TryAcquire is a non-blocking acquire for tests. Returns nil, false if busy.
func (r *RepoLocks) TryAcquire(repoID string, mode LockMode, jobID string) (Lease, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.state(repoID)
	switch mode {
	case Shared:
		if st.exclusive {
			return nil, false
		}
		st.shared++
		return &lease{r: r, repoID: repoID, mode: Shared, jobID: jobID}, true
	case Exclusive:
		if st.exclusive || st.shared > 0 {
			return nil, false
		}
		st.exclusive = true
		return &lease{r: r, repoID: repoID, mode: Exclusive, jobID: jobID}, true
	default:
		return nil, false
	}
}

type lease struct {
	r      *RepoLocks
	repoID string
	mode   LockMode
	jobID  string
	once   sync.Once
}

func (l *lease) RepoID() string { return l.repoID }
func (l *lease) Mode() LockMode { return l.mode }
func (l *lease) JobID() string  { return l.jobID }
func (l *lease) Release() {
	l.once.Do(func() {
		l.r.mu.Lock()
		defer l.r.mu.Unlock()
		st := l.r.state(l.repoID)
		switch l.mode {
		case Shared:
			if st.shared > 0 {
				st.shared--
			}
		case Exclusive:
			st.exclusive = false
		}
		st.cond.Broadcast()
	})
}

// LockModeForJobType maps catalog job type strings to a vault lock mode.
// Returns ok=false for types that do not touch the vault (inventory, noop, update).
func LockModeForJobType(jobType string) (mode LockMode, ok bool) {
	switch jobType {
	case TypeFileBackup, TypeImageBackup, TypeHyperVBackup, TypeRestore, TypeReplicate:
		return Shared, true
	case TypePrune, TypeVerify:
		return Exclusive, true
	default:
		return 0, false
	}
}
