package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/auth"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/deploy"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/routing"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/runtime"
)

// deployHandler implements the Kamui Deploy API.
type deployHandler struct {
	store  *deploy.Store
	ids    *identity.Store
	engine *core.Engine
	hub    *routing.Hub
	routes *runtime.Store
}

func newDeployHandler(store *deploy.Store, ids *identity.Store, engine *core.Engine, hub *routing.Hub, routes *runtime.Store) *deployHandler {
	return &deployHandler{store: store, ids: ids, engine: engine, hub: hub, routes: routes}
}

func (h *deployHandler) authorize(w http.ResponseWriter, r *http.Request, projectID string, perm identity.Permission) (identity.Principal, bool) {
	if h.ids == nil {
		return auth_operator(), true
	}
	p, ok := auth.FromContext(r.Context())
	if !ok {
		apierr.WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return identity.Principal{}, false
	}
	if _, err := h.ids.Authorize(r.Context(), p, projectID, perm); err != nil {
		apierr.WriteError(w, http.StatusForbidden, "forbidden", "you do not have permission for this project deployment")
		return identity.Principal{}, false
	}
	return p, true
}

func auth_operator() identity.Principal { return auth.OperatorPrincipal() }

func (h *deployHandler) register(mux *http.ServeMux, publicMux *http.ServeMux) {
	mux.HandleFunc("POST /v1/projects/{id}/deployments", h.Create)
	mux.HandleFunc("GET /v1/projects/{id}/deployments", h.List)
	mux.HandleFunc("GET /v1/projects/{id}/deployments/{dep}", h.Get)
	mux.HandleFunc("DELETE /v1/projects/{id}/deployments/{dep}", h.Delete)
	mux.HandleFunc("POST /v1/projects/{id}/deployments/{dep}/builds", h.TriggerBuild)
	mux.HandleFunc("GET /v1/projects/{id}/deployments/{dep}/builds", h.ListBuilds)
	mux.HandleFunc("GET /v1/projects/{id}/deployments/{dep}/revisions", h.ListRevisions)
	mux.HandleFunc("POST /v1/projects/{id}/deployments/{dep}/revisions/{rev}/release", h.ReleaseRevision)
	mux.HandleFunc("POST /v1/projects/{id}/deployments/{dep}/start", h.Start)
	mux.HandleFunc("POST /v1/projects/{id}/deployments/{dep}/stop", h.Stop)
	mux.HandleFunc("POST /v1/projects/{id}/deployments/{dep}/rollback", h.Rollback)
	mux.HandleFunc("POST /v1/projects/{id}/deployments/{dep}/logs", h.RuntimeLogs)
	mux.HandleFunc("POST /v1/projects/{id}/deployments/{dep}/webhooks", h.CreateWebhook)
	mux.HandleFunc("GET /v1/projects/{id}/deployments/{dep}/webhooks", h.ListWebhooks)
	mux.HandleFunc("DELETE /v1/projects/{id}/webhooks/{webhook}", h.DeleteWebhook)
	// Deploy service proxy (for remote worker services)
	for _, method := range []string{"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"} {
		mux.HandleFunc(method+" /v1/projects/{id}/deployments/{dep}/proxy/{path...}", h.ProxyDeployment)
		mux.HandleFunc(method+" /v1/projects/{id}/deployments/{dep}/proxy", h.ProxyDeployment)
	}
	publicMux.HandleFunc("POST /v1/webhooks/{webhook}", h.WebhookTrigger)
}

type deployCreateInput struct {
	Name       string            `json:"name"`
	Source     string            `json:"source"`
	SourceRef  string            `json:"source_ref,omitempty"`
	BuildImage string            `json:"build_image,omitempty"`
	Placement  string            `json:"placement,omitempty"`
	WorkerID   string            `json:"worker_id,omitempty"`
	Resources  core.Resources    `json:"resources"`
	Env        map[string]string `json:"env,omitempty"`
}

func (h *deployHandler) projectID(w http.ResponseWriter, r *http.Request) (string, bool) {
	projectID := r.PathValue("id")
	if projectID == "" {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "a project is required")
		return "", false
	}
	return projectID, true
}

