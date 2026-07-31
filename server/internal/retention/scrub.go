package retention

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"

	"github.com/ajthom90/breakwater/server/internal/audit"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

// Scrub modes.
const (
	// ScrubSubset verifies a rotating 1/Nth of contents plus all live manifests.
	ScrubSubset = "subset"
	// ScrubFull reads every content (monthly full read-back).
	ScrubFull = "full"
)

// DefaultScrubSlices is N for the daily 1/Nth rotating subset.
const DefaultScrubSlices = 7

// Verify state values stored on catalog snapshots (surfaced via read-only API).
const (
	VerifyNone    = "none"
	VerifyOK      = "ok"
	VerifyFailed  = "failed"
	VerifyPartial = "partial"
)

// ScrubResult is recorded for UI / digest.
type ScrubResult struct {
	MachineID        string
	Mode             string
	Slice            int
	ContentsChecked  int
	ContentsFailed   int
	ManifestsChecked int
	ManifestsFailed  int
	// AffectedSnapshots lists catalog IDs impacted by corruption.
	AffectedSnapshots []string
	Errors            []string
}

// Scrub runs verification for one machine under a **shared** lease.
//
// Why shared (not exclusive) — M5-F2:
//
//	Scrub is vault-read-only (GetContent / VerifyObject / OpenObject). Catalog
//	verify_state is the only write, and it does not touch the vault. Content is
//	immutable and content-addressed, so two shared holders (backup writer + scrub
//	reader) cannot conflict.
//
//	Prune exclusion comes free from the shared/exclusive discipline: prune takes
//	exclusive, and exclusive cannot be acquired while any shared holder exists.
//	A concurrent scrub therefore blocks prune without needing exclusive itself.
//
//	Holding exclusive for scrub would be actively harmful under S2-F3 writer
//	preference: an exclusive waiter blocks *new* shared acquisitions, so a
//	queued full monthly scrub would stall that machine's entire backup window.
//	Keep exclusive for prune and any future repair-style verify that mutates
//	the vault.
//
// Corruption detection records which snapshots are affected (not only that a
// chunk is bad) via content→snapshot ownership from VerifyObject walks.
func (s *Service) Scrub(ctx context.Context, machineID, mode string, slices int) (*ScrubResult, error) {
	if machineID == "" {
		return nil, fmt.Errorf("machine_id required")
	}
	if mode == "" {
		mode = ScrubSubset
	}
	if slices <= 0 {
		slices = DefaultScrubSlices
	}
	if s.Vaults == nil || s.Keystore == nil {
		return nil, fmt.Errorf("vault/keystore not configured")
	}

	locks := s.Locks
	if locks == nil {
		locks = scheduler.NewRepoLocks()
	}
	// Shared: vault-read-only; prune exclusion is structural (see package comment).
	lease, err := locks.Acquire(ctx, machineID, scheduler.Shared, "scrub-"+machineID)
	if err != nil {
		return nil, fmt.Errorf("shared lease: %w", err)
	}
	defer lease.Release()

	pw, err := s.Keystore.GetRepoPassword(ctx, machineID)
	if err != nil {
		return nil, err
	}
	v, err := s.Vaults.Open(ctx, machineID, pw)
	if err != nil {
		return nil, err
	}

	now := s.now()
	// Deterministic rotating slice: day-of-year mod N (UTC).
	slice := int(now.UTC().YearDay() % slices)
	res := &ScrubResult{MachineID: machineID, Mode: mode, Slice: slice}

	all, err := s.DB.ListAllSnapshotsByMachine(ctx, machineID, 100000)
	if err != nil {
		return nil, err
	}
	grace := DefaultGrace
	if pol, err := s.policyFor(ctx, machineID); err == nil {
		grace = pol.Grace()
	}

	type liveSnap struct {
		id, root string
	}
	var live []liveSnap
	for _, sn := range all {
		if sn.DeletedAt != nil && PruneEligible(*sn.DeletedAt, grace, now) {
			continue
		}
		live = append(live, liveSnap{id: sn.ID, root: sn.RootObjectID})
	}

	contentOwners := map[string][]string{}
	affected := map[string]struct{}{}

	for _, sn := range live {
		res.ManifestsChecked++
		if sn.root == "" {
			continue
		}
		if err := s.walkSnapContents(ctx, v, sn.root, sn.id, contentOwners, res, affected); err != nil {
			res.Errors = append(res.Errors, err.Error())
		}
	}

	for cid, owners := range contentOwners {
		if mode == ScrubSubset && contentSlice(cid, slices) != slice {
			continue
		}
		res.ContentsChecked++
		// GetContent auth-verifies (decrypt + MAC) on read.
		if _, err := v.GetContent(ctx, vault.ContentID(cid)); err != nil {
			res.ContentsFailed++
			res.Errors = append(res.Errors, fmt.Sprintf("GetContent %s: %v", cid, err))
			for _, o := range owners {
				affected[o] = struct{}{}
			}
		}
	}

	for id := range affected {
		res.AffectedSnapshots = append(res.AffectedSnapshots, id)
		_ = s.DB.SetSnapshotVerifyState(ctx, id, VerifyFailed)
	}
	if res.ContentsFailed == 0 && res.ManifestsFailed == 0 {
		for _, sn := range live {
			if _, bad := affected[sn.id]; bad {
				continue
			}
			state := VerifyOK
			if mode == ScrubSubset {
				state = VerifyPartial
			}
			_ = s.DB.SetSnapshotVerifyState(ctx, sn.id, state)
		}
	}

	s.log().Info("scrub complete",
		"machine_id", machineID, "mode", mode, "slice", slice,
		"contents_checked", res.ContentsChecked, "contents_failed", res.ContentsFailed,
		"manifests_checked", res.ManifestsChecked, "affected", len(res.AffectedSnapshots),
	)
	if s.Auditor != nil && (res.ContentsFailed > 0 || res.ManifestsFailed > 0) {
		_ = s.Auditor.Append(ctx, audit.Event{
			Actor: "system", ActorType: audit.ActorSystem,
			Action: "scrub.corruption",
			Target: machineID,
			Detail: map[string]any{
				"affected_snapshots": res.AffectedSnapshots,
				"contents_failed":    res.ContentsFailed,
				"manifests_failed":   res.ManifestsFailed,
			},
		})
	}
	return res, nil
}

