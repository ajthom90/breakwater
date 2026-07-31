package agentgw_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	bwagent "github.com/ajthom90/breakwater/agent"
	"github.com/ajthom90/breakwater/pkg/format"
	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

// TestM4F2_ReachCacheEvictedOnTerminal: after a restore job completes, the
// RestoreServer must not retain a reachability cache entry for that job.
func TestM4F2_ReachCacheEvictedOnTerminal(t *testing.T) {
	env := startDataEnv(t)
	ctx := context.Background()
	if env.Restore == nil {
		t.Fatal("dataEnv.Restore not wired")
	}

	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "f.txt"), []byte("cache-evict"), 0o644)

	stateDir, meta, creds, machineID := enrollRealAgent(t, env, "m4f2-cache")
	_, stop := startRealAgent(t, env, stateDir, meta, creds)
	defer stop()
	waitOnline(t, env.DB, machineID, 5*time.Second)

	p, _ := json.Marshal(map[string]string{"source": src})
	bj, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeFileBackup, Initiator: "m4f2",
		ParamsJSON: string(p),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitJobState(t, env.Engine, bj, catalog.JobStateSuccess, 60*time.Second)
	snaps, _ := env.DB.ListSnapshotsByMachine(ctx, machineID, 1)
	snap := snaps[0]

	// Hold a restore job open, populate reach cache via GetObject, then complete.
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
		"source_snapshot_id": snap.ID,
		"target_path":        t.TempDir(),
		"conflict_policy":    "overwrite",
	})
	rj, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeRestore, Initiator: "m4f2",
		ParamsJSON: string(rparams),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitJobState(t, env.Engine, rj, catalog.JobStateRunning, 5*time.Second)

	// Cross-machine style own-job: GetObject of root populates reach cache
	// when authorizeObject walks active restore jobs.
	restoreCl := breakwaterv1.NewRestoreServiceClient(conn)
	stream, err := restoreCl.GetObject(ctx, &breakwaterv1.GetObjectRequest{ObjectId: snap.RootObjectID})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = stream.Recv()

	if !env.Restore.ReachCacheHas(rj) {
		// Own-repo GetObject may use stream lease without building job reach set.
		// Force cross-path: authorize via job by requesting through a second machine.
		// Simpler: call GetSnapshot isn't enough. For own-repo restore job, reach
		// cache is populated when authorizeObject matches a job with the object
		// in the reachable set — which requires building the set.
		// Re-check after GetObject of a content path... For same-machine restore,
		// authorizeObject prefers jobs that include the object — that builds cache.
		// If still empty, the object was served via own-repo stream path first
		// because job matching continues only when object is in set — chicken/egg:
		// looking at authorizeObject: it builds reach for each job and checks
		// membership. So GetObject of snap root SHOULD populate cache.
		t.Log("reach cache empty after GetObject — retry with explicit cross job shape")
	}

	// If still not cached (own-repo short-circuit), populate by using a second
	// enrolled machine as target with source_machine_id set (forces cross path).
	if !env.Restore.ReachCacheHas(rj) {
		// Fail the held job and run a proper cross-machine restore that builds cache.
		_ = env.Engine.Cancel(ctx, rj, "reprobe")
		// Own-repo path: open vault authorize may skip job set if object found
		// in own vault first after failed job match... Looking at code again:
		// for each job it builds reach and checks object. Building reach caches it.
		// So if job is running and object is root of snap, it should cache.
		// Unless GetObject used stream path without jobs matching —
		// jobMatchesSnapshot requires source_snapshot_id in params. We set it.
		// SourceRepoFromParams empty → source = j.MachineID = mid, matches snap.
		// So it should work. Debug:
		t.Fatalf("expected reach cache populated for running restore job %s after GetObject of root", rj)
	}

	// Complete the job: send JobResult via fake agent default... skipResult means
	// no result. Force-fail via disconnect.
	// Cancel with vault type goes cancelling; force complete via Engine fail path.
	// Easiest: Cancel then wait for timeout is slow. Use HandleResult if exported.
	// failJob is private. OnAgentDisconnect fails running jobs.
	env.Engine.OnAgentDisconnect(ctx, machineID)
	// After disconnect, lease released → OnJobTerminal → EvictReachCache.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !env.Restore.ReachCacheHas(rj) {
			t.Logf("M4-F2: reach cache evicted for job %s after terminal", rj)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if env.Restore.ReachCacheHas(rj) {
		t.Fatalf("reach cache still holds entry for terminal job %s", rj)
	}
}

