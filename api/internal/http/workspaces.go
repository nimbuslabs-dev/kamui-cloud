package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/registry"
)

type Workspace struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	RepositoryURL   string    `json:"repository_url"`
	Branch          string    `json:"branch"`
	Status          string    `json:"status"` // creating, running, stopped, error
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	LastActiveAt    *time.Time `json:"last_active_at,omitempty"`
	ContainerID     string    `json:"container_id,omitempty"`
	CodeServerPort  int       `json:"code_server_port,omitempty"`
	CodeServerURL   string    `json:"code_server_url,omitempty"`
}

type WorkspaceCreate struct {
	Name           string `json:"name"`
	RepositoryURL  string `json:"repository_url"`
	Branch         string `json:"branch,omitempty"`
}

type WorkspaceStore struct {
	mu       sync.RWMutex
	workspaces map[string]*Workspace
	now      func() time.Time
}

func NewWorkspaceStore() *WorkspaceStore {
	return &WorkspaceStore{
		workspaces: make(map[string]*Workspace),
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
	store *WorkspaceStore
	reg   *registry.Registry
}

func NewWorkspaceHandler(reg *registry.Registry) *WorkspaceHandler {
	return &WorkspaceHandler{
		store: NewWorkspaceStore(),
		reg:   reg,
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
	workspaces := h.store.List()
	apierr.WriteJSON(w, http.StatusOK, workspaces)
}

func (h *WorkspaceHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ws, err := h.store.Get(id)
	if err != nil {
		apierr.WriteError(w, http.StatusNotFound, "not_found", "workspace not found")
		return
	}
	apierr.WriteJSON(w, http.StatusOK, ws)
}

func (h *WorkspaceHandler) Start(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	
	err := h.store.Update(id, func(ws *Workspace) {
		ws.Status = "running"
		now := h.store.now()
		ws.LastActiveAt = &now
		// In a real implementation, this would start a code-server container
		// For now, we simulate with a placeholder URL
		ws.CodeServerPort = 8080 + len(h.store.workspaces) // Simplified
		ws.CodeServerURL = "/workspace/" + id
		ws.ContainerID = "simulated-" + id
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

func (h *WorkspaceHandler) Stop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	
	err := h.store.Update(id, func(ws *Workspace) {
		ws.Status = "stopped"
		ws.CodeServerPort = 0
		ws.CodeServerURL = ""
		ws.ContainerID = ""
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

func (h *WorkspaceHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	
	err := h.store.Delete(id)
	if err != nil {
		if errors.Is(err, ErrWorkspaceNotFound) {
			apierr.WriteError(w, http.StatusNotFound, "not_found", "workspace not found")
			return
		}
		apierr.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	
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
	Branch   string   `json:"branch"`
	Ahead    int      `json:"ahead"`
	Behind   int      `json:"behind"`
	Staged   []string `json:"staged"`
	Unstaged []string `json:"unstaged"`
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
	
	// In a real implementation, this would query the workspace container
	// For now, return a simulated status
	status := GitStatus{
		Branch:    ws.Branch,
		Ahead:     0,
		Behind:    0,
		Staged:    []string{},
		Unstaged:  []string{},
		Untracked: []string{},
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
	
	var req GitAddRequest
	if err := readJSON(w, r, 64*1024, &req); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	
	// In a real implementation, this would run `git add` in the workspace container
	// For now, just acknowledge
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
	
	var req GitCommitRequest
	if err := readJSON(w, r, 64*1024, &req); err != nil {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid json")
		return
	}
	
	if req.Message == "" {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "message is required")
		return
	}
	
	// In a real implementation, this would run `git commit` in the workspace container
	// For now, return a simulated commit hash
	apierr.WriteJSON(w, http.StatusOK, map[string]string{
		"hash": "simulated-" + time.Now().Format("20060102150405"),
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
	
	var req GitPushPullRequest
	_ = readJSON(w, r, 64*1024, &req)
	
	// In a real implementation, this would run `git push` in the workspace container
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
	
	var req GitPushPullRequest
	_ = readJSON(w, r, 64*1024, &req)
	
	// In a real implementation, this would run `git pull` in the workspace container
	w.WriteHeader(http.StatusNoContent)
}