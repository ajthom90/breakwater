package chaos_test

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/agentgw"
	"github.com/ajthom90/breakwater/server/internal/retention"
)

// TestChaos_MutationSelfCheck documents mutation self-checks for chaos guards.
//
// Clock skew (drill #5): if CommitSnapshot trusted agent FinishedAt for
// catalog/vault timestamps, drill #5 fails. Guard is server clock assignment
// in agentgw.DataServer.CommitSnapshot.
//
// Scrub alert (drill #8): Notifier.AlertCorruption must fire on corruption.
// Removing Notifier wiring would make #8 fail on waitNotify.
func TestChaos_MutationSelfCheck(t *testing.T) {
	t.Run("clock_skew_threshold", func(t *testing.T) {
		if agentgw.ClockSkewWarnThreshold < time.Hour {
			t.Fatal("ClockSkewWarnThreshold too small")
		}
		if 3*24*time.Hour <= agentgw.ClockSkewWarnThreshold {
			t.Fatal("3d skew would not warn — threshold too large")
		}
	})

	t.Run("scrub_alert_fires_when_corruption_detected", func(t *testing.T) {
		ctx := context.Background()
		env := newDrillEnv(t)
		v := env.openVault(ctx)
		// Larger payload to land in a pack we can flip.
		payload := make([]byte, 64<<10)
		for i := range payload {
			payload[i] = byte(i)
		}
		env.putSnapshot(ctx, v, "x.bin", string(payload), env.Clock.Now())
		if err := env.VM.Close(ctx, env.MachineID); err != nil {
			t.Fatal(err)
		}
		packs := findPackFiles(t, env.ReposDir, env.MachineID)
		if len(packs) == 0 {
			t.Fatal("no packs")
		}
		rng := rand.New(rand.NewSource(42))
		for _, p := range packs {
			bitFlip(t, p, rng)
		}
		_ = os.RemoveAll(filepath.Join(env.Dir, "cache", env.MachineID))
		if _, err := env.VM.Open(ctx, env.MachineID, env.Password); err != nil {
			t.Fatal(err)
		}
		res, err := env.Svc.Scrub(ctx, env.MachineID, retention.ScrubFull, 1)
		if err != nil {
			t.Fatal(err)
		}
		if res.ContentsFailed == 0 && res.ManifestsFailed == 0 {
			t.Skip("bit-flip missed verifiable content; full coverage in TestChaos08")
		}
		_ = waitNotify(t, env.FakeSend, "corruption", 3*time.Second)
		t.Log("mutation self-check: scrub alert path live")
	})

	t.Run("agent_future_timestamp_hazard", func(t *testing.T) {
		// Document why server clock matters for GFS: future agent timestamps
		// still enter keep-set as "newest" relative to server now.
		serverNow := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
		future := retention.Snapshot{ID: "f", Timestamp: serverNow.Add(3 * 24 * time.Hour)}
		past := retention.Snapshot{ID: "p", Timestamp: serverNow.Add(-3 * 24 * time.Hour)}
		r := retention.ComputeKeepSet([]retention.Snapshot{future, past}, retention.Policy{KeepLast: 1}, serverNow)
		if _, ok := r.KeepIDs["f"]; !ok {
			t.Fatal("expected future tip kept as newest — agent +3d would pin wrong tip under keep-last=1")
		}
		if _, ok := r.KeepIDs["p"]; ok {
			t.Fatal("past should be forgotten under keep-last=1 when future tip present")
		}
		t.Log("hazard: agent +3d FinishedAt as CreatedAt would force keep of skewed tip; server clock required")
	})
}
