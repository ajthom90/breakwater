//go:build unix

package chaos_test

import (
	"os"
	"os/exec"
	"syscall"
)

func execCommand(name string, arg ...string) *exec.Cmd {
	return exec.Command(name, arg...)
}

func kill9(pid int) error {
	return syscall.Kill(pid, syscall.SIGKILL)
}

// ensure we reference os for build tags without unused import on some paths
var _ = os.ErrNotExist
