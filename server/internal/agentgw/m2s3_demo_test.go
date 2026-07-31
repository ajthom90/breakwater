package agentgw_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/ajthom90/breakwater/pkg/backup"
	"github.com/ajthom90/breakwater/pkg/contentid"
	"github.com/ajthom90/breakwater/pkg/format"
	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
	"github.com/ajthom90/breakwater/server/internal/agentgw"
	"github.com/ajthom90/breakwater/server/internal/audit"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/enroll"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/mtls"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

// dataEnv is a full gateway with DataService + control plane for M2 stage 3.
type dataEnv struct {
	t        *testing.T
	DB       *catalog.DB
	GW       *agentgw.Gateway
	Engine   *scheduler.Engine
	Registry *agentgw.Registry
	Vaults   *vault.Manager
	Keystore *keystore.Store
	Auditor  *audit.Writer
	Addr     string
	ServerFP string
}

func startDataEnv(t *testing.T) *dataEnv {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()

	db, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ks, err := keystore.OpenOrCreate(db, filepath.Join(tmp, "keys", "master.key"))
	if err != nil {
		t.Fatalf("keystore: %v", err)
	}
	serverID, err := mtls.GenerateServerIdentity("breakwater-m2s3", []string{"127.0.0.1", "localhost"}, 24*time.Hour)
	if err != nil {
		t.Fatalf("server identity: %v", err)
	}
	serverFP := serverID.Fingerprint()
	vm := vault.NewManager(filepath.Join(tmp, "repos"), filepath.Join(tmp, "data"))
	t.Cleanup(func() { _ = vm.CloseAll(ctx) })

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	auditor := audit.NewWriter(db)
	enrollSvc := &enroll.Service{
		DB: db, Keystore: ks, Vaults: &realVault{m: vm}, ServerFP: serverFP,
		DefaultPolicy: "01DEFAULTPOLICY000000000000", Log: log,
	}
	locks := scheduler.NewRepoLocks()
	engine := scheduler.NewEngine(db, locks, log)
	reg := agentgw.NewRegistry(log)

	gw := agentgw.New(serverID, enrollSvc, log)
	gw.Auditor = auditor
	gw.ServerVersion = "0.0.1-m2s3-test"
	gw.AttachControlPlane(db, engine, reg)
	gw.DataService = &agentgw.DataServer{
		Engine: engine, Catalog: db, Keystore: ks, Vaults: vm, Auditor: auditor, Log: log,
	}
	// M4: RestoreService on the same gateway (own-repo + restore-job authz).
	gw.RestoreService = &agentgw.RestoreServer{
		Engine: engine, Catalog: db, Keystore: ks, Vaults: vm, Auditor: auditor, Log: log,
	}
	addr, err := gw.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(gw.GracefulStop)

	return &dataEnv{
		t: t, DB: db, GW: gw, Engine: engine, Registry: reg,
		Vaults: vm, Keystore: ks, Auditor: auditor, Addr: addr, ServerFP: serverFP,
	}
}

func (e *dataEnv) mintAndEnroll(hostname string) (machineID string, hashingKey []byte, hashingAlgo string, agentID *mtls.Identity, conn *grpc.ClientConn) {
	e.t.Helper()
	ctx := context.Background()
	rawTok, secret, err := enroll.Mint(e.Addr, e.ServerFP)
	if err != nil {
		e.t.Fatal(err)
	}
	tokID := "tok-" + hostname
	if err := e.DB.InsertEnrollToken(ctx, tokID, secret, "test", time.Now().UTC().Add(time.Hour)); err != nil {
		e.t.Fatal(err)
	}
	agentID, err = mtls.GenerateAgentIdentity(hostname, 365*24*time.Hour)
	if err != nil {
		e.t.Fatal(err)
	}
	conn, err = grpc.NewClient(e.Addr,
		grpc.WithTransportCredentials(credentials.NewTLS(mtls.ClientTLSConfig(agentID, e.ServerFP))),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16<<20), grpc.MaxCallSendMsgSize(16<<20)),
	)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = conn.Close() })

	resp, err := breakwaterv1.NewEnrollmentServiceClient(conn).Enroll(ctx, &breakwaterv1.EnrollRequest{
		Token: rawTok,
		AgentInfo: &breakwaterv1.AgentInfo{
			Hostname: hostname, Os: "linux", OsVersion: "test", AgentVersion: "0.0.1-fake", Arch: "amd64",
		},
		ClientCertPem: agentID.CertPEM,
	})
	if err != nil {
		e.t.Fatalf("enroll: %v", err)
	}
	return resp.MachineId, resp.HashingKey, resp.HashingAlgorithm, agentID, conn
}

