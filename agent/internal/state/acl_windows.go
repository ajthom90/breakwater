//go:build windows

package state

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// SecureDir restricts path ACL to SYSTEM and Administrators only.
//
// UNTESTED ON WINDOWS until first real CI/VM run — verify:
//   - icacls shows only SYSTEM + BUILTIN\Administrators
//   - inheritance disabled
//   - standard user cannot read identity files
func SecureDir(path string) error {
	if path == "" {
		return fmt.Errorf("empty path")
	}
	// Ensure exists first.
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		return fmt.Errorf("state dir: %w", err)
	}

	var entries []windows.EXPLICIT_ACCESS
	// SYSTEM full control
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return fmt.Errorf("SYSTEM SID: %w", err)
	}
	// Administrators full control
	adminsSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return fmt.Errorf("Administrators SID: %w", err)
	}

	for _, sid := range []*windows.SID{systemSID, adminsSID} {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}

	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("ACLFromEntries: %w", err)
	}

	// PROTECTED_DACL_SECURITY_INFORMATION = disable inheritance + protect DACL
	const protected = windows.PROTECTED_DACL_SECURITY_INFORMATION |
		windows.DACL_SECURITY_INFORMATION

	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		protected,
		nil, nil, acl, nil,
	); err != nil {
		return fmt.Errorf("SetNamedSecurityInfo: %w", err)
	}
	return nil
}
