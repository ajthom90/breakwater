package agentgw_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	bwagent "github.com/ajthom90/breakwater/agent"
	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
	"github.com/ajthom90/breakwater/pkg/restore"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/rescan"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/vault"
	"github.com/ajthom90/breakwater/tools/golden"
)

// ---------------------------------------------------------------------------
// Red-first security properties (M4 non-negotiable)
//
// (a) Machine B reading machine A's repo without a restore job → PermissionDenied
// (b) A restore job for snapshot X must not authorize reads outside X's reachable set
//
// These tests were first run against a deliberately permissive stub that opened
// any enrolled peer's vault for any object id; both failed as expected:
//
//	=== RUN   TestM4_RedFirst_CrossMachineWithoutJobDenied
//	    B GetSnapshot of A's snap succeeded without restore job (authz hole)
//	--- FAIL
//	=== RUN   TestM4_RedFirst_JobDoesNotAuthorizeOutsideReachableSet
//	    B GetObject of out-of-snapshot object succeeded under job for snap X
//	--- FAIL
//
// Production RestoreServer enforces both; tests now expect PermissionDenied.
// ---------------------------------------------------------------------------

func TestM4_RedFirst_CrossMachineWithoutJobDenied(t *testing.T) {
	env := startDataEnv(t)
	ctx := context.Background()

	// Machine A backs up a secret file.
	srcA := t.TempDir()
	secret := []byte("a-secret-payload")
	if err := os.WriteFile(filepath.Join(srcA, "secret.txt"), secret, 0o644); err != nil {
		t.Fatal(err)
	}
	stateA, metaA, credsA, midA := enrollRealAgent(t, env, "m4-sec-a")
	_, stopA := startRealAgent(t, env, stateA, metaA, credsA)
	defer stopA()
	waitOnline(t, env.DB, midA, 5*time.Second)
	_ = metaA
	_ = credsA

	params, _ := json.Marshal(map[string]string{"source": srcA})
	jobA, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: midA, Type: scheduler.TypeFileBackup, Initiator: "m4-sec",
		ParamsJSON: string(params),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitJobState(t, env.Engine, jobA, catalog.JobStateSuccess, 60*time.Second)
	snaps, err := env.DB.ListSnapshotsByMachine(ctx, midA, 1)
	if err != nil || len(snaps) < 1 {
		t.Fatalf("snaps: %v", err)
	}
	snapA := snaps[0]

	// Machine B: enrolled, no restore job.
	_, _, _, _, connB := env.mintAndEnroll("m4-sec-b")
	restoreB := breakwaterv1.NewRestoreServiceClient(connB)

	// (a) B must not GetSnapshot for A's snapshot.
	_, err = restoreB.GetSnapshot(ctx, &breakwaterv1.GetSnapshotRequest{SnapshotId: snapA.ID})
	if err == nil {
		t.Fatal("B GetSnapshot of A's snap succeeded without restore job (authz hole)")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v: %v", status.Code(err), err)
	}

	// (a) B must not ListSnapshots for A.
	_, err = restoreB.ListSnapshots(ctx, &breakwaterv1.ListSnapshotsRequest{MachineId: midA})
	if err == nil {
		t.Fatal("B ListSnapshots(A) succeeded without restore job")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("ListSnapshots want PermissionDenied, got %v: %v", status.Code(err), err)
	}

	// (a) B must not GetObject of A's root.
	stream, err := restoreB.GetObject(ctx, &breakwaterv1.GetObjectRequest{ObjectId: snapA.RootObjectID})
	if err != nil {
		// Immediate rejection is fine.
		if status.Code(err) != codes.PermissionDenied && status.Code(err) != codes.NotFound {
			t.Fatalf("GetObject open: %v", err)
		}
		t.Logf("B GetObject rejected at open: %v", err)
		return
	}
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("B GetObject of A's root succeeded without restore job (authz hole)")
	}
	if status.Code(err) != codes.PermissionDenied && status.Code(err) != codes.NotFound {
		// NotFound is acceptable if we never open foreign vaults (no oracle).
		t.Logf("B GetObject rejected: %v (code=%v)", err, status.Code(err))
	}
	t.Logf("cross-machine without job denied: %v", err)
}

