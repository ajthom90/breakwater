package agentgw

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ajthom90/breakwater/pkg/format"
	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
	"github.com/ajthom90/breakwater/server/internal/audit"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/clock"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/vault"
	"github.com/oklog/ulid/v2"
)

// ClockSkewWarnThreshold is how far an agent-reported FinishedAt may diverge
// from the server clock before a warning is logged and audited (chaos drill #5 /
// PLAN: server clock governs snapshot timestamps).
const ClockSkewWarnThreshold = time.Hour

// MaxCheckContentsBatch is the have/want batch limit (proto / PLAN).
const MaxCheckContentsBatch = 4096

// ImageBlockSizeBytes is the fixed block size for image manifests (PLAN FIDX).
const ImageBlockSizeBytes = 4 << 20 // 4 MiB

// DataServer implements breakwater.v1.DataService — append-only agent data path.
//
// Security (structural):
//   - Machine binding: PeerFromContext cert → machine; job must belong to that
//     machine and be a vault-writing running/cancelling job.
//   - Vault access only via Engine.VaultForJob (lease-checked); never Manager.Open
//     without a held lease for the job.
//   - Cross-machine isolation: job of machine A is rejected for peer machine B;
//     HasContents only consults the caller's repo.
//   - Append-only: only Put/Has/Write/Commit-shaped vault calls.
//   - PutContents re-validates the lease on every message (S3-F2).
//   - PutContents computes the content ID before writing (S3-F3).
//   - Snapshot timestamps use the server Clock (chaos #5); agent FinishedAt is
//     advisory only and skew is warned when |Δ| > ClockSkewWarnThreshold.
type DataServer struct {
	breakwaterv1.UnimplementedDataServiceServer

	Engine   *scheduler.Engine
	Catalog  *catalog.DB
	Keystore *keystore.Store
	Vaults   *vault.Manager
	Auditor  *audit.Writer
	Log      *slog.Logger
	// Clock governs CommitSnapshot timestamps. Nil → clock.System().
	Clock clock.Clock
}

func (d *DataServer) now() time.Time {
	if d.Clock != nil {
		return d.Clock.Now().UTC()
	}
	return clock.System().Now().UTC()
}

// CheckContents is the have/want handshake. Batches of up to 4096 IDs.
func (d *DataServer) CheckContents(ctx context.Context, req *breakwaterv1.CheckContentsRequest) (*breakwaterv1.CheckContentsResponse, error) {
	v, _, err := d.vaultForJobRPC(ctx, req.GetJobId())
	if err != nil {
		return nil, err
	}
	ids := req.GetContentIds()
	if len(ids) > MaxCheckContentsBatch {
		return nil, status.Errorf(codes.InvalidArgument, "CheckContents batch %d exceeds max %d", len(ids), MaxCheckContentsBatch)
	}
	cids := make([]vault.ContentID, len(ids))
	for i, id := range ids {
		cids[i] = vault.ContentID(id)
	}
	present, err := v.HasContents(ctx, cids)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "HasContents: %v", err)
	}
	return &breakwaterv1.CheckContentsResponse{PresentBitmap: packBitmap(present)}, nil
}

// packBitmap packs bools into little-endian bytes (bit i set ⇒ present[i]).
func packBitmap(present []bool) []byte {
	if len(present) == 0 {
		return nil
	}
	out := make([]byte, (len(present)+7)/8)
	for i, p := range present {
		if p {
			out[i/8] |= 1 << (i % 8)
		}
	}
	return out
}

