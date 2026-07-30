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

	// Sparse via FSCTL_SET_SPARSE is covered by the portable sparse fixture (S4-F9).
	// Windows-only list no longer includes FixSparse.

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

// compareACL compares security descriptors via SDDL (S4-F7), not icacls text.
// icacls output is attached only as human-readable detail on mismatch.
//
// UNTESTED ON WINDOWS until first real CI run — verify:
//   - GetNamedSecurityInfo + SDDL round-trip on NTFS
//   - equal SDs with different ACE display order still match (or document if not)
//   - SYSTEM-only fixture SD matches after restore
func compareACL(a, b string) error {
	sddlA, err := fileSDDL(a)
	if err != nil {
		return fmt.Errorf("SDDL a: %w", err)
	}
	sddlB, err := fileSDDL(b)
	if err != nil {
		return fmt.Errorf("SDDL b: %w", err)
	}
	if sddlA == sddlB {
		return nil
	}
	// Human-readable detail only — not the equality oracle.
	detailA := icaclsDetail(a)
	detailB := icaclsDetail(b)
	return fmt.Errorf("ACL/SD mismatch:\n sddl a=%s\n sddl b=%s\n icacls a: %s\n icacls b: %s",
		sddlA, sddlB, detailA, detailB)
}

func fileSDDL(path string) (string, error) {
	// Owner + Group + DACL + SACL + Label — full descriptor for equality.
	const si = windows.OWNER_SECURITY_INFORMATION |
		windows.GROUP_SECURITY_INFORMATION |
		windows.DACL_SECURITY_INFORMATION |
		windows.SACL_SECURITY_INFORMATION |
		windows.LABEL_SECURITY_INFORMATION
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, si)
	if err != nil {
		// SACL may require privilege; fall back to owner+group+dacl.
		const si2 = windows.OWNER_SECURITY_INFORMATION |
			windows.GROUP_SECURITY_INFORMATION |
			windows.DACL_SECURITY_INFORMATION
		sd, err = windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, si2)
		if err != nil {
			return "", err
		}
	}
	s := sd.String()
	if s == "" {
		return "", fmt.Errorf("empty SDDL for %s", path)
	}
	return s, nil
}

func icaclsDetail(path string) string {
	out, err := exec.Command("icacls", path).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("(icacls failed: %v)", err)
	}
	return strings.TrimSpace(strings.ReplaceAll(string(out), path, "<path>"))
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