func TestM4_RedFirst_JobDoesNotAuthorizeOutsideReachableSet(t *testing.T) {
	env := startDataEnv(t)
	ctx := context.Background()

	// A: two independent backups → two snapshots with distinct roots.
	src1 := t.TempDir()
	_ = os.WriteFile(filepath.Join(src1, "in-snap.txt"), []byte("inside-x"), 0o644)
	src2 := t.TempDir()
	_ = os.WriteFile(filepath.Join(src2, "out-snap.txt"), []byte("outside-x"), 0o644)

	stateA, metaA, credsA, midA := enrollRealAgent(t, env, "m4-reach-a")
	_, stopA := startRealAgent(t, env, stateA, metaA, credsA)
	defer stopA()
	waitOnline(t, env.DB, midA, 5*time.Second)

	backupDir := func(src string) catalog.Snapshot {
		t.Helper()
		p, _ := json.Marshal(map[string]string{"source": src})
		id, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
			MachineID: midA, Type: scheduler.TypeFileBackup, Initiator: "m4-reach",
			ParamsJSON: string(p),
		})
		if err != nil {
			t.Fatal(err)
		}
		waitJobState(t, env.Engine, id, catalog.JobStateSuccess, 60*time.Second)
		snaps, err := env.DB.ListSnapshotsByMachine(ctx, midA, 5)
		if err != nil || len(snaps) < 1 {
			t.Fatal(err)
		}
		// Newest first.
		return snaps[0]
	}
	snapOutside := backupDir(src2) // first (older after second backup)
	snapX := backupDir(src1)       // job will authorize only this one

	// Ensure we have two distinct roots.
	if snapX.RootObjectID == snapOutside.RootObjectID {
		t.Fatal("expected distinct roots for two backups")
	}
	// Re-list to identify which is which by source path.
	all, _ := env.DB.ListSnapshotsByMachine(ctx, midA, 10)
	for _, s := range all {
		if s.Source == src1 {
			snapX = s
		}
		if s.Source == src2 {
			snapOutside = s
		}
	}
	if snapX.ID == snapOutside.ID {
		t.Fatal("could not distinguish snapshots")
	}

	// Machine B: enroll with real agent briefly so machine exists, then hold a
	// restore job open with a fake channel (skipResult) so the lease stays held.
	stateB, metaB, credsB, midB := enrollRealAgent(t, env, "m4-reach-b")
	_, stopB := startRealAgent(t, env, stateB, metaB, credsB)
	waitOnline(t, env.DB, midB, 5*time.Second)
	stopB() // free the control channel for a hold agent

	connB, err := grpc.NewClient(env.Addr,
		grpc.WithTransportCredentials(credentials.NewTLS(bwagent.ClientTLSConfig(credsB, metaB.ServerFP))),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16<<20), grpc.MaxCallSendMsgSize(16<<20)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connB.Close() })

	agentHold := openChannel(t, connB, midB)
	agentHold.skipResult = true
	agentHold.heartbeat()
	waitOnline(t, env.DB, midB, 5*time.Second)

	rparams, _ := json.Marshal(map[string]string{
		"source_snapshot_id": snapX.ID,
		"source_machine_id":  midA,
		"target_path":        t.TempDir(),
		"conflict_policy":    "overwrite",
	})
	holdJob, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: midB, Type: scheduler.TypeRestore, Initiator: "m4-reach-hold",
		ParamsJSON: string(rparams),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitJobState(t, env.Engine, holdJob, catalog.JobStateRunning, 5*time.Second)
	if !env.Engine.HasLease(holdJob) {
		t.Fatal("hold restore job must hold lease")
	}
	// Lease must be on SOURCE (A), not B.
	ok, repoID := env.Engine.VaultForJob(holdJob)
	if !ok || repoID != midA {
		t.Fatalf("restore lease repo=%q ok=%v want source %s", repoID, ok, midA)
	}

	restoreCl := breakwaterv1.NewRestoreServiceClient(connB)

	// Allowed: root of snapX.
	stream, err := restoreCl.GetObject(ctx, &breakwaterv1.GetObjectRequest{ObjectId: snapX.RootObjectID})
	if err != nil {
		t.Fatalf("GetObject snapX root: %v", err)
	}
	if _, err := readAllStream(stream); err != nil {
		t.Fatalf("read snapX root: %v", err)
	}

	// (b) Denied: root of snapOutside (different snapshot, same source repo).
	stream2, err := restoreCl.GetObject(ctx, &breakwaterv1.GetObjectRequest{ObjectId: snapOutside.RootObjectID})
	if err != nil {
		if status.Code(err) == codes.PermissionDenied {
			t.Logf("out-of-reach object denied at open: %v", err)
			return
		}
		t.Fatalf("unexpected open err: %v", err)
	}
	_, err = stream2.Recv()
	if err == nil {
		t.Fatal("B GetObject of out-of-snapshot object succeeded under job for snap X (reachability hole)")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied for out-of-reach, got %v: %v", status.Code(err), err)
	}
	t.Logf("reachability boundary enforced: %v", err)
}