// PutContents streams content payloads; server re-hashes and rejects mismatches.
// Lease is re-validated on every message (S3-F2). Content ID is computed before
// any vault write (S3-F3).
func (d *DataServer) PutContents(stream breakwaterv1.DataService_PutContentsServer) error {
	ctx := stream.Context()
	var jobID string
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if jobID == "" {
			jobID = req.GetJobId()
		} else if req.GetJobId() != "" && req.GetJobId() != jobID {
			_ = stream.Send(&breakwaterv1.PutContentsResponse{
				AckSeq: req.GetSeq(), Accepted: false, ErrorMessage: "job_id changed mid-stream",
			})
			continue
		}

		// Per-message lease + job validation (S3-F2): if the job terminated mid-stream
		// (cancel, disconnect), stop accepting and end cleanly.
		v, _, verr := d.vaultForJobRPC(ctx, jobID)
		if verr != nil {
			_ = stream.Send(&breakwaterv1.PutContentsResponse{
				AckSeq: req.GetSeq(), Accepted: false, ErrorMessage: verr.Error(),
			})
			return verr
		}

		clientID := req.GetContentId()
		data := req.GetData()
		if len(data) > vault.MaxPutContentBytes {
			_ = stream.Send(&breakwaterv1.PutContentsResponse{
				AckSeq: req.GetSeq(), Accepted: false,
				ErrorMessage: fmt.Sprintf("payload %d exceeds max %d", len(data), vault.MaxPutContentBytes),
			})
			continue
		}

		// S3-F3: compute expected ID before writing; reject mismatch without vault write.
		serverID, err := v.ComputeContentID(ctx, data)
		if err != nil {
			_ = stream.Send(&breakwaterv1.PutContentsResponse{
				AckSeq: req.GetSeq(), Accepted: false, ErrorMessage: err.Error(),
			})
			continue
		}
		if clientID != "" && clientID != string(serverID) {
			_ = stream.Send(&breakwaterv1.PutContentsResponse{
				AckSeq: req.GetSeq(), Accepted: false,
				ErrorMessage: fmt.Sprintf("content id mismatch: client=%s server=%s", clientID, serverID),
				ContentId:    string(serverID),
			})
			continue
		}

		storedID, err := v.PutContent(ctx, data)
		if err != nil {
			_ = stream.Send(&breakwaterv1.PutContentsResponse{
				AckSeq: req.GetSeq(), Accepted: false, ErrorMessage: err.Error(),
			})
			continue
		}
		// Existing ID is a no-op success (dedup) — PutContent is content-addressed.
		if err := stream.Send(&breakwaterv1.PutContentsResponse{
			AckSeq: req.GetSeq(), Accepted: true, ContentId: string(storedID),
		}); err != nil {
			return err
		}
	}
}

// PutTreeObject stores a directory tree object (JSON) OR materializes an object
// from content_ids already stored via PutContents (S3-F1 first-class field).
// tree_json and content_ids are mutually exclusive.
func (d *DataServer) PutTreeObject(ctx context.Context, req *breakwaterv1.PutTreeObjectRequest) (*breakwaterv1.PutTreeObjectResponse, error) {
	v, _, err := d.vaultForJobRPC(ctx, req.GetJobId())
	if err != nil {
		return nil, err
	}
	raw := req.GetTreeJson()
	cids := req.GetContentIds()

	if len(cids) > 0 && len(raw) > 0 {
		return nil, status.Error(codes.InvalidArgument, "PutTreeObject: tree_json and content_ids are mutually exclusive")
	}
	if len(cids) == 0 && len(raw) == 0 {
		return nil, status.Error(codes.InvalidArgument, "PutTreeObject: tree_json or content_ids required")
	}

	// First-class object materialization from content IDs (S3-F1).
	if len(cids) > 0 {
		ids := make([]vault.ContentID, len(cids))
		for i, id := range cids {
			ids[i] = vault.ContentID(id)
		}
		oid, err := v.ObjectFromContents(ctx, ids)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "ObjectFromContents: %v", err)
		}
		return &breakwaterv1.PutTreeObjectResponse{ObjectId: string(oid)}, nil
	}

	var tree format.TreeObject
	if err := strictJSONDecode(raw, &tree); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "tree_json: %v", err)
	}
	oid, err := v.WriteObject(ctx, vault.SplitterFixed4M, bytes.NewReader(raw))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "WriteObject: %v", err)
	}
	return &breakwaterv1.PutTreeObjectResponse{ObjectId: string(oid)}, nil
}

