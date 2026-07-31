package retention

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ajthom90/breakwater/server/internal/audit"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/clock"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

// Auditor appends hash-chained audit events.
type Auditor interface {
	Append(ctx context.Context, e audit.Event) error
}

// Alerter delivers operator-facing alerts (email/webhook). Optional; when nil,
// scrub corruption still audits but does not notify.
// Implemented by *notify.Notifier in production.
type Alerter interface {
	AlertFailure(machine, jobID, errMsg string)
	AlertMissedBackup(machine string, lastSuccess time.Time, expectedWindow string)
	AlertCorruption(machine string, affectedSnapshots []string, detail string)
}

// Service implements forget / undelete / prune / apply-retention.
// All vault-touching paths take exclusive leases (same as prune/verify).
//
// Server-side only — never wire this into agent gRPC handlers.
type Service struct {
	DB       *catalog.DB
	Vaults   *vault.Manager
	Keystore *keystore.Store
	Locks    *scheduler.RepoLocks
	Clock    clock.Clock
	Auditor  Auditor
	// Notifier is optional; scrub corruption fires AlertCorruption when set.
	Notifier Alerter
	Log      *slog.Logger

	// MinContentAge for vault.Prune (default DefaultPruneMinContentAge).
	// Tests that must reclaim young data set this to 0.
	MinContentAge *time.Duration
}

