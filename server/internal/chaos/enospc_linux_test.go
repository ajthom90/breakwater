//go:build linux

package chaos_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func createTinyFSLinux(t *testing.T, sizeBytes int64) (string, func(), error) {
	t.Helper()
	mount := t.TempDir()
	size := fmt.Sprintf("%dk", sizeBytes/1024)
	if sizeBytes < 1024*1024 {
		size = "1024k"
	}
	cmd := exec.Command("sudo", "-n", "mount", "-t", "tmpfs", "-o", "size="+size, "tmpfs", mount)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", nil, fmt.Errorf("sudo mount tmpfs: %v (%s)", err, out)
	}
	cleanup := func() {
		_ = exec.Command("sudo", "-n", "umount", mount).Run()
	}
	if err := os.WriteFile(filepath.Join(mount, ".ok"), []byte("1"), 0o600); err != nil {
		cleanup()
		return "", nil, err
	}
	return mount, cleanup, nil
}

func createTinyFSDarwin(t *testing.T, sizeBytes int64) (string, func(), error) {
	return "", nil, fmt.Errorf("not darwin")
}
