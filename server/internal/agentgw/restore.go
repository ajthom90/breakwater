package agentgw

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ajthom90/breakwater/pkg/format"
	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
	"github.com/ajthom90/breakwater/server/internal/audit"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/vault"
	"github.com/oklog/ulid/v2"
)

// Stream chunk size for GetObject / GetContentRange responses.
const restoreStreamChunk = 1 << 20 // 1 MiB

// RestoreServer implements breakwater.v1.RestoreService — read-only restore path.
//
// Authorization (structural — M4 / REVIEW-M1 #3):
//
//   - Default: a machine may read only its own repo (PeerFromContext → machine).
//   - Cross-machine restore requires a running/cancelling JOB_TYPE_RESTORE whose
//     target is the caller and whose params name (source_snapshot_id,
//     source_machine_id). Reads are limited to that snapshot's reachable
//     object/content set — a job for snapshot X is not a read-anything ticket.
//   - Open streams hold a Shared lease on the source repo for their lifetime
//     (job lease for job-backed restore; ephemeral stream lease for own-repo
//     browse). Per-chunk revalidation mirrors S3-F2 so streams cannot outlive
//     the lease.
//
// Append-only: no mutating RPCs. Audit: restore.browse / restore.file (not
// per-chunk range reads — one event per operation).
type RestoreServer struct {
	breakwaterv1.UnimplementedRestoreServiceServer

	Engine   *scheduler.Engine
	Catalog  *catalog.DB
	Keystore *keystore.Store
	Vaults   *vault.Manager
	Auditor  *audit.Writer
	Log      *slog.Logger

	// reachCache holds precomputed reachable sets for restore jobs.
	// Entries are evicted when the job's vault lease is released (M4-F2) —
	// wired via Engine.OnJobTerminal so sets do not live for process lifetime.
	reachMu    sync.Mutex
	reachCache map[string]*reachableSet // jobID → set
}

// EvictReachCache drops the reachable set for jobID (M4-F2). Safe to call when
// no entry exists. Registered as Engine.OnJobTerminal from gateway wiring.
func (r *RestoreServer) EvictReachCache(jobID string) {
	if r == nil || jobID == "" {
		return
	}
	r.reachMu.Lock()
	defer r.reachMu.Unlock()
	delete(r.reachCache, jobID)
}

// ReachCacheHas reports whether jobID still has a cached reachable set (tests).
func (r *RestoreServer) ReachCacheHas(jobID string) bool {
	if r == nil {
		return false
	}
	r.reachMu.Lock()
	defer r.reachMu.Unlock()
	_, ok := r.reachCache[jobID]
	return ok
}

// reachableSet is the set of object IDs and content IDs a restore job may read.
type reachableSet struct {
	SnapshotID   string
	SourceRepo   string
	RootObjectID string
	Objects      map[string]struct{}
	Contents     map[string]struct{}
}

// ListSnapshots returns catalog snapshots the caller is allowed to see.
func (r *RestoreServer) ListSnapshots(ctx context.Context, req *breakwaterv1.ListSnapshotsRequest) (*breakwaterv1.ListSnapshotsResponse, error) {
	pi, err := r.requirePeer(ctx)
	if err != nil {
		return nil, err
	}
	wantMachine := req.GetMachineId()
	if wantMachine == "" {
		wantMachine = pi.MachineID
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 50
	}

	// Own machine: list own snapshots.
	if wantMachine == pi.MachineID {
		snaps, err := r.Catalog.ListSnapshotsByMachine(ctx, wantMachine, limit)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list snapshots: %v", err)
		}
		r.auditBrowse(ctx, pi.MachineID, "list", wantMachine, map[string]any{"count": len(snaps)})
		return &breakwaterv1.ListSnapshotsResponse{Snapshots: toProtoSnapshots(snaps)}, nil
	}

	// Cross-machine list: only via active restore job naming that source machine.
	jobs, err := r.Engine.ActiveRestoreJobs(ctx, pi.MachineID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "restore jobs: %v", err)
	}
	allowed := false
	var snapFilter string
	for _, j := range jobs {
		src := scheduler.SourceRepoFromParams(j.ParamsJSON)
		if src == "" {
			src = j.MachineID
		}
		if src == wantMachine {
			allowed = true
			snapFilter = scheduler.SnapshotIDFromParams(j.ParamsJSON)
			break
		}
	}
	if !allowed {
		return nil, status.Error(codes.PermissionDenied, "cannot list another machine's snapshots without a restore job")
	}
	snaps, err := r.Catalog.ListSnapshotsByMachine(ctx, wantMachine, limit)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list snapshots: %v", err)
	}
	// If job names a specific snapshot, only return that one.
	if snapFilter != "" {
		var filtered []catalog.Snapshot
		for _, s := range snaps {
			if s.ID == snapFilter || s.ManifestRef == snapFilter {
				filtered = append(filtered, s)
			}
		}
		snaps = filtered
	}
	r.auditBrowse(ctx, pi.MachineID, "list", wantMachine, map[string]any{"count": len(snaps), "cross": true})
	return &breakwaterv1.ListSnapshotsResponse{Snapshots: toProtoSnapshots(snaps)}, nil
}

