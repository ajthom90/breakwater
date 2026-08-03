package web

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ajthom90/breakwater/server/internal/audit"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/enroll"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/rescan"
	"github.com/ajthom90/breakwater/server/internal/retention"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/vault"
	"github.com/oklog/ulid/v2"
)

// API is the REST surface for the embedded UI and bwctl.
//
// M2 was GET-only. M4 adds audited mutating endpoints:
//
//	POST /api/v1/jobs   — submit agent jobs (file backup, restore, …)
//	POST /api/v1/rescan — rebuild snapshot index from vault manifests
//
// M5 retention (server-side only — never on :9443):
//
//	POST /api/v1/snapshots/{id}/forget
//	POST /api/v1/snapshots/{id}/undelete
//	POST /api/v1/machines/{id}/prune
//	POST /api/v1/machines/{id}/retention
//	POST /api/v1/machines/{id}/scrub
//
// Enrollment (operator path — not destructive):
//
//	POST /api/v1/enroll-tokens — mint one-time agent enrollment token
//	GET  /api/v1/enroll-tokens — list token metadata (never secrets)
//
// Auth remains the single dev API token (M6 replaces with sessions).
//
// # Destructive API gate (M5-F1)
//
// Forget / undelete / prune / retention-apply / scrub are opt-in until M6
// sessions exist. Production defaults EnableDestructiveAPI=false
// (--enable-destructive-api). The read token alone must not grant destroy.
// Audit still records actor/actorType when enabled so M6 can drop real
// identities in without changing call sites.
//
// Enroll-token mint is **not** behind EnableDestructiveAPI: minting creates a
// credential, it does not destroy backup data, and gating it there would force
// operators to enable destroy-capable endpoints just to enroll a machine.
type API struct {
	DB        *catalog.DB
	Auditor   *audit.Writer
	Events    *scheduler.EventHub
	Engine    *scheduler.Engine
	Vaults    *vault.Manager
	Keystore  *keystore.Store
	Retention *retention.Service
	// EnableDestructiveAPI gates M5 retention-mutating endpoints (default false).
	EnableDestructiveAPI bool
	// ServerFP is embedded in mint responses (running server identity).
	ServerFP string
	Version  string
	Log      *slog.Logger
}

// Register mounts /api/v1/* handlers on mux behind auth middleware.
// Caller must wrap with RequireAPIToken before or use Mount.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/summary", a.handleSummary)
	mux.HandleFunc("GET /api/v1/machines", a.handleListMachines)
	mux.HandleFunc("GET /api/v1/machines/{id}", a.handleGetMachine)
	mux.HandleFunc("GET /api/v1/jobs", a.handleListJobs)
	mux.HandleFunc("POST /api/v1/jobs", a.handleSubmitJob)
	mux.HandleFunc("GET /api/v1/snapshots", a.handleListSnapshots)
	mux.HandleFunc("GET /api/v1/audit", a.handleListAudit)
	mux.HandleFunc("GET /api/v1/events", a.handleSSE)
	mux.HandleFunc("POST /api/v1/rescan", a.handleRescan)
	// Enrollment tokens: mint is not destructive (see API doc comment).
	// Gated only by RequireAPIToken (via Mount), never by --enable-destructive-api.
	mux.HandleFunc("POST /api/v1/enroll-tokens", a.handleMintEnrollToken)
	mux.HandleFunc("GET /api/v1/enroll-tokens", a.handleListEnrollTokens)
	// M5 retention — human-authenticated :8443 only; further gated by
	// EnableDestructiveAPI (M5-F1). Always register so paths 403 cleanly when off.
	mux.HandleFunc("POST /api/v1/snapshots/{id}/forget", a.requireDestructive(a.handleForget))
	mux.HandleFunc("POST /api/v1/snapshots/{id}/undelete", a.requireDestructive(a.handleUndelete))
	mux.HandleFunc("POST /api/v1/machines/{id}/prune", a.requireDestructive(a.handlePrune))
	mux.HandleFunc("POST /api/v1/machines/{id}/retention", a.requireDestructive(a.handleApplyRetention))
	mux.HandleFunc("POST /api/v1/machines/{id}/scrub", a.requireDestructive(a.handleScrub))
	mux.HandleFunc("GET /api/v1/policies", a.handleListPolicies)
}

