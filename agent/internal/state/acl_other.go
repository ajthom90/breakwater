//go:build !windows

package state

// SecureDir is a no-op on non-Windows platforms.
// On Windows it restricts the state directory to SYSTEM/Administrators.
func SecureDir(path string) error {
	_ = path
	return nil
}
