package retention_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/pkg/format"
	"github.com/ajthom90/breakwater/server/internal/audit"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/clock"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/retention"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/vault"
	"github.com/oklog/ulid/v2"
)

// TestM5_GraceWindowSurvivesPrune is the first non-negotiable safety property:
// a snapshot soft-deleted but still inside the grace window MUST survive prune
// (vault data intact + restorable). Written red-first before the implementation
// was complete; fails if prune deletes vault manifests without grace check.
func TestM5_GraceWindowSurvivesPrune(t *testing.T) {
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

	machineID := ulid.Make().String()
	if err := db.InsertMachine(ctx, catalog.Machine{
		ID: machineID, CertFP: "fp-" + machineID, Hostname: "grace-host", RepoID: machineID,
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

	// Two snapshots with distinct content.
	root1 := putTree(t, ctx, v, map[string]string{"keep-me.txt": "grace-protected-payload-v1"})
	rec1, err := v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind: vault.KindFileSnapshot, MachineID: machineID,
		Timestamp:    time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		RootObjectID: root1, Source: "/data",
	})
	if err != nil {
		t.Fatal(err)
	}
	root2 := putTree(t, ctx, v, map[string]string{"live.txt": "still-live-payload-v2"})
	rec2, err := v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind: vault.KindFileSnapshot, MachineID: machineID,
		Timestamp:    time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC),
		RootObjectID: root2, Source: "/data",
	})
	if err != nil {
		t.Fatal(err)
	}

	id1, id2 := ulid.Make().String(), ulid.Make().String()
	for _, s := range []catalog.Snapshot{
		{ID: id1, MachineID: machineID, Kind: "file", Source: "/data",
			ManifestRef: string(rec1), RootObjectID: string(root1),
			CreatedAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)},
		{ID: id2, MachineID: machineID, Kind: "file", Source: "/data",
			ManifestRef: string(rec2), RootObjectID: string(root2),
			CreatedAt: time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)},
	} {
		if err := db.InsertSnapshot(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	// Fake clock: forget at t0, prune immediately (well within 7-day grace).
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(t0)
	zero := time.Duration(0)
	svc := &retention.Service{
		DB: db, Vaults: vm, Keystore: ks,
		Locks:         scheduler.NewRepoLocks(),
		Clock:         clk,
		Auditor:       audit.NewWriter(db),
		MinContentAge: &zero, // observe content reclaim when past grace
	}

	if _, err := svc.Forget(ctx, []string{id1}, "test", audit.ActorSystem, "policy", map[string]string{id1: "manual"}); err != nil {
		t.Fatalf("forget: %v", err)
	}
	// Confirm soft-deleted.
	s1, _ := db.SnapshotByID(ctx, id1)
	if s1 == nil || s1.DeletedAt == nil {
		t.Fatal("expected soft-deleted snapshot")
	}

	// Prune immediately — MUST NOT destroy id1's data.
	pr, err := svc.Prune(ctx, machineID, "test", audit.ActorSystem)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(pr.Eligible) != 0 {
		t.Fatalf("within grace: eligible must be empty, got %v", pr.Eligible)
	}

	// Vault still has both manifests; content of forgotten snapshot restores.
	got, err := v.GetSnapshotRecord(ctx, rec1)
	if err != nil || got == nil {
		t.Fatalf("in-grace forgotten snapshot must remain in vault: %v", err)
	}
	body := readFileFromRoot(t, ctx, v, root1, "keep-me.txt")
	if body != "grace-protected-payload-v1" {
		t.Fatalf("grace data corrupted: %q", body)
	}
	body2 := readFileFromRoot(t, ctx, v, root2, "live.txt")
	if body2 != "still-live-payload-v2" {
		t.Fatalf("live data broken: %q", body2)
	}

	// Advance past 7-day grace → prune may reclaim.
	clk.Advance(7*24*time.Hour + time.Second)
	pr2, err := svc.Prune(ctx, machineID, "test", audit.ActorSystem)
	if err != nil {
		t.Fatalf("prune after grace: %v", err)
	}
	if len(pr2.Eligible) != 1 || pr2.Eligible[0] != id1 {
		t.Fatalf("after grace eligible=%v want [%s]", pr2.Eligible, id1)
	}
	// id1 catalog row gone; vault manifest gone; live snap2 still OK.
	if s, _ := db.SnapshotByID(ctx, id1); s != nil {
		t.Fatal("catalog row should be hard-deleted after prune")
	}
	if _, err := v.GetSnapshotRecord(ctx, rec1); err == nil {
		t.Fatal("vault manifest should be gone after prune past grace")
	}
	body2 = readFileFromRoot(t, ctx, v, root2, "live.txt")
	if body2 != "still-live-payload-v2" {
		t.Fatalf("live after prune: %q", body2)
	}
}

func putTree(t *testing.T, ctx context.Context, v vault.Vault, files map[string]string) vault.ObjectID {
	t.Helper()
	entries := make([]format.TreeEntry, 0, len(files))
	for name, content := range files {
		oid, err := v.WriteObject(ctx, vault.SplitterDynamic, bytes.NewReader([]byte(content)))
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, format.TreeEntry{
			Name: name, Type: format.EntryFile, Size: int64(len(content)), ObjectID: string(oid),
		})
	}
	tree := format.TreeObject{Version: format.FormatVersion, Entries: entries}
	raw, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	toid, err := v.WriteObject(ctx, vault.SplitterDynamic, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return toid
}

func readFileFromRoot(t *testing.T, ctx context.Context, v vault.Vault, root vault.ObjectID, name string) string {
	t.Helper()
	rc, err := v.OpenObject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	var tree format.TreeObject
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatalf("decode tree: %v", err)
	}
	for _, e := range tree.Entries {
		if e.Name == name {
			frc, err := v.OpenObject(ctx, vault.ObjectID(e.ObjectID))
			if err != nil {
				t.Fatal(err)
			}
			defer frc.Close()
			b, err := io.ReadAll(frc)
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
	}
	t.Fatalf("file %s not found", name)
	return ""
}
