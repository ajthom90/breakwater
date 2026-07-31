package retention_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/pkg/format"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/clock"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/retention"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/vault"
	"github.com/oklog/ulid/v2"
)

// TestProperty_RandomForgetPruneNeverOrphans is PLAN's explicit M5 property:
// random snapshot sets + policies + forget/prune interleavings never leave a
// surviving snapshot unable to fully restore. Prints the seed on failure.
func TestProperty_RandomForgetPruneNeverOrphans(t *testing.T) {
	seed := time.Now().UnixNano()
	// Allow override for reproduction: go test -args is awkward; use env-less fixed
	// re-run by editing seed or failing with printed seed.
	t.Logf("property seed=%d", seed)
	rng := rand.New(rand.NewSource(seed))

	const iters = 12
	for i := 0; i < iters; i++ {
		if err := runOnePropertyIter(t, rng, seed, i); err != nil {
			t.Fatalf("SEED %d iter %d: %v", seed, i, err)
		}
	}
}

func runOnePropertyIter(t *testing.T, rng *rand.Rand, seed int64, iter int) error {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := catalog.Open(filepath.Join(dir, "c.db"))
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
		ID: machineID, CertFP: "fp-" + machineID, Hostname: "prop", RepoID: machineID, Status: "enrolled",
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

	// Random policy — persist into catalog so ApplyRetention/Prune use it.
	graceDays := 1 + rng.Intn(3) // 1–3 days for faster property
	pol := retention.Policy{
		ID: "p", KeepLast: rng.Intn(5), KeepHourly: rng.Intn(3),
		KeepDaily: rng.Intn(7), KeepWeekly: rng.Intn(4),
		KeepMonthly: rng.Intn(3), KeepYearly: rng.Intn(2),
		PruneGraceDays: graceDays,
	}
	// Override default policy counts + grace for this machine's retention.
	if _, err := db.SQL().Exec(`
		UPDATE policies SET keep_last=?, keep_hourly=?, keep_daily=?, keep_weekly=?,
		keep_monthly=?, keep_yearly=?, prune_grace_days=? WHERE is_default=1`,
		pol.KeepLast, pol.KeepHourly, pol.KeepDaily, pol.KeepWeekly,
		pol.KeepMonthly, pol.KeepYearly, pol.PruneGraceDays); err != nil {
		return err
	}

	nSnaps := 3 + rng.Intn(8)
	type snapRec struct {
		id, manifest string
		root         vault.ObjectID
		ts           time.Time
		payload      string
	}
	var snaps []snapRec
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for j := 0; j < nSnaps; j++ {
		payload := fmt.Sprintf("payload-seed%d-iter%d-j%d-%d", seed, iter, j, rng.Int63())
		root := putTree(t, ctx, v, map[string]string{"f.txt": payload})
		ts := base.Add(time.Duration(j) * time.Hour * time.Duration(1+rng.Intn(48)))
		rec, err := v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
			Kind: vault.KindFileSnapshot, MachineID: machineID,
			Timestamp: ts, RootObjectID: root, Source: "/d",
		})
		if err != nil {
			return err
		}
		id := ulid.Make().String()
		if err := db.InsertSnapshot(ctx, catalog.Snapshot{
			ID: id, MachineID: machineID, Kind: "file", Source: "/d",
			ManifestRef: string(rec), RootObjectID: string(root), CreatedAt: ts,
		}); err != nil {
			return err
		}
		snaps = append(snaps, snapRec{id: id, manifest: string(rec), root: root, ts: ts, payload: payload})
	}

	clk := clock.NewFake(base.Add(30 * 24 * time.Hour))
	zero := time.Duration(0)
	svc := &retention.Service{
		DB: db, Vaults: vm, Keystore: ks,
		Locks: scheduler.NewRepoLocks(), Clock: clk, MinContentAge: &zero,
	}

	// Random interleaving: apply retention, manual forgets, advance clock, prune.
	steps := 5 + rng.Intn(10)
	for step := 0; step < steps; step++ {
		switch rng.Intn(4) {
		case 0:
			if _, err := svc.ApplyRetention(ctx, machineID, "prop", "system"); err != nil {
				return fmt.Errorf("apply: %w", err)
			}
		case 1:
			// manual forget a random live snap (not the newest tip if only one)
			live, _ := db.ListSnapshotsByMachine(ctx, machineID, 1000)
			if len(live) > 1 {
				// never forget newest tip alone if keep would fail — pure forget is OK
				pick := live[1+rng.Intn(len(live)-1)]
				if _, err := svc.Forget(ctx, []string{pick.ID}, "prop", "system", pol.ID, nil); err != nil {
					return fmt.Errorf("forget: %w", err)
				}
			}
		case 2:
			clk.Advance(time.Duration(1+rng.Intn(48)) * time.Hour)
		case 3:
			if _, err := svc.Prune(ctx, machineID, "prop", "system"); err != nil {
				return fmt.Errorf("prune: %w", err)
			}
		}
	}
	// Always finish with a prune past grace so GC runs.
	clk.Advance(time.Duration(pol.PruneGraceDays+1) * 24 * time.Hour)
	if _, err := svc.Prune(ctx, machineID, "prop", "system"); err != nil {
		return fmt.Errorf("final prune: %w", err)
	}

	// Assert every surviving catalog snapshot (non-deleted) fully restores.
	// Also assert vault-only: every remaining vault manifest restores.
	live, err := db.ListSnapshotsByMachine(ctx, machineID, 1000)
	if err != nil {
		return err
	}
	// keep-last property: at least min(keep-last, original) if we only used apply
	// — not always true after random manual forgets; only assert restoreability.

	payloadByID := map[string]string{}
	for _, s := range snaps {
		payloadByID[s.id] = s.payload
	}
	for _, sn := range live {
		want := payloadByID[sn.ID]
		if want == "" {
			// may be unknown if we only have catalog — still walk content
			if err := walkRestoreAll(ctx, v, vault.ObjectID(sn.RootObjectID)); err != nil {
				return fmt.Errorf("survive %s walk: %w", sn.ID, err)
			}
			continue
		}
		got := readFileFromRoot(t, ctx, v, vault.ObjectID(sn.RootObjectID), "f.txt")
		if got != want {
			return fmt.Errorf("survive %s payload want %q got %q", sn.ID, want, got)
		}
	}

	// Soft-deleted within grace (if any left) must still restore.
	soft, _ := db.ListSoftDeletedSnapshots(ctx, machineID)
	for _, sn := range soft {
		if err := walkRestoreAll(ctx, v, vault.ObjectID(sn.RootObjectID)); err != nil {
			return fmt.Errorf("in-grace soft %s walk: %w", sn.ID, err)
		}
	}
	return nil
}