// GetSnapshot returns one snapshot metadata row if authorized.
func (r *RestoreServer) GetSnapshot(ctx context.Context, req *breakwaterv1.GetSnapshotRequest) (*breakwaterv1.GetSnapshotResponse, error) {
	pi, err := r.requirePeer(ctx)
	if err != nil {
		return nil, err
	}
	sid := req.GetSnapshotId()
	if sid == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot_id required")
	}
	snap, err := r.lookupSnapshot(ctx, sid)
	if err != nil {
		return nil, err
	}
	if _, _, err := r.authorizeSnapshot(ctx, pi.MachineID, snap); err != nil {
		return nil, err
	}
	r.auditBrowse(ctx, pi.MachineID, "get", snap.ID, map[string]any{
		"source_machine": snap.MachineID,
	})
	return toProtoSnapshot(snap), nil
}

// GetObject streams object bytes (tree JSON or file content).
func (r *RestoreServer) GetObject(req *breakwaterv1.GetObjectRequest, stream breakwaterv1.RestoreService_GetObjectServer) error {
	ctx := stream.Context()
	pi, err := r.requirePeer(ctx)
	if err != nil {
		return err
	}
	oid := req.GetObjectId()
	if oid == "" {
		return status.Error(codes.InvalidArgument, "object_id required")
	}

	auth, lease, err := r.authorizeObject(ctx, pi.MachineID, oid)
	if err != nil {
		return err
	}
	// lease may be nil when job-backed (job holds the lease); non-nil for stream lease.
	if lease != nil {
		defer lease.Release()
	}

	v, err := r.openVault(ctx, auth.SourceRepo)
	if err != nil {
		return err
	}
	rc, err := v.OpenObject(ctx, vault.ObjectID(oid))
	if err != nil {
		return status.Errorf(codes.NotFound, "OpenObject: %v", err)
	}
	defer rc.Close()

	// One restore.file audit per GetObject (not per chunk).
	r.auditFile(ctx, pi.MachineID, oid, map[string]any{
		"source_repo": auth.SourceRepo,
		"job_id":      auth.JobID,
		"cross":       auth.SourceRepo != pi.MachineID,
	})

	buf := make([]byte, restoreStreamChunk)
	for {
		// Per-chunk lease revalidation (S3-F2 analog for restore streams).
		if err := r.revalidateLease(ctx, auth, lease); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return status.FromContextError(err).Err()
		}
		n, readErr := rc.Read(buf)
		if n > 0 {
			if err := stream.Send(&breakwaterv1.GetObjectResponse{Data: append([]byte(nil), buf[:n]...)}); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return status.Errorf(codes.Internal, "read object: %v", readErr)
		}
	}
}

