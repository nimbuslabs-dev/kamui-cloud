package httpapi

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/auth"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/routing"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/runtime"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/workspaceinput"
)

type WorkspaceHandler struct {
	store           *workspaceStore
	identity        *identity.Store
	managerURL      string
	token           string
	client          *http.Client
	operations      chan struct{}
	hub             *routing.Hub
	routes          *runtime.Store
	engine          *core.Engine
	idleTimeout     time.Duration
	idleCheckTicker *time.Ticker
	idleStopCh      chan struct{}
}

func newWorkspaceHandler(dataPath, managerURL, token string) (*WorkspaceHandler, error) {
	return newWorkspaceHandlerWithIdentity(dataPath, managerURL, token, nil, nil, nil, nil, 0)
}

func newWorkspaceHandlerWithIdentity(dataPath, managerURL, token string, id *identity.Store, hub *routing.Hub, routes *runtime.Store, engine *core.Engine, idleTimeout time.Duration) (*WorkspaceHandler, error) {
	store, err := openWorkspaceStore(dataPath)
	if err != nil {
		return nil, err
	}
	h := &WorkspaceHandler{
		store:      store,
		identity:   id,
		managerURL: strings.TrimRight(managerURL, "/"),
		token:      token,
		client: &http.Client{
			Timeout: 75 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		operations:  make(chan struct{}, 1),
		hub:         hub,
		routes:      routes,
		engine:      engine,
		idleTimeout: idleTimeout,
		idleStopCh:  make(chan struct{}),
	}
	if idleTimeout > 0 {
		h.startIdleChecker()
	}
	return h, nil
}

func (h *WorkspaceHandler) startIdleChecker() {
	h.idleCheckTicker = time.NewTicker(5 * time.Minute)
	go func() {
		for {
			select {
			case <-h.idleCheckTicker.C:
				h.checkIdleWorkspaces()
			case <-h.idleStopCh:
				return
			}
		}
	}()
}

func (h *WorkspaceHandler) checkIdleWorkspaces() {
	if h.idleTimeout <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	workspaces := h.store.list()
	now := time.Now().UTC()
	for _, ws := range workspaces {
		if ws.Status != "running" {
			continue
		}
		if ws.LastActiveAt == nil {
			continue
		}
		idle := now.Sub(*ws.LastActiveAt)
		if idle >= h.idleTimeout {
			// Stop the workspace but keep the volume
			h.lifecycleInternal(ctx, ws, "stop")
		}
	}
}

func (h *WorkspaceHandler) lifecycleInternal(ctx context.Context, ws Workspace, operation string) {
	input := any(ws.ID)
	if operation == "start" {
		payload := map[string]string{
			"id": ws.ID, "name": ws.Name,
			"repository_url": ws.RepositoryURL, "branch": ws.Branch,
		}
		input = payload
	}
	_ = h.manager(ctx, http.MethodPost, "/workspace/"+operation, input, nil)
	// Note: we don't update the store here; the next status check will reflect the change
}

func validateWorkspaceCreate(input WorkspaceCreate) string {
	name := strings.TrimSpace(input.Name)
	switch {
	case name == "" || len(name) > 100 || strings.ContainsFunc(name, unicode.IsControl):
		return "name must be between 1 and 100 bytes without control characters"
	case !workspaceinput.ValidRepositoryURL(input.RepositoryURL):
		return "repository_url must be a GitHub HTTPS repository URL without credentials"
	case input.Branch != "" && !workspaceinput.ValidBranch(input.Branch):
		return "invalid branch name"
	default:
		return ""
	}
}

func workspaceProjectID(ws Workspace) string {
	if ws.ProjectID != "" {
		return ws.ProjectID
	}
	return identity.DefaultProject
}

func requestedProjectID(r *http.Request, bodyProject string) string {
	if bodyProject != "" {
		return bodyProject
	}
	if h := strings.TrimSpace(r.Header.Get("X-Kamui-Project")); h != "" {
		return h
	}
	if q := strings.TrimSpace(r.URL.Query().Get("project_id")); q != "" {
		return q
	}
	if p, ok := auth.FromContext(r.Context()); ok && p.ActiveProjectID != "" {
		return p.ActiveProjectID
	}
	return identity.DefaultProject
}

func (h *WorkspaceHandler) authorizeProject(w http.ResponseWriter, r *http.Request, projectID string, perm identity.Permission) (identity.Principal, bool) {
	if h.identity == nil {
		return auth.OperatorPrincipal(), true
	}
	p, ok := auth.FromContext(r.Context())
	if !ok {
		apierr.WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return identity.Principal{}, false
	}
	if _, err := h.identity.Authorize(r.Context(), p, projectID, perm); err != nil {
		apierr.WriteError(w, http.StatusForbidden, "forbidden", "you do not have permission for this project operation")
		return identity.Principal{}, false
	}
	return p, true
}

func (h *WorkspaceHandler) userGitToken(ctx context.Context, p identity.Principal) string {
	if h.identity == nil || p.User.ID == "" {
		return ""
	}
	tok, err := h.identity.GetUserGitHubToken(ctx, p.User.ID)
	if err != nil {
		return ""
	}
	return tok
}

func (h *WorkspaceHandler) Create(w http.ResponseWriter, r *http.Request) {
	var input WorkspaceCreate
	if !readWorkspaceJSON(w, r, &input) {
		return
	}
	if message := validateWorkspaceCreate(input); message != "" {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", message)
		return
	}
	projectID := requestedProjectID(r, input.ProjectID)
	p, ok := h.authorizeProject(w, r, projectID, identity.PermWorkspaceWrite)
	if !ok {
		return
	}
	var workspace Workspace
	var err error
	if h.identity == nil {
		workspace, err = h.store.create(input)
	} else {
		workspace, err = h.store.createOwned(input, projectID, p.User.ID)
	}
	if err != nil {
		apierr.WriteError(w, http.StatusInternalServerError, "storage_error", "could not save workspace metadata")
		return
	}
	if h.identity != nil {
		_ = h.identity.RecordAudit(r.Context(), p.User.ID, projectID, "workspace.create", "workspace", workspace.ID, "success", workspace.Name)
	}
	apierr.WriteJSON(w, http.StatusCreated, workspace)
}

func readWorkspaceJSON(w http.ResponseWriter, r *http.Request, input any) bool {
	if err := readJSON(w, r, 64*1024, input); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return false
	}
	return true
}

