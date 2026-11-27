package httpapi

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/workspaceinput"
)

type Workspace struct {
	ID            string     `json:"id"`
	ProjectID     string     `json:"project_id,omitempty"`
	OwnerID       string     `json:"owner_id,omitempty"`
	Name          string     `json:"name"`
	RepositoryURL string     `json:"repository_url"`
	Branch        string     `json:"branch"`
	Status        string     `json:"status"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	LastActiveAt  *time.Time `json:"last_active_at,omitempty"`
	ContainerID   string     `json:"container_id,omitempty"`
	CodeServerURL string     `json:"code_server_url,omitempty"`
}

type WorkspaceCreate struct {
	Name          string `json:"name"`
	RepositoryURL string `json:"repository_url"`
	Branch        string `json:"branch,omitempty"`
	ProjectID     string `json:"project_id,omitempty"`
}

type workspaceFile struct {
	Version    int                  `json:"version"`
	Workspaces map[string]Workspace `json:"workspaces"`
}

type workspaceStore struct {
	mu         sync.RWMutex
	path       string
	workspaces map[string]Workspace
}

func openWorkspaceStore(path string) (*workspaceStore, error) {
	store := &workspaceStore{path: path, workspaces: make(map[string]Workspace)}
	if path == "" {
		return store, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, store.save(store.workspaces)
	}
	if err != nil {
		return nil, fmt.Errorf("read workspace metadata: %w", err)
	}

	var file workspaceFile
	if err := decodeJSON(bytes.NewReader(data), &file); err != nil || file.Version != 1 || file.Workspaces == nil {
		return nil, errors.New("invalid workspace metadata file or unsupported version")
	}
	for id, workspace := range file.Workspaces {
		if !workspaceinput.ValidID(id) || id != workspace.ID || validateWorkspaceCreate(WorkspaceCreate{
			Name: workspace.Name, RepositoryURL: workspace.RepositoryURL, Branch: workspace.Branch,
		}) != "" {
			return nil, errors.New("invalid workspace record in metadata file")
		}
	}
	store.workspaces = file.Workspaces

	// Verify the directory permits future atomic replacements without rewriting existing data.
	probe, err := os.CreateTemp(filepath.Dir(path), ".workspaces-*")
	if err != nil {
		return nil, fmt.Errorf("verify workspace metadata directory: %w", err)
	}
	if err := errors.Join(probe.Close(), os.Remove(probe.Name())); err != nil {
		return nil, fmt.Errorf("clean up workspace metadata probe: %w", err)
	}
	return store, nil
}

func (s *workspaceStore) save(workspaces map[string]Workspace) error {
	if s.path == "" {
		return nil
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer dir.Close()

	tmp, err := os.CreateTemp(dir.Name(), ".workspaces-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	err = json.NewEncoder(tmp).Encode(workspaceFile{Version: 1, Workspaces: workspaces})
	if err == nil {
		err = tmp.Sync()
	}
	if err = errors.Join(err, tmp.Close()); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return err
	}

	// The rename has committed the visible state. Keep memory consistent even if
	// the filesystem cannot confirm directory durability after a power loss.
	s.workspaces = workspaces
	return dir.Sync()
}

func (s *workspaceStore) put(id string, ws *Workspace) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := maps.Clone(s.workspaces)
	if ws == nil {
		delete(next, id)
	} else {
		next[id] = *ws
	}
	if err := s.save(next); err != nil {
		return err
	}
	s.workspaces = next
	return nil
}

func (s *workspaceStore) create(input WorkspaceCreate) (Workspace, error) {
	return s.createOwned(input, "", "")
}

func (s *workspaceStore) createOwned(input WorkspaceCreate, projectID, ownerID string) (Workspace, error) {
	now := time.Now().UTC()
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Workspace{}, err
	}
	if projectID == "" {
		projectID = input.ProjectID
	}
	ws := Workspace{
		ID: "ws-" + hex.EncodeToString(id[:]), ProjectID: projectID, OwnerID: ownerID,
		Name:          strings.TrimSpace(input.Name),
		RepositoryURL: input.RepositoryURL, Branch: input.Branch, Status: "stopped",
		CreatedAt: now, UpdatedAt: now,
	}
	return ws, s.put(ws.ID, &ws)
}

func (s *workspaceStore) get(id string) (Workspace, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	workspace, ok := s.workspaces[id]
	return workspace, ok
}

func (s *workspaceStore) list() []Workspace {
	s.mu.RLock()
	defer s.mu.RUnlock()
	workspaces := make([]Workspace, 0, len(s.workspaces))
	for _, workspace := range s.workspaces {
		workspaces = append(workspaces, workspace)
	}
	slices.SortFunc(workspaces, func(a, b Workspace) int {
		if byCreated := b.CreatedAt.Compare(a.CreatedAt); byCreated != 0 {
			return byCreated
		}
		return strings.Compare(a.ID, b.ID)
	})
	return workspaces
}