// requireDestructive rejects retention-mutating calls when the opt-in is off.
func (a *API) requireDestructive(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.EnableDestructiveAPI {
			writeJSONError(w, http.StatusForbidden,
				"destructive retention API disabled; pass --enable-destructive-api (pre-M6; see PROGRESS.md)")
			return
		}
		next(w, r)
	}
}

// Mount registers API routes under auth on the given mux.
func (a *API) Mount(mux *http.ServeMux, token string) {
	apiMux := http.NewServeMux()
	a.Register(apiMux)
	mux.Handle("/api/v1/", RequireAPIToken(token)(apiMux))
}

// --- response DTOs (JSON snake_case for the UI) ---

type machineDTO struct {
	ID           string  `json:"id"`
	Hostname     string  `json:"hostname"`
	OS           string  `json:"os"`
	Status       string  `json:"status"`
	LastSeen     *string `json:"last_seen"`
	AgentVersion string  `json:"agent_version"`
}

type inventoryDTO struct {
	Kind       string         `json:"kind"`
	ExternalID string         `json:"external_id"`
	Name       string         `json:"name"`
	Details    map[string]any `json:"details,omitempty"`
	RCTCapable bool           `json:"rct_capable"`
}

type machineDetailDTO struct {
	machineDTO
	Inventory []inventoryDTO `json:"inventory"`
}

type jobDTO struct {
	ID           string  `json:"id"`
	MachineID    string  `json:"machine_id"`
	Type         string  `json:"type"`
	State        string  `json:"state"`
	StartedAt    *string `json:"started_at"`
	FinishedAt   *string `json:"finished_at"`
	BytesRead    int64   `json:"bytes_read"`
	BytesStored  int64   `json:"bytes_stored"`
	ErrorMessage string  `json:"error_message,omitempty"`
	CreatedAt    string  `json:"created_at"`
}

type snapshotDTO struct {
	ID          string  `json:"id"`
	MachineID   string  `json:"machine_id"`
	Kind        string  `json:"kind"`
	Source      string  `json:"source"`
	BytesRead   int64   `json:"bytes_read"`
	BytesStored int64   `json:"bytes_stored"`
	JobID       string  `json:"job_id,omitempty"`
	CreatedAt   string  `json:"created_at"`
	VerifyState string  `json:"verify_state"`
	DeletedAt   *string `json:"deleted_at,omitempty"`
}

