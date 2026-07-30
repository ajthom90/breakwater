//go:build !windows

package service

import (
	"os"
	"path/filepath"

	"github.com/ajthom90/breakwater/agent/internal/state"
)

// readPendingEnrollToken reads stateDir/pending-enroll.token (S4-F2).
func readPendingEnrollToken(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, state.PendingEnrollTokenFile))
	if err != nil {
		return ""
	}
	return string(raw)
}

// clearPendingEnrollToken deletes the pending token file (delete, not blank).
func clearPendingEnrollToken(stateDir string) {
	if stateDir == "" {
		return
	}
	_ = os.Remove(filepath.Join(stateDir, state.PendingEnrollTokenFile))
}
