package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/registry"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/scheduler"
)

type JobHandler struct {
	reg   *registry.Registry
	queue *scheduler.Queue
}

func NewJobHandler(reg *registry.Registry, queue *scheduler.Queue) *JobHandler {
	return &JobHandler{
		reg:   reg,
		queue: queue,
	}
}

type jobCreateRequest struct {
	Image   string   `json:"image"`
	Command []string `json:"command,omitempty"`
}

type claimRequest struct {
	NodeID string `json:"node_id"`
}

type finishRequest struct {
	ExitCode *int   `json:"exit_code"`
	Logs     string `json:"logs,omitempty"`
}

func (h *JobHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req jobCreateRequest
	if err := readJSON(w, r, 64*1024, &req); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}

	if req.Image == "" || len(req.Image) > 255 || strings.ContainsAny(req.Image, " \t\n\r") {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid image")
		return
	}

	if len(req.Command) > 64 {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "command exceeds 64 items")
		return
	}

	job := h.queue.Create(req.Image, req.Command)
	apierr.WriteJSON(w, http.StatusCreated, job)
}

func (h *JobHandler) List(w http.ResponseWriter, r *http.Request) {
	jobs := h.queue.List()
	if jobs == nil {
		jobs = []scheduler.Job{}
	}
	apierr.WriteJSON(w, http.StatusOK, jobs)
}

func (h *JobHandler) Claim(w http.ResponseWriter, r *http.Request) {
	var req claimRequest
	if err := readJSON(w, r, 64*1024, &req); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}

	if req.NodeID == "" || !h.reg.Exists(req.NodeID) {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid node_id")
		return
	}

	job, ok := h.queue.Claim(req.NodeID)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	apierr.WriteJSON(w, http.StatusOK, job)
}

func (h *JobHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, err := h.queue.Get(id)
	if err != nil {
		if errors.Is(err, scheduler.ErrNotFound) {
			apierr.WriteError(w, http.StatusNotFound, "not_found", "job not found")
			return
		}
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	apierr.WriteJSON(w, http.StatusOK, job)
}

func (h *JobHandler) Finish(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req finishRequest
	if err := readJSON(w, r, 256*1024, &req); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}

	if req.ExitCode == nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "exit_code is required")
		return
	}

	if len(req.Logs) > 65536 {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "logs too long")
		return
	}

	job, err := h.queue.Finish(id, *req.ExitCode, req.Logs)
	if err != nil {
		if errors.Is(err, scheduler.ErrNotFound) {
			apierr.WriteError(w, http.StatusNotFound, "not_found", "job not found")
			return
		}
		if errors.Is(err, scheduler.ErrConflict) {
			apierr.WriteError(w, http.StatusConflict, "conflict", "job not running")
			return
		}
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	apierr.WriteJSON(w, http.StatusOK, job)
}
