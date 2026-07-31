//go:build !unix

package chaos_test

import (
	"fmt"
	"os"
	"os/exec"
)

func execCommand(name string, arg ...string) *exec.Cmd {
	return exec.Command(name, arg...)
}

func kill9(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	// Windows: Kill is not SIGKILL but is the closest process-death equivalent.
	if err := p.Kill(); err != nil {
		return fmt.Errorf("process kill: %w", err)
	}
	return nil
}
