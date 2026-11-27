package httpapi

import (
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
	registerCoreWithIdentity(mux, e, nil, nil)
}

func registerCoreWithIdentity(mux *http.ServeMux, e *core.Engine, idStore *identity.Store, workerStore *worker.Store) {
	authorizeProject := func(w http.ResponseWriter, r *http.Request, projectID string, perm identity.Permission) (identity.Principal, bool) {
		if idStore == nil {
			return auth.OperatorPrincipal(), true
		}
		p, ok := auth.FromContext(r.Context())
		if !ok {
			apierr.WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
			return identity.Principal{}, false
		}
		if _, err := idStore.Authorize(r.Context(), p, projectID, perm); err != nil {
			apierr.WriteError(w, http.StatusForbidden, "forbidden", "you do not have permission for this project workload")
			return identity.Principal{}, false
		}
		return p, true
	}

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
		// A client that omits the placement mode inherits the project's
		// scheduling preferences; explicit modes always win.
		explicitPlacement := input.Placement
		projectID := requestedProjectID(r, "")
		if workerStore != nil && explicitPlacement == "" {
			if prefs, err := workerStore.ProjectPrefs(r.Context(), projectID); err == nil && prefs.Mode != core.PlacementAuto {
				input.Placement = prefs.Mode
				if prefs.Mode == core.PlacementManual {
					input.WorkerID = firstWorker(prefs.PreferredWorkers)
					input.PreferredWorkers = nil
				} else {
					input.PreferredWorkers = prefs.PreferredWorkers
					input.Fallback = &prefs.Fallback
				}
			}
		}
		// Clients can never self-declare managed infrastructure.
		input.ManagedInfra = false
		input.ExtraTmpfs = nil
		if err := input.Normalize(); err != nil {
			apierr.WriteError(w, http.StatusBadRequest, "invalid_workload", err.Error())
			return
		}
		p, ok := authorizeProject(w, r, projectID, identity.PermWorkloadWrite)
		if !ok {
			return
		}
		input.SetScope(projectID, p.User.ID)
		if input.Placement == core.PlacementManual && workerStore != nil {
			workers, err := workerStore.Workers(r.Context())
			if err != nil {
				coreError(w, err)
				return
			}
			if err := core.CheckManualSubmission(input, projectID, p.User.ID, workers, time.Now().UTC()); err != nil {
				apierr.WriteError(w, http.StatusUnprocessableEntity, "invalid_placement", err.Error())
				return
			}
		}
		var item core.Workload
		var created bool
		var err error
		if store, ok := e.Repo.(*core.Store); ok {
			item, created, err = store.SubmitForProjectAs(r.Context(), projectID, p.User.ID, input, key)
		} else {
			item, created, err = e.Repo.Submit(r.Context(), input, key)
		}
		if err != nil {
			coreError(w, err)
			return
		}
		if created && idStore != nil {
			_ = idStore.RecordAudit(r.Context(), p.User.ID, projectID, "workload.submit", "workload", item.ID, "success", item.Submission.Kind+":"+item.Submission.Image)
		}
		w.Header().Set("Location", "/v1/workloads/"+item.ID)
		status := http.StatusOK
		if created {
			status = http.StatusAccepted
		}
		apierr.WriteJSON(w, status, item)
	})
	mux.HandleFunc("GET /v1/workloads", func(w http.ResponseWriter, r *http.Request) {
		if idStore != nil {
			p, ok := auth.FromContext(r.Context())
			if !ok {
				apierr.WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
				return
			}
			explicit := strings.TrimSpace(r.Header.Get("X-Kamui-Project"))
			if explicit == "" {
				explicit = strings.TrimSpace(r.URL.Query().Get("project_id"))
			}
			var projectIDs []string
			if explicit != "" {
				if _, ok := authorizeProject(w, r, explicit, identity.PermWorkloadRead); !ok {
					return
				}
				projectIDs = []string{explicit}
			} else {
				var err error
				projectIDs, err = idStore.UserProjectIDs(r.Context(), p.User.ID)
				if err != nil {
					coreError(w, err)
					return
				}
			}
			if store, ok := e.Repo.(*core.Store); ok {
				items, err := store.ListForProjects(r.Context(), projectIDs)
				if err != nil {
					coreError(w, err)
					return
				}
				apierr.WriteJSON(w, http.StatusOK, items)
				return
			}
		}
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
		if _, ok := authorizeProject(w, r, item.ProjectID, identity.PermWorkloadRead); !ok {
			return
		}
		apierr.WriteJSON(w, http.StatusOK, item)
	})
	mux.HandleFunc("POST /v1/workloads/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		existing, err := e.Repo.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			coreError(w, err)
			return
		}
		if _, ok := authorizeProject(w, r, existing.ProjectID, identity.PermWorkloadWrite); !ok {
			return
		}
		item, err := e.Cancel(r.Context(), r.PathValue("id"))
		if err != nil {
			coreError(w, err)
			return
		}
		apierr.WriteJSON(w, http.StatusOK, item)
	})
	mux.HandleFunc("GET /v1/workloads/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		existing, err := e.Repo.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			coreError(w, err)
			return
		}
		if _, ok := authorizeProject(w, r, existing.ProjectID, identity.PermWorkloadRead); !ok {
			return
		}
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
		if idStore != nil {
			if store, ok := e.Repo.(*core.Store); ok {
				p, ok := auth.FromContext(r.Context())
				if !ok {
					apierr.WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
					return
				}
				projectIDs, err := idStore.UserProjectIDs(r.Context(), p.User.ID)
				if err != nil {
					coreError(w, err)
					return
				}
				events, err := store.EventsForProjects(r.Context(), projectIDs, after)
				if err != nil {
					coreError(w, err)
					return
				}
				apierr.WriteJSON(w, http.StatusOK, events)
				return
			}
		}
		events, err := e.Repo.Events(r.Context(), after)
		if err != nil {
			coreError(w, err)
			return
		}
		apierr.WriteJSON(w, http.StatusOK, events)
	})
}

func firstWorker(preferred []string) string {
	if len(preferred) > 0 {
		return preferred[0]
	}
	return ""
}
