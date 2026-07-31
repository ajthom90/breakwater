package agentgw_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ajthom90/breakwater/server/internal/scheduler"
)

// TestM5_AgentHasNoForgetOrPrunePath is the second non-negotiable safety
// property: an agent must have no path to trigger forget or prune.
//
// Asserts:
//  1. Engine.Submit rejects TypePrune (server-only).
//  2. TypePrune / TypeVerify are IsServerOnly.
//  3. Registered gRPC services on the agent gateway expose no Forget/Prune/
//     DeleteSnapshot/Undelete methods (append-only ransomware boundary).
func TestM5_AgentHasNoForgetOrPrunePath(t *testing.T) {
	env := startControlEnv(t)
	machineID, _, _ := env.mintAndEnroll("m5-boundary")

	// (1) Submit path
	_, err := env.Engine.Submit(context.Background(), scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypePrune,
	})
	if err == nil {
		t.Fatal("agent path must not Submit prune")
	}
	_, err = env.Engine.Submit(context.Background(), scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeVerify,
	})
	if err == nil {
		t.Fatal("agent path must not Submit verify/scrub")
	}

	// (2) Type registry
	if !scheduler.IsServerOnly(scheduler.TypePrune) {
		t.Fatal("prune must be server-only")
	}
	if !scheduler.IsServerOnly(scheduler.TypeVerify) {
		t.Fatal("verify must be server-only")
	}
	if scheduler.IsAgentDispatchable(scheduler.TypePrune) {
		t.Fatal("prune must not be agent-dispatchable")
	}

	// (3) gRPC method surface — no destructive retention RPCs on :9443.
	gs := env.GW.GRPCServer()
	if gs == nil {
		t.Fatal("grpc server nil")
	}
	info := gs.GetServiceInfo()
	forbidden := []string{
		"Forget", "Prune", "Undelete", "DeleteSnapshot", "DeleteContent",
		"Retention", "Scrub",
	}
	for svc, si := range info {
		for _, m := range si.Methods {
			for _, bad := range forbidden {
				if strings.Contains(strings.ToLower(m.Name), strings.ToLower(bad)) {
					t.Errorf("forbidden agent method %s/%s", svc, m.Name)
				}
			}
		}
	}
	// Explicit allow-list: DataService must be put/check/commit only.
	// startControlEnv may not register DataService — when present, check.
	for svc, si := range info {
		if !strings.Contains(svc, "DataService") {
			continue
		}
		for _, m := range si.Methods {
			switch m.Name {
			case "CheckContents", "PutContents", "PutTreeObject", "PutImageManifest", "CommitSnapshot":
				// allowed append-only
			default:
				t.Errorf("unexpected DataService method %s (append-only boundary)", m.Name)
			}
		}
	}
}