// GetContentRange streams a slice of a content blob.
func (r *RestoreServer) GetContentRange(req *breakwaterv1.GetContentRangeRequest, stream breakwaterv1.RestoreService_GetContentRangeServer) error {
	ctx := stream.Context()
	pi, err := r.requirePeer(ctx)
	if err != nil {
		return err
	}
	cid := req.GetContentId()
	if cid == "" {
		return status.Error(codes.InvalidArgument, "content_id required")
	}
	offset := req.GetOffset()
	length := req.GetLength()
	if offset < 0 {
		return status.Error(codes.InvalidArgument, "offset must be >= 0")
	}

	auth, lease, err := r.authorizeContent(ctx, pi.MachineID, cid)
	if err != nil {
		return err
	}
	if lease != nil {
		defer lease.Release()
	}

	v, err := r.openVault(ctx, auth.SourceRepo)
	if err != nil {
		return err
	}
	data, err := v.GetContent(ctx, vault.ContentID(cid))
	if err != nil {
		return status.Errorf(codes.NotFound, "GetContent: %v", err)
	}
	if offset > int64(len(data)) {
		return status.Error(codes.InvalidArgument, "offset beyond content length")
	}
	end := int64(len(data))
	if length > 0 && offset+length < end {
		end = offset + length
	}
	slice := data[offset:end]

	// No per-range audit noise — covered by the restore operation (job / GetObject).
	// Documented: GetContentRange itself is not audited per call.

	for off := 0; off < len(slice); {
		if err := r.revalidateLease(ctx, auth, lease); err != nil {
			return err
		}
		n := restoreStreamChunk
		if off+n > len(slice) {
			n = len(slice) - off
		}
		if err := stream.Send(&breakwaterv1.GetContentRangeResponse{Data: append([]byte(nil), slice[off:off+n]...)}); err != nil {
			return err
		}
		off += n
	}
	return nil
}

// restoreAuth is the result of authorization for a restore read.
type restoreAuth struct {
	SourceRepo string
	JobID      string // empty when own-repo browse (stream lease)
	// Cross is true when reading another machine's repo via a restore job.
	Cross bool
	// Reach is non-nil for cross-machine: object/content must be in the set.
	Reach *reachableSet
}

func (r *RestoreServer) requirePeer(ctx context.Context) (PeerInfo, error) {
	pi, ok := PeerFromContext(ctx)
	if !ok || pi.MachineID == "" {
		return PeerInfo{}, status.Error(codes.PermissionDenied, "not enrolled")
	}
	if r.Engine == nil || r.Catalog == nil || r.Keystore == nil || r.Vaults == nil {
		return PeerInfo{}, status.Error(codes.Internal, "restore plane not configured")
	}
	return pi, nil
}

func (r *RestoreServer) lookupSnapshot(ctx context.Context, id string) (*catalog.Snapshot, error) {
	snap, err := r.Catalog.SnapshotByID(ctx, id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "snapshot lookup: %v", err)
	}
	if snap == nil {
		// Try manifest ref.
		snap, err = r.Catalog.SnapshotByManifestRef(ctx, id)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "snapshot lookup: %v", err)
		}
	}
	if snap == nil {
		return nil, status.Error(codes.NotFound, "unknown snapshot_id")
	}
	return snap, nil
}

// authorizeSnapshot returns auth for reading snapshot metadata / its tree.
func (r *RestoreServer) authorizeSnapshot(ctx context.Context, caller string, snap *catalog.Snapshot) (*restoreAuth, scheduler.Lease, error) {
	if snap.MachineID == caller {
		return &restoreAuth{SourceRepo: caller}, nil, nil
	}
	// Cross: need matching restore job.
	jobs, err := r.Engine.ActiveRestoreJobs(ctx, caller)
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "restore jobs: %v", err)
	}
	for _, j := range jobs {
		if !jobMatchesSnapshot(j, snap) {
			continue
		}
		leaseOK, repoID := r.Engine.VaultForJob(j.ID)
		if !leaseOK {
			continue
		}
		reach, err := r.reachableForJob(ctx, j, snap, repoID)
		if err != nil {
			return nil, nil, status.Errorf(codes.Internal, "reachability: %v", err)
		}
		return &restoreAuth{
			SourceRepo: repoID,
			JobID:      j.ID,
			Cross:      true,
			Reach:      reach,
		}, nil, nil
	}
	return nil, nil, status.Error(codes.PermissionDenied, "cannot read another machine's snapshot without a restore job")
}

func jobMatchesSnapshot(j catalog.Job, snap *catalog.Snapshot) bool {
	src := scheduler.SourceRepoFromParams(j.ParamsJSON)
	if src == "" {
		src = j.MachineID
	}
	if src != snap.MachineID {
		return false
	}
	sid := scheduler.SnapshotIDFromParams(j.ParamsJSON)
	if sid == "" {
		return false
	}
	return sid == snap.ID || sid == snap.ManifestRef
}

