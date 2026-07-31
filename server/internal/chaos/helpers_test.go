package chaos_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/pkg/format"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/clock"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/notify"
	"github.com/ajthom90/breakwater/server/internal/retention"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/vault"
	"github.com/oklog/ulid/v2"
)

// drillEnv is a self-contained vault+catalog harness for chaos drills that do
// not need the full agent gRPC stack.
type drillEnv struct {
	t         *testing.T
	Dir       string
	ReposDir  string
	DB        *catalog.DB
	KS        *keystore.Store
	VM        *vault.Manager
	Locks     *scheduler.RepoLocks
	Clock     *clock.Fake
	MachineID string
	Password  string
	Notifier  *notify.Notifier
	FakeSend  *notify.FakeSender
	Svc       *retention.Service
}

func newDrillEnv(t *testing.T) *drillEnv {
	t.Helper()
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
	repos := filepath.Join(dir, "repos")
	vm := vault.NewManager(repos, dir)
	t.Cleanup(func() { _ = vm.CloseAll(ctx) })

	machineID := ulid.Make().String()
	if err := db.InsertMachine(ctx, catalog.Machine{
		ID: machineID, CertFP: "fp-" + machineID, Hostname: "chaos-host",
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

	clk := clock.NewFake(time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC))
	fake := &notify.FakeSender{}
	n := notify.New(fake, clk, nil)
	n.DefaultTo = []string{"ops@chaos.test"}
	nctx, cancel := context.WithCancel(context.Background())
	n.Start(nctx)
	t.Cleanup(func() {
		cancel()
		n.Close()
	})

	zero := time.Duration(0)
	locks := scheduler.NewRepoLocks()
	svc := &retention.Service{
		DB: db, Vaults: vm, Keystore: ks, Locks: locks,
		Clock: clk, MinContentAge: &zero, Notifier: n,
	}

	return &drillEnv{
		t: t, Dir: dir, ReposDir: repos, DB: db, KS: ks, VM: vm,
		Locks: locks, Clock: clk, MachineID: machineID, Password: pw,
		Notifier: n, FakeSend: fake, Svc: svc,
	}
}

func (e *drillEnv) openVault(ctx context.Context) vault.Vault {
	e.t.Helper()
	v, err := e.VM.Open(ctx, e.MachineID, e.Password)
	if err != nil {
		e.t.Fatalf("open vault: %v", err)
	}
	return v
}

// putSnapshot writes a file-tree snapshot with one file and mirrors it to catalog.
func (e *drillEnv) putSnapshot(ctx context.Context, v vault.Vault, name, payload string, ts time.Time) (catalogID string, root vault.ObjectID, manifest vault.SnapshotRecordID) {
	e.t.Helper()
	root = putTree(e.t, ctx, v, map[string]string{name: payload})
	rec, err := v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind: vault.KindFileSnapshot, MachineID: e.MachineID,
		Timestamp: ts, RootObjectID: root, Source: "/chaos",
	})
	if err != nil {
		e.t.Fatalf("PutSnapshotRecord: %v", err)
	}
	id := ulid.Make().String()
	if err := e.DB.InsertSnapshot(ctx, catalog.Snapshot{
		ID: id, MachineID: e.MachineID, Kind: "file", Source: "/chaos",
		ManifestRef: string(rec), RootObjectID: string(root), CreatedAt: ts,
	}); err != nil {
		e.t.Fatalf("InsertSnapshot: %v", err)
	}
	return id, root, rec
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

