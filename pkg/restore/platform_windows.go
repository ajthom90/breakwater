//go:build windows

package restore

// PlatformACLADSRestore applies NTFS security descriptors and alternate data
// streams after a file body is written. STUB: not implemented / untested.
//
// PLAN requires BackupWrite + SeRestorePrivilege for correct ACL/ADS restore.
// Do not claim this works until Windows VM evidence exists (PROGRESS).
//
// When implemented, wire via Options.PlatformRestore in the agent restore path.
var PlatformACLADSRestore func(path string, securityDescriptor string, ads []ADSRef) error

// ADSRef is a portable ADS descriptor for the platform hook.
type ADSRef struct {
	Name string
	Data []byte
}
