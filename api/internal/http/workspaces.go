package httpapi

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/workspaceinput"
)

type WorkspaceHandler struct {
	store      *workspaceStore
	managerURL string
	token      string
	client     *http.Client
	operations chan struct{}
}

func newWorkspaceHandler(dataPath, managerURL, token string) (*WorkspaceHandler, error) {
	store, err := openWorkspaceStore(dataPath)
	if err != nil {
		return nil, err
	}
	return &WorkspaceHandler{
		store: store, managerURL: strings.TrimRight(managerURL, "/"), token: token,
		client: &http.Client{
			Timeout: 75 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		operations: make(chan struct{}, 1),
	}, nil
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

func (h *WorkspaceHandler) Create(w http.ResponseWriter, r *http.Request) {
	var input WorkspaceCreate
	if !readWorkspaceJSON(w, r, &input) {
		return
	}
	if message := validateWorkspaceCreate(input); message != "" {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", message)
		return
	}
	workspace, err := h.store.create(input)
	if err != nil {
		apierr.WriteError(w, http.StatusInternalServerError, "storage_error", "could not save workspace metadata")
		return
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

	workspaces := h.store.list()
	for i := range workspaces {
		workspaces[i] = h.dockerStatus(ctx, workspaces[i])
	}
	apierr.WriteJSON(w, http.StatusOK, workspaces)
}

func (h *WorkspaceHandler) workspace(w http.ResponseWriter, r *http.Request) (Workspace, bool) {
	workspace, ok := h.store.get(r.PathValue("id"))
	if !ok {
		apierr.WriteError(w, http.StatusNotFound, "not_found", "workspace not found")
	}
	return workspace, ok
}

func (h *WorkspaceHandler) Get(w http.ResponseWriter, r *http.Request) {
	if workspace, ok := h.workspace(w, r); ok {
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

func (h *WorkspaceHandler) lifecycle(w http.ResponseWriter, r *http.Request, operation string) {
	if !h.acquire(w, r) {
		return
	}
	defer func() { <-h.operations }()

	workspace, ok := h.workspace(w, r)
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
		input = map[string]string{
			"id": workspace.ID, "name": workspace.Name,
			"repository_url": workspace.RepositoryURL, "branch": workspace.Branch,
		}
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
}

func (h *WorkspaceHandler) git(w http.ResponseWriter, r *http.Request, command string, args []string, result any) bool {
	if !h.acquire(w, r) {
		return false
	}
	defer func() { <-h.operations }()

	workspace, ok := h.workspace(w, r)
	if !ok {
		return false
	}
	input := gitCommand{WorkspaceID: workspace.ID, Command: command, Args: args}
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
