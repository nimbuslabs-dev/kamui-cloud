package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/registry"
)

type Workspace struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	RepositoryURL   string     `json:"repository_url"`
	Branch          string     `json:"branch"`
	Status          string     `json:"status"` // creating, running, stopped, error
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	LastActiveAt    *time.Time `json:"last_active_at,omitempty"`
	ContainerID     string     `json:"container_id,omitempty"`
	CodeServerPort  int        `json:"code_server_port,omitempty"`
	CodeServerURL   string     `json:"code_server_url,omitempty"`
}

type WorkspaceCreate struct {
	Name           string `json:"name"`
	RepositoryURL  string `json:"repository_url"`
	Branch         string `json:"branch,omitempty"`
}

type WorkspaceStore struct {
	mu          sync.RWMutex
	workspaces  map[string]*Workspace
	managerURL  string
	token       string
	now         func() time.Time
}

func NewWorkspaceStore(managerURL, token string) *WorkspaceStore {
	return &WorkspaceStore{
		workspaces: make(map[string]*Workspace),
		managerURL: managerURL,
		token:      token,
		now:        time.Now,
	}
}

func (s *WorkspaceStore) generateID() string {
	for {
		var b [4]byte
		_, _ = rand.Read(b[:])
		id := "ws-" + hex.EncodeToString(b[:])
		if _, exists := s.workspaces[id]; !exists {
			return id
		}
	}
}

func (s *WorkspaceStore) Create(req WorkspaceCreate) *Workspace {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := s.generateID()
	now := s.now()
	branch := req.Branch
	if branch == "" {
		branch = "main"
	}

	ws := &Workspace{
		ID:             id,
		Name:           req.Name,
		RepositoryURL:  req.RepositoryURL,
		Branch:         branch,
		Status:         "creating",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	s.workspaces[id] = ws
	return ws
}

func (s *WorkspaceStore) List() []Workspace {
	s.mu.RLock()
	defer s.mu.RUnlock()

	workspaces := make([]Workspace, 0, len(s.workspaces))
	for _, ws := range s.workspaces {
		workspaces = append(workspaces, *ws)
	}
	return workspaces
}

func (s *WorkspaceStore) Get(id string) (*Workspace, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ws, exists := s.workspaces[id]
	if !exists {
		return nil, ErrWorkspaceNotFound
	}
	return ws, nil
}

func (s *WorkspaceStore) Update(id string, updates func(*Workspace)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	ws, exists := s.workspaces[id]
	if !exists {
		return ErrWorkspaceNotFound
	}
	updates(ws)
	ws.UpdatedAt = s.now()
	return nil
}

func (s *WorkspaceStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.workspaces[id]; !exists {
		return ErrWorkspaceNotFound
	}
	delete(s.workspaces, id)
	return nil
}

var ErrWorkspaceNotFound = errors.New("workspace not found")

type WorkspaceHandler struct {
	store      *WorkspaceStore
	reg        *registry.Registry
	managerURL string
	token      string
	httpClient *http.Client
}

func NewWorkspaceHandler(reg *registry.Registry, managerURL, token string) *WorkspaceHandler {
	return &WorkspaceHandler{
		store:      NewWorkspaceStore(managerURL, token),
		reg:        reg,
		managerURL: managerURL,
		token:      token,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (h *WorkspaceHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req WorkspaceCreate
	if err := readJSON(w, r, 64*1024, &req); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}

	if req.Name == "" {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}
	if req.RepositoryURL == "" {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "repository_url is required")
		return
	}
	if !strings.HasPrefix(req.RepositoryURL, "http://") && !strings.HasPrefix(req.RepositoryURL, "https://") {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "repository_url must be http or https")
		return
	}

	ws := h.store.Create(req)
	apierr.WriteJSON(w, http.StatusCreated, ws)
}

func (h *WorkspaceHandler) List(w http.ResponseWriter, r *http.Request) {
	// Sync with workspace manager for actual status
	workspaces := h.store.List()
	
	// Update status from workspace manager
	for i := range workspaces {
		h.syncWorkspaceStatus(r.Context(), &workspaces[i])
	}
	
	apierr.WriteJSON(w, http.StatusOK, workspaces)
}