// TestM4_RestoreRoundTrip: golden → backup → RestoreService restore → compare.
func TestM4_RestoreRoundTrip(t *testing.T) {
	env := startDataEnv(t)
	ctx := context.Background()

	src := t.TempDir()
	gen, err := golden.Generate(golden.Options{Root: src})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("golden created=%v skipped=%d", gen.Created, len(gen.Skipped))

	stateDir, meta, creds, machineID := enrollRealAgent(t, env, "m4-roundtrip")
	_, stop := startRealAgent(t, env, stateDir, meta, creds)
	defer stop()
	waitOnline(t, env.DB, machineID, 5*time.Second)

	params, _ := json.Marshal(map[string]string{"source": src})
	jobID, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeFileBackup, Initiator: "m4",
		ParamsJSON: string(params),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitJobState(t, env.Engine, jobID, catalog.JobStateSuccess, 120*time.Second)

	snaps, err := env.DB.ListSnapshotsByMachine(ctx, machineID, 1)
	if err != nil || len(snaps) < 1 {
		t.Fatalf("snapshots: %v", err)
	}
	snap := snaps[0]

	// Restore via real RestoreService + pkg/restore (not vault helper).
	conn, err := grpc.NewClient(env.Addr,
		grpc.WithTransportCredentials(credentials.NewTLS(bwagent.ClientTLSConfig(creds, meta.ServerFP))),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16<<20), grpc.MaxCallSendMsgSize(16<<20)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	restoreCl := breakwaterv1.NewRestoreServiceClient(conn)

	// List + GetSnapshot (own repo).
	list, err := restoreCl.ListSnapshots(ctx, &breakwaterv1.ListSnapshotsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.GetSnapshots()) < 1 {
		t.Fatal("ListSnapshots empty")
	}
	gs, err := restoreCl.GetSnapshot(ctx, &breakwaterv1.GetSnapshotRequest{SnapshotId: snap.ID})
	if err != nil {
		t.Fatal(err)
	}
	if gs.GetRootObjectId() != snap.RootObjectID {
		t.Fatalf("root mismatch")
	}

	// Alternate-path restore.
	alt := t.TempDir()
	stats, err := restore.Run(ctx, restore.Options{
		RootObjectID: snap.RootObjectID,
		TargetRoot:   alt,
		Conflict:     restore.ConflictOverwrite,
		Reader:       &restore.GRPCReader{Client: restoreCl},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stats.Skipped {
		if s.Reason == "" {
			t.Fatalf("silent skip: %+v", s)
		}
		t.Logf("restore skip: %s — %s", s.Path, s.Reason)
	}

	cmp, err := golden.Compare(src, alt, golden.CompareOptions{
		CompareACL: true, CompareADS: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cmp.Equal() {
		t.Fatalf("golden compare failed: %+v", cmp.Diffs)
	}
	t.Logf("M4 round-trip OK matched=%d files=%d bytes=%d", cmp.MatchedFiles, stats.Files, stats.Bytes)

	// Agent JOB_TYPE_RESTORE to another path.
	agentDest := t.TempDir()
	rparams, _ := json.Marshal(map[string]string{
		"source_snapshot_id": snap.ID,
		"target_path":        agentDest,
		"conflict_policy":    "overwrite",
	})
	rj, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeRestore, Initiator: "m4",
		ParamsJSON: string(rparams),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitJobState(t, env.Engine, rj, catalog.JobStateSuccess, 60*time.Second)
	cmp2, err := golden.Compare(src, agentDest, golden.CompareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !cmp2.Equal() {
		t.Fatalf("agent restore compare: %+v", cmp2.Diffs)
	}
}

// TestM4_CrossMachineRestore: A backs up; B restores via job; B cannot read without job.
func TestM4_CrossMachineRestore(t *testing.T) {
	env := startDataEnv(t)
	ctx := context.Background()

	src := t.TempDir()
	payload := []byte("cross-machine-bytes-m4")
	if err := os.WriteFile(filepath.Join(src, "x.txt"), payload, 0o644); err != nil {
		t.Fatal(err)
	}

	stateA, metaA, credsA, midA := enrollRealAgent(t, env, "m4-cross-a")
	_, stopA := startRealAgent(t, env, stateA, metaA, credsA)
	defer stopA()
	waitOnline(t, env.DB, midA, 5*time.Second)

	p, _ := json.Marshal(map[string]string{"source": src})
	bj, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: midA, Type: scheduler.TypeFileBackup, Initiator: "m4", ParamsJSON: string(p),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitJobState(t, env.Engine, bj, catalog.JobStateSuccess, 60*time.Second)
	snaps, _ := env.DB.ListSnapshotsByMachine(ctx, midA, 1)
	snap := snaps[0]

	stateB, metaB, credsB, midB := enrollRealAgent(t, env, "m4-cross-b")
	_, stopB := startRealAgent(t, env, stateB, metaB, credsB)
	defer stopB()
	waitOnline(t, env.DB, midB, 5*time.Second)

	dest := t.TempDir()
	rparams, _ := json.Marshal(map[string]string{
		"source_snapshot_id": snap.ID,
		"source_machine_id":  midA,
		"target_path":        dest,
		"conflict_policy":    "overwrite",
	})
	rj, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: midB, Type: scheduler.TypeRestore, Initiator: "m4",
		ParamsJSON: string(rparams),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitJobState(t, env.Engine, rj, catalog.JobStateSuccess, 60*time.Second)

	got, err := os.ReadFile(filepath.Join(dest, "x.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("cross restore bytes: %q", got)
	}
	t.Logf("cross-machine restore OK A=%s → B=%s", midA, midB)

	// After job terminal, B must not read A's objects.
	connB, err := grpc.NewClient(env.Addr,
		grpc.WithTransportCredentials(credentials.NewTLS(bwagent.ClientTLSConfig(credsB, metaB.ServerFP))),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connB.Close() })
	restoreB := breakwaterv1.NewRestoreServiceClient(connB)
	_, err = restoreB.GetSnapshot(ctx, &breakwaterv1.GetSnapshotRequest{SnapshotId: snap.ID})
	if err == nil {
		t.Fatal("B GetSnapshot after job terminal must be denied")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code=%v: %v", status.Code(err), err)
	}
}

// TestM4_RestoreLeaseBlocksPrune: open restore stream holds shared; exclusive blocked.
func TestM4_RestoreLeaseBlocksPrune(t *testing.T) {
	env := startDataEnv(t)
	ctx := context.Background()

	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "f.txt"), []byte("lease-test"), 0o644)
	stateDir, meta, creds, machineID := enrollRealAgent(t, env, "m4-lease")
	_, stop := startRealAgent(t, env, stateDir, meta, creds)
	defer stop()
	waitOnline(t, env.DB, machineID, 5*time.Second)

	p, _ := json.Marshal(map[string]string{"source": src})
	bj, _ := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeFileBackup, Initiator: "m4", ParamsJSON: string(p),
	})
	waitJobState(t, env.Engine, bj, catalog.JobStateSuccess, 60*time.Second)
	snaps, _ := env.DB.ListSnapshotsByMachine(ctx, machineID, 1)
	root := snaps[0].RootObjectID

	// Stop real agent so we can hold a restore job without it completing.
	stop()
	time.Sleep(100 * time.Millisecond)

	conn, err := grpc.NewClient(env.Addr,
		grpc.WithTransportCredentials(credentials.NewTLS(bwagent.ClientTLSConfig(creds, meta.ServerFP))),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16<<20)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	hold := openChannel(t, conn, machineID)
	hold.skipResult = true
	hold.heartbeat()
	waitOnline(t, env.DB, machineID, 5*time.Second)

	rparams, _ := json.Marshal(map[string]string{
		"source_snapshot_id": snaps[0].ID,
		"target_path":        t.TempDir(),
		"conflict_policy":    "overwrite",
	})
	rj, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeRestore, Initiator: "m4",
		ParamsJSON: string(rparams),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitJobState(t, env.Engine, rj, catalog.JobStateRunning, 5*time.Second)
	if !env.Engine.HasLease(rj) {
		t.Fatal("restore job must hold shared lease")
	}

	// Also open a GetObject stream (stream lease / revalidation path).
	restoreCl := breakwaterv1.NewRestoreServiceClient(conn)
	stream, err := restoreCl.GetObject(ctx, &breakwaterv1.GetObjectRequest{ObjectId: root})
	if err != nil {
		t.Fatal(err)
	}
	// Read one chunk then leave stream open while we probe exclusive.
	_, _ = stream.Recv()

	// Exclusive must not acquire while shared held.
	if l, ok := env.Engine.Locks.TryAcquire(machineID, scheduler.Exclusive, "prune-probe"); ok {
		l.Release()
		t.Fatal("prune exclusive must be blocked while restore holds shared")
	}
	shared, ex := env.Engine.Locks.Held(machineID)
	if shared < 1 || ex != 0 {
		t.Fatalf("held shared=%d exclusive=%d", shared, ex)
	}
	t.Logf("prune blocked: shared=%d exclusive=%d job=%s", shared, ex, rj)
}

// TestM4_ConflictPoliciesViaEngine exercises overwrite/rename/skip through pkg/restore unit path
// is in pkg/restore; this checks agent restore rename mode end-to-end lightly.
func TestM4_ConflictRenameAgent(t *testing.T) {
	env := startDataEnv(t)
	ctx := context.Background()
	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "f.txt"), []byte("new"), 0o644)
	stateDir, meta, creds, machineID := enrollRealAgent(t, env, "m4-conflict")
	_, stop := startRealAgent(t, env, stateDir, meta, creds)
	defer stop()
	waitOnline(t, env.DB, machineID, 5*time.Second)

	p, _ := json.Marshal(map[string]string{"source": src})
	bj, _ := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeFileBackup, Initiator: "m4", ParamsJSON: string(p),
	})
	waitJobState(t, env.Engine, bj, catalog.JobStateSuccess, 60*time.Second)
	snaps, _ := env.DB.ListSnapshotsByMachine(ctx, machineID, 1)

	dest := t.TempDir()
	_ = os.WriteFile(filepath.Join(dest, "f.txt"), []byte("old"), 0o644)
	rparams, _ := json.Marshal(map[string]string{
		"source_snapshot_id": snaps[0].ID,
		"target_path":        dest,
		"conflict_policy":    "rename",
	})
	rj, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeRestore, Initiator: "m4",
		ParamsJSON: string(rparams),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitJobState(t, env.Engine, rj, catalog.JobStateSuccess, 60*time.Second)
	old, _ := os.ReadFile(filepath.Join(dest, "f.txt"))
	if string(old) != "old" {
		t.Fatalf("original should remain: %q", old)
	}
	renamed, err := os.ReadFile(filepath.Join(dest, "f.txt.restored"))
	if err != nil {
		t.Fatal(err)
	}
	if string(renamed) != "new" {
		t.Fatalf("renamed: %q", renamed)
	}
}

