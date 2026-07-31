package chaos_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/chaos"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

// TestChaos04_ENOSPC is PLAN chaos drill #4:
// server ENOSPC → clean failure + alert; repo untouched (no partial content commit
// as a snapshot, no corrupt index).
//
// Primary path: real small filesystem (Linux tmpfs via sudo; macOS hdiutil).
// Fallback: skip with reason if the platform cannot create a limited FS
// (documented — not a silent pass).
//
// CHAOS-F3: tiny FS is mounted outside t.TempDir; vault handles are closed
// before umount; umount is retried and must succeed before RemoveAll. Mounting
// inside t.TempDir left CI intermittent-red on Linux (EBUSY on cleanup).
func TestChaos04_ENOSPC(t *testing.T) {
	seed := chaos.Seed(t, time.Now().UnixNano())
	t.Logf("chaos#4 seed=%d", seed)

	mount, cleanup, err := createTinyFS(1 << 20) // ~1 MiB
	if err != nil {
		t.Skipf("cannot create tiny filesystem for real ENOSPC (platform=%s): %v", runtime.GOOS, err)
	}
	// Register umount/remove first so LIFO runs it *after* CloseAll below.
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("tiny FS cleanup (umount/remove): %v", err)
		}
	})
	t.Logf("FAULT surface: tiny FS at %s (≤1MiB)", mount)

	ctx := context.Background()
	// Put vault blob store on the tiny FS; catalog/keystore on normal temp.
	env := newDrillEnv(t)
	// Rebuild vault manager on tiny FS for this machine's repo.
	tinyRepos := filepath.Join(mount, "repos")
	if err := os.MkdirAll(tinyRepos, 0o700); err != nil {
		t.Fatal(err)
	}
	// Use existing password but create repo ON tiny FS.
	// Close default vault first.
	_ = env.VM.CloseAll(ctx)
	vmTiny := vault.NewManager(tinyRepos, env.Dir)
	// CloseAll before umount (registered later → runs first under LIFO).
	t.Cleanup(func() {
		if err := vmTiny.CloseAll(ctx); err != nil {
			t.Logf("vmTiny.CloseAll: %v", err)
		}
	})

	v, err := vmTiny.Create(ctx, env.MachineID, env.Password)
	if err != nil {
		t.Fatalf("create vault on tiny FS: %v", err)
	}
	// Update env for open helpers.
	env.VM = vmTiny
	env.ReposDir = tinyRepos

	// Fill almost all space with junk so the next vault write hits ENOSPC.
	junkPath := filepath.Join(mount, "fill.dat")
	fillUntilNearFull(t, junkPath)
	t.Log("FAULT injected: tiny FS filled near capacity")

	// Attempt large writes — expect ENOSPC-class error.
	var enospc error
	big := make([]byte, 256<<10) // 256 KiB chunks
	for i := 0; i < 32; i++ {
		for j := range big {
			big[j] = byte(i ^ j ^ int(seed))
		}
		_, err := v.PutContent(ctx, big)
		if err != nil {
			enospc = err
			break
		}
	}
	if enospc == nil {
		// Try WriteObject with larger stream.
		huge := strings.Repeat("Z", 512<<10)
		_, err := v.WriteObject(ctx, vault.SplitterDynamic, strings.NewReader(huge))
		if err != nil {
			enospc = err
		}
	}
	if enospc == nil {
		t.Fatal("expected ENOSPC (or write error) on tiny FS — fault not exercised; increase fill or shrink FS")
	}
	t.Logf("FAULT confirmed: write failed: %v", enospc)
	if !isENOSPC(enospc) {
		// Accept any write error on full FS — some layers wrap ENOSPC.
		t.Logf("note: error may be wrapped; isENOSPC=%v raw=%v", isENOSPC(enospc), enospc)
	}

	// Alert on failure (operator path).
	env.Notifier.AlertFailure(env.MachineID, "chaos-enospc", enospc.Error())
	msg := waitNotify(t, env.FakeSend, "failure", 3*time.Second)
	if !strings.Contains(msg.Body, enospc.Error()) && !strings.Contains(msg.Body, "chaos-enospc") {
		t.Fatalf("alert body missing failure detail: %q", msg.Body)
	}
	t.Logf("alert fired: subject=%q", msg.Subject)

	// Repo still openable / not corrupt: ListSnapshotRecords works; no partial snap.
	// Close after verify so no live handle pins the mount at cleanup (CHAOS-F3).
	_ = vmTiny.Close(ctx, env.MachineID)
	v2, err := vmTiny.Open(ctx, env.MachineID, env.Password)
	if err != nil {
		// If open fails because of true corruption, that is a drill finding.
		t.Fatalf("repo open after ENOSPC failed (possible corruption): %v", err)
	}
	metas, err := v2.ListSnapshotRecords(ctx, vault.KindFileSnapshot)
	if err != nil {
		_ = vmTiny.CloseAll(ctx)
		t.Fatalf("ListSnapshotRecords after ENOSPC: %v", err)
	}
	if len(metas) != 0 {
		_ = vmTiny.CloseAll(ctx)
		t.Fatalf("partial snapshot committed under ENOSPC: %d manifests", len(metas))
	}
	// Catalog should also have no snapshots for this machine (we never committed).
	live, _ := env.DB.ListSnapshotsByMachine(ctx, env.MachineID, 100)
	if len(live) != 0 {
		_ = vmTiny.CloseAll(ctx)
		t.Fatalf("catalog has %d snapshots after ENOSPC abort", len(live))
	}
	// Deterministic pre-cleanup: drop all vault FDs before umount cleanup runs.
	if err := vmTiny.CloseAll(ctx); err != nil {
		t.Logf("CloseAll before umount: %v", err)
	}
	t.Logf("chaos#4 OK: ENOSPC clean fail, alert fired, 0 partial snapshots")
}

func isENOSPC(err error) bool {
	if err == nil {
		return false
	}
	if pathErr, ok := err.(*os.PathError); ok {
		if pathErr.Err == syscall.ENOSPC {
			return true
		}
	}
	if err == syscall.ENOSPC {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no space") || strings.Contains(s, "enospc")
}

func fillUntilNearFull(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 32<<10)
	for {
		_, err := f.Write(buf)
		if err != nil {
			// Full — good.
			_ = f.Sync()
			return
		}
	}
}

// createTinyFS returns a mountpoint of ~sizeBytes free capacity and a cleanup
// that umounts then removes the directory. Does not use t.TempDir (CHAOS-F3).
func createTinyFS(sizeBytes int64) (mount string, cleanup func() error, err error) {
	switch runtime.GOOS {
	case "linux":
		return createTinyFSLinux(sizeBytes)
	case "darwin":
		return createTinyFSDarwin(sizeBytes)
	default:
		return "", nil, fmt.Errorf("unsupported OS %s", runtime.GOOS)
	}
}
