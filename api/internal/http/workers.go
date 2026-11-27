package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/auth"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/worker"
)

// workerHandler serves both the worker-agent protocol (worker-token auth) and
// the owner/admin management surface (session auth). The two surfaces are
// strictly separated: worker tokens never touch management, and sessions never
// speak for a worker.
type workerHandler struct {
	store  *worker.Store
	engine *core.Engine
	ids    *identity.Store
}

func newWorkerHandler(store *worker.Store, engine *core.Engine, ids *identity.Store) *workerHandler {
	return &workerHandler{store: store, engine: engine, ids: ids}
}

type heartbeatInput struct {
	Capabilities *core.Capabilities `json:"capabilities,omitempty"`
	Telemetry    *core.Telemetry    `json:"telemetry,omitempty"`
	RTTMS        float64            `json:"rtt_ms,omitempty"`
}

type reportInput struct {
	Fence     string     `json:"fence"`
	Phase     string     `json:"phase,omitempty"` // ack|running|exit|renew
	ExitCode  *int       `json:"exit_code,omitempty"`
	Started   bool       `json:"started,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	LogTail   string     `json:"log_tail,omitempty"`
}

type pollAssignment struct {
	WorkloadID string         `json:"workload_id"`
	Attempt    int            `json:"attempt"`
	Fence      string         `json:"fence"`
	Image      string         `json:"image"`
	Args       []string       `json:"args,omitempty"`
	Resources  core.Resources `json:"resources"`
	Deadline   time.Time      `json:"deadline"`
}

type pollStop struct {
	WorkloadID string `json:"workload_id"`
	Attempt    int    `json:"attempt"`
	Fence      string `json:"fence"`
}

type pollActive struct {
	WorkloadID string     `json:"workload_id"`
	Attempt    int        `json:"attempt"`
	Fence      string     `json:"fence"`
	State      core.State `json:"state"`
	LeaseUntil time.Time  `json:"lease_until"`
}

type pollResponse struct {
	Assignments   []pollAssignment   `json:"assignments"`
	Stop          []pollStop         `json:"stop"`
	Active        []pollActive       `json:"active"`
	MaxConcurrent int                `json:"max_concurrent"`
	Policy        *core.WorkerPolicy `json:"policy,omitempty"`
}

// authenticateWorker validates the worker bearer token and the path identity.
func (h *workerHandler) authenticateWorker(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	workerID, err := h.store.AuthenticateWorker(r.Context(), strings.TrimPrefix(header, prefix))
	if err != nil {
		return "", false
	}
	if pathID := r.PathValue("id"); pathID != "" && pathID != workerID {
		return "", false
	}
	return workerID, true
}

func workerAuthFailed(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, worker.ErrForbidden):
		apierr.WriteError(w, http.StatusForbidden, "worker_forbidden", "this worker may not perform that operation")
	case errors.Is(err, worker.ErrUnauthorized):
		apierr.WriteError(w, http.StatusUnauthorized, "worker_unauthorized", "worker credentials are invalid")
	default:
		slog.Warn("worker authentication failed", "error", err)
		apierr.WriteError(w, http.StatusUnauthorized, "worker_unauthorized", "worker credentials are invalid")
	}
}

// registerWorkerProtocol mounts the worker-agent endpoints. They live on the
// public mux because worker tokens are their own authentication scheme;
// management endpoints live on the session-authenticated mux behind the
// administrator guard.
func (h *workerHandler) registerWorkerProtocol(publicMux, mux *http.ServeMux, adminOnly func(http.HandlerFunc) http.HandlerFunc) {
	publicMux.HandleFunc("POST /v1/workers/enroll", h.Enroll)
	publicMux.HandleFunc("POST /v1/workers/{id}/heartbeat", h.heartbeat)
	publicMux.HandleFunc("POST /v1/workers/{id}/poll", h.poll)
	publicMux.HandleFunc("POST /v1/workers/{id}/assignments/{workload}/report", h.report)

	mux.HandleFunc("POST /v1/workers/enrollments", adminOnly(h.CreateEnrollment))
	mux.HandleFunc("GET /v1/workers/enrollments", adminOnly(h.ListEnrollments))
	mux.HandleFunc("DELETE /v1/workers/enrollments/{id}", adminOnly(h.RevokeEnrollment))
	mux.HandleFunc("GET /v1/workers", adminOnly(h.ListWorkers))
	mux.HandleFunc("GET /v1/workers/{id}", adminOnly(h.GetWorker))
	mux.HandleFunc("POST /v1/workers/{id}/approve", adminOnly(h.setWorkerStatus(core.WorkerStateApproved)))
	mux.HandleFunc("POST /v1/workers/{id}/revoke", adminOnly(h.setWorkerStatus(core.WorkerStateRevoked)))
	mux.HandleFunc("POST /v1/workers/{id}/drain", adminOnly(h.setWorkerStatus(core.WorkerStateDraining)))
	mux.HandleFunc("POST /v1/workers/{id}/enable", adminOnly(h.setWorkerStatus(core.WorkerStateApproved)))
	mux.HandleFunc("POST /v1/workers/{id}/disable", adminOnly(h.setWorkerStatus(core.WorkerStateDisabled)))
	mux.HandleFunc("PUT /v1/workers/{id}/policy", adminOnly(h.UpdatePolicy))
	mux.HandleFunc("GET /v1/scheduler/status", adminOnly(h.SchedulerStatus))
	mux.HandleFunc("GET /v1/workloads/{id}/explanation", h.Explanation)
	mux.HandleFunc("GET /v1/projects/{id}/scheduling-preferences", h.GetProjectPrefs)
	mux.HandleFunc("PUT /v1/projects/{id}/scheduling-preferences", h.PutProjectPrefs)
	mux.HandleFunc("GET /v1/projects/{id}/workers", h.ListProjectWorkers)
}

func (h *workerHandler) Enroll(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Token    string `json:"token"`
		Name     string `json:"name"`
		Hostname string `json:"hostname"`
	}
	if err := readJSON(w, r, 8<<10, &input); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid enrollment request")
		return
	}
	if input.Token == "" {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "an enrollment token is required")
		return
	}
	workerID, workerToken, approved, err := h.store.Enroll(r.Context(), input.Token, input.Name, input.Hostname)
	if err != nil {
		switch {
		case errors.Is(err, worker.ErrUnauthorized):
			apierr.WriteError(w, http.StatusUnauthorized, "enrollment_rejected", "the enrollment token is unknown, expired, used or revoked")
		case errors.Is(err, worker.ErrConflict):
			apierr.WriteError(w, http.StatusConflict, "enrollment_conflict", "the worker identity could not be created")
		default:
			slog.Warn("worker enrollment failed", "error", err)
			apierr.WriteError(w, http.StatusInternalServerError, "enrollment_failed", "enrollment could not be completed")
		}
		return
	}
	apierr.WriteJSON(w, http.StatusCreated, map[string]any{
		"worker_id":    workerID,
		"worker_token": workerToken,
		"approved":     approved,
	})
}

func (h *workerHandler) heartbeat(w http.ResponseWriter, r *http.Request) {
	workerID, ok := h.authenticateWorker(r)
	if !ok {
		apierr.WriteError(w, http.StatusUnauthorized, "worker_unauthorized", "worker credentials are invalid")
		return
	}
	var input heartbeatInput
	if err := readJSON(w, r, 64<<10, &input); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid heartbeat payload")
		return
	}
	if err := h.store.Heartbeat(r.Context(), workerID, input.Capabilities, input.Telemetry, input.RTTMS); err != nil {
		workerAuthFailed(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *workerHandler) poll(w http.ResponseWriter, r *http.Request) {
	workerID, ok := h.authenticateWorker(r)
	if !ok {
		apierr.WriteError(w, http.StatusUnauthorized, "worker_unauthorized", "worker credentials are invalid")
		return
	}
	info, err := h.store.RequireWorker(r.Context(), workerID)
	if err != nil {
		workerAuthFailed(w, err)
		return
	}
	if info.Status != core.WorkerStateApproved && info.Status != core.WorkerStateDraining {
		apierr.WriteError(w, http.StatusForbidden, "worker_forbidden", "worker is not approved for assignments")
		return
	}
	assignments, err := h.store.WorkerAssignments(r.Context(), workerID)
	if err != nil {
		slog.Warn("read worker assignments", "worker_id", workerID, "error", err)
		apierr.WriteError(w, http.StatusServiceUnavailable, "scheduler_unavailable", "assignments are temporarily unavailable")
		return
	}
	response := pollResponse{Assignments: []pollAssignment{}, Stop: []pollStop{}, Active: []pollActive{}}
	for _, item := range assignments {
		if item.Assignment == nil {
			continue
		}
		switch item.State {
		case core.Assigned:
			response.Assignments = append(response.Assignments, pollAssignment{
				WorkloadID: item.ID, Attempt: item.Attempt, Fence: item.Assignment.Fence,
				Image: item.Submission.Image, Args: item.Submission.Args,
				Resources: item.Submission.Resources, Deadline: item.Assignment.LeaseUntil,
			})
		case core.Starting, core.Running, core.Unhealthy:
			response.Active = append(response.Active, pollActive{
				WorkloadID: item.ID, Attempt: item.Attempt, Fence: item.Assignment.Fence,
				State: item.State, LeaseUntil: item.Assignment.LeaseUntil,
			})
			if item.CancelRequested || item.StopReason != "" {
				response.Stop = append(response.Stop, pollStop{
					WorkloadID: item.ID, Attempt: item.Attempt, Fence: item.Assignment.Fence,
				})
			}
		case core.Stopping:
			response.Stop = append(response.Stop, pollStop{
				WorkloadID: item.ID, Attempt: item.Attempt, Fence: item.Assignment.Fence,
			})
		}
	}
	if info.Policy.MaxConcurrent > 0 {
		response.MaxConcurrent = info.Policy.MaxConcurrent
		response.Policy = &info.Policy
	}
	apierr.WriteJSON(w, http.StatusOK, response)
}

var reportPhases = map[string]string{
	"ack":     core.ReportAck,
	"running": core.ReportStart,
	"exit":    core.ReportExit,
	"renew":   core.ReportRenew,
}

func (h *workerHandler) report(w http.ResponseWriter, r *http.Request) {
	workerID, ok := h.authenticateWorker(r)
	if !ok {
		apierr.WriteError(w, http.StatusUnauthorized, "worker_unauthorized", "worker credentials are invalid")
		return
	}
	if _, err := h.store.RequireWorker(r.Context(), workerID); err != nil {
		workerAuthFailed(w, err)
		return
	}
	var input reportInput
	if err := readJSON(w, r, 64<<10, &input); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid report payload")
		return
	}
	kind, known := reportPhases[strings.ToLower(strings.TrimSpace(input.Phase))]
	if !known {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "phase must be ack, running, exit or renew")
		return
	}
	workloadID := r.PathValue("workload")
	if workloadID == "" || input.Fence == "" {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "an assignment workload ID and fence token are required")
		return
	}
	payload, err := json.Marshal(struct {
		ExitCode  *int       `json:"exit_code,omitempty"`
		Started   bool       `json:"started,omitempty"`
		StartedAt *time.Time `json:"started_at,omitempty"`
		LogTail   string     `json:"log_tail,omitempty"`
	}{ExitCode: input.ExitCode, Started: input.Started, StartedAt: input.StartedAt, LogTail: input.LogTail})
	if err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid report payload")
		return
	}
	if err := h.store.RecordReport(r.Context(), workerID, workloadID, input.Fence, kind, payload); err != nil {
		if errors.Is(err, worker.ErrBadInput) {
			apierr.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		slog.Warn("record worker report", "worker_id", workerID, "error", err)
		apierr.WriteError(w, http.StatusServiceUnavailable, "report_unavailable", "reports are temporarily unavailable")
		return
	}
	apierr.WriteJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (h *workerHandler) CreateEnrollment(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name        string `json:"name"`
		TTLSeconds  int    `json:"ttl_seconds,omitempty"`
		AutoApprove bool   `json:"auto_approve,omitempty"`
	}
	if err := readJSON(w, r, 8<<10, &input); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid enrollment request")
		return
	}
	ttl := 15 * time.Minute
	if input.TTLSeconds != 0 {
		ttl = time.Duration(input.TTLSeconds) * time.Second
	}
	p, ok := auth.FromContext(r.Context())
	if !ok {
		apierr.WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return
	}
	enrollment, token, err := h.store.CreateEnrollment(r.Context(), p.User.ID, input.Name, ttl, input.AutoApprove)
	if err != nil {
		if errors.Is(err, worker.ErrBadInput) {
			apierr.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		slog.Warn("create enrollment", "error", err)
		apierr.WriteError(w, http.StatusInternalServerError, "enrollment_failed", "enrollment could not be created")
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, "", "worker.enrollment.create", "worker_enrollment", enrollment.ID, "success", enrollment.Name)
	}
	apierr.WriteJSON(w, http.StatusCreated, map[string]any{
		"enrollment": enrollment,
		"token":      token, // returned exactly once
		"expires_at": enrollment.ExpiresAt,
	})
}

func (h *workerHandler) ListEnrollments(w http.ResponseWriter, r *http.Request) {
	items, err := h.store.ListEnrollments(r.Context())
	if err != nil {
		slog.Warn("list enrollments", "error", err)
		apierr.WriteError(w, http.StatusServiceUnavailable, "enrollments_unavailable", "enrollments are temporarily unavailable")
		return
	}
	apierr.WriteJSON(w, http.StatusOK, items)
}

func (h *workerHandler) RevokeEnrollment(w http.ResponseWriter, r *http.Request) {
	if err := h.store.RevokeEnrollment(r.Context(), r.PathValue("id")); err != nil {
		if errors.Is(err, worker.ErrNotFound) {
			apierr.WriteError(w, http.StatusNotFound, "not_found", "enrollment not found or already used")
			return
		}
		slog.Warn("revoke enrollment", "error", err)
		apierr.WriteError(w, http.StatusServiceUnavailable, "enrollments_unavailable", "enrollments are temporarily unavailable")
		return
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *workerHandler) ListWorkers(w http.ResponseWriter, r *http.Request) {
	items, err := h.store.List(r.Context())
	if err != nil {
		slog.Warn("list workers", "error", err)
		apierr.WriteError(w, http.StatusServiceUnavailable, "workers_unavailable", "workers are temporarily unavailable")
		return
	}
	now := time.Now().UTC()
	out := make([]map[string]any, 0, len(items))
	for _, info := range items {
		out = append(out, workerView(info, now))
	}
	apierr.WriteJSON(w, http.StatusOK, out)
}

func (h *workerHandler) GetWorker(w http.ResponseWriter, r *http.Request) {
	info, err := h.store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, worker.ErrNotFound) {
		apierr.WriteError(w, http.StatusNotFound, "not_found", "worker not found")
		return
	}
	if err != nil {
		slog.Warn("get worker", "error", err)
		apierr.WriteError(w, http.StatusServiceUnavailable, "workers_unavailable", "workers are temporarily unavailable")
		return
	}
	apierr.WriteJSON(w, http.StatusOK, workerView(info, time.Now().UTC()))
}

func workerView(info worker.WorkerInfo, now time.Time) map[string]any {
	return map[string]any{
		"id":              info.ID,
		"name":            info.Name,
		"hostname":        info.Hostname,
		"state":           info.State(now),
		"status":          info.Status,
		"enrolled_at":     info.EnrolledAt,
		"last_heartbeat":  info.LastHeartbeat,
		"capabilities":    info.Capabilities,
		"telemetry":       info.Telemetry,
		"policy":          info.Policy,
		"latency_ms":      info.LatencyMS,
		"reliability":     info.Reliability,
		"token_last_used": info.TokenLastUsed,
	}
}

func (h *workerHandler) setWorkerStatus(status string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.FromContext(r.Context())
		if !ok {
			apierr.WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
			return
		}
		info, err := h.store.SetStatus(r.Context(), r.PathValue("id"), status)
		if errors.Is(err, worker.ErrNotFound) {
			apierr.WriteError(w, http.StatusNotFound, "not_found", "worker not found or already in that state")
			return
		}
		if errors.Is(err, worker.ErrBadInput) {
			apierr.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if err != nil {
			slog.Warn("set worker status", "error", err)
			apierr.WriteError(w, http.StatusServiceUnavailable, "workers_unavailable", "workers are temporarily unavailable")
			return
		}
		if h.ids != nil {
			_ = h.ids.RecordAudit(r.Context(), p.User.ID, "", "worker."+status, "worker", info.ID, "success", info.Name)
		}
		apierr.WriteJSON(w, http.StatusOK, workerView(info, time.Now().UTC()))
	}
}

func (h *workerHandler) UpdatePolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		apierr.WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return
	}
	var policy core.WorkerPolicy
	if err := readJSON(w, r, 16<<10, &policy); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid worker policy JSON")
		return
	}
	workerID := r.PathValue("id")
	if err := h.store.UpdatePolicy(r.Context(), workerID, policy); err != nil {
		if errors.Is(err, worker.ErrNotFound) {
			apierr.WriteError(w, http.StatusNotFound, "not_found", "worker not found")
			return
		}
		if errors.Is(err, worker.ErrBadInput) {
			apierr.WriteError(w, http.StatusBadRequest, "invalid_policy", err.Error())
			return
		}
		slog.Warn("update worker policy", "error", err)
		apierr.WriteError(w, http.StatusServiceUnavailable, "workers_unavailable", "workers are temporarily unavailable")
		return
	}
	if info, err := h.store.Get(r.Context(), workerID); err == nil && h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, "", "worker.policy.update", "worker", info.ID, "success", info.Name)
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// SchedulerStatus summarizes the durable queue, assignments and recent
// placement decisions for operators.
func (h *workerHandler) SchedulerStatus(w http.ResponseWriter, r *http.Request) {
	if h.engine == nil {
		apierr.WriteError(w, http.StatusServiceUnavailable, "scheduler_unavailable", "the core scheduler is not enabled")
		return
	}
	items, err := h.engine.Repo.List(r.Context(), true)
	if err != nil {
		slog.Warn("scheduler status", "error", err)
		apierr.WriteError(w, http.StatusServiceUnavailable, "scheduler_unavailable", "scheduler state is temporarily unavailable")
		return
	}
	now := time.Now().UTC()
	queueByProject := map[string]int{}
	queueByPriority := map[string]int{}
	oldest := time.Time{}
	type assignmentView struct {
		WorkloadID string     `json:"workload_id"`
		WorkerID   string     `json:"worker_id"`
		Attempt    int        `json:"attempt"`
		State      core.State `json:"state"`
		LeaseUntil time.Time  `json:"lease_until"`
		Acked      bool       `json:"acked"`
	}
	assignments := make([]assignmentView, 0, len(items))
	queueDepth := 0
	for _, item := range items {
		switch item.State {
		case core.Pending, core.Queued:
			queueDepth++
			queueByProject[item.ProjectID]++
			queueByPriority[strconv.Itoa(item.Submission.Priority)]++
			if oldest.IsZero() || item.CreatedAt.Before(oldest) {
				oldest = item.CreatedAt
			}
		default:
			if item.Assignment != nil {
				assignments = append(assignments, assignmentView{
					WorkloadID: item.ID, WorkerID: item.Assignment.WorkerID, Attempt: item.Attempt,
					State: item.State, LeaseUntil: item.Assignment.LeaseUntil,
					Acked: !item.Assignment.AckedAt.IsZero(),
				})
			}
		}
	}
	decisions, _ := h.store.RecentDecisions(r.Context(), 20)
	workers, _ := h.store.Workers(r.Context())
	workerViews := make([]map[string]any, 0, len(workers))
	for _, item := range workers {
		workerViews = append(workerViews, map[string]any{
			"id": item.ID, "state": item.State, "health": item.Health,
			"active_assignments": item.Active,
		})
	}
	oldestSeconds := 0.0
	if !oldest.IsZero() {
		oldestSeconds = now.Sub(oldest).Seconds()
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{
		"queue": map[string]any{
			"depth":                  queueDepth,
			"by_project":             queueByProject,
			"by_priority":            queueByPriority,
			"oldest_waiting_seconds": oldestSeconds,
		},
		"workers":     workerViews,
		"assignments": assignments,
		"decisions":   decisions,
	})
}

// Explanation returns the durable placement decisions for one workload.
func (h *workerHandler) Explanation(w http.ResponseWriter, r *http.Request) {
	if h.engine == nil || h.ids == nil {
		apierr.WriteError(w, http.StatusServiceUnavailable, "scheduler_unavailable", "explanations require the identity-aware core scheduler")
		return
	}
	id := r.PathValue("id")
	item, err := h.engine.Repo.Get(r.Context(), id)
	if err != nil {
		coreError(w, err)
		return
	}
	p, ok := auth.FromContext(r.Context())
	if !ok {
		apierr.WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return
	}
	if _, err := h.ids.Authorize(r.Context(), p, item.ProjectID, identity.PermWorkloadRead); err != nil {
		apierr.WriteError(w, http.StatusForbidden, "forbidden", "you do not have permission for this project workload")
		return
	}
	decisions, err := h.store.Decisions(r.Context(), id)
	if err != nil {
		slog.Warn("read decisions", "error", err)
		apierr.WriteError(w, http.StatusServiceUnavailable, "scheduler_unavailable", "explanations are temporarily unavailable")
		return
	}
	apierr.WriteJSON(w, http.StatusOK, decisions)
}

type schedulingPrefsInput struct {
	Mode             string   `json:"mode"`
	PreferredWorkers []string `json:"preferred_workers,omitempty"`
	Fallback         *bool    `json:"fallback,omitempty"`
}

// GetProjectPrefs returns the project's scheduling defaults.
func (h *workerHandler) GetProjectPrefs(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if !h.authorizeProjectRead(w, r, projectID) {
		return
	}
	prefs, err := h.store.ProjectPrefs(r.Context(), projectID)
	if err != nil {
		slog.Warn("read scheduling preferences", "error", err)
		apierr.WriteError(w, http.StatusServiceUnavailable, "scheduler_unavailable", "scheduling preferences are temporarily unavailable")
		return
	}
	apierr.WriteJSON(w, http.StatusOK, prefs)
}

// PutProjectPrefs stores the project's scheduling defaults. Manual device
// mapping is expressed as preferred mode with fallback disabled, or as
// per-submission manual placement.
func (h *workerHandler) PutProjectPrefs(w http.ResponseWriter, r *http.Request) {
	p, ok := h.authorizeProjectSetting(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	var input schedulingPrefsInput
	if err := readJSON(w, r, 8<<10, &input); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid scheduling preferences JSON")
		return
	}
	prefs, err := h.store.SaveProjectPrefs(r.Context(), r.PathValue("id"), input.Mode, input.PreferredWorkers, input.Fallback, p.User.ID)
	if err != nil {
		if errors.Is(err, worker.ErrBadInput) {
			apierr.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		slog.Warn("save scheduling preferences", "error", err)
		apierr.WriteError(w, http.StatusServiceUnavailable, "scheduler_unavailable", "scheduling preferences are temporarily unavailable")
		return
	}
	apierr.WriteJSON(w, http.StatusOK, prefs)
}

func (h *workerHandler) authorizeProjectSetting(w http.ResponseWriter, r *http.Request, projectID string) (identity.Principal, bool) {
	if h.ids == nil {
		p := auth.OperatorPrincipal()
		return p, true
	}
	p, ok := auth.FromContext(r.Context())
	if !ok {
		apierr.WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return identity.Principal{}, false
	}
	if _, err := h.ids.Authorize(r.Context(), p, projectID, identity.PermProjectUpdate); err != nil {
		apierr.WriteError(w, http.StatusForbidden, "forbidden", "project administrator permission is required")
		return identity.Principal{}, false
	}
	return p, true
}

func (h *workerHandler) authorizeProjectRead(w http.ResponseWriter, r *http.Request, projectID string) bool {
	if h.ids == nil {
		return true
	}
	p, ok := auth.FromContext(r.Context())
	if !ok {
		apierr.WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return false
	}
	if _, err := h.ids.Authorize(r.Context(), p, projectID, identity.PermProjectRead); err != nil {
		apierr.WriteError(w, http.StatusForbidden, "forbidden", "you do not have permission for this project")
		return false
	}
	return true
}

// ListProjectWorkers shows members which workers their project may use.
func (h *workerHandler) ListProjectWorkers(w http.ResponseWriter, r *http.Request) {
	if h.ids == nil {
		apierr.WriteError(w, http.StatusServiceUnavailable, "workers_unavailable", "workers require identity-aware mode")
		return
	}
	projectID := r.PathValue("id")
	p, ok := auth.FromContext(r.Context())
	if !ok {
		apierr.WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return
	}
	if _, err := h.ids.Authorize(r.Context(), p, projectID, identity.PermWorkloadRead); err != nil {
		apierr.WriteError(w, http.StatusForbidden, "forbidden", "you do not have permission for this project")
		return
	}
	items, err := h.store.List(r.Context())
	if err != nil {
		slog.Warn("list project workers", "error", err)
		apierr.WriteError(w, http.StatusServiceUnavailable, "workers_unavailable", "workers are temporarily unavailable")
		return
	}
	now := time.Now().UTC()
	out := make([]map[string]any, 0, len(items))
	for _, info := range items {
		if info.Status != core.WorkerStateApproved && info.Status != core.WorkerStateDraining {
			continue
		}
		permitted := len(info.Policy.Projects) == 0
		for _, project := range info.Policy.Projects {
			if project == projectID {
				permitted = true
				break
			}
		}
		if !permitted {
			continue
		}
		out = append(out, map[string]any{
			"id":      info.ID,
			"name":    info.Name,
			"state":   info.State(now),
			"runtime": runtimeSummary(info),
			"gpus":    gpuSummary(info),
			"policy":  publicPolicy(info.Policy),
		})
	}
	apierr.WriteJSON(w, http.StatusOK, out)
}

func runtimeSummary(info worker.WorkerInfo) map[string]any {
	if info.Capabilities == nil {
		return nil
	}
	summary := map[string]any{
		"os":   info.Capabilities.OS,
		"arch": info.Capabilities.Arch,
	}
	if info.Capabilities.LogicalCPUs != nil {
		summary["logical_cpus"] = *info.Capabilities.LogicalCPUs
	}
	if info.Capabilities.MemoryTotalBytes != nil {
		summary["memory_total_bytes"] = *info.Capabilities.MemoryTotalBytes
	}
	if info.Capabilities.MemoryAvailableBytes != nil {
		summary["memory_available_bytes"] = *info.Capabilities.MemoryAvailableBytes
	}
	return summary
}

func gpuSummary(info worker.WorkerInfo) []map[string]any {
	if info.Capabilities == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(info.Capabilities.GPUs))
	for _, gpu := range info.Capabilities.GPUs {
		out = append(out, map[string]any{
			"vendor":       gpu.Vendor,
			"model":        gpu.Model,
			"capabilities": gpu.Capabilities,
		})
	}
	return out
}

// publicPolicy hides the user allowlist from non-admin project views.
func publicPolicy(policy core.WorkerPolicy) core.WorkerPolicy {
	policy.Users = nil
	return policy
}
