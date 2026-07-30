// Package inventory reports local volumes (and later VMs) to the control plane.
// Windows: full volume enumeration. Non-Windows: best-effort mounts for CI/dev.
package inventory

import breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"

// Collect returns volumes available on this host.
// On non-Windows this is a degraded report of what we can see — never silent empty
// when something is known; callers send InventoryReport then JobResult.
func Collect() *breakwaterv1.InventoryReport {
	return &breakwaterv1.InventoryReport{
		Volumes: volumes(),
		Vms:     nil, // Hyper-V module is Phase 3
	}
}
