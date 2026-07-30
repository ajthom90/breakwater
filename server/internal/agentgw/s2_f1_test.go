package agentgw_test

import (
	"context"
	"testing"
	"time"

	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
)

// TestS2F1_MalformedInventoryDoesNotKillChannel (S2-F1): one empty-id volume among
// good items must not tear down the channel or fail concurrent running jobs.
// FAILS on 70e26a2 (ReplaceMachineInventory hard-errors → stream-fatal → disconnect).
func TestS2F1_MalformedInventoryDoesNotKillChannel(t *testing.T) {
	env := startControlEnv(t)
	ctx := context.Background()
	machineID, _, conn := env.mintAndEnroll("s2f1-host")

	agent := openChannel(t, conn, machineID)
	// Hold a running job (no result yet).
	agent.mu.Lock()
	agent.skipResult = true
	agent.mu.Unlock()

	heldID, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeNoop, Initiator: "s2-f1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := agent.waitJobStarts(1, 3*time.Second); len(got) < 1 {
		t.Fatalf("held job not started: %v", got)
	}
	j, _ := env.Engine.Job(ctx, heldID)
	if j.State != catalog.JobStateRunning {
		t.Fatalf("held job state=%s", j.State)
	}

	// Inventory with one bad item (empty id) and one good volume.
	if err := agent.stream.Send(&breakwaterv1.AgentToServer{
		Msg: &breakwaterv1.AgentToServer_Inventory{
			Inventory: &breakwaterv1.InventoryReport{
				Volumes: []*breakwaterv1.VolumeInfo{
					{Id: "", Mount: "D:\\", SizeBytes: 1, FsType: "ntfs"}, // malformed
					{Id: "vol-good", Mount: "C:\\", SizeBytes: 100, FsType: "ntfs"},
				},
			},
		},
	}); err != nil {
		t.Fatalf("send inventory: %v", err)
	}

	// Channel must stay up: heartbeat still works.
	time.Sleep(100 * time.Millisecond)
	agent.heartbeat()

	// Good inventory item persisted.
	deadline := time.Now().Add(2 * time.Second)
	var inv []catalog.InventoryItem
	for time.Now().Before(deadline) {
		inv, _ = env.DB.ListMachineInventory(ctx, machineID)
		if len(inv) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(inv) < 1 {
		t.Fatalf("S2-F1: good inventory item not persisted (channel may have died); inv=%v", inv)
	}
	found := false
	for _, it := range inv {
		if it.ExternalID == "vol-good" {
			found = true
		}
		if it.ExternalID == "" {
			t.Fatal("empty external_id should not be stored")
		}
	}
	if !found {
		t.Fatalf("S2-F1: vol-good missing from inventory: %+v", inv)
	}

	// Concurrent running job must still be running (not force-failed by disconnect).
	j, _ = env.Engine.Job(ctx, heldID)
	if j.State != catalog.JobStateRunning {
		t.Fatalf("S2-F1: held job state=%s want running (channel teardown collateral)", j.State)
	}

	// Complete held job to prove channel still accepts results.
	if err := agent.stream.Send(&breakwaterv1.AgentToServer{
		Msg: &breakwaterv1.AgentToServer_JobResult{
			JobResult: &breakwaterv1.JobResult{JobId: heldID, Success: true},
		},
	}); err != nil {
		t.Fatalf("JobResult after bad inventory: %v", err)
	}
	waitJobState(t, env.Engine, heldID, catalog.JobStateSuccess, 3*time.Second)
}