func walkRestoreAll(ctx context.Context, v vault.Vault, root vault.ObjectID) error {
	rc, err := v.OpenObject(ctx, root)
	if err != nil {
		return err
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	var tree format.TreeObject
	if err := json.Unmarshal(raw, &tree); err != nil {
		return err
	}
	for _, e := range tree.Entries {
		if e.Type != format.EntryFile || e.ObjectID == "" {
			continue
		}
		if _, err := v.VerifyObject(ctx, vault.ObjectID(e.ObjectID)); err != nil {
			return fmt.Errorf("verify %s: %w", e.Name, err)
		}
		frc, err := v.OpenObject(ctx, vault.ObjectID(e.ObjectID))
		if err != nil {
			return err
		}
		if _, err := io.ReadAll(frc); err != nil {
			frc.Close()
			return err
		}
		frc.Close()
	}
	return nil
}

// TestProperty_KeepSetInvariants pure keep-set properties with printed seed.
func TestProperty_KeepSetInvariants(t *testing.T) {
	seed := time.Now().UnixNano()
	t.Logf("keepset seed=%d", seed)
	rng := rand.New(rand.NewSource(seed))
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 200; i++ {
		n := 1 + rng.Intn(40)
		var snaps []retention.Snapshot
		for j := 0; j < n; j++ {
			snaps = append(snaps, retention.Snapshot{
				ID:        fmt.Sprintf("s%04d", j),
				Timestamp: now.Add(-time.Duration(rng.Intn(365*24)) * time.Hour),
			})
		}
		p := retention.Policy{
			KeepLast: rng.Intn(10), KeepHourly: rng.Intn(24),
			KeepDaily: rng.Intn(30), KeepWeekly: rng.Intn(12),
			KeepMonthly: rng.Intn(24), KeepYearly: rng.Intn(5),
		}
		r := retention.ComputeKeepSet(snaps, p, now)

		// newest always kept
		newest := snaps[0]
		for _, s := range snaps {
			if s.Timestamp.After(newest.Timestamp) || (s.Timestamp.Equal(newest.Timestamp) && s.ID > newest.ID) {
				newest = s
			}
		}
		if _, ok := r.KeepIDs[newest.ID]; !ok {
			t.Fatalf("SEED %d iter %d: newest %s not kept", seed, i, newest.ID)
		}

		// never fewer than min(keep-last, n)
		wantMin := p.KeepLast
		if wantMin > n {
			wantMin = n
		}
		if wantMin > 0 && len(r.KeepIDs) < wantMin {
			t.Fatalf("SEED %d iter %d: keep %d < keep-last %d", seed, i, len(r.KeepIDs), wantMin)
		}

		// idempotence
		var survivors []retention.Snapshot
		for _, s := range snaps {
			if _, ok := r.KeepIDs[s.ID]; ok {
				survivors = append(survivors, s)
			}
		}
		r2 := retention.ComputeKeepSet(survivors, p, now)
		if len(r2.Forget) != 0 {
			t.Fatalf("SEED %d iter %d: second pass forgot %v", seed, i, r2.Forget)
		}
	}
}

// TestProperty_GraceNeverPhysicallyDeletes ensures PruneEligible is false inside window.
func TestProperty_GraceNeverPhysicallyDeletes(t *testing.T) {
	seed := time.Now().UnixNano()
	t.Logf("grace seed=%d", seed)
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < 100; i++ {
		grace := time.Duration(1+rng.Intn(14)) * 24 * time.Hour
		deleted := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		// inside window
		now := deleted.Add(time.Duration(rng.Int63n(int64(grace))))
		if retention.PruneEligible(deleted, grace, now) {
			t.Fatalf("SEED %d: eligible inside grace deleted=%v now=%v grace=%v", seed, deleted, now, grace)
		}
		// at/after window
		now2 := deleted.Add(grace)
		if !retention.PruneEligible(deleted, grace, now2) {
			t.Fatalf("SEED %d: not eligible at grace boundary", seed)
		}
	}
}
