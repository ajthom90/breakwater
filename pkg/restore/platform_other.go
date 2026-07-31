//go:build !windows

package restore

// PlatformACLADSRestore is nil on non-Windows. Windows ACL/ADS restore via
// BackupWrite is not implemented for the portable path and is marked
// untested-on-Windows until a VM proves it (M4 / PLAN).
var PlatformACLADSRestore func(path string, securityDescriptor string, ads []ADSRef) error

// ADSRef is a portable ADS descriptor for the platform hook.
type ADSRef struct {
	Name string
	Data []byte
}
