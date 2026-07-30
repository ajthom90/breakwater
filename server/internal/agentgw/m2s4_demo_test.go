package agentgw_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	bwagent "github.com/ajthom90/breakwater/agent"
	"github.com/ajthom90/breakwater/pkg/format"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/enroll"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/vault"
	"github.com/ajthom90/breakwater/tools/golden"
)

// TestM2S4_GoldenRoundTrip is the stage-4 portable demo gate:
//
//	golden.Generate → real agent enroll + control loop → FILE_BACKUP →
//	restore from vault → golden.Compare (portable subset).
//
// Windows-only fixtures are skip-with-record on this darwin/linux host.
func TestM2S4_GoldenRoundTrip(t *testing.T) {
	env := startDataEnv(t)
	ctx := context.Background()

	src := t.TempDir()
	gen, err := golden.Generate(golden.Options{Root: src})
	if err != nil {
		t.Fatal(err)
	}
	if len(gen.Created) < 4 {
		t.Fatalf("golden created too few fixtures: %v", gen.Created)
	}
	for _, s := range gen.Skipped {
		if s.Reason == "" {
			t.Fatalf("silent skip: %+v", s)
		}
		t.Logf("golden skip: %s — %s", s.Fixture, s.Reason)
	}
	t.Logf("golden created=%v", gen.Created)

	stateDir, meta, creds, machineID := enrollRealAgent(t, env, "m2s4-golden")
	_, stop := startRealAgent(t, env, stateDir, meta, creds)
	defer stop()
	waitOnline(t, env.DB, machineID, 5*time.Second)

	params, _ := json.Marshal(map[string]string{"source": src})
	jobID, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID:  machineID,
		Type:       scheduler.TypeFileBackup,
		Initiator:  "m2s4-demo",
		ParamsJSON: string(params),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitJobState(t, env.Engine, jobID, catalog.JobStateSuccess, 120*time.Second)
	j, _ := env.Engine.Job(ctx, jobID)
	t.Logf("backup job=%s bytes_stored=%d", jobID, j.BytesStored)
	if j.BytesStored <= 0 {
		t.Fatalf("bytes_stored=%d want >0", j.BytesStored)
	}

	snaps, err := env.DB.ListSnapshotsByMachine(ctx, machineID, 5)
	if err != nil || len(snaps) < 1 {
		t.Fatalf("catalog snapshots: %v len=%d", err, len(snaps))
	}
	snap := snaps[0]
	pw, err := env.Keystore.GetRepoPassword(ctx, machineID)
	if err != nil {
		t.Fatal(err)
	}
	v, err := env.Vaults.Open(ctx, machineID, pw)
	if err != nil {
		t.Fatal(err)
	}
	restored := t.TempDir()
	if err := restoreTreeToDir(t, ctx, v, vault.ObjectID(snap.RootObjectID), restored); err != nil {
		t.Fatal(err)
	}

	cmp, err := golden.Compare(src, restored, golden.CompareOptions{
		CompareTimestamps: false,
		CompareACL:        true,
		CompareADS:        true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range cmp.SkippedChecks {
		t.Logf("compare skip: %s — %s", s.Fixture, s.Reason)
	}
	if !cmp.Equal() {
		t.Fatalf("golden compare failed (%d diffs): %+v", len(cmp.Diffs), cmp.Diffs)
	}
	t.Logf("M2S4 golden round-trip OK: matched=%d created=%d skipped_gen=%d skipped_cmp=%d",
		cmp.MatchedFiles, len(gen.Created), len(gen.Skipped), len(cmp.SkippedChecks))
}

// TestM2S4_AgentCancelConfirmation runs the real agent against the gateway and
// asserts Cancel produces a terminal JobResult (cancel-confirmation contract).
func TestM2S4_AgentCancelConfirmation(t *testing.T) {
	env := startDataEnv(t)
	ctx := context.Background()

	src := t.TempDir()
	big := make([]byte, 10<<20)
	for i := range big {
		big[i] = byte(i)
	}
	if err := os.WriteFile(filepath.Join(src, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		_ = os.WriteFile(filepath.Join(src, "f"+itoaS4(i)+".txt"), []byte("x"), 0o644)
	}

	stateDir, meta, creds, machineID := enrollRealAgent(t, env, "m2s4-cancel")
	_, stop := startRealAgent(t, env, stateDir, meta, creds)
	defer stop()
	waitOnline(t, env.DB, machineID, 5*time.Second)

	params, _ := json.Marshal(map[string]string{"source": src})
	jobID, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeFileBackup, Initiator: "cancel-test",
		ParamsJSON: string(params),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitJobState(t, env.Engine, jobID, catalog.JobStateRunning, 10*time.Second)
	if err := env.Engine.Cancel(ctx, jobID, "m2s4-test"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		j, err := env.Engine.Job(ctx, jobID)
		if err != nil {
			t.Fatal(err)
		}
		switch j.State {
		case catalog.JobStateCancelled, catalog.JobStateFailed, catalog.JobStateSuccess:
			t.Logf("cancel confirmation: job state=%s err=%q", j.State, j.ErrorMessage)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	j, _ := env.Engine.Job(ctx, jobID)
	t.Fatalf("job not terminal after cancel: state=%s", j.State)
}

// TestM2S4_ReconnectIdempotency verifies completed job_ids survive reconnect.
func TestM2S4_ReconnectIdempotency(t *testing.T) {
	env := startDataEnv(t)
	ctx := context.Background()

	stateDir, meta, creds, machineID := enrollRealAgent(t, env, "m2s4-reconnect")
	_, stop := startRealAgent(t, env, stateDir, meta, creds)
	waitOnline(t, env.DB, machineID, 5*time.Second)

	jobID, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeNoop, Initiator: "reconnect-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitJobState(t, env.Engine, jobID, catalog.JobStateSuccess, 10*time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !stateDir.HasCompleted(jobID) {
		time.Sleep(20 * time.Millisecond)
	}
	if !stateDir.HasCompleted(jobID) {
		t.Fatal("agent did not record completed job_id")
	}

	stop()
	time.Sleep(100 * time.Millisecond)
	_, stop2 := startRealAgent(t, env, stateDir, meta, creds)
	defer stop2()
	waitOnline(t, env.DB, machineID, 5*time.Second)

	invID, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeInventory, Initiator: "reconnect-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitJobState(t, env.Engine, invID, catalog.JobStateSuccess, 10*time.Second)
	if !stateDir.HasCompleted(jobID) {
		t.Fatal("completed set lost across reconnect")
	}
	t.Logf("reconnect OK; prior job_id=%s still completed; inventory=%s success", jobID, invID)
}

func enrollRealAgent(t *testing.T, env *dataEnv, hostname string) (*bwagent.StateDir, *bwagent.StateIdentity, *bwagent.Creds, string) {
	t.Helper()
	ctx := context.Background()
	stateDir, err := bwagent.OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rawTok, secret, err := enroll.Mint(env.Addr, env.ServerFP)
	if err != nil {
		t.Fatal(err)
	}
	tokID := "tok-" + hostname
	if err := env.DB.InsertEnrollToken(ctx, tokID, secret, "test", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	res, err := bwagent.Enroll(ctx, bwagent.EnrollOptions{
		Token: rawTok, StateDir: stateDir, Hostname: hostname, Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	meta, creds, err := stateDir.LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return stateDir, meta, creds, res.MachineID
}

func startRealAgent(t *testing.T, env *dataEnv, stateDir *bwagent.StateDir, meta *bwagent.StateIdentity, creds *bwagent.Creds) (*bwagent.ControlAgent, func()) {
	t.Helper()
	dial := func(ctx context.Context) (*grpc.ClientConn, error) {
		return grpc.NewClient(env.Addr,
			grpc.WithTransportCredentials(credentials.NewTLS(bwagent.ClientTLSConfig(creds, meta.ServerFP))),
			grpc.WithKeepaliveParams(bwagent.ClientParameters),
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16<<20), grpc.MaxCallSendMsgSize(16<<20)),
		)
	}
	ag := bwagent.NewControl(bwagent.ControlConfig{
		State: stateDir, Meta: meta, Creds: creds, Version: "test", Dial: dial,
		HeartbeatInterval: 2 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- ag.Run(ctx) }()
	stop := func() {
		ag.Stop()
		cancel()
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
		}
	}
	t.Cleanup(stop)
	return ag, stop
}

func restoreTreeToDir(t *testing.T, ctx context.Context, v vault.Vault, rootOID vault.ObjectID, dest string) error {
	t.Helper()
	return restoreTreeToDirRec(t, ctx, v, rootOID, dest)
}

func restoreTreeToDirRec(t *testing.T, ctx context.Context, v vault.Vault, oid vault.ObjectID, dir string) error {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw := readObj(t, ctx, v, oid)
	var tree format.TreeObject
	if err := json.Unmarshal(raw, &tree); err != nil {
		return err
	}
	for _, ent := range tree.Entries {
		p := filepath.Join(dir, ent.Name)
		switch ent.Type {
		case format.EntryDir:
			if err := restoreTreeToDirRec(t, ctx, v, vault.ObjectID(ent.ObjectID), p); err != nil {
				return err
			}
		case format.EntryFile:
			data := readObj(t, ctx, v, vault.ObjectID(ent.ObjectID))
			if err := os.WriteFile(p, data, 0o644); err != nil {
				return err
			}
		case format.EntrySymlink:
			if err := os.Symlink(ent.ReparseData, p); err != nil {
				return err
			}
		default:
			t.Logf("restore: skip entry type %s name=%s", ent.Type, ent.Name)
		}
	}
	return nil
}

func itoaS4(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}