// authorizeObject resolves which repo may serve objectID for caller.
func (r *RestoreServer) authorizeObject(ctx context.Context, caller, objectID string) (*restoreAuth, scheduler.Lease, error) {
	// Prefer active restore jobs that include this object (covers cross + own-job).
	jobs, err := r.Engine.ActiveRestoreJobs(ctx, caller)
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "restore jobs: %v", err)
	}
	for _, j := range jobs {
		leaseOK, repoID := r.Engine.VaultForJob(j.ID)
		if !leaseOK {
			continue
		}
		snapID := scheduler.SnapshotIDFromParams(j.ParamsJSON)
		if snapID == "" {
			continue
		}
		snap, err := r.lookupSnapshot(ctx, snapID)
		if err != nil {
			continue
		}
		if !jobMatchesSnapshot(j, snap) {
			continue
		}
		reach, err := r.reachableForJob(ctx, j, snap, repoID)
		if err != nil {
			return nil, nil, status.Errorf(codes.Internal, "reachability: %v", err)
		}
		if _, ok := reach.Objects[objectID]; !ok {
			// Object not in this job's reachable set — try other jobs or own repo.
			continue
		}
		return &restoreAuth{
			SourceRepo: repoID,
			JobID:      j.ID,
			Cross:      repoID != caller,
			Reach:      reach,
		}, nil, nil
	}

	// Own-repo browse: stream-scoped shared lease.
	// Cross-machine without a matching job → deny (do not open other vaults).
	// If caller has any active cross restore job, objects outside reachability
	// for those jobs are still denied for foreign repos; own repo remains open.
	streamID := newStreamID()
	lease, err := r.Engine.AcquireStreamLease(ctx, caller, streamID)
	if err != nil {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "stream lease: %v", err)
	}
	// Verify object exists in own vault (NotFound vs PermissionDenied for foreign IDs).
	v, err := r.openVault(ctx, caller)
	if err != nil {
		lease.Release()
		return nil, nil, err
	}
	rc, err := v.OpenObject(ctx, vault.ObjectID(objectID))
	if err != nil {
		lease.Release()
		// If there is a cross job, a miss here might be an out-of-reach foreign object.
		if len(jobs) > 0 {
			return nil, nil, status.Error(codes.PermissionDenied, "object not in restore job reachable set")
		}
		return nil, nil, status.Errorf(codes.NotFound, "OpenObject: %v", err)
	}
	_ = rc.Close()
	return &restoreAuth{SourceRepo: caller}, lease, nil
}

func (r *RestoreServer) authorizeContent(ctx context.Context, caller, contentID string) (*restoreAuth, scheduler.Lease, error) {
	jobs, err := r.Engine.ActiveRestoreJobs(ctx, caller)
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "restore jobs: %v", err)
	}
	for _, j := range jobs {
		leaseOK, repoID := r.Engine.VaultForJob(j.ID)
		if !leaseOK {
			continue
		}
		snapID := scheduler.SnapshotIDFromParams(j.ParamsJSON)
		if snapID == "" {
			continue
		}
		snap, lerr := r.lookupSnapshot(ctx, snapID)
		if lerr != nil {
			continue
		}
		if !jobMatchesSnapshot(j, snap) {
			continue
		}
		reach, rerr := r.reachableForJob(ctx, j, snap, repoID)
		if rerr != nil {
			return nil, nil, status.Errorf(codes.Internal, "reachability: %v", rerr)
		}
		if _, ok := reach.Contents[contentID]; !ok {
			continue
		}
		return &restoreAuth{
			SourceRepo: repoID,
			JobID:      j.ID,
			Cross:      repoID != caller,
			Reach:      reach,
		}, nil, nil
	}

	streamID := newStreamID()
	lease, err := r.Engine.AcquireStreamLease(ctx, caller, streamID)
	if err != nil {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "stream lease: %v", err)
	}
	v, err := r.openVault(ctx, caller)
	if err != nil {
		lease.Release()
		return nil, nil, err
	}
	if _, err := v.GetContent(ctx, vault.ContentID(contentID)); err != nil {
		lease.Release()
		if len(jobs) > 0 {
			return nil, nil, status.Error(codes.PermissionDenied, "content not in restore job reachable set")
		}
		return nil, nil, status.Errorf(codes.NotFound, "GetContent: %v", err)
	}
	return &restoreAuth{SourceRepo: caller}, lease, nil
}