func (h *WorkspaceHandler) syncWorkspaceStatus(ctx context.Context, ws *Workspace) {
	url := h.managerURL + "/workspace/status?id=" + ws.ID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return
	}
	
	var statusResp struct {
		Status          string `json:"status"`
		CodeServerPort  int    `json:"code_server_port"`
		ContainerID     string `json:"container_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		return
	}
	
	h.store.Update(ws.ID, func(w *Workspace) {
		w.Status = statusResp.Status
		w.CodeServerPort = statusResp.CodeServerPort
		w.ContainerID = statusResp.ContainerID
		w.CodeServerURL = "/workspace/" + ws.ID
		if statusResp.Status == "running" {
			now := h.store.now()
			w.LastActiveAt = &now
		}
	})
}

func (h *WorkspaceHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ws, err := h.store.Get(id)
	if err != nil {
		apierr.WriteError(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	
	// Sync status before returning
	h.syncWorkspaceStatus(r.Context(), ws)
	
	// Re-fetch updated workspace
	ws, _ = h.store.Get(id)
	apierr.WriteJSON(w, http.StatusOK, ws)
}

func (h *WorkspaceHandler) Start(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	
	ws, err := h.store.Get(id)
	if err != nil {
		apierr.WriteError(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	
	if ws.Status == "running" {
		apierr.WriteError(w, http.StatusConflict, "conflict", "workspace already running")
		return
	}
	
	// Call workspace manager to start
	url := h.managerURL + "/workspace/start"
	body := map[string]interface{}{
		"id":              ws.ID,
		"name":            ws.Name,
		"repository_url":  ws.RepositoryURL,
		"branch":          ws.Branch,
	}
	jsonBody, _ := json.Marshal(body)
	
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(jsonBody))
	if err != nil {
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	
	resp, err := h.httpClient.Do(req)
	if err != nil {
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "failed to contact workspace manager")
		return
	}
	defer resp.Body.Close()
	
	if resp.StatusCode >= 400 {
		var errResp apierr.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		apierr.WriteError(w, resp.StatusCode, errResp.Error.Code, errResp.Error.Message)
		return
	}
	
	// Sync status
	h.syncWorkspaceStatus(r.Context(), ws)
	ws, _ = h.store.Get(id)
	apierr.WriteJSON(w, http.StatusOK, ws)
}

func (h *WorkspaceHandler) Stop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	
	ws, err := h.store.Get(id)
	if err != nil {
		apierr.WriteError(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	
	if ws.Status != "running" {
		apierr.WriteError(w, http.StatusConflict, "conflict", "workspace not running")
		return
	}
	
	url := h.managerURL + "/workspace/stop"
	body := map[string]string{"workspace_id": ws.ID}
	jsonBody, _ := json.Marshal(body)
	
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(jsonBody))
	if err != nil {
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	
	resp, err := h.httpClient.Do(req)
	if err != nil {
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "failed to contact workspace manager")
		return
	}
	defer resp.Body.Close()
	
	if resp.StatusCode >= 400 {
		var errResp apierr.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		apierr.WriteError(w, resp.StatusCode, errResp.Error.Code, errResp.Error.Message)
		return
	}
	
	h.syncWorkspaceStatus(r.Context(), ws)
	ws, _ = h.store.Get(id)
	apierr.WriteJSON(w, http.StatusOK, ws)
}

func (h *WorkspaceHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	
	ws, err := h.store.Get(id)
	if err != nil {
		apierr.WriteError(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	
	// Call workspace manager to delete
	url := h.managerURL + "/workspace/delete"
	body := map[string]string{"workspace_id": ws.ID}
	jsonBody, _ := json.Marshal(body)
	
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(jsonBody))
	if err != nil {
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	
	resp, err := h.httpClient.Do(req)
	if err != nil {
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "failed to contact workspace manager")
		return
	}
	defer resp.Body.Close()
	
	if resp.StatusCode >= 400 {
		var errResp apierr.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		apierr.WriteError(w, resp.StatusCode, errResp.Error.Code, errResp.Error.Message)
		return
	}
	
	// Remove from local store
	_ = h.store.Delete(id)
	w.WriteHeader(http.StatusNoContent)
}

type WorkspaceUpdate struct {
	Status           *string `json:"status,omitempty"`
	CodeServerPort   *int    `json:"code_server_port,omitempty"`
	CodeServerURL    *string `json:"code_server_url,omitempty"`
	ContainerID      *string `json:"container_id,omitempty"`
	LastActiveAt     *string `json:"last_active_at,omitempty"`
}

func (h *WorkspaceHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	
	var req WorkspaceUpdate
	if err := readJSON(w, r, 64*1024, &req); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	
	err := h.store.Update(id, func(ws *Workspace) {
		if req.Status != nil {
			ws.Status = *req.Status
		}
		if req.CodeServerPort != nil {
			ws.CodeServerPort = *req.CodeServerPort
		}
		if req.CodeServerURL != nil {
			ws.CodeServerURL = *req.CodeServerURL
		}
		if req.ContainerID != nil {
			ws.ContainerID = *req.ContainerID
		}
		if req.LastActiveAt != nil {
			if t, err := time.Parse(time.RFC3339, *req.LastActiveAt); err == nil {
				ws.LastActiveAt = &t
			}
		}
	})
	
	if err != nil {
		if errors.Is(err, ErrWorkspaceNotFound) {
			apierr.WriteError(w, http.StatusNotFound, "not_found", "workspace not found")
			return
		}
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	
	ws, _ := h.store.Get(id)
	apierr.WriteJSON(w, http.StatusOK, ws)
}

// Git status types
type GitStatus struct {
	Branch    string   `json:"branch"`
	Ahead     int      `json:"ahead"`
	Behind    int      `json:"behind"`
	Staged    []string `json:"staged"`
	Unstaged  []string `json:"unstaged"`
	Untracked []string `json:"untracked"`
}

type GitAddRequest struct {
	Files []string `json:"files"`
}

type GitCommitRequest struct {
	Message string `json:"message"`
}

type GitPushPullRequest struct {
	Remote string `json:"remote,omitempty"`
	Branch string `json:"branch,omitempty"`
}

func (h *WorkspaceHandler) GitStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ws, err := h.store.Get(id)
	if err != nil {
		apierr.WriteError(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	
	if ws.Status != "running" {
		apierr.WriteError(w, http.StatusConflict, "conflict", "workspace not running")
		return
	}
	
	// Call workspace manager for git status
	url := h.managerURL + "/workspace/git"
	body := map[string]interface{}{
		"workspace_id": ws.ID,
		"command":      "status",
	}
	jsonBody, _ := json.Marshal(body)
	
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(jsonBody))
	if err != nil {
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	
	resp, err := h.httpClient.Do(req)
	if err != nil {
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "failed to contact workspace manager")
		return
	}
	defer resp.Body.Close()
	
	if resp.StatusCode >= 400 {
		var errResp apierr.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		apierr.WriteError(w, resp.StatusCode, errResp.Error.Code, errResp.Error.Message)
		return
	}
	
	var gitResp struct {
		Branch     string   `json:"branch"`
		Staged     []string `json:"staged"`
		Unstaged   []string `json:"unstaged"`
		Untracked  []string `json:"untracked"`
		RawOutput  string   `json:"raw_output"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&gitResp); err != nil {
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "failed to parse git status")
		return
	}
	
	status := GitStatus{
		Branch:    gitResp.Branch,
		Ahead:     0, // Would need git rev-list to compute
		Behind:    0,
		Staged:    gitResp.Staged,
		Unstaged:  gitResp.Unstaged,
		Untracked: gitResp.Untracked,
	}
	apierr.WriteJSON(w, http.StatusOK, status)
}

