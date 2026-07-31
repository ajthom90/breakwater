//go:build !linux && !darwin

package chaos_test

import (
	"fmt"
	"runtime"
)

func createTinyFSLinux(sizeBytes int64) (string, func() error, error) {
	return "", nil, fmt.Errorf("ENOSPC tiny FS not supported on %s", runtime.GOOS)
}

func createTinyFSDarwin(sizeBytes int64) (string, func() error, error) {
	return "", nil, fmt.Errorf("ENOSPC tiny FS not supported on %s", runtime.GOOS)
}