func (h *deployHandler) deployment(w http.ResponseWriter, r *http.Request, perm identity.Permission) (identity.Principal, deploy.Deployment, bool) {
	projectID, ok := h.projectID(w, r)
	if !ok {
		return identity.Principal{}, deploy.Deployment{}, false
	}
	p, ok := h.authorize(w, r, projectID, perm)
	if !ok {
		return identity.Principal{}, deploy.Deployment{}, false
	}
	deployment, err := h.store.GetDeployment(r.Context(), projectID, r.PathValue("dep"))
	if err != nil {
		if errors.Is(err, deploy.ErrNotFound) {
			apierr.WriteError(w, http.StatusNotFound, "not_found", "deployment not found")
		} else {
			runtimeError(w, err)
		}
		return identity.Principal{}, deploy.Deployment{}, false
	}
	return p, deployment, true
}

func (h *deployHandler) Create(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.projectID(w, r)
	if !ok {
		return
	}
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	var input deployCreateInput
	if err := readJSON(w, r, 64<<10, &input); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid deployment JSON")
		return
	}
	deployment, err := h.store.CreateDeployment(r.Context(), projectID, input.Name, input.Source, input.SourceRef,
		input.BuildImage, input.Placement, input.WorkerID, input.Resources, input.Env, p.User.ID)
	if err != nil {
		deployError(w, err)
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, projectID, "deploy.create", "deployment", deployment.ID, "success", deployment.Name)
	}
	w.Header().Set("Location", "/v1/projects/"+projectID+"/deployments/"+deployment.ID)
	apierr.WriteJSON(w, http.StatusCreated, deployment)
}

func (h *deployHandler) List(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.projectID(w, r)
	if !ok {
		return
	}
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	items, err := h.store.ListDeployments(r.Context(), projectID)
	if err != nil {
		runtimeError(w, err)
		return
	}
	for i := range items {
		h.store.SyncDeploymentState(r.Context(), &items[i])
	}
	apierr.WriteJSON(w, http.StatusOK, items)
}

func (h *deployHandler) Get(w http.ResponseWriter, r *http.Request) {
	p, deployment, ok := h.deployment(w, r, identity.PermWorkloadRead)
	if !ok {
		return
	}
	h.store.SyncDeploymentState(r.Context(), &deployment)
	revisions, _ := h.store.ListRevisions(r.Context(), deployment.ID)
	builds, _ := h.store.ListBuilds(r.Context(), deployment.ID, 10)
	var workload any
	if deployment.CurrentRevision > 0 {
		for _, rev := range revisions {
			if rev.Revision == deployment.CurrentRevision && rev.WorkloadID != "" {
				if item, found := h.engineWorkload(r, rev.WorkloadID); found {
					workload = item
				}
			}
		}
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{
		"deployment": deployment,
		"revisions":  revisions,
		"builds":     builds,
		"runtime":    workload,
		"viewer":     p.User.ID,
	})
}

func (h *deployHandler) engineWorkload(r *http.Request, workloadID string) (any, bool) {
	if h.engine == nil {
		return nil, false
	}
	item, err := h.engine.Repo.Get(r.Context(), workloadID)
	if err != nil {
		return nil, false
	}
	return item, true
}

