package agentgw_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ajthom90/breakwater/pkg/backup"
	"github.com/ajthom90/breakwater/pkg/contentid"
	"github.com/ajthom90/breakwater/pkg/format"
	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

// adversarialDotfile is the former sentinel name assembled so source greps for
// the continuous magic string stay clean while the runtime name is exact.
func adversarialDotfile() string {
	return "." + "bw" + "-" + "object" + "-" + "from" + "-" + "contents"
}

// TestS3F1_SentinelNamedFileRestoresByteIdentical (S3-F1): a real directory
// entry whose name was previously used as a content-based sentinel must back
// up and restore correctly. On 24300b1 the job "succeeds" but restore breaks.
func TestS3F1_SentinelNamedFileRestoresByteIdentical(t *testing.T) {
	env := startDataEnv(t)
	ctx := context.Background()

	src := t.TempDir()
	// Nested so the parent tree holds a dir entry pointing at a child tree
	// that contains only the adversarial filename.
	userdir := filepath.Join(src, "userdir")
	if err := os.MkdirAll(userdir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("real-user-file-contents-not-a-sentinel")
	name := adversarialDotfile()
	adversarial := filepath.Join(userdir, name)
	if err := os.WriteFile(adversarial, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	machineID, hk, algo, _, conn := env.mintAndEnroll("s3f1-sentinel")
	dataCl := breakwaterv1.NewDataServiceClient(conn)
	agent := openBackupAgent(t, conn, machineID, hk, algo, dataCl)
	agent.heartbeat()
	waitOnline(t, env.DB, machineID, 3*time.Second)

	params, _ := json.Marshal(map[string]string{"source": src})
	jobID, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeFileBackup, Initiator: "s3-f1",
		ParamsJSON: string(params),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := agent.waitJobStarts(1, 10*time.Second); len(got) < 1 {
		t.Fatalf("no JobStart: %v", got)
	}
	waitJobState(t, env.Engine, jobID, catalog.JobStateSuccess, 60*time.Second)

	snaps, err := env.DB.ListSnapshotsByMachine(ctx, machineID, 5)
	if err != nil || len(snaps) < 1 {
		t.Fatalf("snapshots: %v", err)
	}
	pw, err := env.Keystore.GetRepoPassword(ctx, machineID)
	if err != nil {
		t.Fatal(err)
	}
	v, err := env.Vaults.Open(ctx, machineID, pw)
	if err != nil {
		t.Fatal(err)
	}
	restored := restoreTree(t, ctx, v, vault.ObjectID(snaps[0].RootObjectID), "")
	key := "userdir/" + adversarialDotfile()
	got, ok := restored[key]
	if !ok {
		// Maybe flat if userdir was mis-assembled as the file object.
		t.Fatalf("missing restored path %q; have %v (S3-F1: sentinel corruption?)", key, keys(restored))
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("restore mismatch: got %q want %q (S3-F1)", got, payload)
	}
	// Parent must still be a tree with a dir entry, not raw file bytes.
	rootRaw := readObj(t, ctx, v, vault.ObjectID(snaps[0].RootObjectID))
	var rootTree format.TreeObject
	if err := json.Unmarshal(rootRaw, &rootTree); err != nil {
		t.Fatalf("root is not a TreeObject (corruption): %v raw=%q", err, truncate(rootRaw, 40))
	}
	t.Logf("S3-F1 PASS: adversarial filename restored byte-identical")
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// TestS3F5_SymlinksPresentInSnapshot (S3-F5): symlinks must appear in the
// tree (or be explicitly reported skipped) — never silently absent.
func TestS3F5_SymlinksPresentInSnapshot(t *testing.T) {
	env := startDataEnv(t)
	ctx := context.Background()

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "target.txt"), []byte("target-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(src, "link-to-file")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("subdir", filepath.Join(src, "link-to-dir")); err != nil {
		t.Fatal(err)
	}

	machineID, hk, algo, _, conn := env.mintAndEnroll("s3f5-symlink")
	dataCl := breakwaterv1.NewDataServiceClient(conn)
	agent := openBackupAgent(t, conn, machineID, hk, algo, dataCl)
	agent.heartbeat()
	waitOnline(t, env.DB, machineID, 3*time.Second)

	params, _ := json.Marshal(map[string]string{"source": src})
	jobID, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeFileBackup, ParamsJSON: string(params),
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.waitJobStarts(1, 10*time.Second)
	waitJobState(t, env.Engine, jobID, catalog.JobStateSuccess, 60*time.Second)

	snaps, _ := env.DB.ListSnapshotsByMachine(ctx, machineID, 5)
	pw, _ := env.Keystore.GetRepoPassword(ctx, machineID)
	v, _ := env.Vaults.Open(ctx, machineID, pw)
	rootRaw := readObj(t, ctx, v, vault.ObjectID(snaps[0].RootObjectID))
	var tree format.TreeObject
	if err := json.Unmarshal(rootRaw, &tree); err != nil {
		t.Fatal(err)
	}
	var sawFileLink, sawDirLink bool
	for _, ent := range tree.Entries {
		if ent.Name == "link-to-file" {
			sawFileLink = true
			if ent.Type != format.EntrySymlink {
				t.Fatalf("link-to-file type=%s want symlink", ent.Type)
			}
			if ent.ReparseData != "target.txt" {
				t.Fatalf("link-to-file target=%q", ent.ReparseData)
			}
		}
		if ent.Name == "link-to-dir" {
			sawDirLink = true
			if ent.Type != format.EntrySymlink || ent.ReparseData != "subdir" {
				t.Fatalf("link-to-dir: type=%s target=%q", ent.Type, ent.ReparseData)
			}
		}
	}
	if !sawFileLink || !sawDirLink {
		t.Fatalf("symlinks silently missing from tree (S3-F5): entries=%+v", tree.Entries)
	}
	t.Logf("S3-F5 PASS: both symlinks present in snapshot tree")
}

// TestS3F2_CancelMidPutContentsRejectsSubsequent (S3-F2).
func TestS3F2_CancelMidPutContentsRejectsSubsequent(t *testing.T) {
	env := startDataEnv(t)
	ctx := context.Background()
	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o644)

	mach, _, _, _, conn := env.mintAndEnroll("s3f2-midstream")
	agent := openChannel(t, conn, mach)
	agent.skipResult = true
	agent.heartbeat()
	waitOnline(t, env.DB, mach, 3*time.Second)
	params, _ := json.Marshal(map[string]string{"source": src})
	job, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: mach, Type: scheduler.TypeFileBackup, ParamsJSON: string(params),
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.waitJobStarts(1, 5*time.Second)
	waitJobState(t, env.Engine, job, catalog.JobStateRunning, 3*time.Second)

	ds := breakwaterv1.NewDataServiceClient(conn)
	stream, err := ds.PutContents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// First message OK while running.
	if err := stream.Send(&breakwaterv1.PutContentsRequest{
		JobId: job, Data: []byte("chunk-one"), Seq: 1,
	}); err != nil {
		t.Fatal(err)
	}
	ack1, err := stream.Recv()
	if err != nil || !ack1.GetAccepted() {
		t.Fatalf("first put: err=%v ack=%v", err, ack1)
	}

	// Cancel then agent confirms → lease released (mid-stream terminal).
	if err := env.Engine.Cancel(ctx, job, "mid-stream cancel"); err != nil {
		t.Fatal(err)
	}
	if err := env.Engine.HandleResult(ctx, mach, scheduler.Result{
		JobID: job, Success: false, ErrorMessage: "cancelled",
	}); err != nil {
		t.Fatal(err)
	}
	j, _ := env.Engine.Job(ctx, job)
	if j.State != catalog.JobStateCancelled {
		t.Fatalf("state=%s want cancelled", j.State)
	}
	if env.Engine.HasLease(job) {
		t.Fatal("lease should be released")
	}

	// Subsequent PutContents message must be rejected.
	if err := stream.Send(&breakwaterv1.PutContentsRequest{
		JobId: job, Data: []byte("chunk-two"), Seq: 2,
	}); err != nil {
		// send may fail if server closed stream — also OK
		t.Logf("send after cancel: %v", err)
		return
	}
	ack2, err := stream.Recv()
	if err != nil {
		// stream terminated — acceptable for S3-F2
		t.Logf("stream ended after cancel: %v", err)
		return
	}
	if ack2.GetAccepted() {
		t.Fatal("S3-F2: PutContents accepted after job terminal / lease released")
	}
	t.Logf("S3-F2 PASS: subsequent message rejected: %s", ack2.GetErrorMessage())
}

// TestS3F3_MismatchLeavesRepoUnchanged (S3-F3): mismatched content_id must not
// write to the vault.
func TestS3F3_MismatchLeavesRepoUnchanged(t *testing.T) {
	env := startDataEnv(t)
	ctx := context.Background()
	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o644)

	mach, _, _, _, conn := env.mintAndEnroll("s3f3-mismatch")
	agent := openChannel(t, conn, mach)
	agent.skipResult = true
	agent.heartbeat()
	waitOnline(t, env.DB, mach, 3*time.Second)
	params, _ := json.Marshal(map[string]string{"source": src})
	job, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: mach, Type: scheduler.TypeFileBackup, ParamsJSON: string(params),
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.waitJobStarts(1, 5*time.Second)
	waitJobState(t, env.Engine, job, catalog.JobStateRunning, 3*time.Second)

	pw, _ := env.Keystore.GetRepoPassword(ctx, mach)
	v, err := env.Vaults.Open(ctx, mach, pw)
	if err != nil {
		t.Fatal(err)
	}
	before, err := v.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}

	ds := breakwaterv1.NewDataServiceClient(conn)
	stream, err := ds.PutContents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&breakwaterv1.PutContentsRequest{
		JobId: job, ContentId: "00000000000000000000000000000000",
		Data: []byte("should-not-be-stored"), Seq: 1,
	}); err != nil {
		t.Fatal(err)
	}
	_ = stream.CloseSend()
	ack, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if ack.GetAccepted() {
		t.Fatal("mismatch must not be accepted")
	}

	after, err := v.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.UserContentCount != before.UserContentCount {
		t.Fatalf("S3-F3: mismatch wrote to vault: user_contents before=%d after=%d",
			before.UserContentCount, after.UserContentCount)
	}
	t.Logf("S3-F3 PASS: mismatch rejected, user_contents unchanged=%d", after.UserContentCount)
}