type managerFailure struct {
	status int
	detail apierr.ErrorDetail
}

func (e *managerFailure) write(w http.ResponseWriter) {
	apierr.WriteError(w, e.status, e.detail.Code, e.detail.Message)
}

func managerUnavailable() *managerFailure {
	return &managerFailure{http.StatusBadGateway, apierr.ErrorDetail{
		Code: "manager_unavailable", Message: "workspace manager is unavailable",
	}}
}

func (h *WorkspaceHandler) manager(ctx context.Context, method, path string, input, output any) *managerFailure {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return managerUnavailable()
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.managerURL+path, body)
	if err != nil {
		return managerUnavailable()
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return managerUnavailable()
	}
	defer resp.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 400 && resp.StatusCode < 600 && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		var failure apierr.ErrorResponse
		if err := decoder.Decode(&failure); err == nil && failure.Error.Code != "" && failure.Error.Message != "" {
			return &managerFailure{resp.StatusCode, failure.Error}
		}
		return managerUnavailable()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return managerUnavailable()
	}
	if output != nil && decoder.Decode(output) != nil {
		return managerUnavailable()
	}
	return nil
}

func (h *WorkspaceHandler) dockerStatus(ctx context.Context, workspace Workspace) Workspace {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	workspace.Status, workspace.ContainerID, workspace.CodeServerURL = "unavailable", "", ""
	var status struct {
		Status      string `json:"status"`
		ContainerID string `json:"container_id"`
	}
	if failure := h.manager(ctx, http.MethodGet, "/workspace/status?id="+url.QueryEscape(workspace.ID), nil, &status); failure != nil {
		return workspace
	}
	switch status.Status {
	case "running", "stopped", "starting", "stopping", "creating", "error":
		workspace.Status = status.Status
		workspace.ContainerID = status.ContainerID
		if status.Status == "running" {
			workspace.CodeServerURL = "/workspace/" + workspace.ID + "/"
		}
	}
	return workspace
}