func (h *deployHandler) Delete(w http.ResponseWriter, r *http.Request) {
	p, deployment, ok := h.deployment(w, r, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	// Stop the runtime first; deletion must not leave orphan containers.
	if deployment.CurrentRevision > 0 {
		_ = h.store.StopRuntime(r.Context(), deployment)
	}
	if err := h.store.DeleteDeployment(r.Context(), deployment.ProjectID, deployment.ID); err != nil {
		runtimeError(w, err)
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, deployment.ProjectID, "deploy.delete", "deployment", deployment.ID, "success", deployment.Name)
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *deployHandler) TriggerBuild(w http.ResponseWriter, r *http.Request) {
	p, deployment, ok := h.deployment(w, r, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	var input struct {
		Commit     string `json:"commit,omitempty"`
		Dockerfile string `json:"dockerfile,omitempty"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(w, r, 160<<10, &input); err != nil {
			apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid build request JSON")
			return
		}
	}
	if !deploy.ValidCommit(input.Commit) {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "commit must be a git SHA prefix")
		return
	}
	var gitToken string
	if h.ids != nil {
		gitToken, _ = h.ids.GetUserGitHubToken(r.Context(), p.User.ID)
	}
	var dockerfile []byte
	if deployment.SourceKind == "dockerfile" {
		if input.Dockerfile == "" {
			apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "this deployment source requires a Dockerfile")
			return
		}
		dockerfile = []byte(input.Dockerfile)
	}
	build, err := h.store.TriggerBuild(r.Context(), deployment, input.Commit, gitToken, dockerfile, p.User.ID)
	if err != nil {
		deployError(w, err)
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, deployment.ProjectID, "deploy.build", "deployment", deployment.ID, "started", build.ID)
	}
	apierr.WriteJSON(w, http.StatusAccepted, build)
}

func (h *deployHandler) ListBuilds(w http.ResponseWriter, r *http.Request) {
	_, deployment, ok := h.deployment(w, r, identity.PermWorkloadRead)
	if !ok {
		return
	}
	items, err := h.store.ListBuilds(r.Context(), deployment.ID, 20)
	if err != nil {
		runtimeError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, items)
}

func (h *deployHandler) ListRevisions(w http.ResponseWriter, r *http.Request) {
	_, deployment, ok := h.deployment(w, r, identity.PermWorkloadRead)
	if !ok {
		return
	}
	items, err := h.store.ListRevisions(r.Context(), deployment.ID)
	if err != nil {
		runtimeError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, items)
}

func (h *deployHandler) ReleaseRevision(w http.ResponseWriter, r *http.Request) {
	p, deployment, ok := h.deployment(w, r, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	revision, err := strconv.Atoi(r.PathValue("rev"))
	if err != nil || revision < 1 {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "a revision number is required")
		return
	}
	rev, err := h.store.ReleaseRevision(r.Context(), deployment, revision, p.User.ID)
	if err != nil {
		deployError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusAccepted, rev)
}

func (h *deployHandler) Start(w http.ResponseWriter, r *http.Request) {
	p, deployment, ok := h.deployment(w, r, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	if err := h.store.StartRuntime(r.Context(), deployment, p.User.ID); err != nil {
		deployError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (h *deployHandler) Stop(w http.ResponseWriter, r *http.Request) {
	_, deployment, ok := h.deployment(w, r, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	if err := h.store.StopRuntime(r.Context(), deployment); err != nil {
		deployError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (h *deployHandler) Rollback(w http.ResponseWriter, r *http.Request) {
	p, deployment, ok := h.deployment(w, r, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	rev, err := h.store.Rollback(r.Context(), deployment, p.User.ID)
	if err != nil {
		deployError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusAccepted, rev)
}

// RuntimeLogs returns the runtime workload's log tail for the current revision.
func (h *deployHandler) RuntimeLogs(w http.ResponseWriter, r *http.Request) {
	_, deployment, ok := h.deployment(w, r, identity.PermWorkloadRead)
	if !ok {
		return
	}
	if deployment.CurrentRevision == 0 || h.engine == nil {
		apierr.WriteJSON(w, http.StatusOK, map[string]any{"attempt": 0, "tail": ""})
		return
	}
	rev, err := h.store.GetRevision(r.Context(), deployment.ID, deployment.CurrentRevision)
	if err != nil || rev.WorkloadID == "" {
		apierr.WriteJSON(w, http.StatusOK, map[string]any{"attempt": 0, "tail": ""})
		return
	}
	logs, err := h.engine.Logs(r.Context(), rev.WorkloadID)
	if err != nil {
		runtimeError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, logs)
}

// ProxyDeployment forwards authorized project requests to the deployment
// service's route. Upstream destinations come from the route registry, never
// from client-supplied URLs, which prevents arbitrary internal proxying.
// Supports WebSocket upgrades for remote worker services.
func (h *deployHandler) ProxyDeployment(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	deployment, err := h.store.GetDeploymentByPath(r.Context(), projectID, r.PathValue("dep"))
	if err != nil {
		deployError(w, err)
		return
	}
	if h.engine == nil || h.routes == nil {
		apierr.WriteError(w, http.StatusServiceUnavailable, "proxy_unavailable", "service routing is unavailable")
		return
	}
	rev, err := h.store.GetRevision(r.Context(), deployment.ID, deployment.CurrentRevision)
	if err != nil || rev.WorkloadID == "" {
		apierr.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "no runtime workload for this revision")
		return
	}
	route, err := h.routes.RouteLookup(r.Context(), rev.WorkloadID)
	if err != nil || route.ProjectID != projectID {
		apierr.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "the deployment service has no live route")
		return
	}
	item, err := h.engine.Repo.Get(r.Context(), rev.WorkloadID)
	if err != nil || !liveServiceRoute(route, item, time.Now()) {
		apierr.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "the service assignment is no longer live")
		return
	}
	transport := &http.Transport{DisableKeepAlives: true, ResponseHeaderTimeout: 60 * time.Second}
	host := "kamui-service"
	if route.WorkerID == core.LocalWorker {
		resolver, ok := h.engine.Executor.(ContainerResolver)
		if !ok {
			apierr.WriteError(w, http.StatusServiceUnavailable, "proxy_unavailable", "local service resolver is unavailable")
			return
		}
		addr, err := resolver.ContainerServiceAddress(r.Context(), route.WorkloadID, route.Attempt)
		if err != nil {
			apierr.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "local service is unreachable")
			return
		}
		host = net.JoinHostPort(addr, strconv.Itoa(route.Port))
	} else {
		if h.hub == nil || h.hub.Get(route.WorkerID) == nil {
			apierr.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "the assigned worker has no live tunnel")
			return
		}
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			tunnel := h.hub.Get(route.WorkerID)
			if tunnel == nil {
				return nil, routing.ErrTunnelClosed
			}
			return tunnel.DialContext(ctx, route.WorkloadID, route.Fence, route.Port)
		}
	}
	proxyService(w, r, host, transport)
}

func (h *deployHandler) CreateWebhook(w http.ResponseWriter, r *http.Request) {
	p, deployment, ok := h.deployment(w, r, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	var input struct {
		BranchFilter string `json:"branch_filter,omitempty"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(w, r, 8<<10, &input); err != nil {
			apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid webhook JSON")
			return
		}
	}
	webhook, secret, err := h.store.CreateWebhook(r.Context(), deployment.ProjectID, deployment.ID, input.BranchFilter, p.User.ID)
	if err != nil {
		runtimeError(w, err)
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, deployment.ProjectID, "deploy.webhook.create", "webhook", webhook.ID, "success", deployment.Name)
	}
	apierr.WriteJSON(w, http.StatusCreated, map[string]any{
		"webhook":      webhook,
		"secret":       secret, // shown once
		"delivery_url": "/v1/webhooks/" + webhook.ID,
	})
}

func (h *deployHandler) ListWebhooks(w http.ResponseWriter, r *http.Request) {
	_, deployment, ok := h.deployment(w, r, identity.PermWorkloadRead)
	if !ok {
		return
	}
	items, err := h.store.ListWebhooks(r.Context(), deployment.ID)
	if err != nil {
		runtimeError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, items)
}

func (h *deployHandler) DeleteWebhook(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	if err := h.store.DeleteWebhook(r.Context(), projectID, r.PathValue("webhook")); err != nil {
		runtimeError(w, err)
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, projectID, "deploy.webhook.delete", "webhook", r.PathValue("webhook"), "success", "")
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// WebhookTrigger is the provider-neutral, signature-verified entry point.
func (h *deployHandler) WebhookTrigger(w http.ResponseWriter, r *http.Request) {
	webhookID := r.PathValue("webhook")
	signature := r.Header.Get("X-Kamui-Signature")
	timestamp := r.Header.Get("X-Kamui-Timestamp")
	body, err := io.ReadAll(io.LimitReader(r.Body, 256<<10))
	if err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "unreadable delivery body")
		return
	}
	var payload struct {
		Branch string `json:"branch"`
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid delivery payload")
		return
	}
	trigger, err := h.store.MatchWebhook(r.Context(), webhookID, signature, timestamp, string(body), payload.Branch)
	if err != nil {
		deployError(w, err)
		return
	}
	if trigger.Deployment.SourceKind != "git" {
		apierr.WriteError(w, http.StatusUnprocessableEntity, "invalid_trigger", "webhook triggers require a git source")
		return
	}
	gitToken := ""
	build, err := h.store.TriggerBuild(r.Context(), trigger.Deployment, payload.Commit, gitToken, nil, trigger.Actor)
	if err != nil {
		deployError(w, err)
		return
	}
	slog.Info("webhook build triggered", "deployment", trigger.Deployment.ID, "build", build.ID)
	apierr.WriteJSON(w, http.StatusAccepted, build)
}

func deployError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, deploy.ErrNotFound):
		apierr.WriteError(w, http.StatusNotFound, "not_found", "deployment resource not found")
	case errors.Is(err, deploy.ErrConflict):
		apierr.WriteError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, deploy.ErrForbidden):
		apierr.WriteError(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, deploy.ErrBadInput):
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		slog.Warn("deploy operation failed", "error", err)
		apierr.WriteError(w, http.StatusServiceUnavailable, "deploy_unavailable", "the deployment operation is temporarily unavailable")
	}
}