func contentSlice(cid string, slices int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(cid))
	return int(h.Sum32() % uint32(slices))
}

func (s *Service) walkSnapContents(
	ctx context.Context, v vault.Vault, root, snapID string,
	owners map[string][]string, res *ScrubResult, affected map[string]struct{},
) error {
	type item struct {
		oid   string
		depth int
	}
	queue := []item{{oid: root, depth: 0}}
	seen := map[string]struct{}{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if _, ok := seen[cur.oid]; ok {
			continue
		}
		seen[cur.oid] = struct{}{}
		ids, err := v.VerifyObject(ctx, vault.ObjectID(cur.oid))
		if err != nil {
			res.ManifestsFailed++
			affected[snapID] = struct{}{}
			return fmt.Errorf("snapshot %s object %s: %w", snapID, cur.oid, err)
		}
		for _, cid := range ids {
			owners[string(cid)] = appendUnique(owners[string(cid)], snapID)
		}
		rc, err := v.OpenObject(ctx, vault.ObjectID(cur.oid))
		if err != nil {
			affected[snapID] = struct{}{}
			return fmt.Errorf("snapshot %s open %s: %w", snapID, cur.oid, err)
		}
		raw, err := io.ReadAll(io.LimitReader(rc, 16<<20))
		rc.Close()
		if err != nil {
			affected[snapID] = struct{}{}
			return err
		}
		for _, ch := range extractTreeChildOIDs(raw) {
			if cur.depth < 64 {
				queue = append(queue, item{oid: ch, depth: cur.depth + 1})
			}
		}
	}
	return nil
}

func appendUnique(ss []string, s string) []string {
	for _, x := range ss {
		if x == s {
			return ss
		}
	}
	return append(ss, s)
}

func extractTreeChildOIDs(raw []byte) []string {
	type entry struct {
		OID string `json:"oid"`
		ADS []struct {
			OID string `json:"oid"`
		} `json:"ads"`
	}
	type tree struct {
		Entries []entry `json:"entries"`
	}
	var t tree
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil
	}
	var out []string
	for _, e := range t.Entries {
		if e.OID != "" {
			out = append(out, e.OID)
		}
		for _, a := range e.ADS {
			if a.OID != "" {
				out = append(out, a.OID)
			}
		}
	}
	return out
}
