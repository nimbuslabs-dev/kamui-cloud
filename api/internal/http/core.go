package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
)

func coreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, core.ErrNotFound):
		apierr.WriteError(w, http.StatusNotFound, "not_found", "workload not found")
	case errors.Is(err, core.ErrConflict):
		apierr.WriteError(w, http.StatusConflict, "idempotency_conflict", "the idempotency key was used for a different workload")
	case errors.Is(err, core.ErrLimit):
		apierr.WriteError(w, http.StatusTooManyRequests, "workload_limit", err.Error())
	default:
		slog.Warn("core request failed", "error", err)
		apierr.WriteError(w, http.StatusServiceUnavailable, "core_unavailable", "the requested core operation is temporarily unavailable")
	}
}

func registerCore(mux *http.ServeMux, e *core.Engine) {
	mux.HandleFunc("GET /v1/workers/local", func(w http.ResponseWriter, r *http.Request) {
		item, err := e.Repo.Worker(r.Context())
		if err != nil {
			coreError(w, err)
			return
		}
		apierr.WriteJSON(w, http.StatusOK, item)
	})
	mux.HandleFunc("POST /v1/workloads", func(w http.ResponseWriter, r *http.Request) {
		var input core.Submission
		key := r.Header.Get("Idempotency-Key")
		if !core.ValidKey(key) {
			apierr.WriteError(w, http.StatusBadRequest, "invalid_key", "Idempotency-Key must contain 1–128 letters, digits or ._:-")
			return
		}
		if err := readJSON(w, r, 32<<10, &input); err != nil {
			apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid workload JSON")
			return
		}
		if err := input.Normalize(); err != nil {
			apierr.WriteError(w, http.StatusBadRequest, "invalid_workload", err.Error())
			return
		}
		item, created, err := e.Repo.Submit(r.Context(), input, key)
		if err != nil {
			coreError(w, err)
			return
		}
		w.Header().Set("Location", "/v1/workloads/"+item.ID)
		status := http.StatusOK
		if created {
			status = http.StatusAccepted
		}
		apierr.WriteJSON(w, status, item)
	})
	mux.HandleFunc("GET /v1/workloads", func(w http.ResponseWriter, r *http.Request) {
		items, err := e.Repo.List(r.Context(), false)
		if err != nil {
			coreError(w, err)
			return
		}
		apierr.WriteJSON(w, http.StatusOK, items)
	})
	mux.HandleFunc("GET /v1/workloads/{id}", func(w http.ResponseWriter, r *http.Request) {
		item, err := e.Repo.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			coreError(w, err)
			return
		}
		apierr.WriteJSON(w, http.StatusOK, item)
	})
	mux.HandleFunc("POST /v1/workloads/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		item, err := e.Cancel(r.Context(), r.PathValue("id"))
		if err != nil {
			coreError(w, err)
			return
		}
		apierr.WriteJSON(w, http.StatusOK, item)
	})
	mux.HandleFunc("GET /v1/workloads/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		logs, err := e.Logs(r.Context(), r.PathValue("id"))
		if err != nil {
			coreError(w, err)
			return
		}
		apierr.WriteJSON(w, http.StatusOK, logs)
	})
	mux.HandleFunc("GET /v1/workload-events", func(w http.ResponseWriter, r *http.Request) {
		var after int64
		if raw := r.URL.Query().Get("after"); raw != "" {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || id < 0 {
				apierr.WriteError(w, http.StatusBadRequest, "invalid_cursor", "after must be a nonnegative event ID")
				return
			}
			after = id
		}
		events, err := e.Repo.Events(r.Context(), after)
		if err != nil {
			coreError(w, err)
			return
		}
		apierr.WriteJSON(w, http.StatusOK, events)
	})
}