func (r *RestoreServer) revalidateLease(ctx context.Context, auth *restoreAuth, streamLease scheduler.Lease) error {
	if auth.JobID != "" {
		ok, _ := r.Engine.VaultForJob(auth.JobID)
		if !ok {
			return status.Error(codes.FailedPrecondition, "restore job lease no longer held")
		}
		// Job must still be running/cancelling.
		j, err := r.Catalog.JobByID(ctx, auth.JobID)
		if err != nil || j == nil {
			return status.Error(codes.FailedPrecondition, "restore job missing")
		}
		if j.State != catalog.JobStateRunning && j.State != catalog.JobStateCancelling {
			return status.Error(codes.FailedPrecondition, "restore job is not active")
		}
		return nil
	}
	// Stream lease: shared count on source repo must still be positive.
	if streamLease == nil {
		return status.Error(codes.FailedPrecondition, "no stream lease")
	}
	shared, _ := r.Engine.Locks.Held(auth.SourceRepo)
	if shared <= 0 {
		return status.Error(codes.FailedPrecondition, "stream lease lost")
	}
	return nil
}

func (r *RestoreServer) openVault(ctx context.Context, repoID string) (vault.Vault, error) {
	pw, err := r.Keystore.GetRepoPassword(ctx, repoID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "repo password: %v", err)
	}
	v, err := r.Vaults.Open(ctx, repoID, pw)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "vault open: %v", err)
	}
	return v, nil
}

// reachableForJob builds or returns the cached reachable object/content set.
func (r *RestoreServer) reachableForJob(ctx context.Context, j catalog.Job, snap *catalog.Snapshot, sourceRepo string) (*reachableSet, error) {
	r.reachMu.Lock()
	if r.reachCache == nil {
		r.reachCache = make(map[string]*reachableSet)
	}
	if s, ok := r.reachCache[j.ID]; ok {
		r.reachMu.Unlock()
		return s, nil
	}
	r.reachMu.Unlock()

	v, err := r.openVault(ctx, sourceRepo)
	if err != nil {
		return nil, err
	}
	set := &reachableSet{
		SnapshotID:   snap.ID,
		SourceRepo:   sourceRepo,
		RootObjectID: snap.RootObjectID,
		Objects:      make(map[string]struct{}),
		Contents:     make(map[string]struct{}),
	}
	if err := walkReachable(ctx, v, snap.RootObjectID, set); err != nil {
		return nil, err
	}

	r.reachMu.Lock()
	r.reachCache[j.ID] = set
	r.reachMu.Unlock()
	return set, nil
}

// walkReachable marks all object IDs and data content IDs reachable from root.
// Depth is bounded by format.MaxTreeDepth (shared with prune mark — M4-F1).
func walkReachable(ctx context.Context, v vault.Vault, rootOID string, set *reachableSet) error {
	if rootOID == "" {
		return fmt.Errorf("empty root object id")
	}
	set.Objects[rootOID] = struct{}{}
	raw, err := readVaultObject(ctx, v, vault.ObjectID(rootOID))
	if err != nil {
		return err
	}
	var tree format.TreeObject
	if err := json.Unmarshal(raw, &tree); err != nil {
		// Root might not be a tree if misconfigured; fail closed for reachability.
		return fmt.Errorf("decode root tree: %w", err)
	}
	return walkTreeReachable(ctx, v, &tree, set, 0, "")
}

