//go:build windows

package service

import (
	"os"
	"path/filepath"

	"github.com/ajthom90/breakwater/agent/internal/state"
	"golang.org/x/sys/windows/registry"
)

// readPendingEnrollToken sources, in order (S4-F2):
//  1. stateDir/pending-enroll.token (SecureDir-restricted — preferred)
//  2. legacy HKLM\Software\Breakwater\Agent\PendingEnrollToken (migrate → file, delete key)
//
// UNTESTED ON WINDOWS until first real MSI install run — verify:
//   - token file mode/ACL under ProgramData\Breakwater
//   - registry value deleted (not blanked) after migrate/enroll
//   - msiexec /l*v logs redact BWTOKEN (MsiHiddenProperties)
func readPendingEnrollToken(stateDir string) string {
	if stateDir != "" {
		p := filepath.Join(stateDir, state.PendingEnrollTokenFile)
		if raw, err := os.ReadFile(p); err == nil && len(raw) > 0 {
			return string(raw)
		}
	}
	// Legacy registry path (pre-S4-F2 MSI).
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\Breakwater\Agent`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue("PendingEnrollToken")
	if err != nil || v == "" {
		return ""
	}
	// Migrate into SecureDir file and delete the world-readable registry value.
	if stateDir != "" {
		_ = os.WriteFile(filepath.Join(stateDir, state.PendingEnrollTokenFile), []byte(v), 0o600)
	}
	_ = k.DeleteValue("PendingEnrollToken")
	return v
}

// clearPendingEnrollToken deletes file + registry value (delete, not blank) (S4-F2).
func clearPendingEnrollToken(stateDir string) {
	if stateDir != "" {
		_ = os.Remove(filepath.Join(stateDir, state.PendingEnrollTokenFile))
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\Breakwater\Agent`, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	_ = k.DeleteValue("PendingEnrollToken")
}
