package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/auth"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/runtime"
)

// runtimeHandler serves the shared execution prerequisites: artifact staging,
// project secrets, scoped registry credentials, volume placements and
// assignment-bound service routes. Every endpoint is project-scoped.
type runtimeHandler struct {
	store  *runtime.Store
	engine *core.Engine
	ids    *identity.Store
}

func newRuntimeHandler(store *runtime.Store, engine *core.Engine, ids *identity.Store) *runtimeHandler {
	return &runtimeHandler{store: store, engine: engine, ids: ids}
}

func (h *runtimeHandler) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/projects/{id}/artifacts", h.UploadArtifact)
	mux.HandleFunc("GET /v1/projects/{id}/artifacts", h.ListArtifacts)
	mux.HandleFunc("GET /v1/projects/{id}/artifacts/{art}", h.GetArtifact)
	mux.HandleFunc("GET /v1/projects/{id}/artifacts/{art}/content", h.DownloadArtifact)
	mux.HandleFunc("DELETE /v1/projects/{id}/artifacts/{art}", h.DeleteArtifact)
	mux.HandleFunc("PUT /v1/projects/{id}/secrets/{name}", h.PutSecret)
	mux.HandleFunc("DELETE /v1/projects/{id}/secrets/{name}", h.DeleteSecret)
	mux.HandleFunc("GET /v1/projects/{id}/secrets", h.ListSecrets)
	mux.HandleFunc("PUT /v1/projects/{id}/registry/{host}", h.PutRegistryCredential)
	mux.HandleFunc("DELETE /v1/projects/{id}/registry/{host}", h.DeleteRegistryCredential)
	mux.HandleFunc("GET /v1/projects/{id}/volumes", h.ListVolumePlacements)
	mux.HandleFunc("GET /v1/projects/{id}/services/{workload}/route", h.GetServiceRoute)
	mux.HandleFunc("GET /v1/projects/{id}/services/{workload}/proxy/", h.ProxyService)
	mux.HandleFunc("GET /v1/projects/{id}/services/{workload}/proxy", h.ProxyService)
}

func (h *runtimeHandler) authorize(w http.ResponseWriter, r *http.Request, projectID string, perm identity.Permission) (identity.Principal, bool) {
	if h.ids == nil {
		return auth.OperatorPrincipal(), true
	}
	p, ok := auth.FromContext(r.Context())
	if !ok {
		apierr.WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return identity.Principal{}, false
	}
	if _, err := h.ids.Authorize(r.Context(), p, projectID, perm); err != nil {
		apierr.WriteError(w, http.StatusForbidden, "forbidden", "you do not have permission for this project resource")
		return identity.Principal{}, false
	}
	return p, true
}

func runtimeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtime.ErrNotFound):
		apierr.WriteError(w, http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, runtime.ErrConflict):
		apierr.WriteError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, runtime.ErrTooLarge):
		apierr.WriteError(w, http.StatusRequestEntityTooLarge, "too_large", err.Error())
	case errors.Is(err, runtime.ErrBadInput):
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		slog.Warn("runtime operation failed", "error", err)
		apierr.WriteError(w, http.StatusServiceUnavailable, "runtime_unavailable", "the requested runtime operation is temporarily unavailable")
	}
}

func (h *runtimeHandler) UploadArtifact(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if kind == "" {
		kind = "generic"
	}
	if name == "" {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "an artifact name query parameter is required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.store.MaxBlobBytes()+1)
	artifact, err := h.store.CreateArtifact(r.Context(), projectID, name, kind, r.Header.Get("Content-Type"), p.User.ID, r.Body)
	if err != nil {
		runtimeError(w, err)
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, projectID, "artifact.create", "artifact", artifact.ID, "success", name)
	}
	w.Header().Set("Location", "/v1/projects/"+projectID+"/artifacts/"+artifact.ID)
	apierr.WriteJSON(w, http.StatusCreated, artifact)
}

func (h *runtimeHandler) ListArtifacts(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := h.store.ListArtifacts(r.Context(), projectID, limit)
	if err != nil {
		runtimeError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, items)
}

func (h *runtimeHandler) GetArtifact(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	artifact, err := h.store.GetArtifact(r.Context(), projectID, r.PathValue("art"))
	if err != nil {
		runtimeError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, artifact)
}

func (h *runtimeHandler) DownloadArtifact(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	artifact, err := h.store.GetArtifact(r.Context(), projectID, r.PathValue("art"))
	if err != nil {
		runtimeError(w, err)
		return
	}
	reader, size, err := h.store.OpenArtifact(r.Context(), artifact)
	if err != nil {
		runtimeError(w, err)
		return
	}
	defer reader.Close()
	w.Header().Set("Content-Type", artifact.ContentType)
	w.Header().Set("X-Checksum-Sha256", artifact.Checksum)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	if _, err := io.Copy(w, reader); err != nil {
		slog.Warn("artifact download failed", "artifact", artifact.ID, "error", err)
	}
	h.store.MarkArtifactDownloaded(r.Context(), artifact.ID)
}