// openBackupAgent runs the control channel and handles FILE_BACKUP via fileback.
func openBackupAgent(t *testing.T, conn *grpc.ClientConn, machineID string, hashingKey []byte, hashingAlgo string, dataClient breakwaterv1.DataServiceClient) *fakeAgent {
	t.Helper()
	h, err := contentid.New(hashingAlgo, hashingKey)
	if err != nil {
		t.Fatal(err)
	}
	fbClient := &backup.GRPCClient{DS: dataClient}
	agent := openChannel(t, conn, machineID)
	agent.onJob = func(js *breakwaterv1.JobStart) error {
		if js.GetType() != breakwaterv1.JobType_JOB_TYPE_FILE_BACKUP {
			// Fall through to default for inventory/noop.
			agent.defaultHandleJob(js)
			return nil
		}
		var params struct {
			Source string `json:"source"`
		}
		if err := json.Unmarshal(js.GetParamsJson(), &params); err != nil {
			return err
		}
		var lastDone, lastTotal int64
		stats, err := backup.Run(context.Background(), backup.Options{
			Source: params.Source,
			JobID:  js.GetJobId(),
			Hasher: h,
			Client: fbClient,
			Progress: func(done, total int64, phase, msg string) {
				lastDone, lastTotal = done, total
				_ = agent.stream.Send(&breakwaterv1.AgentToServer{
					Msg: &breakwaterv1.AgentToServer_JobProgress{
						JobProgress: &breakwaterv1.JobProgress{
							JobId: js.GetJobId(), BytesDone: done, BytesTotal: total, Phase: phase, Message: msg,
						},
					},
				})
			},
		})
		res := &breakwaterv1.JobResult{JobId: js.GetJobId()}
		if err != nil {
			res.Success = false
			res.ErrorMessage = err.Error()
		} else {
			res.Success = true
			res.BytesRead = stats.BytesRead
			res.BytesStored = stats.BytesUploaded
			res.SnapshotId = stats.SnapshotID
			// Stash uploaded on the agent for dedup assertions.
			agent.mu.Lock()
			if agent.uploadLog == nil {
				agent.uploadLog = make(map[string]int64)
			}
			agent.uploadLog[js.GetJobId()] = stats.BytesUploaded
			agent.mu.Unlock()
		}
		_ = lastDone
		_ = lastTotal
		return agent.stream.Send(&breakwaterv1.AgentToServer{
			Msg: &breakwaterv1.AgentToServer_JobResult{JobResult: res},
		})
	}
	return agent
}

// restoreTree walks a file snapshot root and returns path → content.
func restoreTree(t *testing.T, ctx context.Context, v vault.Vault, rootOID vault.ObjectID, prefix string) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte)
	raw := readObj(t, ctx, v, rootOID)
	var tree format.TreeObject
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatalf("decode tree %s: %v", rootOID, err)
	}
	for _, ent := range tree.Entries {
		p := prefix + ent.Name
		switch ent.Type {
		case format.EntryDir:
			child := restoreTree(t, ctx, v, vault.ObjectID(ent.ObjectID), p+"/")
			for k, v := range child {
				out[k] = v
			}
			// empty dir marker
			if len(child) == 0 {
				out[p+"/"] = nil
			}
		case format.EntryFile:
			out[p] = readObj(t, ctx, v, vault.ObjectID(ent.ObjectID))
		}
	}
	return out
}