// TestM4_ServerLossDrill: wipe catalog snapshot index, rescan, list + restore.
func TestM4_ServerLossDrill(t *testing.T) {
	env := startDataEnv(t)
	ctx := context.Background()

	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "drill.txt"), []byte("server-loss-ok"), 0o644)
	stateDir, meta, creds, machineID := enrollRealAgent(t, env, "m4-drill")
	_, stop := startRealAgent(t, env, stateDir, meta, creds)
	defer stop()
	waitOnline(t, env.DB, machineID, 5*time.Second)

	p, _ := json.Marshal(map[string]string{"source": src})
	bj, _ := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeFileBackup, Initiator: "m4", ParamsJSON: string(p),
	})
	waitJobState(t, env.Engine, bj, catalog.JobStateSuccess, 60*time.Second)
	before, _ := env.DB.ListSnapshotsByMachine(ctx, machineID, 10)
	if len(before) < 1 {
		t.Fatal("need snapshot before wipe")
	}
	rootBefore := before[0].RootObjectID

	// Wipe rebuildable index only (machines + keystore retained — recovery kit story).
	if err := env.DB.DeleteAllSnapshots(ctx); err != nil {
		t.Fatal(err)
	}
	afterWipe, _ := env.DB.ListSnapshotsByMachine(ctx, machineID, 10)
	if len(afterWipe) != 0 {
		t.Fatalf("wipe incomplete: %d", len(afterWipe))
	}

	res, err := rescan.Run(ctx, rescan.Options{
		DB: env.DB, Keystore: env.Keystore, Vaults: env.Vaults,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.SnapshotsAdded < 1 {
		t.Fatalf("rescan added=%d found=%d errs=%v", res.SnapshotsAdded, res.SnapshotsFound, res.Errors)
	}
	rebuilt, err := env.DB.ListSnapshotsByMachine(ctx, machineID, 10)
	if err != nil || len(rebuilt) < 1 {
		t.Fatalf("after rescan: %v len=%d", err, len(rebuilt))
	}
	if rebuilt[0].RootObjectID != rootBefore {
		t.Fatalf("root after rescan %s want %s", rebuilt[0].RootObjectID, rootBefore)
	}

	// Restore still works via RestoreService.
	conn, err := grpc.NewClient(env.Addr,
		grpc.WithTransportCredentials(credentials.NewTLS(bwagent.ClientTLSConfig(creds, meta.ServerFP))),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	dest := t.TempDir()
	stats, err := restore.Run(ctx, restore.Options{
		RootObjectID: rebuilt[0].RootObjectID,
		TargetRoot:   dest,
		Conflict:     restore.ConflictOverwrite,
		Reader:       &restore.GRPCReader{Client: breakwaterv1.NewRestoreServiceClient(conn)},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "drill.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "server-loss-ok" {
		t.Fatalf("got %q", got)
	}
	t.Logf("server-loss drill OK: rescan added=%d restore files=%d", res.SnapshotsAdded, stats.Files)
}

func readAllStream(stream breakwaterv1.RestoreService_GetObjectClient) ([]byte, error) {
	var out []byte
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, resp.GetData()...)
	}
}

// silence unused import if vault not needed in some builds
var _ = vault.ObjectID("")