func (s *Service) now() time.Time {
	if s.Clock == nil {
		// Fail closed: without an injected clock, refuse to evaluate retention.
		// Callers (including production) must wire clock.System().
		panic("retention.Service: Clock is nil — refuse to use wall clock implicitly")
	}
	return s.Clock.Now().UTC()
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// ForgetResult describes a forget operation for audit/logs.
type ForgetResult struct {
	Forgotten []string
	PolicyID  string
	Mass      bool
	// Reasons maps snapshot id → rule string when forget came from ApplyRetention.
	Reasons map[string]string
}

// Forget soft-deletes catalog rows. Does not touch the vault.
// actor is recorded in audit (user id or "system").
func (s *Service) Forget(ctx context.Context, snapshotIDs []string, actor, actorType, policyID string, reasons map[string]string) (*ForgetResult, error) {
	if len(snapshotIDs) == 0 {
		return &ForgetResult{Reasons: reasons}, nil
	}
	now := s.now()
	var forgotten []string
	for _, id := range snapshotIDs {
		snap, err := s.DB.SnapshotByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if snap == nil {
			continue
		}
		if snap.DeletedAt != nil {
			continue // already forgotten
		}
		if err := s.DB.SoftDeleteSnapshot(ctx, id, now); err != nil {
			return nil, fmt.Errorf("forget %s: %w", id, err)
		}
		forgotten = append(forgotten, id)
		s.log().Info("retention.forget",
			"snapshot_id", id,
			"machine_id", snap.MachineID,
			"policy_id", policyID,
			"reason", reasons[id],
			"deleted_at", now.Format(time.RFC3339Nano),
		)
	}
	mass := len(forgotten) >= MassForgetThreshold
	if s.Auditor != nil && len(forgotten) > 0 {
		detail := map[string]any{
			"snapshot_ids": forgotten,
			"count":        len(forgotten),
			"policy_id":    policyID,
			"mass_forget":  mass,
		}
		if reasons != nil {
			detail["reasons"] = reasons
		}
		_ = s.Auditor.Append(ctx, audit.Event{
			Actor: actor, ActorType: actorType,
			Action: audit.ActionRetentionForget,
			Target: forgotten[0],
			Detail: detail,
		})
	}
	return &ForgetResult{Forgotten: forgotten, PolicyID: policyID, Mass: mass, Reasons: reasons}, nil
}

// Undelete clears deleted_at if still within the grace window.
func (s *Service) Undelete(ctx context.Context, snapshotID, actor, actorType string) error {
	snap, err := s.DB.SnapshotByID(ctx, snapshotID)
	if err != nil {
		return err
	}
	if snap == nil {
		return fmt.Errorf("snapshot %s not found", snapshotID)
	}
	if snap.DeletedAt == nil {
		return fmt.Errorf("snapshot %s is not soft-deleted", snapshotID)
	}
	pol, err := s.policyFor(ctx, snap.MachineID)
	if err != nil {
		return err
	}
	grace := pol.Grace()
	deadline := snap.DeletedAt.Add(grace)
	if !s.now().Before(deadline) {
		return fmt.Errorf("snapshot %s grace window expired at %s (grace %s)", snapshotID, deadline.Format(time.RFC3339), grace)
	}
	ok, err := s.DB.UndeleteSnapshot(ctx, snapshotID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("undelete %s: no rows updated", snapshotID)
	}
	s.log().Info("retention.undelete", "snapshot_id", snapshotID, "machine_id", snap.MachineID)
	if s.Auditor != nil {
		_ = s.Auditor.Append(ctx, audit.Event{
			Actor: actor, ActorType: actorType,
			Action: audit.ActionRetentionUndelete,
			Target: snapshotID,
			Detail: map[string]any{"machine_id": snap.MachineID},
		})
	}
	return nil
}

// PruneResult records what a prune run removed.
type PruneResult struct {
	MachineID        string
	Eligible         []string // catalog ids past grace
	ManifestsRemoved int
	Grace            time.Duration
	MinContentAge    time.Duration
}

// Prune reclaims space for one machine's vault:
//  1. exclusive lease
//  2. delete vault manifests for soft-deleted snapshots past grace
//  3. vault.Prune (mark treats remaining manifests — including in-grace — as LIVE)
//  4. hard-delete those catalog rows
//
// Soft-deleted-but-within-grace snapshots are NOT eligible; their data survives.
func (s *Service) Prune(ctx context.Context, machineID, actor, actorType string) (*PruneResult, error) {
	if machineID == "" {
		return nil, fmt.Errorf("machine_id required")
	}
	if s.Vaults == nil || s.Keystore == nil {
		return nil, fmt.Errorf("vault/keystore not configured")
	}
	pol, err := s.policyFor(ctx, machineID)
	if err != nil {
		return nil, err
	}
	grace := pol.Grace()
	now := s.now()
	cutoff := now.Add(-grace)

	soft, err := s.DB.ListSoftDeletedSnapshots(ctx, machineID)
	if err != nil {
		return nil, err
	}
	var eligible []catalog.Snapshot
	for _, snap := range soft {
		if snap.DeletedAt == nil {
			continue
		}
		// Eligible only when deleted_at is at or before cutoff (fully past grace).
		if !snap.DeletedAt.After(cutoff) {
			eligible = append(eligible, snap)
		}
	}

	minAge := vault.DefaultPruneMinContentAge
	if s.MinContentAge != nil {
		minAge = *s.MinContentAge
	}

	result := &PruneResult{
		MachineID:     machineID,
		Grace:         grace,
		MinContentAge: minAge,
	}
	for _, e := range eligible {
		result.Eligible = append(result.Eligible, e.ID)
	}

	// Exclusive lease — same discipline as TypePrune (must not race backup/restore).
	locks := s.Locks
	if locks == nil {
		locks = scheduler.NewRepoLocks()
	}
	lease, err := locks.Acquire(ctx, machineID, scheduler.Exclusive, "retention-prune-"+machineID)
	if err != nil {
		return nil, fmt.Errorf("exclusive lease: %w", err)
	}
	defer lease.Release()

	pw, err := s.Keystore.GetRepoPassword(ctx, machineID)
	if err != nil {
		return nil, fmt.Errorf("repo password: %w", err)
	}
	v, err := s.Vaults.Open(ctx, machineID, pw)
	if err != nil {
		return nil, fmt.Errorf("open vault: %w", err)
	}

	for _, snap := range eligible {
		if snap.ManifestRef == "" {
			continue
		}
		if err := v.DeleteSnapshotRecord(ctx, vault.SnapshotRecordID(snap.ManifestRef)); err != nil {
			// Fail closed: do not continue reclaiming if a manifest delete fails.
			return nil, fmt.Errorf("delete vault manifest %s (catalog %s): %w", snap.ManifestRef, snap.ID, err)
		}
		result.ManifestsRemoved++
	}

	if err := v.Prune(ctx, vault.WithMinContentAge(minAge)); err != nil {
		return nil, fmt.Errorf("vault prune: %w", err)
	}

	for _, snap := range eligible {
		if err := s.DB.HardDeleteSnapshot(ctx, snap.ID); err != nil {
			return nil, fmt.Errorf("hard delete catalog %s: %w", snap.ID, err)
		}
	}

	s.log().Info("retention.prune_run",
		"machine_id", machineID,
		"eligible", len(eligible),
		"manifests_removed", result.ManifestsRemoved,
		"grace", grace.String(),
		"min_content_age", minAge.String(),
		"policy_id", pol.ID,
	)
	if s.Auditor != nil {
		_ = s.Auditor.Append(ctx, audit.Event{
			Actor: actor, ActorType: actorType,
			Action: audit.ActionRetentionPruneRun,
			Target: machineID,
			Detail: map[string]any{
				"eligible_ids":      result.Eligible,
				"count":             len(result.Eligible),
				"manifests_removed": result.ManifestsRemoved,
				"grace_seconds":     int(grace.Seconds()),
				"min_content_age_s": int(minAge.Seconds()),
				"policy_id":         pol.ID,
			},
		})
	}
	return result, nil
}

// ApplyRetention computes the keep-set for live (non-deleted) snapshots and
// forgets the complement. Does not prune.
func (s *Service) ApplyRetention(ctx context.Context, machineID, actor, actorType string) (*ForgetResult, error) {
	pol, err := s.policyFor(ctx, machineID)
	if err != nil {
		return nil, err
	}
	snaps, err := s.DB.ListSnapshotsByMachine(ctx, machineID, 10000)
	if err != nil {
		return nil, err
	}
	input := make([]Snapshot, 0, len(snaps))
	for _, sn := range snaps {
		input = append(input, Snapshot{ID: sn.ID, Timestamp: sn.CreatedAt})
	}
	ks := ComputeKeepSet(input, pol, s.now())
	reasons := make(map[string]string, len(ks.Forget))
	for _, id := range ks.Forget {
		reasons[id] = "not-in-keep-set"
	}
	// Enrich keep reasons for log (not in forget reasons).
	s.log().Info("retention.apply",
		"machine_id", machineID,
		"policy_id", pol.ID,
		"live", len(snaps),
		"keep", len(ks.KeepIDs),
		"forget_candidates", len(ks.Forget),
	)
	return s.Forget(ctx, ks.Forget, actor, actorType, pol.ID, reasons)
}

func (s *Service) policyFor(ctx context.Context, machineID string) (Policy, error) {
	cp, err := s.DB.PolicyForMachine(ctx, machineID)
	if err != nil {
		return Policy{}, err
	}
	if cp == nil {
		return StandardServer(), nil
	}
	return PolicyFromCatalog(cp), nil
}

// PolicyFromCatalog maps a catalog row to the pure retention Policy.
func PolicyFromCatalog(p *catalog.Policy) Policy {
	if p == nil {
		return StandardServer()
	}
	return Policy{
		ID:             p.ID,
		Name:           p.Name,
		ScheduleCron:   p.ScheduleCron,
		WindowStart:    p.WindowStart,
		WindowEnd:      p.WindowEnd,
		KeepLast:       p.KeepLast,
		KeepHourly:     p.KeepHourly,
		KeepDaily:      p.KeepDaily,
		KeepWeekly:     p.KeepWeekly,
		KeepMonthly:    p.KeepMonthly,
		KeepYearly:     p.KeepYearly,
		PruneGraceDays: p.PruneGraceDays,
		IsDefault:      p.IsDefault,
	}
}

// PruneEligible reports whether a soft-deleted snapshot is past grace at now.
func PruneEligible(deletedAt time.Time, grace time.Duration, now time.Time) bool {
	if grace < 0 {
		grace = 0
	}
	return !deletedAt.After(now.Add(-grace))
}
