package web

import (
	"net/http"
	"strconv"
	"time"

	"github.com/ajthom90/breakwater/server/internal/audit"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
)

// API is the read-only REST surface for the embedded UI and future bwctl.
type API struct {
	DB      *catalog.DB
	Auditor *audit.Writer
	Events  *scheduler.EventHub
	Version string
}

// Register mounts /api/v1/* handlers on mux behind auth middleware.
// Caller must wrap with RequireAPIToken before or use Mount.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/summary", a.handleSummary)
	mux.HandleFunc("GET /api/v1/machines", a.handleListMachines)
	mux.HandleFunc("GET /api/v1/machines/{id}", a.handleGetMachine)
	mux.HandleFunc("GET /api/v1/jobs", a.handleListJobs)
	mux.HandleFunc("GET /api/v1/snapshots", a.handleListSnapshots)
	mux.HandleFunc("GET /api/v1/audit", a.handleListAudit)
	mux.HandleFunc("GET /api/v1/events", a.handleSSE)
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