func (h *WorkspaceHandler) GitAdd(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ws, err := h.store.Get(id)
	if err != nil {
		apierr.WriteError(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	
	if ws.Status != "running" {
		apierr.WriteError(w, http.StatusConflict, "conflict", "workspace not running")
		return
	}
	
	var addReq GitAddRequest
	if err := readJSON(w, r, 64*1024, &addReq); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	
	url := h.managerURL + "/workspace/git"
	body := map[string]interface{}{
		"workspace_id": ws.ID,
		"command":      "add",
		"args":         addReq.Files,
	}
	jsonBody, _ := json.Marshal(body)
	
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(jsonBody))
	if err != nil {
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+h.token)
	httpReq.Header.Set("Content-Type", "application/json")
	
	resp, err := h.httpClient.Do(httpReq)
	if err != nil {
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "failed to contact workspace manager")
		return
	}
	defer resp.Body.Close()
	
	if resp.StatusCode >= 400 {
		var errResp apierr.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		apierr.WriteError(w, resp.StatusCode, errResp.Error.Code, errResp.Error.Message)
		return
	}
	
	w.WriteHeader(http.StatusNoContent)
}

func (h *WorkspaceHandler) GitCommit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ws, err := h.store.Get(id)
	if err != nil {
		apierr.WriteError(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	
	if ws.Status != "running" {
		apierr.WriteError(w, http.StatusConflict, "conflict", "workspace not running")
		return
	}
	
	var commitReq GitCommitRequest
	if err := readJSON(w, r, 64*1024, &commitReq); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	
	if commitReq.Message == "" {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "message is required")
		return
	}
	
	url := h.managerURL + "/workspace/git"
	body := map[string]interface{}{
		"workspace_id": ws.ID,
		"command":      "commit",
		"args":         []string{commitReq.Message},
	}
	jsonBody, _ := json.Marshal(body)
	
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(jsonBody))
	if err != nil {
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+h.token)
	httpReq.Header.Set("Content-Type", "application/json")
	
	resp, err := h.httpClient.Do(httpReq)
	if err != nil {
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "failed to contact workspace manager")
		return
	}
	defer resp.Body.Close()
	
	if resp.StatusCode >= 400 {
		var errResp apierr.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		apierr.WriteError(w, resp.StatusCode, errResp.Error.Code, errResp.Error.Message)
		return
	}
	
	var result struct {
		Output string `json:"output"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&result)
	
	// Extract commit hash from output
	lines := strings.Split(result.Output, "\n")
	commitHash := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "[") && strings.Contains(line, "]") {
			parts := strings.Split(line, " ")
			for _, p := range parts {
				if len(p) >= 7 && len(p) <= 40 {
					commitHash = p
					break
				}
			}
		}
	}
	if commitHash == "" {
		commitHash = "committed-" + time.Now().Format("20060102150405")
	}
	
	apierr.WriteJSON(w, http.StatusOK, map[string]string{
		"hash": commitHash,
	})
}

func (h *WorkspaceHandler) GitPush(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ws, err := h.store.Get(id)
	if err != nil {
		apierr.WriteError(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	
	if ws.Status != "running" {
		apierr.WriteError(w, http.StatusConflict, "conflict", "workspace not running")
		return
	}
	
	var pushReq GitPushPullRequest
	_ = readJSON(w, r, 64*1024, &pushReq)
	
	url := h.managerURL + "/workspace/git"
	body := map[string]interface{}{
		"workspace_id": ws.ID,
		"command":      "push",
		"args":         []string{pushReq.Remote, pushReq.Branch},
	}
	jsonBody, _ := json.Marshal(body)
	
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(jsonBody))
	if err != nil {
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+h.token)
	httpReq.Header.Set("Content-Type", "application/json")
	
	resp, err := h.httpClient.Do(httpReq)
	if err != nil {
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "failed to contact workspace manager")
		return
	}
	defer resp.Body.Close()
	
	if resp.StatusCode >= 400 {
		var errResp apierr.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		apierr.WriteError(w, resp.StatusCode, errResp.Error.Code, errResp.Error.Message)
		return
	}
	
	w.WriteHeader(http.StatusNoContent)
}

func (h *WorkspaceHandler) GitPull(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ws, err := h.store.Get(id)
	if err != nil {
		apierr.WriteError(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	
	if ws.Status != "running" {
		apierr.WriteError(w, http.StatusConflict, "conflict", "workspace not running")
		return
	}
	
	var pullReq GitPushPullRequest
	_ = readJSON(w, r, 64*1024, &pullReq)
	
	url := h.managerURL + "/workspace/git"
	body := map[string]interface{}{
		"workspace_id": ws.ID,
		"command":      "pull",
		"args":         []string{pullReq.Remote, pullReq.Branch},
	}
	jsonBody, _ := json.Marshal(body)
	
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(jsonBody))
	if err != nil {
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+h.token)
	httpReq.Header.Set("Content-Type", "application/json")
	
	resp, err := h.httpClient.Do(httpReq)
	if err != nil {
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "failed to contact workspace manager")
		return
	}
	defer resp.Body.Close()
	
	if resp.StatusCode >= 400 {
		var errResp apierr.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		apierr.WriteError(w, resp.StatusCode, errResp.Error.Code, errResp.Error.Message)
		return
	}
	
	w.WriteHeader(http.StatusNoContent)
}