func (h *WorkspaceHandler) List(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()

	allowed := map[string]bool{}
	explicitProject := strings.TrimSpace(r.Header.Get("X-Kamui-Project"))
	if explicitProject == "" {
		explicitProject = strings.TrimSpace(r.URL.Query().Get("project_id"))
	}
	if h.identity != nil {
		p, ok := auth.FromContext(r.Context())
		if !ok {
			apierr.WriteError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
			return
		}
		if explicitProject != "" {
			if _, ok := h.authorizeProject(w, r, explicitProject, identity.PermWorkspaceRead); !ok {
				return
			}
			allowed[explicitProject] = true
		} else {
			ids, err := h.identity.UserProjectIDs(r.Context(), p.User.ID)
			if err != nil {
				apierr.WriteError(w, http.StatusInternalServerError, "storage_error", "could not load project memberships")
				return
			}
			for _, id := range ids {
				allowed[id] = true
			}
		}
	}

	all := h.store.list()
	workspaces := make([]Workspace, 0, len(all))
	for _, ws := range all {
		if h.identity != nil && !allowed[workspaceProjectID(ws)] {
			continue
		}
		workspaces = append(workspaces, h.dockerStatus(ctx, ws))
	}
	apierr.WriteJSON(w, http.StatusOK, workspaces)
}

func (h *WorkspaceHandler) workspace(w http.ResponseWriter, r *http.Request, perm identity.Permission) (Workspace, identity.Principal, bool) {
	workspace, ok := h.store.get(r.PathValue("id"))
	if !ok {
		apierr.WriteError(w, http.StatusNotFound, "not_found", "workspace not found")
		return Workspace{}, identity.Principal{}, false
	}
	p, ok := h.authorizeProject(w, r, workspaceProjectID(workspace), perm)
	if !ok {
		return Workspace{}, identity.Principal{}, false
	}
	return workspace, p, true
}

func (h *WorkspaceHandler) Get(w http.ResponseWriter, r *http.Request) {
	if workspace, _, ok := h.workspace(w, r, identity.PermWorkspaceRead); ok {
		apierr.WriteJSON(w, http.StatusOK, h.dockerStatus(r.Context(), workspace))
	}
}

func (h *WorkspaceHandler) Start(w http.ResponseWriter, r *http.Request) {
	h.lifecycle(w, r, "start")
}

func (h *WorkspaceHandler) Stop(w http.ResponseWriter, r *http.Request) {
	h.lifecycle(w, r, "stop")
}

func (h *WorkspaceHandler) Restart(w http.ResponseWriter, r *http.Request) {
	h.lifecycle(w, r, "restart")
}

func (h *WorkspaceHandler) Delete(w http.ResponseWriter, r *http.Request) {
	h.lifecycle(w, r, "delete")
}

func (h *WorkspaceHandler) acquire(w http.ResponseWriter, r *http.Request) bool {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case h.operations <- struct{}{}:
		return true
	case <-r.Context().Done():
		return false
	case <-timer.C:
		apierr.WriteError(w, http.StatusConflict, "workspace_busy", "a workspace operation is already in progress")
		return false
	}
}

// Logs streams the bounded workspace container log tail through the manager.
func (h *WorkspaceHandler) Logs(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := h.workspace(w, r, identity.PermWorkspaceRead)
	if !ok {
		return
	}
	var result struct {
		Tail string `json:"tail"`
	}
	if failure := h.manager(r.Context(), "GET", "/workspace/logs?id="+url.QueryEscape(workspace.ID), nil, &result); failure != nil {
		failure.write(w)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, result)
}

