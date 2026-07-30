//go:build windows

package state

import "os"

// syncDir fsyncs the directory after rename (S4-F4).
//
// UNTESTED ON WINDOWS — FlushFileBuffers on a directory handle has different
// semantics than POSIX; verify on first real Windows run that identity.json
// survives a hard power loss after enroll.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		// Opening a directory may fail on some Windows versions; treat as soft.
		return nil
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		// Soft-fail: rename already landed; Windows durability is best-effort here.
		return nil
	}
	return nil
}
