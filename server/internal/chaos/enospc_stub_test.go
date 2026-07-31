//go:build !linux && !darwin

package chaos_test

import (
	"fmt"
	"runtime"
	"testing"
)

func createTinyFSLinux(t *testing.T, sizeBytes int64) (string, func(), error) {
	return "", nil, fmt.Errorf("ENOSPC tiny FS not supported on %s", runtime.GOOS)
}

func createTinyFSDarwin(t *testing.T, sizeBytes int64) (string, func(), error) {
	return "", nil, fmt.Errorf("ENOSPC tiny FS not supported on %s", runtime.GOOS)
}
