//go:build darwin

package chaos_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func createTinyFSDarwin(t *testing.T, sizeBytes int64) (string, func(), error) {
	t.Helper()
	// RAM disk: sectors of 512 bytes. ~2 MiB minimum for a usable HFS+ volume.
	sectors := sizeBytes / 512
	if sectors < 4096 {
		sectors = 4096
	}
	// Prefer modern diskutil image attach; fall back to hdiutil ram://.
	out, err := exec.Command("hdiutil", "attach", "-nomount", fmt.Sprintf("ram://%d", sectors)).CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("hdiutil attach: %v (%s)", err, out)
	}
	// Output may include deprecation warnings; find the /dev/diskN token.
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
	// Format: use newfs_hfs directly (diskutil eraseVolume is flaky on raw ram disks).
	if out, err := exec.Command("newfs_hfs", "-v", "BWENOSPC", dev).CombinedOutput(); err != nil {
		_ = exec.Command("hdiutil", "detach", dev, "-force").Run()
		return "", nil, fmt.Errorf("newfs_hfs: %v (%s)", err, out)
	}
	mount := filepath.Join(t.TempDir(), "mnt")
	if err := os.MkdirAll(mount, 0o755); err != nil {
		_ = exec.Command("hdiutil", "detach", dev, "-force").Run()
		return "", nil, err
	}
	if out, err := exec.Command("mount", "-t", "hfs", dev, mount).CombinedOutput(); err != nil {
		_ = exec.Command("hdiutil", "detach", dev, "-force").Run()
		return "", nil, fmt.Errorf("mount: %v (%s)", err, out)
	}
	sub := filepath.Join(mount, "bw")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		_ = exec.Command("umount", mount).Run()
		_ = exec.Command("hdiutil", "detach", dev, "-force").Run()
		return "", nil, err
	}
	cleanup := func() {
		_ = exec.Command("umount", mount).Run()
		_ = exec.Command("hdiutil", "detach", dev, "-force").Run()
	}
	return sub, cleanup, nil
}

func createTinyFSLinux(t *testing.T, sizeBytes int64) (string, func(), error) {
	return "", nil, fmt.Errorf("not linux")
}
