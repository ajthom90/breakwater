//go:build windows

package golden

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// generateWindows creates Windows-only fixtures.
//
// UNTESTED ON WINDOWS until first real CI run — verify each fixture:
//   - long-path-gt260 under extended paths
//   - SYSTEM-only ACL via icacls
//   - ADS readable via type file:stream
//   - sparse file FSCTL_SET_SPARSE
//   - junction loop without hang in backup walk
//   - share-locked exclusive open works
func generateWindows(root string, res *Result) error {
	// Long path >260 chars.
	longBase := filepath.Join(root, `windows\long`)
	comp := strings.Repeat("p", 20)
	p := longBase
	for len(p) < 280 {
		p = filepath.Join(p, comp)
	}
	if err := os.MkdirAll(p, 0o755); err != nil {
		res.Skipped = append(res.Skipped, Skip{Fixture: FixLongPath, Reason: "mkdir long path: " + err.Error()})
	} else {
		f := filepath.Join(p, "leaf.txt")
		if err := os.WriteFile(f, []byte("long path\n"), 0o644); err != nil {
			res.Skipped = append(res.Skipped, Skip{Fixture: FixLongPath, Reason: "write long: " + err.Error()})
		} else {
			rel, _ := filepath.Rel(root, f)
			res.Created = append(res.Created, FixLongPath)
			res.Paths[FixLongPath] = []string{rel}
		}
	}

	// SYSTEM-only ACL via icacls.
	aclFile := filepath.Join(root, `windows\acl-system-only.txt`)
	if err := os.MkdirAll(filepath.Dir(aclFile), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(aclFile, []byte("acl payload\n"), 0o644); err != nil {
		return err
	}
	cmd := exec.Command("icacls", aclFile, "/inheritance:r", "/grant:r", "SYSTEM:(F)")
	if out, err := cmd.CombinedOutput(); err != nil {
		res.Skipped = append(res.Skipped, Skip{
			Fixture: FixACLSystemOnly,
			Reason:  fmt.Sprintf("icacls: %v (%s)", err, string(out)),
		})
	} else {
		rel, _ := filepath.Rel(root, aclFile)
		res.Created = append(res.Created, FixACLSystemOnly)
		res.Paths[FixACLSystemOnly] = []string{rel}
	}

	// ADS
	adsFile := filepath.Join(root, `windows\ads-host.txt`)
	if err := os.WriteFile(adsFile, []byte("main stream\n"), 0o644); err != nil {
		return err
	}
	adsPath := adsFile + ":secret"
	if err := os.WriteFile(adsPath, []byte("hidden ads payload\n"), 0o644); err != nil {
		res.Skipped = append(res.Skipped, Skip{Fixture: FixADS, Reason: "ADS write: " + err.Error()})
	} else {
		rel, _ := filepath.Rel(root, adsFile)
		res.Created = append(res.Created, FixADS)
		res.Paths[FixADS] = []string{rel}
	}

	// Sparse file
	sp := filepath.Join(root, `windows\sparse.bin`)
	if err := createSparse(sp, 64<<20); err != nil {
		res.Skipped = append(res.Skipped, Skip{Fixture: FixSparse, Reason: err.Error()})
	} else {
		rel, _ := filepath.Rel(root, sp)
		res.Created = append(res.Created, FixSparse)
		res.Paths[FixSparse] = []string{rel}
	}

	// Junction loop
	loopDir := filepath.Join(root, `windows\loop`)
	if err := os.MkdirAll(loopDir, 0o755); err != nil {
		return err
	}
	junc := filepath.Join(loopDir, "to-parent")
	cmd = exec.Command("cmd", "/c", "mklink", "/J", junc, loopDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		res.Skipped = append(res.Skipped, Skip{
			Fixture: FixJunctionLoop,
			Reason:  fmt.Sprintf("mklink /J: %v (%s)", err, string(out)),
		})
	} else {
		rel, _ := filepath.Rel(root, junc)
		res.Created = append(res.Created, FixJunctionLoop)
		res.Paths[FixJunctionLoop] = []string{rel}
	}

	// Deny-share-locked file (create + exclusive open probe).
	lockFile := filepath.Join(root, `windows\share-locked.txt`)
	if err := os.WriteFile(lockFile, []byte("locked payload\n"), 0o644); err != nil {
		return err
	}
	h, err := openExclusive(lockFile)
	if err != nil {
		res.Skipped = append(res.Skipped, Skip{Fixture: FixDenyShareLocked, Reason: err.Error()})
	} else {
		_ = windows.CloseHandle(h)
		rel, _ := filepath.Rel(root, lockFile)
		res.Created = append(res.Created, FixDenyShareLocked)
		res.Paths[FixDenyShareLocked] = []string{rel}
	}

	return nil
}

func createSparse(path string, size int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	const fsctlSetSparse = 0x000900c4
	var bytesReturned uint32
	err = windows.DeviceIoControl(
		windows.Handle(f.Fd()),
		fsctlSetSparse,
		nil, 0, nil, 0, &bytesReturned, nil,
	)
	if err != nil {
		return fmt.Errorf("FSCTL_SET_SPARSE: %w", err)
	}
	if err := f.Truncate(size); err != nil {
		return err
	}
	if _, err := f.Seek(size-4096, 0); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	for i := range buf {
		buf[i] = 0xEF
	}
	_, err = f.Write(buf)
	return err
}

func openExclusive(path string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(
		p,
		windows.GENERIC_READ,
		0, // exclusive
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
}

func compareACL(a, b string) error {
	outA, err := exec.Command("icacls", a).CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls a: %w", err)
	}
	outB, err := exec.Command("icacls", b).CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls b: %w", err)
	}
	na := normalizeICACLS(string(outA), a)
	nb := normalizeICACLS(string(outB), b)
	if na != nb {
		return fmt.Errorf("ACL mismatch:\n--- a ---\n%s\n--- b ---\n%s", na, nb)
	}
	return nil
}

func normalizeICACLS(out, path string) string {
	s := strings.ReplaceAll(out, path, "<path>")
	return strings.TrimSpace(s)
}

func compareADS(a, b string) error {
	for _, stream := range []string{":secret"} {
		da, errA := os.ReadFile(a + stream)
		db, errB := os.ReadFile(b + stream)
		if os.IsNotExist(errA) && os.IsNotExist(errB) {
			continue
		}
		if errA != nil || errB != nil {
			return fmt.Errorf("ADS %s: a=%v b=%v", stream, errA, errB)
		}
		if string(da) != string(db) {
			return fmt.Errorf("ADS %s content mismatch", stream)
		}
	}
	return nil
}
