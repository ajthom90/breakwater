package web

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/ajthom90/breakwater/server/internal/audit"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/rescan"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

// API is the REST surface for the embedded UI and bwctl.
//
// M2 was GET-only. M4 adds audited mutating endpoints:
//
//	POST /api/v1/jobs   — submit agent jobs (file backup, restore, …)
//	POST /api/v1/rescan — rebuild snapshot index from vault manifests
//
// Auth remains the single dev API token (M6 replaces with sessions).
type API struct {
	DB       *catalog.DB
	Auditor  *audit.Writer
	Events   *scheduler.EventHub
	Engine   *scheduler.Engine
	Vaults   *vault.Manager
	Keystore *keystore.Store
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
	ID          string `json:"id"`
	MachineID   string `json:"machine_id"`
	Kind        string `json:"kind"`
	Source      string `json:"source"`
	BytesRead   int64  `json:"bytes_read"`
	BytesStored int64  `json:"bytes_stored"`
	JobID       string `json:"job_id,omitempty"`
	CreatedAt   string `json:"created_at"`
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
			CreatedAt: formatTime(s.CreatedAt),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": out})
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
