//go:build darwin

package chaos_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// createTinyFSDarwin attaches a small RAM disk for real ENOSPC injection.
//
// CHAOS-F3: mount directory is os.MkdirTemp (not t.TempDir) so Go's TempDir
// RemoveAll cannot race a still-mounted volume. Cleanup umounts/detaches with
// retries, then removes the directory.
//
// Note: return values are unnamed intentionally — a prior bug used a named
// return `mount` and `return sub, cleanup, nil`, which reassigned the closed-
// over mount path to the subdir and made umount miss the real mount point.
func createTinyFSDarwin(sizeBytes int64) (string, func() error, error) {
	// RAM disk: sectors of 512 bytes. ~2 MiB minimum for a usable HFS+ volume.
	sectors := sizeBytes / 512
	if sectors < 4096 {
		sectors = 4096
	}
	out, err := exec.Command("hdiutil", "attach", "-nomount", fmt.Sprintf("ram://%d", sectors)).CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("hdiutil attach: %v (%s)", err, out)
	}
	dev := ""
	for _, tok := range strings.Fields(string(out)) {
		if strings.HasPrefix(tok, "/dev/") {
			dev = tok
			break
		}
	}
	if dev == "" {
		return "", nil, fmt.Errorf("empty/invalid ram disk device from %q", out)
	}
	if out, err := exec.Command("newfs_hfs", "-v", "BWENOSPC", dev).CombinedOutput(); err != nil {
		_ = exec.Command("hdiutil", "detach", dev, "-force").Run()
		return "", nil, fmt.Errorf("newfs_hfs: %v (%s)", err, out)
	}
	base, err := os.MkdirTemp("", "bw-enospc-*")
	if err != nil {
		_ = exec.Command("hdiutil", "detach", dev, "-force").Run()
		return "", nil, err
	}
	mountPoint := filepath.Join(base, "mnt")
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		_ = exec.Command("hdiutil", "detach", dev, "-force").Run()
		_ = os.RemoveAll(base)
		return "", nil, err
	}
	if out, err := exec.Command("mount", "-t", "hfs", dev, mountPoint).CombinedOutput(); err != nil {
		_ = exec.Command("hdiutil", "detach", dev, "-force").Run()
		_ = os.RemoveAll(base)
		return "", nil, fmt.Errorf("mount: %v (%s)", err, out)
	}
	sub := filepath.Join(mountPoint, "bw")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		_ = unmountAndRemoveDarwin(mountPoint, base, dev)
		return "", nil, err
	}
	// Capture locals by value for the cleanup closure.
	mp, b, d := mountPoint, base, dev
	cleanup := func() error {
		return unmountAndRemoveDarwin(mp, b, d)
	}
	return sub, cleanup, nil
}

func unmountAndRemoveDarwin(mountPoint, base, dev string) error {
	var last error
	for attempt := 0; attempt < 20; attempt++ {
		// Prefer diskutil force unmount (handles busy open files better than umount).
		if out, err := exec.Command("diskutil", "unmount", "force", mountPoint).CombinedOutput(); err == nil {
			last = nil
			break
		} else {
			last = fmt.Errorf("diskutil unmount force: %v (%s)", err, strings.TrimSpace(string(out)))
		}
		if out, err := exec.Command("umount", "-f", mountPoint).CombinedOutput(); err == nil {
			last = nil
			break
		} else {
			last = fmt.Errorf("umount -f: %v (%s)", err, strings.TrimSpace(string(out)))
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Eject the RAM disk (force). Prefer diskutil eject; fall back to hdiutil.
	if out, err := exec.Command("diskutil", "eject", dev).CombinedOutput(); err != nil {
		if out2, err2 := exec.Command("hdiutil", "detach", dev, "-force").CombinedOutput(); err2 != nil {
			msg := string(out2) + string(out)
			if !strings.Contains(msg, "No such") && !strings.Contains(msg, "not currently") &&
				!strings.Contains(msg, "ejected") {
				if last == nil {
					last = fmt.Errorf("eject/detach %s: %v (%s)", dev, err2, strings.TrimSpace(string(out2)))
				}
			}
		}
	}
	// Retry RemoveAll — after force eject the mount should be gone.
	var remErr error
	for attempt := 0; attempt < 10; attempt++ {
		remErr = os.RemoveAll(base)
		if remErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if remErr != nil {
		return fmt.Errorf("remove mount base %s: %w (prior: %v)", base, remErr, last)
	}
	return nil
}

func createTinyFSLinux(sizeBytes int64) (string, func() error, error) {
	return "", nil, fmt.Errorf("not linux")
}