func (h *runtimeHandler) DeleteArtifact(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermWorkloadWrite)
	if !ok {
		return
	}
	if err := h.store.DeleteArtifact(r.Context(), projectID, r.PathValue("art")); err != nil {
		runtimeError(w, err)
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, projectID, "artifact.delete", "artifact", r.PathValue("art"), "success", "")
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *runtimeHandler) PutSecret(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	name := r.PathValue("name")
	p, ok := h.authorize(w, r, projectID, identity.PermProjectUpdate)
	if !ok {
		return
	}
	var input struct {
		Value string `json:"value"`
	}
	if err := readJSON(w, r, 96<<10, &input); err != nil || input.Value == "" {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "a secret value is required")
		return
	}
	if err := h.store.PutSecret(r.Context(), projectID, name, input.Value, p.User.ID); err != nil {
		runtimeError(w, err)
		return
	}
	// Audit records the operation, never the value.
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, projectID, "secret."+name+".set", "secret", name, "success", "")
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *runtimeHandler) DeleteSecret(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	name := r.PathValue("name")
	p, ok := h.authorize(w, r, projectID, identity.PermProjectUpdate)
	if !ok {
		return
	}
	if err := h.store.DeleteSecret(r.Context(), projectID, name); err != nil {
		runtimeError(w, err)
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, projectID, "secret."+name+".delete", "secret", name, "success", "")
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *runtimeHandler) ListSecrets(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := h.authorize(w, r, projectID, identity.PermProjectUpdate); !ok {
		return
	}
	items, err := h.store.ListSecrets(r.Context(), projectID)
	if err != nil {
		runtimeError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, items)
}

func (h *runtimeHandler) PutRegistryCredential(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	host := r.PathValue("host")
	p, ok := h.authorize(w, r, projectID, identity.PermProjectUpdate)
	if !ok {
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(w, r, 16<<10, &input); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid registry credential JSON")
		return
	}
	if err := h.store.PutRegistryCredential(r.Context(), projectID, host, input.Username, input.Password, p.User.ID); err != nil {
		runtimeError(w, err)
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, projectID, "registry."+host+".set", "registry_credential", host, "success", "")
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *runtimeHandler) DeleteRegistryCredential(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	p, ok := h.authorize(w, r, projectID, identity.PermProjectUpdate)
	if !ok {
		return
	}
	if err := h.store.DeleteRegistryCredential(r.Context(), projectID, r.PathValue("host")); err != nil {
		runtimeError(w, err)
		return
	}
	if h.ids != nil {
		_ = h.ids.RecordAudit(r.Context(), p.User.ID, projectID, "registry."+r.PathValue("host")+".delete", "registry_credential", r.PathValue("host"), "success", "")
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *runtimeHandler) ListVolumePlacements(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	items, err := h.store.ListVolumePlacements(r.Context(), projectID)
	if err != nil {
		runtimeError(w, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, items)
}

// GetServiceRoute returns the current route for a hosted service. Members of
// the owning project only; routes are bound to live, fenced assignments.
func (h *runtimeHandler) GetServiceRoute(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	workloadID := r.PathValue("workload")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	route, err := h.store.GetRoute(r.Context(), workloadID)
	if err != nil {
		runtimeError(w, err)
		return
	}
	if route.ProjectID != projectID {
		apierr.WriteError(w, http.StatusNotFound, "not_found", "route not found")
		return
	}
	apierr.WriteJSON(w, http.StatusOK, route)
}

// ProxyService forwards member requests to a live service container on the
// project network. This is the local-access path: it requires the coordinator
// and the assigned worker to share a Docker daemon; remote workers report
// routes but are not proxied here.
func (h *runtimeHandler) ProxyService(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	workloadID := r.PathValue("workload")
	if _, ok := h.authorize(w, r, projectID, identity.PermWorkloadRead); !ok {
		return
	}
	route, err := h.store.GetRoute(r.Context(), workloadID)
	if err != nil {
		runtimeError(w, err)
		return
	}
	if route.ProjectID != projectID {
		apierr.WriteError(w, http.StatusNotFound, "not_found", "route not found")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	h.proxyContainer(w, r, route)
}

// proxyContainer resolves the current container address on the project network
// and reverse-proxies to it. Container discovery happens per request so a
// replaced assignment always wins over a stale address.
func (h *runtimeHandler) proxyContainer(w http.ResponseWriter, r *http.Request, route runtime.ServiceRoute) {
	if h.engine == nil {
		apierr.WriteError(w, http.StatusServiceUnavailable, "proxy_unavailable", "the service proxy requires the core executor")
		return
	}
	resolver, ok := h.engine.Executor.(ContainerResolver)
	if !ok {
		apierr.WriteError(w, http.StatusServiceUnavailable, "proxy_unavailable", "service routing requires a shared local Docker daemon; remote worker routing is not available yet")
		return
	}
	addr, err := resolver.ContainerServiceAddress(r.Context(), route.WorkloadID, route.Attempt)
	if err != nil {
		slog.Warn("resolve service container", "workload_id", route.WorkloadID, "error", err)
		apierr.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "the service container is not reachable from the coordinator")
		return
	}
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(addr, strconv.Itoa(route.Port))}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	proxy.ServeHTTP(w, r)
}

// ContainerResolver resolves where a workload's container is reachable from
// the coordinator. Implemented by the local Docker executor.
type ContainerResolver interface {
	ContainerServiceAddress(ctx context.Context, workloadID string, attempt int) (string, error)
}
