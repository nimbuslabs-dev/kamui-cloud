package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type WorkspaceConfig struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	RepositoryURL  string `json:"repository_url"`
	Branch         string `json:"branch"`
	CodeServerPort int    `json:"code_server_port"`
}

type WorkspaceManager struct {
	apiURL       string
	token        string
	mu           sync.Mutex
	portAllocator int
	workspaces   map[string]*WorkspaceConfig
}

func NewWorkspaceManager(apiURL, token string) *WorkspaceManager {
	return &WorkspaceManager{
		apiURL:        apiURL,
		token:         token,
		portAllocator: 8081,
		workspaces:    make(map[string]*WorkspaceConfig),
	}
}

func (wm *WorkspaceManager) allocatePort() int {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	port := wm.portAllocator
	wm.portAllocator++
	return port
}

func (wm *WorkspaceManager) StartWorkspace(ctx context.Context, config WorkspaceConfig) error {
	port := wm.allocatePort()
	config.CodeServerPort = port
	config.ID = strings.TrimPrefix(config.ID, "ws-")

	log.Printf("Starting workspace %s on port %d (simulated)", config.ID, port)

	// Simulate startup delay
	time.Sleep(1 * time.Second)

	wm.workspaces[config.ID] = &config

	if err := wm.updateWorkspaceStatus(ctx, config.ID, "running", port); err != nil {
		log.Printf("Warning: failed to update API: %v", err)
	}

	log.Printf("Started workspace %s on port %d", config.ID, port)
	return nil
}

func (wm *WorkspaceManager) StopWorkspace(ctx context.Context, workspaceID string) error {
	log.Printf("Stopping workspace %s (simulated)", workspaceID)

	if err := wm.updateWorkspaceStatus(ctx, workspaceID, "stopped", 0); err != nil {
		log.Printf("Warning: failed to update API: %v", err)
	}

	delete(wm.workspaces, workspaceID)
	log.Printf("Stopped workspace %s", workspaceID)
	return nil
}

func (wm *WorkspaceManager) DeleteWorkspace(ctx context.Context, workspaceID string) error {
	log.Printf("Deleting workspace %s (simulated)", workspaceID)

	if err := wm.StopWorkspace(ctx, workspaceID); err != nil {
		log.Printf("Warning: failed to stop workspace: %v", err)
	}

	log.Printf("Deleted workspace %s", workspaceID)
	return nil
}

func (wm *WorkspaceManager) updateWorkspaceStatus(ctx context.Context, workspaceID, status string, port int) error {
	url := fmt.Sprintf("%s/v1/workspaces/%s", wm.apiURL, workspaceID)

	body := map[string]interface{}{
		"status":             status,
		"code_server_port":   port,
		"code_server_url":    fmt.Sprintf("/workspace/%s", workspaceID),
		"last_active_at":     time.Now().Format(time.RFC3339),
	}

	jsonBody, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, "PATCH", url, strings.NewReader(string(jsonBody)))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+wm.token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	return nil
}

func (wm *WorkspaceManager) HealthCheck(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func main() {
	apiURL := os.Getenv("API_URL")
	if apiURL == "" {
		apiURL = "http://localhost:8080"
	}

	token := os.Getenv("KAMUI_TOKEN")
	if token == "" {
		log.Fatal("KAMUI_TOKEN is required")
	}

	manager := NewWorkspaceManager(apiURL, token)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", manager.HealthCheck)

	mux.HandleFunc("/workspace/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var config WorkspaceConfig
		if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		if err := manager.StartWorkspace(ctx, config); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "started"})
	})

	mux.HandleFunc("/workspace/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			WorkspaceID string `json:"workspace_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		if err := manager.StopWorkspace(ctx, req.WorkspaceID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "stopped"})
	})

	mux.HandleFunc("/workspace/delete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			WorkspaceID string `json:"workspace_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		if err := manager.DeleteWorkspace(ctx, req.WorkspaceID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
	})

	server := &http.Server{
		Addr:    ":8082",
		Handler: mux,
	}

	go func() {
		log.Println("Workspace manager listening on :8082")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	server.Shutdown(ctx)
}