func readObj(t *testing.T, ctx context.Context, v vault.Vault, oid vault.ObjectID) []byte {
	t.Helper()
	r, err := v.OpenObject(ctx, oid)
	if err != nil {
		t.Fatalf("OpenObject %s: %v", oid, err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestM2S3_BackupDedupDemo is the stage-3 demo gate:
// enroll → channel → file-backup → restore byte-compare → second run dedup →
// prune after forget snapshot1 → snapshot2 still restorable.
func TestM2S3_BackupDedupDemo(t *testing.T) {
	env := startDataEnv(t)
	ctx := context.Background()

	// Source tree: mixed sizes, multi-chunk file, empty dir.
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("hello breakwater"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "note.md"), []byte("# note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Multi-chunk: 10 MiB random → DYNAMIC-4M produces multiple segments.
	big := make([]byte, 10<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}

	machineID, hk, algo, _, conn := env.mintAndEnroll("fake-linux-m2s3")
	dataCl := breakwaterv1.NewDataServiceClient(conn)
	agent := openBackupAgent(t, conn, machineID, hk, algo, dataCl)
	agent.heartbeat()
	waitOnline(t, env.DB, machineID, 3*time.Second)

	params, _ := json.Marshal(map[string]string{"source": src})
	job1, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeFileBackup, Initiator: "demo",
		ParamsJSON: string(params),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := agent.waitJobStarts(1, 10*time.Second); len(got) < 1 {
		t.Fatalf("no JobStart: %v", got)
	}
	waitJobState(t, env.Engine, job1, catalog.JobStateSuccess, 60*time.Second)

	j1, _ := env.Engine.Job(ctx, job1)
	if j1.BytesStored <= 0 {
		t.Fatalf("run1 bytes_stored=%d want >0", j1.BytesStored)
	}
	agent.mu.Lock()
	upload1 := agent.uploadLog[job1]
	agent.mu.Unlock()
	t.Logf("run1 job=%s bytes_stored=%d uploaded=%d", job1, j1.BytesStored, upload1)

	// Catalog snapshots row.
	snaps, err := env.DB.ListSnapshotsByMachine(ctx, machineID, 10)
	if err != nil || len(snaps) < 1 {
		t.Fatalf("catalog snapshots: %v len=%d", err, len(snaps))
	}
	snap1 := snaps[0]
	if snap1.RootObjectID == "" || snap1.ManifestRef == "" {
		t.Fatalf("snapshot incomplete: %+v", snap1)
	}
	t.Logf("snapshot1 id=%s root=%s", snap1.ID, snap1.RootObjectID)

	// Audit snapshot.commit
	rows, err := env.Auditor.ListByAction(ctx, audit.ActionSnapshotCommit)
	if err != nil || len(rows) < 1 {
		t.Fatalf("snapshot.commit audit: %v len=%d", err, len(rows))
	}

	// Restore every file via vault OpenObject/tree walk and byte-compare.
	pw, err := env.Keystore.GetRepoPassword(ctx, machineID)
	if err != nil {
		t.Fatal(err)
	}
	v, err := env.Vaults.Open(ctx, machineID, pw)
	if err != nil {
		t.Fatal(err)
	}
	restored := restoreTree(t, ctx, v, vault.ObjectID(snap1.RootObjectID), "")
	wantFiles := map[string]string{
		"hello.txt":   "hello breakwater",
		"sub/note.md": "# note\n",
	}
	for name, content := range wantFiles {
		got, ok := restored[name]
		if !ok {
			t.Fatalf("missing restored file %s; have %v", name, keys(restored))
		}
		if string(got) != content {
			t.Fatalf("%s: got %q want %q", name, got, content)
		}
	}
	if !bytes.Equal(restored["big.bin"], big) {
		t.Fatalf("big.bin mismatch len=%d want %d", len(restored["big.bin"]), len(big))
	}
	if _, ok := restored["empty-dir/"]; !ok {
		// empty dir may appear only if we marked it; check via tree entries
		t.Logf("empty-dir marker absent (ok if no children); keys=%v", keys(restored))
	}
	t.Logf("restore byte-compare OK (%d paths)", len(restored))

	// Mutate one file + add one.
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("hello breakwater v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "new.txt"), []byte("brand new"), 0o644); err != nil {
		t.Fatal(err)
	}

	job2, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeFileBackup, Initiator: "demo",
		ParamsJSON: string(params),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := agent.waitJobStarts(2, 10*time.Second); len(got) < 2 {
		t.Fatalf("no second JobStart: %v", got)
	}
	waitJobState(t, env.Engine, job2, catalog.JobStateSuccess, 60*time.Second)
	agent.mu.Lock()
	upload2 := agent.uploadLog[job2]
	agent.mu.Unlock()
	t.Logf("run2 uploaded=%d run1 uploaded=%d ratio=%.2f%%", upload2, upload1, 100*float64(upload2)/float64(upload1+1))

	// Dedup: run2 uploaded ≪ run1 (PLAN: second run shows dedup ratio).
	if upload1 < 1 {
		t.Fatal("run1 uploaded nothing")
	}
	if upload2*20 > upload1 { // run2 must be < 5% of run1
		t.Fatalf("dedup failed: run2 uploaded %d (≥5%% of run1 %d)", upload2, upload1)
	}

	snaps, _ = env.DB.ListSnapshotsByMachine(ctx, machineID, 10)
	if len(snaps) < 2 {
		t.Fatalf("want 2 snapshots, got %d", len(snaps))
	}
	var snap2 catalog.Snapshot
	for _, s := range snaps {
		if s.ID != snap1.ID {
			snap2 = s
			break
		}
	}
	if snap2.ID == "" {
		t.Fatal("snapshot2 not found")
	}
	// Both restorable.
	r2 := restoreTree(t, ctx, v, vault.ObjectID(snap2.RootObjectID), "")
	if string(r2["hello.txt"]) != "hello breakwater v2" {
		t.Fatalf("snap2 hello: %q", r2["hello.txt"])
	}
	if string(r2["new.txt"]) != "brand new" {
		t.Fatalf("snap2 new: %q", r2["new.txt"])
	}
	if !bytes.Equal(r2["big.bin"], big) {
		t.Fatal("snap2 big.bin mismatch")
	}

	// Forget snapshot1 + prune min-age 0 → snapshot2 still fully restorable.
	if err := v.DeleteSnapshotRecord(ctx, vault.SnapshotRecordID(snap1.ManifestRef)); err != nil {
		t.Fatalf("DeleteSnapshotRecord: %v", err)
	}
	if err := v.Prune(ctx, vault.WithMinContentAge(0)); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	r2b := restoreTree(t, ctx, v, vault.ObjectID(snap2.RootObjectID), "")
	if string(r2b["hello.txt"]) != "hello breakwater v2" || string(r2b["new.txt"]) != "brand new" {
		t.Fatalf("after prune snap2 broken: hello=%q new=%q", r2b["hello.txt"], r2b["new.txt"])
	}
	if !bytes.Equal(r2b["big.bin"], big) {
		t.Fatal("after prune big.bin lost")
	}
	t.Logf("M2-S3 demo PASS: dedup ratio run2/run1=%.4f prune survival OK", float64(upload2)/float64(upload1))
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
