//go:build !windows

package golden

import "fmt"

// generateWindows is never called on non-Windows (Generate short-circuits).
func generateWindows(root string, res *Result) error {
	return fmt.Errorf("generateWindows called on non-Windows")
}

func compareACL(_, _ string) error { return nil }
func compareADS(_, _ string) error { return nil }