// TestM4F1_DeepTreeRestoreReachability: tree deeper than old 256 limit restores
// via RestoreService (reachability walk must share the raised bound).
func TestM4F1_DeepTreeRestoreReachability(t *testing.T) {
	const depth = 300
	env := startDataEnv(t)
	ctx := context.Background()

	// Build deep tree directly in vault (faster than agent backup of 300 dirs).
	stateDir, meta, creds, machineID := enrollRealAgent(t, env, "m4f1-deep")
	// Need machine enrolled for keystore; open vault and plant snapshot.
	pw, err := env.Keystore.GetRepoPassword(ctx, machineID)
	if err != nil {
		t.Fatal(err)
	}
	v, err := env.Vaults.Open(ctx, machineID, pw)
	if err != nil {
		t.Fatal(err)
	}

	fileOID, err := v.WriteObject(ctx, vault.SplitterFixed4M, bytes.NewReader([]byte("deep-ok\n")))
	if err != nil {
		t.Fatal(err)
	}
	childOID := fileOID
	childIsFile := true
	for i := 0; i < depth; i++ {
		var entries []format.TreeEntry
		if childIsFile {
			entries = []format.TreeEntry{{Name: "leaf.txt", Type: format.EntryFile, Size: 8, ObjectID: string(childOID)}}
			childIsFile = false
		} else {
			entries = []format.TreeEntry{{Name: fmt.Sprintf("d%d", i), Type: format.EntryDir, ObjectID: string(childOID)}}
		}
		raw, _ := json.Marshal(format.TreeObject{Version: format.FormatVersion, Entries: entries})
		oid, err := v.WriteObject(ctx, vault.SplitterFixed4M, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		childOID = oid
	}
	rootOID := childOID
	manID, err := v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind: vault.KindFileSnapshot, MachineID: machineID,
		Timestamp: time.Now().UTC(), RootObjectID: rootOID, Source: "/deep",
	})
	if err != nil {
		t.Fatal(err)
	}
	snapID := "deep-snap-" + machineID[:8]
	if err := env.DB.InsertSnapshot(ctx, catalog.Snapshot{
		ID: snapID, MachineID: machineID, Kind: "file", Source: "/deep",
		ManifestRef: string(manID), RootObjectID: string(rootOID),
	}); err != nil {
		t.Fatal(err)
	}

	// Own-repo GetObject of root must succeed (walk not required for open of root).
	conn, err := grpc.NewClient(env.Addr,
		grpc.WithTransportCredentials(credentials.NewTLS(bwagent.ClientTLSConfig(creds, meta.ServerFP))),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16<<20)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	restoreCl := breakwaterv1.NewRestoreServiceClient(conn)

	// Hold restore job so reachability walk runs on GetObject of root.
	// Start control channel.
	ag, stop := startRealAgent(t, env, stateDir, meta, creds)
	_ = ag
	defer stop()
	waitOnline(t, env.DB, machineID, 5*time.Second)

	// Cross-machine style isn't needed: force reach walk via a held job from B.
	// Simpler: use authorizeObject own-repo path for GetObject (no reach walk).
	// To exercise walkTreeReachable, need a cross-machine job.
	stateB, metaB, credsB, midB := enrollRealAgent(t, env, "m4f1-deep-b")
	stopB := func() {}
	_, stopB = startRealAgent(t, env, stateB, metaB, credsB)
	waitOnline(t, env.DB, midB, 5*time.Second)
	stopB()
	time.Sleep(50 * time.Millisecond)

	connB, err := grpc.NewClient(env.Addr,
		grpc.WithTransportCredentials(credentials.NewTLS(bwagent.ClientTLSConfig(credsB, metaB.ServerFP))),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16<<20)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connB.Close() })
	hold := openChannel(t, connB, midB)
	hold.skipResult = true
	hold.heartbeat()
	waitOnline(t, env.DB, midB, 5*time.Second)

	rparams, _ := json.Marshal(map[string]string{
		"source_snapshot_id": snapID,
		"source_machine_id":  machineID,
		"target_path":        t.TempDir(),
		"conflict_policy":    "overwrite",
	})
	rj, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: midB, Type: scheduler.TypeRestore, Initiator: "m4f1",
		ParamsJSON: string(rparams),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitJobState(t, env.Engine, rj, catalog.JobStateRunning, 5*time.Second)

	restoreB := breakwaterv1.NewRestoreServiceClient(connB)
	stream, err := restoreB.GetObject(ctx, &breakwaterv1.GetObjectRequest{ObjectId: string(rootOID)})
	if err != nil {
		t.Fatalf("GetObject deep root (reach walk depth=%d): %v", depth, err)
	}
	data, err := readAllGetObject(stream)
	if err != nil {
		t.Fatalf("read deep root: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("empty root object")
	}
	if !env.Restore.ReachCacheHas(rj) {
		t.Fatal("expected reach cache after deep walk")
	}
	t.Logf("M4-F1 restore reachability depth=%d root_bytes=%d job=%s", depth, len(data), rj)
	_ = restoreCl
}

func readAllGetObject(stream breakwaterv1.RestoreService_GetObjectClient) ([]byte, error) {
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

// TestM4F1_SharedMaxTreeDepthConstant documents the shared bound.
func TestM4F1_SharedMaxTreeDepthConstant(t *testing.T) {
	if format.MaxTreeDepth != 4096 {
		t.Fatalf("format.MaxTreeDepth=%d want 4096 (M4-F1 decision)", format.MaxTreeDepth)
	}
	if format.MaxTreeDepth <= 256 {
		t.Fatal("MaxTreeDepth must exceed historical prune limit of 256")
	}
}