type auditDTO struct {
	ID        string `json:"id"`
	TS        string `json:"ts"`
	Actor     string `json:"actor"`
	ActorType string `json:"actor_type"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Detail    string `json:"detail"`
}

type summaryDTO struct {
	MachinesTotal   int `json:"machines_total"`
	MachinesOnline  int `json:"machines_online"`
	MachinesOffline int `json:"machines_offline"`
	JobsLast24h     int `json:"jobs_last_24h"`
	JobsSuccess24h  int `json:"jobs_success_24h"`
	JobsFailed24h   int `json:"jobs_failed_24h"`
	JobsRunning     int `json:"jobs_running"`
	SnapshotsTotal  int `json:"snapshots_total"`
	// Capacity is null until vault stats aggregation lands (post-M2).
	CapacityBytes *int64 `json:"capacity_bytes"`
	// Explicit UI marker so placeholders are never mistaken for real data.
	CapacityNote string `json:"capacity_note"`
}

func (a *API) handleSummary(w http.ResponseWriter, r *http.Request) {
	s, err := a.DB.Summary(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "summary failed")
		return
	}
	writeJSON(w, http.StatusOK, summaryDTO{
		MachinesTotal:   s.MachinesTotal,
		MachinesOnline:  s.MachinesOnline,
		MachinesOffline: s.MachinesOffline,
		JobsLast24h:     s.JobsLast24h,
		JobsSuccess24h:  s.JobsSuccess24h,
		JobsFailed24h:   s.JobsFailed24h,
		JobsRunning:     s.JobsRunning,
		SnapshotsTotal:  s.SnapshotsTotal,
		CapacityBytes:   nil,
		CapacityNote:    "Not implemented in M2 — capacity stats require vault aggregation",
	})
}

func (a *API) handleListMachines(w http.ResponseWriter, r *http.Request) {
	machines, err := a.DB.ListMachines(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list machines failed")
		return
	}
	out := make([]machineDTO, 0, len(machines))
	for _, m := range machines {
		out = append(out, toMachineDTO(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"machines": out})
}

func (a *API) handleGetMachine(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "machine id required")
		return
	}
	m, err := a.DB.MachineByID(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if m == nil {
		writeJSONError(w, http.StatusNotFound, "machine not found")
		return
	}
	inv, err := a.DB.ListMachineInventory(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "inventory failed")
		return
	}
	items := make([]inventoryDTO, 0, len(inv))
	for _, it := range inv {
		items = append(items, inventoryDTO{
			Kind: it.Kind, ExternalID: it.ExternalID, Name: it.Name,
			Details: it.Details, RCTCapable: it.RCTCapable,
		})
	}
	writeJSON(w, http.StatusOK, machineDetailDTO{
		machineDTO: toMachineDTO(*m),
		Inventory:  items,
	})
}

func (a *API) handleListJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	jobs, err := a.DB.ListJobs(r.Context(), catalog.JobListFilter{
		MachineID: q.Get("machine_id"),
		State:     q.Get("state"),
		Limit:     limit,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list jobs failed")
		return
	}
	out := make([]jobDTO, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, toJobDTO(j))
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

// submitJobRequest is POST /api/v1/jobs body.
// type: file|restore|inventory|noop|… (catalog job type strings)
// For restore: params must include source_snapshot_id, target_path, conflict_policy;
// optional source_machine_id for cross-machine (target is machine_id).
type submitJobRequest struct {
	MachineID string          `json:"machine_id"`
	Type      string          `json:"type"`
	Params    json.RawMessage `json:"params"`
	Initiator string          `json:"initiator"`
}

func (a *API) handleSubmitJob(w http.ResponseWriter, r *http.Request) {
	if a.Engine == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "job engine not configured")
		return
	}
	var req submitJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.MachineID == "" || req.Type == "" {
		writeJSONError(w, http.StatusBadRequest, "machine_id and type required")
		return
	}
	// Map wire-friendly names to catalog types.
	jobType := req.Type
	switch req.Type {
	case "file", "file_backup", "JOB_TYPE_FILE_BACKUP":
		jobType = scheduler.TypeFileBackup
	case "restore", "JOB_TYPE_RESTORE":
		jobType = scheduler.TypeRestore
	case "inventory":
		jobType = scheduler.TypeInventory
	case "noop":
		jobType = scheduler.TypeNoop
	}
	params := "{}"
	if len(req.Params) > 0 {
		params = string(req.Params)
	}
	initiator := req.Initiator
	if initiator == "" {
		initiator = "api"
	}
	jobID, err := a.Engine.Submit(r.Context(), scheduler.SubmitRequest{
		MachineID:  req.MachineID,
		Type:       jobType,
		ParamsJSON: params,
		Initiator:  initiator,
	})
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if a.Auditor != nil {
		_ = a.Auditor.Append(r.Context(), audit.Event{
			Actor: initiator, ActorType: audit.ActorUser,
			Action: audit.ActionJobRunManual, Target: jobID,
			Detail: map[string]any{
				"machine_id": req.MachineID,
				"type":       jobType,
			},
		})
	}
	writeJSON(w, http.StatusCreated, map[string]any{"job_id": jobID, "type": jobType})
}

func (a *API) handleRescan(w http.ResponseWriter, r *http.Request) {
	if a.Vaults == nil || a.Keystore == nil || a.DB == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "rescan not configured")
		return
	}
	res, err := rescan.Run(r.Context(), rescan.Options{
		DB: a.DB, Keystore: a.Keystore, Vaults: a.Vaults, Log: a.Log,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if a.Auditor != nil {
		_ = a.Auditor.Append(r.Context(), audit.Event{
			Actor: "api", ActorType: audit.ActorUser,
			Action: audit.ActionCatalogRescan, Target: "snapshots",
			Detail: map[string]any{
				"machines_scanned": res.MachinesScanned,
				"snapshots_found":  res.SnapshotsFound,
				"snapshots_added":  res.SnapshotsAdded,
				"errors":           len(res.Errors),
			},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"machines_scanned": res.MachinesScanned,
		"snapshots_found":  res.SnapshotsFound,
		"snapshots_added":  res.SnapshotsAdded,
		"errors":           res.Errors,
	})
}

func (a *API) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	snaps, err := a.DB.ListSnapshots(r.Context(), catalog.SnapshotListFilter{
		MachineID: q.Get("machine_id"),
		Limit:     limit,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list snapshots failed")
		return
	}
	out := make([]snapshotDTO, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, snapshotDTO{
			ID: s.ID, MachineID: s.MachineID, Kind: s.Kind, Source: s.Source,
			BytesRead: s.BytesRead, BytesStored: s.BytesStored, JobID: s.JobID,
			CreatedAt: formatTime(s.CreatedAt), VerifyState: s.VerifyState,
			DeletedAt: formatTimePtr(s.DeletedAt),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": out})
}

func (a *API) handleForget(w http.ResponseWriter, r *http.Request) {
	if a.Retention == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "retention not configured")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "snapshot id required")
		return
	}
	res, err := a.Retention.Forget(r.Context(), []string{id}, "api", audit.ActorUser, "", map[string]string{id: "manual"})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"forgotten": res.Forgotten, "mass_forget": res.Mass})
}

func (a *API) handleUndelete(w http.ResponseWriter, r *http.Request) {
	if a.Retention == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "retention not configured")
		return
	}
	id := r.PathValue("id")
	if err := a.Retention.Undelete(r.Context(), id, "api", audit.ActorUser); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"undeleted": id})
}

func (a *API) handlePrune(w http.ResponseWriter, r *http.Request) {
	if a.Retention == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "retention not configured")
		return
	}
	mid := r.PathValue("id")
	res, err := a.Retention.Prune(r.Context(), mid, "api", audit.ActorUser)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"machine_id":        res.MachineID,
		"eligible":          res.Eligible,
		"manifests_removed": res.ManifestsRemoved,
		"grace":             res.Grace.String(),
	})
}

func (a *API) handleApplyRetention(w http.ResponseWriter, r *http.Request) {
	if a.Retention == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "retention not configured")
		return
	}
	mid := r.PathValue("id")
	res, err := a.Retention.ApplyRetention(r.Context(), mid, "api", audit.ActorUser)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"forgotten": res.Forgotten, "mass_forget": res.Mass, "policy_id": res.PolicyID,
	})
}

func (a *API) handleScrub(w http.ResponseWriter, r *http.Request) {
	if a.Retention == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "retention not configured")
		return
	}
	mid := r.PathValue("id")
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = retention.ScrubSubset
	}
	res, err := a.Retention.Scrub(r.Context(), mid, mode, retention.DefaultScrubSlices)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"machine_id":         res.MachineID,
		"mode":               res.Mode,
		"slice":              res.Slice,
		"contents_checked":   res.ContentsChecked,
		"contents_failed":    res.ContentsFailed,
		"manifests_checked":  res.ManifestsChecked,
		"manifests_failed":   res.ManifestsFailed,
		"affected_snapshots": res.AffectedSnapshots,
		"errors":             res.Errors,
	})
}

func (a *API) handleListPolicies(w http.ResponseWriter, r *http.Request) {
	if a.DB == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "catalog not configured")
		return
	}
	pols, err := a.DB.ListPolicies(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": pols})
}

func (a *API) handleListAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	var events []audit.Record
	var chainOK bool
	var chainErr string
	if a.Auditor != nil {
		var err error
		events, err = a.Auditor.ListEvents(r.Context(), audit.ListFilter{Limit: limit})
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "list audit failed")
			return
		}
		if err := a.Auditor.VerifyChain(r.Context()); err != nil {
			chainOK = false
			chainErr = err.Error()
		} else {
			chainOK = true
		}
	}
	out := make([]auditDTO, 0, len(events))
	for _, e := range events {
		out = append(out, auditDTO{
			ID: e.ID, TS: e.TS, Actor: e.Actor, ActorType: e.ActorType,
			Action: e.Action, Target: e.Target, Detail: e.Detail,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events":    out,
		"chain_ok":  chainOK,
		"chain_err": chainErr,
	})
}

// mintEnrollTokenRequest is POST /api/v1/enroll-tokens body.
//
// advertise_addr is the host:port the *agent* will dial (often a LAN IP of the
// TrueNAS box), not the server's own bind address. Required; never guessed from
// the HTTP request. Server fingerprint always comes from a.ServerFP.
type mintEnrollTokenRequest struct {
	AdvertiseAddr string `json:"advertise_addr"`
	TTLSeconds    int    `json:"ttl_seconds"` // optional; default 24h (PLAN)
	Note          string `json:"note"`        // optional; audit only
	CreatedBy     string `json:"created_by"`  // optional; defaults to "api"
}

func (a *API) handleMintEnrollToken(w http.ResponseWriter, r *http.Request) {
	if a.DB == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "catalog not configured")
		return
	}
	if a.ServerFP == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "server identity fingerprint not configured")
		return
	}
	var req mintEnrollTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	advertise := strings.TrimSpace(req.AdvertiseAddr)
	if advertise == "" {
		writeJSONError(w, http.StatusBadRequest, "advertise_addr required (host:port the agent will dial, e.g. 10.0.0.5:9443)")
		return
	}
	if _, _, err := net.SplitHostPort(advertise); err != nil {
		writeJSONError(w, http.StatusBadRequest, "advertise_addr must be host:port (got "+advertise+")")
		return
	}

	ttl := enroll.DefaultTTL
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	// Cap TTL to a sane upper bound (30d) to avoid permanent tokens by accident.
	const maxTTL = 30 * 24 * time.Hour
	if ttl > maxTTL {
		writeJSONError(w, http.StatusBadRequest, "ttl_seconds exceeds maximum (30 days)")
		return
	}

	raw, secret, err := enroll.Mint(advertise, a.ServerFP)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "mint failed")
		return
	}
	// secret is plaintext of the token secret portion only — hashed before store.
	id := ulid.MustNew(ulid.Timestamp(time.Now()), ulid.Monotonic(rand.Reader, 0)).String()
	expires := time.Now().UTC().Add(ttl)
	createdBy := strings.TrimSpace(req.CreatedBy)
	if createdBy == "" {
		createdBy = "api"
	}
	if note := strings.TrimSpace(req.Note); note != "" && createdBy == "api" {
		// Prefer note as created_by label when no explicit actor was given.
		createdBy = note
	}
	if err := a.DB.InsertEnrollToken(r.Context(), id, secret, createdBy, expires); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "persist token failed")
		return
	}

	// Log only id + advertise — never the token or secret.
	if a.Log != nil {
		a.Log.Info("enrollment token minted",
			"token_id", id,
			"advertise_addr", advertise,
			"expires_at", expires.Format(time.RFC3339),
			"created_by", createdBy,
		)
	}
	if a.Auditor != nil {
		// WithoutCancel: audit must survive client disconnect (S1-F1).
		_ = a.Auditor.Append(context.WithoutCancel(r.Context()), audit.Event{
			Actor: createdBy, ActorType: audit.ActorUser,
			Action: audit.ActionMachineTokenCreate, Target: id,
			Detail: map[string]any{
				"advertise_addr": advertise,
				"ttl_seconds":    int(ttl.Seconds()),
				"note":           strings.TrimSpace(req.Note),
				// secret intentionally omitted
			},
		})
	}

	// Full token string returned once; never again.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         id,
		"token":      raw,
		"expires_at": expires.Format(time.RFC3339Nano),
		"advertise":  advertise,
	})
}

func (a *API) handleListEnrollTokens(w http.ResponseWriter, r *http.Request) {
	if a.DB == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "catalog not configured")
		return
	}
	limit := 50
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			limit = n
		}
	}
	rows, err := a.DB.ListEnrollTokens(r.Context(), limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Never return secret or secret_hash.
	out := make([]map[string]any, 0, len(rows))
	for _, t := range rows {
		m := map[string]any{
			"id":         t.ID,
			"created_by": t.CreatedBy,
			"created_at": formatTime(t.CreatedAt),
			"expires_at": formatTime(t.ExpiresAt),
			"used":       t.UsedAt != nil,
		}
		if t.UsedAt != nil {
			m["used_at"] = formatTime(*t.UsedAt)
		}
		if t.MachineID != "" {
			m["machine_id"] = t.MachineID
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

func toMachineDTO(m catalog.Machine) machineDTO {
	return machineDTO{
		ID: m.ID, Hostname: m.Hostname, OS: m.OSInfo, Status: m.Status,
		LastSeen: formatTimePtr(m.LastSeenAt), AgentVersion: m.AgentVersion,
	}
}

func toJobDTO(j catalog.Job) jobDTO {
	return jobDTO{
		ID: j.ID, MachineID: j.MachineID, Type: j.Type, State: j.State,
		StartedAt: formatTimePtr(j.StartedAt), FinishedAt: formatTimePtr(j.FinishedAt),
		BytesRead: j.BytesRead, BytesStored: j.BytesStored,
		ErrorMessage: j.ErrorMessage, CreatedAt: formatTime(j.CreatedAt),
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func formatTimePtr(t *time.Time) *string {
	if t == nil || t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}
