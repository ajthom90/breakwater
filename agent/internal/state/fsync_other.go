//go:build !windows

package state

import "os"

// syncDir fsyncs the directory so the rename is durable (S4-F4).
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