// assertAllSurvivorsRestorable walks every non-deleted catalog snapshot (and
// in-grace soft-deleted) and fully reads every file object — same invariant as
// M5 property tests. Returns the count checked.
func assertAllSurvivorsRestorable(t *testing.T, ctx context.Context, db *catalog.DB, v vault.Vault, machineID string) int {
	t.Helper()
	live, err := db.ListSnapshotsByMachine(ctx, machineID, 100000)
	if err != nil {
		t.Fatal(err)
	}
	soft, err := db.ListSoftDeletedSnapshots(ctx, machineID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, sn := range live {
		if err := walkRestoreAll(ctx, v, vault.ObjectID(sn.RootObjectID)); err != nil {
			t.Fatalf("live snapshot %s (root %s) not restorable: %v", sn.ID, sn.RootObjectID, err)
		}
		n++
	}
	for _, sn := range soft {
		if err := walkRestoreAll(ctx, v, vault.ObjectID(sn.RootObjectID)); err != nil {
			t.Fatalf("in-grace soft snapshot %s not restorable: %v", sn.ID, err)
		}
		n++
	}
	// Vault-side: every remaining vault manifest must also restore.
	metas, err := v.ListSnapshotRecords(ctx, vault.KindFileSnapshot)
	if err != nil {
		t.Fatalf("ListSnapshotRecords: %v", err)
	}
	for _, m := range metas {
		rec, err := v.GetSnapshotRecord(ctx, m.ID)
		if err != nil {
			t.Fatalf("GetSnapshotRecord %s: %v", m.ID, err)
		}
		if err := walkRestoreAll(ctx, v, rec.RootObjectID); err != nil {
			t.Fatalf("vault manifest %s not restorable: %v", m.ID, err)
		}
	}
	return n
}

func walkRestoreAll(ctx context.Context, v vault.Vault, root vault.ObjectID) error {
	rc, err := v.OpenObject(ctx, root)
	if err != nil {
		return fmt.Errorf("open root: %w", err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	var tree format.TreeObject
	if err := json.Unmarshal(raw, &tree); err != nil {
		return fmt.Errorf("decode tree: %w", err)
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
			return fmt.Errorf("open file %s: %w", e.Name, err)
		}
		if _, err := io.ReadAll(frc); err != nil {
			frc.Close()
			return fmt.Errorf("read file %s: %w", e.Name, err)
		}
		frc.Close()
	}
	return nil
}

// findPackFiles returns kopia *content pack* blob files under the machine repo.
// Only paths under the "p/" pack directory are returned — flipping format
// blobs (kopia.repository.f), indexes, or logs makes the repo unopenable and
// does not exercise scrub's content authentication path.
func findPackFiles(t *testing.T, reposDir, machineID string) []string {
	t.Helper()
	root := filepath.Join(reposDir, machineID)
	var packs []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if info.Size() < 64 {
			return nil
		}
		// kopia content packs live under …/p/<shard>/<id>.f
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) < 2 || parts[0] != "p" {
			return nil
		}
		packs = append(packs, path)
		return nil
	})
	return packs
}

// corruptPackFile damages path so content auth must fail on read.
// Strategy: XOR a multi-byte window through the middle of the pack (not a single
// bit — a single flip can land in padding/unused regions and leave GetContent
// happy, which made the drill flaky). Returns offset and first before/after bytes.
func corruptPackFile(t *testing.T, path string, rng *rand.Rand) (offset int64, before, after byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() < 64 {
		t.Fatal("pack too small")
	}
	// Corrupt ~min(256, size/4) bytes starting near the middle.
	win := int64(256)
	if st.Size()/4 < win {
		win = st.Size() / 4
	}
	if win < 16 {
		win = 16
	}
	start := st.Size()/4 + rng.Int63n(st.Size()/2)
	if start+win > st.Size() {
		start = st.Size() - win
	}
	buf := make([]byte, win)
	if _, err := f.ReadAt(buf, start); err != nil {
		t.Fatal(err)
	}
	before = buf[0]
	for i := range buf {
		buf[i] ^= 0xFF
	}
	after = buf[0]
	if _, err := f.WriteAt(buf, start); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	return start, before, after
}

// bitFlip is a thin alias kept for mutation_selfcheck; prefer corruptPackFile.
func bitFlip(t *testing.T, path string, rng *rand.Rand) (offset int64, before, after byte) {
	return corruptPackFile(t, path, rng)
}

func waitNotify(t *testing.T, fake *notify.FakeSender, kind string, timeout time.Duration) notify.Message {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, m := range fake.Messages() {
			if m.Kind == kind {
				return m
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no notify message kind=%s within %v (have %d msgs)", kind, timeout, len(fake.Messages()))
	return notify.Message{}
}
