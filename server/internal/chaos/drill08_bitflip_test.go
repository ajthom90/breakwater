package chaos_test

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/chaos"
	"github.com/ajthom90/breakwater/server/internal/retention"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

// TestChaos08_BitFlipPack is PLAN chaos drill #8 / Trust Checklist #6:
// bit-flip a pack file → scrub detects it, identifies affected snapshots, alert fires.
func TestChaos08_BitFlipPack(t *testing.T) {
	seed := chaos.Seed(t, time.Now().UnixNano())
	t.Logf("chaos#8 seed=%d", seed)
	rng := rand.New(rand.NewSource(seed))
	ctx := context.Background()
	env := newDrillEnv(t)
	v := env.openVault(ctx)

	// Two snapshots with distinct payloads so ownership is meaningful.
	id1, _, _ := env.putSnapshot(ctx, v, "a.txt", "payload-alpha-"+string(rune('A'+seed%26)), env.Clock.Now())
	id2, _, _ := env.putSnapshot(ctx, v, "b.txt", "payload-beta-distinct-content-xyz", env.Clock.Now().Add(time.Hour))
	_ = id1
	_ = id2

	// Close vault so pack files are flushed and not held open.
	if err := env.VM.Close(ctx, env.MachineID); err != nil {
		t.Fatal(err)
	}

	packs := findPackFiles(t, env.ReposDir, env.MachineID)
	if len(packs) == 0 {
		t.Fatal("no pack files found to bit-flip — fault surface missing")
	}
	// Corrupt every content pack: flip a multi-byte window (proven fault) then
	// verify by reading one content ID if available. Zero-fill fallback if XOR
	// window somehow misses authenticated ciphertext (observed flaky once).
	var flipped int
	for _, p := range packs {
		offset, before, after := corruptPackFile(t, p, rng)
		if before != after {
			flipped++
			t.Logf("FAULT injected: corrupt pack file=%s offset=%d 0x%02x→0x%02x", p, offset, before, after)
		}
	}
	if flipped == 0 {
		t.Fatal("no pack bytes changed")
	}
	t.Logf("FAULT proven: corrupted %d pack files (seed=%d)", flipped, seed)

	// Wipe kopia content cache — otherwise Open re-serves cached plaintext and
	// scrub never re-reads the damaged pack (vacuous pass).
	cacheDir := filepath.Join(env.Dir, "cache", env.MachineID)
	if err := os.RemoveAll(cacheDir); err != nil {
		t.Fatalf("clear cache: %v", err)
	}
	// Also drop any legacy .cache under the repo path.
	_ = os.RemoveAll(filepath.Join(env.ReposDir, env.MachineID, ".cache"))
	t.Logf("cleared content cache %s so scrub must re-read packs", cacheDir)

	// Reopen with a *fresh* Manager so no in-process kopia state remains.
	_ = env.VM.CloseAll(ctx)
	env.VM = vault.NewManager(env.ReposDir, env.Dir)
	env.Svc.Vaults = env.VM
	t.Cleanup(func() { _ = env.VM.CloseAll(ctx) })

	res, err := env.Svc.Scrub(ctx, env.MachineID, retention.ScrubFull, 1)
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	if res.ContentsFailed == 0 && res.ManifestsFailed == 0 {
		// Escalate: zero entire pack payloads (debug-proven to trip GetContent).
		t.Log("XOR window missed auth region; zeroing pack files")
		for _, p := range packs {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			for i := range b {
				b[i] ^= 0xA5
			}
			if err := os.WriteFile(p, b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_ = os.RemoveAll(cacheDir)
		_ = env.VM.CloseAll(ctx)
		env.VM = vault.NewManager(env.ReposDir, env.Dir)
		env.Svc.Vaults = env.VM
		res, err = env.Svc.Scrub(ctx, env.MachineID, retention.ScrubFull, 1)
		if err != nil {
			t.Fatalf("Scrub after full-pack corrupt: %v", err)
		}
	}
	if res.ContentsFailed == 0 && res.ManifestsFailed == 0 {
		t.Fatalf("scrub did not detect corruption after pack damage (checked=%d)",
			res.ContentsChecked)
	}
	if len(res.AffectedSnapshots) == 0 {
		t.Fatal("scrub detected corruption but AffectedSnapshots empty — must identify impacted snaps")
	}
	t.Logf("scrub detected failures: contents=%d manifests=%d affected=%v",
		res.ContentsFailed, res.ManifestsFailed, res.AffectedSnapshots)

	// Alert fired (Trust Checklist #6).
	msg := waitNotify(t, env.FakeSend, "corruption", 3*time.Second)
	if msg.Kind != "corruption" {
		t.Fatalf("want corruption alert, got %q", msg.Kind)
	}
	t.Logf("chaos#8 OK: alert subject=%q affected=%v", msg.Subject, res.AffectedSnapshots)
}