func (h *WorkspaceHandler) lifecycle(w http.ResponseWriter, r *http.Request, operation string) {
	if !h.acquire(w, r) {
		return
	}
	defer func() { <-h.operations }()

	workspace, principal, ok := h.workspace(w, r, identity.PermWorkspaceWrite)
	if !ok {
		return
	}
	ctx := r.Context()
	id := map[string]string{"workspace_id": workspace.ID}
	if operation == "restart" {
		if failure := h.manager(ctx, http.MethodPost, "/workspace/stop", id, nil); failure != nil {
			failure.write(w)
			return
		}
		operation = "start"
	}
	input := any(id)
	if operation == "start" {
		payload := map[string]string{
			"id": workspace.ID, "name": workspace.Name,
			"repository_url": workspace.RepositoryURL, "branch": workspace.Branch,
		}
		if gitToken := h.userGitToken(ctx, principal); gitToken != "" {
			payload["git_token"] = gitToken
		}
		input = payload
	}
	if failure := h.manager(ctx, http.MethodPost, "/workspace/"+operation, input, nil); failure != nil {
		failure.write(w)
		return
	}
	if operation == "delete" {
		if err := h.store.put(workspace.ID, nil); err != nil {
			apierr.WriteError(w, http.StatusInternalServerError, "storage_error", "container was removed but workspace metadata could not be removed")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	now := time.Now().UTC()
	workspace.UpdatedAt = now
	if operation == "start" {
		workspace.LastActiveAt = &now
	}
	if err := h.store.put(workspace.ID, &workspace); err != nil {
		apierr.WriteError(w, http.StatusInternalServerError, "storage_error", "workspace operation completed but metadata could not be saved")
		return
	}
	apierr.WriteJSON(w, http.StatusOK, h.dockerStatus(ctx, workspace))
}

type GitStatus struct {
	Branch    string   `json:"branch"`
	Ahead     int      `json:"ahead"`
	Behind    int      `json:"behind"`
	Staged    []string `json:"staged"`
	Unstaged  []string `json:"unstaged"`
	Untracked []string `json:"untracked"`
}

type gitCommand struct {
	WorkspaceID string   `json:"workspace_id"`
	Command     string   `json:"command"`
	Args        []string `json:"args,omitempty"`
	GitToken    string   `json:"git_token,omitempty"`
}

func (h *WorkspaceHandler) git(w http.ResponseWriter, r *http.Request, command string, args []string, result any) bool {
	if !h.acquire(w, r) {
		return false
	}
	defer func() { <-h.operations }()

	perm := identity.PermWorkspaceWrite
	if command == "status" {
		perm = identity.PermWorkspaceRead
	}
	workspace, principal, ok := h.workspace(w, r, perm)
	if !ok {
		return false
	}
	input := gitCommand{WorkspaceID: workspace.ID, Command: command, Args: args}
	if command == "push" || command == "pull" {
		input.GitToken = h.userGitToken(r.Context(), principal)
	}
	if failure := h.manager(r.Context(), http.MethodPost, "/workspace/git", input, result); failure != nil {
		failure.write(w)
		return false
	}
	return true
}

func (h *WorkspaceHandler) GitStatus(w http.ResponseWriter, r *http.Request) {
	status := GitStatus{Staged: []string{}, Unstaged: []string{}, Untracked: []string{}}
	if h.git(w, r, "status", nil, &status) {
		apierr.WriteJSON(w, http.StatusOK, status)
	}
}

func (h *WorkspaceHandler) GitAdd(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Files []string `json:"files"`
	}
	if readWorkspaceJSON(w, r, &input) && h.git(w, r, "add", input.Files, nil) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *WorkspaceHandler) GitCommit(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Message string `json:"message"`
	}
	if !readWorkspaceJSON(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Message) == "" || len(input.Message) > 4096 || strings.ContainsRune(input.Message, 0) {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid commit message")
		return
	}
	var result struct {
		Hash string `json:"hash"`
	}
	if h.git(w, r, "commit", []string{input.Message}, &result) {
		_, err := hex.DecodeString(result.Hash)
		if err != nil || (len(result.Hash) != 40 && len(result.Hash) != 64) {
			apierr.WriteError(w, http.StatusBadGateway, "invalid_commit", "workspace manager did not return a full commit hash")
			return
		}
		apierr.WriteJSON(w, http.StatusOK, result)
	}
}

func (h *WorkspaceHandler) GitBranch(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Branch string `json:"branch"`
	}
	if !readWorkspaceJSON(w, r, &input) {
		return
	}
	if !workspaceinput.ValidBranch(input.Branch) {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid branch name")
		return
	}
	if h.git(w, r, "branch", []string{input.Branch}, nil) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *WorkspaceHandler) GitPush(w http.ResponseWriter, r *http.Request) {
	h.gitTransfer(w, r, "push")
}

func (h *WorkspaceHandler) GitPull(w http.ResponseWriter, r *http.Request) {
	h.gitTransfer(w, r, "pull")
}

