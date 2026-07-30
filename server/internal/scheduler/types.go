package scheduler

import breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"

// Catalog / engine job type strings. Open registry — stage 3 adds backup/restore/prune
// execution; stage 2 wires inventory + noop + the lock map for future types.
const (
	TypeInventory    = "inventory"
	TypeNoop         = "noop"
	TypeFileBackup   = "file"
	TypeImageBackup  = "image"
	TypeHyperVBackup = "hyperv"
	TypeRestore      = "restore"
	TypePrune        = "prune"
	TypeVerify       = "verify"
	TypeReplicate    = "replicate"
	TypeUpdate       = "update"
)

// MaxPendingJobsPerMachine is the offline dispatch queue bound (oldest-first).
// Submit rejects new agent-bound jobs when the machine already has this many
// pending. Documented for the stage-4 agent and operators.
const MaxPendingJobsPerMachine = 64

// AgentJobTypes are types that may be dispatched down ControlService.Channel.
// prune/verify/replicate are SERVER-side only — never offered on the channel.
var AgentJobTypes = map[string]bool{
	TypeInventory:    true,
	TypeNoop:         true,
	TypeFileBackup:   true,
	TypeImageBackup:  true,
	TypeHyperVBackup: true,
	TypeRestore:      true,
	TypeUpdate:       true,
}

// ServerOnlyJobTypes must never be dispatched to agents (append-only boundary).
var ServerOnlyJobTypes = map[string]bool{
	TypePrune:     true,
	TypeVerify:    true,
	TypeReplicate: true,
}

// IsAgentDispatchable reports whether a job type may leave the server on Channel.
func IsAgentDispatchable(jobType string) bool {
	return AgentJobTypes[jobType]
}

// IsServerOnly reports whether a job type is forbidden on the agent channel.
func IsServerOnly(jobType string) bool {
	return ServerOnlyJobTypes[jobType]
}

// WireJobType maps catalog type → frozen proto JobType.
// inventory/noop have no dedicated enum values; they use UNSPECIFIED and
// params_json {"kind":"<type>"} so the stage-4 agent can branch without a
// proto change (proto is frozen — see PROGRESS deviations).
func WireJobType(jobType string) breakwaterv1.JobType {
	switch jobType {
	case TypeFileBackup:
		return breakwaterv1.JobType_JOB_TYPE_FILE_BACKUP
	case TypeImageBackup:
		return breakwaterv1.JobType_JOB_TYPE_IMAGE_BACKUP
	case TypeHyperVBackup:
		return breakwaterv1.JobType_JOB_TYPE_HYPERV_BACKUP
	case TypeRestore:
		return breakwaterv1.JobType_JOB_TYPE_RESTORE
	case TypeUpdate:
		return breakwaterv1.JobType_JOB_TYPE_UPDATE
	default:
		// inventory, noop, and any future server-only types if mis-sent
		return breakwaterv1.JobType_JOB_TYPE_UNSPECIFIED
	}
}

// KnownJobType reports whether the engine accepts the type string.
func KnownJobType(jobType string) bool {
	switch jobType {
	case TypeInventory, TypeNoop,
		TypeFileBackup, TypeImageBackup, TypeHyperVBackup,
		TypeRestore, TypePrune, TypeVerify, TypeReplicate, TypeUpdate:
		return true
	default:
		return false
	}
}
