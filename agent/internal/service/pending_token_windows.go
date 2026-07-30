//go:build windows

package service

import (
	"golang.org/x/sys/windows/registry"
)

// readPendingEnrollToken reads HKLM\Software\Breakwater\Agent\PendingEnrollToken
// written by the MSI when BWTOKEN= is supplied at install.
//
// UNTESTED ON WINDOWS until first real MSI install run.
func readPendingEnrollToken() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\Breakwater\Agent`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue("PendingEnrollToken")
	if err != nil || v == "" {
		return ""
	}
	return v
}

func clearPendingEnrollToken() {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\Breakwater\Agent`, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	_ = k.SetStringValue("PendingEnrollToken", "")
}