// TestS3F1_ContentIdsPathWorks exercises the additive content_ids field end-to-end.
func TestS3F1_ContentIdsPathWorks(t *testing.T) {
	env := startDataEnv(t)
	ctx := context.Background()
	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644)

	mach, hk, algo, _, conn := env.mintAndEnroll("s3f1-cids")
	h, err := contentid.New(algo, hk)
	if err != nil {
		t.Fatal(err)
	}
	agent := openChannel(t, conn, mach)
	agent.skipResult = true
	agent.heartbeat()
	waitOnline(t, env.DB, mach, 3*time.Second)
	params, _ := json.Marshal(map[string]string{"source": src})
	job, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: mach, Type: scheduler.TypeFileBackup, ParamsJSON: string(params),
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.waitJobStarts(1, 5*time.Second)
	waitJobState(t, env.Engine, job, catalog.JobStateRunning, 3*time.Second)

	cl := &backup.GRPCClient{DS: breakwaterv1.NewDataServiceClient(conn)}
	data := []byte("multi-chunk-via-content-ids")
	id, err := h.ContentID(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.PutContent(ctx, job, id, data); err != nil {
		t.Fatal(err)
	}
	oid, err := cl.PutObjectFromContents(ctx, job, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if oid == "" {
		t.Fatal("empty object id")
	}
	// Mutual exclusion: both fields → error
	ds := breakwaterv1.NewDataServiceClient(conn)
	_, err = ds.PutTreeObject(ctx, &breakwaterv1.PutTreeObjectRequest{
		JobId: job, TreeJson: []byte(`{"v":1,"entries":[]}`), ContentIds: []string{id},
	})
	if err == nil || status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected mutual exclusion InvalidArgument, got %v", err)
	}
	t.Logf("S3-F1 content_ids path OK oid=%s", oid)
}
