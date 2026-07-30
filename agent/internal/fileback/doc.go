// Package fileback re-exports the portable file backup pipeline from pkg/backup.
// Stage-4 Windows agent should import pkg/backup (or this package) and layer
// VSS / BackupRead on top. Implementation lives in pkg so server tests share it.
package fileback

import "github.com/ajthom90/breakwater/pkg/backup"

// Aliases for callers that prefer the agent module path.
type (
	Client       = backup.Client
	Options      = backup.Options
	Stats        = backup.Stats
	ProgressFunc = backup.ProgressFunc
	GRPCClient   = backup.GRPCClient
)

// Run is backup.Run.
var Run = backup.Run
