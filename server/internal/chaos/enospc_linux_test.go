//go:build linux

package chaos_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// createTinyFSLinux mounts a size-limited tmpfs for real ENOSPC injection.
//
// CHAOS-F3: the mount directory is created with os.MkdirTemp (NOT t.TempDir).
// Go's TempDir cleanup runs RemoveAll and fails with EBUSY while a filesystem
// is still mounted — which marked this drill intermittent-red on Linux CI.
// Cleanup owns umount (with retries) then RemoveAll; the caller must close all
// open vault handles before cleanup runs (drill registers CloseAll first via
// t.Cleanup LIFO).
//
// Return values are unnamed so a `return writable, cleanup, nil` cannot
// reassign a closed-over mount path (darwin CHAOS-F3 footgun).
func createTinyFSLinux(sizeBytes int64) (string, func() error, error) {
	mount, err := os.MkdirTemp("", "bw-enospc-*")
	if err != nil {
		return "", nil, err
	}
	size := fmt.Sprintf("%dk", sizeBytes/1024)
	if sizeBytes < 1024*1024 {
		size = "1024k"
	}
	cmd := exec.Command("sudo", "-n", "mount", "-t", "tmpfs", "-o", "size="+size, "tmpfs", mount)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.RemoveAll(mount)
		return "", nil, fmt.Errorf("sudo mount tmpfs: %v (%s)", err, out)
	}
	mp := mount // capture for closure
	cleanup := func() error {
		return unmountAndRemoveLinux(mp)
	}
	if err := os.WriteFile(filepath.Join(mount, ".ok"), []byte("1"), 0o600); err != nil {
		_ = cleanup()
		return "", nil, err
	}
	return mount, cleanup, nil
}

func unmountAndRemoveLinux(mount string) error {
	var last error
	for attempt := 0; attempt < 30; attempt++ {
		// Prefer a normal unmount so the directory becomes a regular empty dir.
		cmd := exec.Command("sudo", "-n", "umount", mount)
		out, err := cmd.CombinedOutput()
		if err == nil {
			last = nil
			break
		}
		msg := string(out) + err.Error()
		last = fmt.Errorf("umount %s: %v (%s)", mount, err, strings.TrimSpace(string(out)))
		// After a few tries, lazy-detach so open FDs cannot pin the mount forever.
		if attempt >= 5 || strings.Contains(strings.ToLower(msg), "busy") {
			_ = exec.Command("sudo", "-n", "umount", "-l", mount).Run()
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Confirm nothing is still mounted on this path.
	if stillMountedLinux(mount) {
		_ = exec.Command("sudo", "-n", "umount", "-l", mount).Run()
		time.Sleep(100 * time.Millisecond)
		if stillMountedLinux(mount) {
			if last == nil {
				last = fmt.Errorf("mount still active after umount attempts: %s", mount)
			}
			return last
		}
	}
	var remErr error
	for attempt := 0; attempt < 10; attempt++ {
		remErr = os.RemoveAll(mount)
		if remErr == nil {
			break
		}
		_ = exec.Command("sudo", "-n", "umount", "-l", mount).Run()
		time.Sleep(50 * time.Millisecond)
	}
	if remErr != nil {
		return fmt.Errorf("remove mount dir after umount: %w (prior: %v)", remErr, last)
	}
	return nil
}

func stillMountedLinux(mount string) bool {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == mount {
			return true
		}
	}
	return false
}

func createTinyFSDarwin(sizeBytes int64) (string, func() error, error) {
	return "", nil, fmt.Errorf("not darwin")
}
