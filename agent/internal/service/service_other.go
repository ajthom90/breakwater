//go:build !windows

package service

import "log/slog"

func isWindowsService() bool { return false }

func runWindowsService(_ Config, _ *slog.Logger) error {
	panic("runWindowsService is Windows-only")
}