func walkTreeReachable(ctx context.Context, v vault.Vault, tree *format.TreeObject, set *reachableSet, depth int, pathPrefix string) error {
	if depth > format.MaxTreeDepth {
		return fmt.Errorf("restore: tree depth exceeds %d (runaway guard); path=%q snapshot=%s — forget this snapshot or flatten the source tree",
			format.MaxTreeDepth, pathPrefix, set.SnapshotID)
	}
	for _, ent := range tree.Entries {
		childPath := pathPrefix
		if childPath == "" {
			childPath = ent.Name
		} else if ent.Name != "" {
			childPath = pathPrefix + "/" + ent.Name
		}
		switch ent.Type {
		case format.EntryDir:
			if ent.ObjectID == "" {
				continue
			}
			set.Objects[ent.ObjectID] = struct{}{}
			raw, err := readVaultObject(ctx, v, vault.ObjectID(ent.ObjectID))
			if err != nil {
				return err
			}
			var child format.TreeObject
			if err := json.Unmarshal(raw, &child); err != nil {
				return err
			}
			if err := walkTreeReachable(ctx, v, &child, set, depth+1, childPath); err != nil {
				return err
			}
		case format.EntryFile:
			if ent.ObjectID != "" {
				set.Objects[ent.ObjectID] = struct{}{}
				// Stream-order content IDs (S5-F1) — not VerifyObject map order.
				ids, err := v.ObjectDataContentIDs(ctx, vault.ObjectID(ent.ObjectID))
				if err != nil {
					// Empty file / missing: still mark object; content optional.
					ids = nil
				}
				for _, id := range ids {
					set.Contents[string(id)] = struct{}{}
				}
			}
			for _, ads := range ent.ADS {
				if ads.ObjectID == "" {
					continue
				}
				set.Objects[ads.ObjectID] = struct{}{}
				ids, err := v.ObjectDataContentIDs(ctx, vault.ObjectID(ads.ObjectID))
				if err != nil {
					continue
				}
				for _, id := range ids {
					set.Contents[string(id)] = struct{}{}
				}
			}
		case format.EntrySymlink, format.EntryReparse:
			// No object payload.
		}
	}
	return nil
}

func readVaultObject(ctx context.Context, v vault.Vault, oid vault.ObjectID) ([]byte, error) {
	rc, err := v.OpenObject(ctx, oid)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func toProtoSnapshots(snaps []catalog.Snapshot) []*breakwaterv1.GetSnapshotResponse {
	out := make([]*breakwaterv1.GetSnapshotResponse, 0, len(snaps))
	for i := range snaps {
		out = append(out, toProtoSnapshot(&snaps[i]))
	}
	return out
}

func toProtoSnapshot(s *catalog.Snapshot) *breakwaterv1.GetSnapshotResponse {
	kind := breakwaterv1.SnapshotKind_SNAPSHOT_KIND_FILE
	switch s.Kind {
	case "image":
		kind = breakwaterv1.SnapshotKind_SNAPSHOT_KIND_IMAGE
	case "hyperv":
		kind = breakwaterv1.SnapshotKind_SNAPSHOT_KIND_HYPERV
	}
	return &breakwaterv1.GetSnapshotResponse{
		SnapshotId:   s.ID,
		Kind:         kind,
		RootObjectId: s.RootObjectID,
		Source:       s.Source,
		CreatedAt:    timestamppb.New(s.CreatedAt),
		MachineId:    s.MachineID,
	}
}

func (r *RestoreServer) auditBrowse(ctx context.Context, actor, op, target string, detail map[string]any) {
	if r.Auditor == nil {
		return
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["op"] = op
	if aerr := r.Auditor.Append(context.WithoutCancel(ctx), audit.Event{
		Actor: actor, ActorType: audit.ActorAgent,
		Action: audit.ActionRestoreBrowse, Target: target, Detail: detail,
	}); aerr != nil && r.Log != nil {
		r.Log.Error("audit append failed", "action", audit.ActionRestoreBrowse, "err", aerr)
	}
}

func (r *RestoreServer) auditFile(ctx context.Context, actor, target string, detail map[string]any) {
	if r.Auditor == nil {
		return
	}
	if aerr := r.Auditor.Append(context.WithoutCancel(ctx), audit.Event{
		Actor: actor, ActorType: audit.ActorAgent,
		Action: audit.ActionRestoreFile, Target: target, Detail: detail,
	}); aerr != nil && r.Log != nil {
		r.Log.Error("audit append failed", "action", audit.ActionRestoreFile, "err", aerr)
	}
}

func newStreamID() string {
	return ulid.MustNew(ulid.Timestamp(time.Now()), ulid.Monotonic(rand.Reader, 0)).String()
}