// PutImageManifest stores a fixed-block image manifest after strict validation.
// Enforces PLAN's 4 MiB fixed-block invariant (S3-F4).
func (d *DataServer) PutImageManifest(ctx context.Context, req *breakwaterv1.PutImageManifestRequest) (*breakwaterv1.PutImageManifestResponse, error) {
	v, _, err := d.vaultForJobRPC(ctx, req.GetJobId())
	if err != nil {
		return nil, err
	}
	raw := req.GetManifestJson()
	var man format.ImageManifest
	if err := strictJSONDecode(raw, &man); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "manifest_json: %v", err)
	}
	if man.BlockSize != 0 && man.BlockSize != ImageBlockSizeBytes {
		return nil, status.Errorf(codes.InvalidArgument,
			"image block_size must be %d (4 MiB); got %d", ImageBlockSizeBytes, man.BlockSize)
	}
	oid, err := v.WriteObject(ctx, vault.SplitterFixed4M, bytes.NewReader(raw))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "WriteObject: %v", err)
	}
	return &breakwaterv1.PutImageManifestResponse{ObjectId: string(oid)}, nil
}

// CommitSnapshot finalizes a snapshot record and mirrors it into the catalog.
//
// Timestamp authority (chaos drill #5 / PLAN): the server Clock always sets the
// vault SnapshotRecord.Timestamp and catalog created_at. Agent FinishedAt is
// recorded in Extra/audit only; large skew is warned so operators can fix the
// agent host clock without the agent skewing retention.
func (d *DataServer) CommitSnapshot(ctx context.Context, req *breakwaterv1.CommitSnapshotRequest) (*breakwaterv1.CommitSnapshotResponse, error) {
	v, job, err := d.vaultForJobRPCFull(ctx, req.GetJobId())
	if err != nil {
		return nil, err
	}
	kind, err := mapSnapshotKind(req.GetKind())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	serverTS := d.now()
	var agentTS time.Time
	var skew time.Duration
	skewWarned := false
	if req.GetFinishedAt() != nil {
		agentTS = req.GetFinishedAt().AsTime().UTC()
		skew = serverTS.Sub(agentTS)
		if skew < 0 {
			skew = -skew
		}
		if skew > ClockSkewWarnThreshold {
			skewWarned = true
			if d.Log != nil {
				d.Log.Warn("agent clock skew on CommitSnapshot; server clock governs",
					"machine_id", job.MachineID,
					"job_id", job.ID,
					"server_ts", serverTS.Format(time.RFC3339Nano),
					"agent_finished_at", agentTS.Format(time.RFC3339Nano),
					"skew", skew.String(),
				)
			}
		}
	}

	extra := map[string]string{}
	if !agentTS.IsZero() {
		extra["agent_finished_at"] = agentTS.Format(time.RFC3339Nano)
		if skewWarned {
			extra["agent_clock_skew"] = skew.String()
		}
	}

	rec := vault.SnapshotRecord{
		Kind:         kind,
		MachineID:    job.MachineID,
		Timestamp:    serverTS, // server clock — never agent FinishedAt
		RootObjectID: vault.ObjectID(req.GetRootObjectId()),
		Source:       req.GetSource(),
		JobID:        job.ID,
		Extra:        extra,
	}
	manifestID, err := v.PutSnapshotRecord(ctx, rec)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "PutSnapshotRecord: %v", err)
	}

	// Catalog mirror (rebuildable index) — created_at is serverTS (retention input).
	snapID := ulid.MustNew(ulid.Timestamp(serverTS), ulid.Monotonic(rand.Reader, 0)).String()
	catalogKind := catalogKind(kind)
	if d.Catalog != nil {
		if err := d.Catalog.InsertSnapshot(ctx, catalog.Snapshot{
			ID:           snapID,
			MachineID:    job.MachineID,
			Kind:         catalogKind,
			Source:       req.GetSource(),
			ManifestRef:  string(manifestID),
			RootObjectID: req.GetRootObjectId(),
			JobID:        job.ID,
			BytesRead:    req.GetBytesRead(),
			BytesStored:  req.GetBytesStored(),
			CreatedAt:    serverTS,
		}); err != nil {
			return nil, status.Errorf(codes.Internal, "catalog InsertSnapshot: %v", err)
		}
	}

	// Audit: snapshot.commit (not per-chunk).
	if d.Auditor != nil {
		detail := map[string]any{
			"manifest_ref":   string(manifestID),
			"root_object_id": req.GetRootObjectId(),
			"job_id":         job.ID,
			"kind":           catalogKind,
			"source":         req.GetSource(),
			"server_ts":      serverTS.Format(time.RFC3339Nano),
		}
		if !agentTS.IsZero() {
			detail["agent_finished_at"] = agentTS.Format(time.RFC3339Nano)
		}
		if skewWarned {
			detail["agent_clock_skew"] = skew.String()
			detail["clock_skew_warned"] = true
		}
		if aerr := d.Auditor.Append(context.WithoutCancel(ctx), audit.Event{
			Actor:     job.MachineID,
			ActorType: audit.ActorAgent,
			Action:    audit.ActionSnapshotCommit,
			Target:    snapID,
			Detail:    detail,
		}); aerr != nil && d.Log != nil {
			d.Log.Error("audit append failed", "action", audit.ActionSnapshotCommit, "err", aerr)
		}
	}

	return &breakwaterv1.CommitSnapshotResponse{
		SnapshotId:  snapID,
		ManifestRef: string(manifestID),
	}, nil
}