func (h *WorkspaceHandler) gitTransfer(w http.ResponseWriter, r *http.Request, command string) {
	var input struct {
		Remote string `json:"remote"`
		Branch string `json:"branch"`
	}
	if r.ContentLength != 0 && !readWorkspaceJSON(w, r, &input) {
		return
	}
	if input.Remote != "" && input.Remote != "origin" {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "only the origin remote is supported")
		return
	}
	if input.Branch != "" && !workspaceinput.ValidBranch(input.Branch) {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid branch name")
		return
	}
	if h.git(w, r, command, []string{input.Remote, input.Branch}, nil) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// ProxyIDE forwards authorized project requests to the workspace IDE
// (code-server) WebSocket. For remote workers, it uses the authenticated
// worker tunnel. The WebSocket upgrade is forwarded through the tunnel.
func (h *WorkspaceHandler) ProxyIDE(w http.ResponseWriter, r *http.Request) {
	workspace, _, ok := h.workspace(w, r, identity.PermWorkspaceRead)
	if !ok {
		return
	}
	if h.engine == nil || h.routes == nil {
		apierr.WriteError(w, http.StatusServiceUnavailable, "proxy_unavailable", "IDE routing is unavailable")
		return
	}
	// Get the workload ID for this workspace. Workspaces run as service workloads.
	// We need to find the service workload associated with this workspace.
	workloadID := "ws-" + workspace.ID
	route, err := h.routes.RouteLookup(r.Context(), workloadID)
	if err != nil {
		// Try to get the container address from the manager for local workspaces
		if h.hub == nil || h.hub.Get(core.LocalWorker) == nil {
			apierr.WriteError(w, http.StatusServiceUnavailable, "ide_unavailable", "no live route or tunnel for workspace")
			return
		}
	}
	transport := &http.Transport{DisableKeepAlives: false, ResponseHeaderTimeout: 60 * time.Second}
	host := "kamui-workspace-" + workspace.ID + ":8080"
	if route.WorkerID == core.LocalWorker {
		resolver, ok := h.engine.Executor.(ContainerResolver)
		if !ok {
			apierr.WriteError(w, http.StatusServiceUnavailable, "proxy_unavailable", "local IDE resolver is unavailable")
			return
		}
		addr, err := resolver.ContainerServiceAddress(r.Context(), route.WorkloadID, route.Attempt)
		if err != nil {
			apierr.WriteError(w, http.StatusServiceUnavailable, "ide_unavailable", "local workspace is unreachable")
			return
		}
		host = net.JoinHostPort(addr, "8080")
	} else {
		if h.hub == nil || h.hub.Get(route.WorkerID) == nil {
			apierr.WriteError(w, http.StatusServiceUnavailable, "ide_unavailable", "the assigned worker has no live tunnel")
			return
		}
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			tunnel := h.hub.Get(route.WorkerID)
			if tunnel == nil {
				return nil, routing.ErrTunnelClosed
			}
			return tunnel.DialContext(ctx, route.WorkloadID, route.Fence, 8080)
		}
	}
	proxy := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		Rewrite: func(req *httputil.ProxyRequest) {
			req.Out.URL.Scheme = "http"
			req.Out.URL.Host = host
			req.Out.URL.Path = strings.TrimPrefix(req.In.URL.Path, "/v1/workspaces/"+workspace.ID+"/ide")
			req.Out.URL.RawPath = ""
			req.Out.Header.Del("Authorization")
			req.Out.Header.Del("Cookie")
			req.Out.Header.Del("X-Kamui-Token")
		},
		ModifyResponse: func(response *http.Response) error {
			response.Header.Del("Set-Cookie")
			response.Header.Set("Cache-Control", "no-store")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			apierr.WriteError(w, http.StatusBadGateway, "ide_unavailable", "the workspace IDE could not complete the request")
		},
	}
	if r.Header.Get("Upgrade") != "" {
		// WebSocket upgrade - pass through
		proxy.ServeHTTP(w, r)
		return
	}
	// Regular HTTP request
	proxy.ServeHTTP(w, r)
}

// IdleShutdown manually triggers the idle workspace check.
// POST /v1/workspaces/idle-shutdown
func (h *WorkspaceHandler) IdleShutdown(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authorizeProject(w, r, "", identity.PermWorkspaceWrite); !ok {
		return
	}
	h.checkIdleWorkspaces()
	apierr.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}