func mapSnapshotKind(k breakwaterv1.SnapshotKind) (vault.SnapshotKind, error) {
	switch k {
	case breakwaterv1.SnapshotKind_SNAPSHOT_KIND_FILE:
		return vault.KindFileSnapshot, nil
	case breakwaterv1.SnapshotKind_SNAPSHOT_KIND_IMAGE:
		return vault.KindImageSnapshot, nil
	default:
		return "", fmt.Errorf("unsupported snapshot kind %v", k)
	}
}

func catalogKind(k vault.SnapshotKind) string {
	switch k {
	case vault.KindFileSnapshot:
		return "file"
	case vault.KindImageSnapshot:
		return "image"
	default:
		return string(k)
	}
}

// vaultForJobRPC resolves peer machine, validates job, checks lease, opens vault.
func (d *DataServer) vaultForJobRPC(ctx context.Context, jobID string) (vault.Vault, string, error) {
	v, j, err := d.vaultForJobRPCFull(ctx, jobID)
	if err != nil {
		return nil, "", err
	}
	return v, j.ID, nil
}

func (d *DataServer) vaultForJobRPCFull(ctx context.Context, jobID string) (vault.Vault, *catalog.Job, error) {
	pi, ok := PeerFromContext(ctx)
	if !ok || pi.MachineID == "" {
		return nil, nil, status.Error(codes.PermissionDenied, "not enrolled")
	}
	if jobID == "" {
		return nil, nil, status.Error(codes.InvalidArgument, "job_id required")
	}
	if d.Engine == nil || d.Catalog == nil || d.Keystore == nil || d.Vaults == nil {
		return nil, nil, status.Error(codes.Internal, "data plane not configured")
	}

	j, err := d.Catalog.JobByID(ctx, jobID)
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "job lookup: %v", err)
	}
	if j == nil {
		return nil, nil, status.Error(codes.InvalidArgument, "unknown job_id")
	}
	// Cross-machine isolation: job must belong to caller's machine.
	if j.MachineID != pi.MachineID {
		return nil, nil, status.Error(codes.PermissionDenied, "job does not belong to this machine")
	}
	// Job must be vault-writing and active (running or cancelling — may still write).
	if !scheduler.HoldsVaultLease(j.Type) {
		return nil, nil, status.Error(codes.InvalidArgument, "job type does not allow vault writes")
	}
	if j.State != catalog.JobStateRunning && j.State != catalog.JobStateCancelling {
		return nil, nil, status.Error(codes.FailedPrecondition, "job is not in a vault-writing state")
	}

	// Structural lease check — data plane must not Open without engine lease.
	leaseOK, repoID := d.Engine.VaultForJob(jobID)
	if !leaseOK {
		return nil, nil, status.Error(codes.FailedPrecondition, "no vault lease held for job")
	}
	if repoID != j.MachineID {
		return nil, nil, status.Error(codes.Internal, "lease repo mismatch")
	}

	pw, err := d.Keystore.GetRepoPassword(ctx, j.MachineID)
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "repo password: %v", err)
	}
	v, err := d.Vaults.Open(ctx, j.MachineID, pw)
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "vault open: %v", err)
	}
	return v, j, nil
}

// strictJSONDecode mirrors vault's write-boundary discipline (DisallowUnknownFields + EOF).
func strictJSONDecode(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing data after JSON value")
		}
		return err
	}
	return nil
}
