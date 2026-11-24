package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

type WorkspaceConfig struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	RepositoryURL  string `json:"repository_url"`
	Branch         string `json:"branch"`
	CodeServerPort int    `json:"code_server_port"`
}

type WorkspaceManager struct {
	dockerClient *client.Client
	apiURL       string
	token        string
	mu           sync.Mutex
	portAllocator int
	workspaces   map[string]*WorkspaceConfig
}

func NewWorkspaceManager(apiURL, token string) (*WorkspaceManager, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	return &WorkspaceManager{
		dockerClient:  cli,
		apiURL:        apiURL,
		token:         token,
		portAllocator: 8081,
		workspaces:    make(map[string]*WorkspaceConfig),
	}, nil
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

	workspaceDir := fmt.Sprintf("/workspaces/%s", config.ID)
	containerName := fmt.Sprintf("kamui-workspace-%s", config.ID)

	// Pull code-server image
	log.Printf("Pulling code-server image...")
	pullResp, err := wm.dockerClient.ImagePull(ctx, "codercom/code-server:4.98.2", image.PullOptions{})
	if err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}
	io.Copy(io.Discard, pullResp)
	pullResp.Close()

	hostConfig := &container.HostConfig{
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeBind,
				Source: workspaceDir,
				Target: "/home/coder/project",
			},
		},
		PortBindings: nat.PortMap{
			nat.Port("8080/tcp"): []nat.PortBinding{
				{HostIP: "0.0.0.0", HostPort: strconv.Itoa(port)},
			},
		},
		Resources: container.Resources{
			CPUCount: 2,
			Memory:   4 * 1024 * 1024 * 1024, // 4GB
		},
		RestartPolicy: container.RestartPolicy{
			Name: "unless-stopped",
		},
	}

	containerConfig := &container.Config{
		Image: "codercom/code-server:4.98.2",
		Env: []string{
			fmt.Sprintf("KAMUI_WORKSPACE_ID=%s", config.ID),
			fmt.Sprintf("KAMUI_WORKSPACE_NAME=%s", config.Name),
			fmt.Sprintf("KAMUI_REPOSITORY_URL=%s", config.RepositoryURL),
			fmt.Sprintf("KAMUI_BRANCH=%s", config.Branch),
			"DOCKER_USER=coder",
			"PASSWORD=",
			"SUDO_PASSWORD=",
		},
		ExposedPorts: nat.PortSet{
			nat.Port("8080/tcp"): {},
		},
		Cmd: []string{
			"--bind-addr", "0.0.0.0:8080",
			"--auth", "none",
			"/home/coder/project",
		},
		User: "1000:1000",
	}

	networkingConfig := &network.NetworkingConfig{}

	resp, err := wm.dockerClient.ContainerCreate(ctx, containerConfig, hostConfig, networkingConfig, nil, containerName)
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}

	if err := wm.dockerClient.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}

	// Wait for container to be ready
	time.Sleep(3 * time.Second)

	// Clone repository if needed
	if err := wm.cloneRepository(ctx, resp.ID, config.RepositoryURL, config.Branch); err != nil {
		log.Printf("Warning: failed to clone repository: %v", err)
	}

	wm.workspaces[config.ID] = &config

	if err := wm.updateWorkspaceStatus(ctx, config.ID, "running", port, resp.ID); err != nil {
		log.Printf("Warning: failed to update API: %v", err)
	}

	log.Printf("Started workspace %s on port %d (container: %s)", config.ID, port, resp.ID[:12])
	return nil
}

func (wm *WorkspaceManager) cloneRepository(ctx context.Context, containerID, repoURL, branch string) error {
	// Check if repo already exists
	checkConfig := container.ExecOptions{
		Cmd:          []string{"sh", "-c", "test -d /home/coder/project/.git"},
		AttachStdout: true,
		AttachStderr: true,
		User:         "1000",
	}

	execResp, err := wm.dockerClient.ContainerExecCreate(ctx, containerID, checkConfig)
	if err != nil {
		return err
	}

	resp, err := wm.dockerClient.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return err
	}
	defer resp.Close()

	// If repo exists, just fetch latest
	repoExists := true
	buf := make([]byte, 1024)
	n, _ := resp.Reader.Read(buf)
	if n > 0 && strings.Contains(string(buf[:n]), "No such file") {
		repoExists = false
	}

	if repoExists {
		log.Printf("Repository already exists, fetching latest...")
		return wm.execInContainer(ctx, containerID, []string{"git", "fetch", "origin"})
	}

	// Clone the repository
	log.Printf("Cloning repository: %s", repoURL)
	cloneConfig := container.ExecOptions{
		Cmd:          []string{"sh", "-c", fmt.Sprintf("git clone --branch %s %s /home/coder/project 2>&1", branch, repoURL)},
		AttachStdout: true,
		AttachStderr: true,
		User:         "1000",
	}

	execResp, err = wm.dockerClient.ContainerExecCreate(ctx, containerID, cloneConfig)
	if err != nil {
		return err
	}

	resp, err = wm.dockerClient.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return err
	}
	defer resp.Close()

	buf = make([]byte, 4096)
	for {
		n, err := resp.Reader.Read(buf)
		if n > 0 {
			log.Printf("Git clone: %s", string(buf[:n]))
		}
		if err != nil {
			break
		}
	}

	return nil
}

func (wm *WorkspaceManager) execInContainer(ctx context.Context, containerID string, cmd []string) error {
	execConfig := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
		User:         "1000",
	}

	execResp, err := wm.dockerClient.ContainerExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return err
	}

	resp, err := wm.dockerClient.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return err
	}
	defer resp.Close()

	buf := make([]byte, 4096)
	for {
		n, err := resp.Reader.Read(buf)
		if n > 0 {
			// Output is multiplexed, just discard
		}
		if err != nil {
			break
		}
	}

	return nil
}

func (wm *WorkspaceManager) StopWorkspace(ctx context.Context, workspaceID string) error {
	containerName := fmt.Sprintf("kamui-workspace-%s", workspaceID)

	filterArgs := filters.NewArgs()
	filterArgs.Add("name", containerName)

	containers, err := wm.dockerClient.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filterArgs,
	})
	if err != nil {
		return err
	}

	if len(containers) == 0 {
		return fmt.Errorf("container not found")
	}

	timeout := 10
	if err := wm.dockerClient.ContainerStop(ctx, containers[0].ID, container.StopOptions{Timeout: &timeout}); err != nil {
		return err
	}

	if err := wm.updateWorkspaceStatus(ctx, workspaceID, "stopped", 0, ""); err != nil {
		log.Printf("Warning: failed to update API: %v", err)
	}

	delete(wm.workspaces, workspaceID)
	log.Printf("Stopped workspace %s", workspaceID)
	return nil
}

func (wm *WorkspaceManager) DeleteWorkspace(ctx context.Context, workspaceID string) error {
	if err := wm.StopWorkspace(ctx, workspaceID); err != nil {
		log.Printf("Warning: failed to stop workspace: %v", err)
	}

	containerName := fmt.Sprintf("kamui-workspace-%s", workspaceID)
	filterArgs := filters.NewArgs()
	filterArgs.Add("name", containerName)

	containers, err := wm.dockerClient.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filterArgs,
	})
	if err != nil {
		return err
	}

	for _, c := range containers {
		if err := wm.dockerClient.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil {
			log.Printf("Warning: failed to remove container: %v", err)
		}
	}

	// Note: We keep the workspace directory for persistence
	// To fully delete: os.RemoveAll(fmt.Sprintf("/workspaces/%s", workspaceID))

	log.Printf("Deleted workspace %s", workspaceID)
	return nil
}

func (wm *WorkspaceManager) GetWorkspaceStatus(ctx context.Context, workspaceID string) (string, int, string, error) {
	containerName := fmt.Sprintf("kamui-workspace-%s", workspaceID)
	filterArgs := filters.NewArgs()
	filterArgs.Add("name", containerName)

	containers, err := wm.dockerClient.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filterArgs,
	})
	if err != nil {
		return "", 0, "", err
	}

	if len(containers) == 0 {
		return "not_found", 0, "", fmt.Errorf("container not found")
	}

	c := containers[0]
	status := c.State
	port := 0
	for _, p := range c.Ports {
		if p.PrivatePort == 8080 && p.Type == "tcp" {
			port = int(p.PublicPort)
			break
		}
	}

	return status, port, c.ID[:12], nil
}

func (wm *WorkspaceManager) updateWorkspaceStatus(ctx context.Context, workspaceID, status string, port int, containerID string) error {
	url := fmt.Sprintf("%s/v1/workspaces/%s", wm.apiURL, workspaceID)

	body := map[string]interface{}{
		"status":             status,
		"code_server_port":   port,
		"code_server_url":    fmt.Sprintf("/workspace/%s", workspaceID),
		"last_active_at":     time.Now().Format(time.RFC3339),
		"container_id":       containerID,
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

	manager, err := NewWorkspaceManager(apiURL, token)
	if err != nil {
		log.Fatalf("Failed to create workspace manager: %v", err)
	}

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

	mux.HandleFunc("/workspace/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		workspaceID := r.URL.Query().Get("id")
		if workspaceID == "" {
			http.Error(w, "id parameter required", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		status, port, containerID, err := manager.GetWorkspaceStatus(ctx, workspaceID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":           status,
			"code_server_port": port,
			"container_id":     containerID,
		})
	})

	mux.HandleFunc("/workspace/git", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			WorkspaceID string   `json:"workspace_id"`
			Command     string   `json:"command"` // status, add, commit, push, pull
			Args        []string `json:"args"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		containerName := fmt.Sprintf("kamui-workspace-%s", req.WorkspaceID)
		ctx := r.Context()

		filterArgs := filters.NewArgs()
		filterArgs.Add("name", containerName)

		containers, err := manager.dockerClient.ContainerList(ctx, container.ListOptions{
			All:     true,
			Filters: filterArgs,
		})
		if err != nil || len(containers) == 0 {
			http.Error(w, "workspace container not found", http.StatusNotFound)
			return
		}

		containerID := containers[0].ID
		var cmd []string

		switch req.Command {
		case "status":
			cmd = []string{"git", "status", "--porcelain=v1", "--branch"}
		case "add":
			if len(req.Args) == 0 {
				cmd = []string{"git", "add", "-A"}
			} else {
				cmd = append([]string{"git", "add"}, req.Args...)
			}
		case "commit":
			if len(req.Args) == 0 {
				http.Error(w, "commit message required", http.StatusBadRequest)
				return
			}
			cmd = []string{"git", "commit", "-m", req.Args[0]}
		case "push":
			remote := "origin"
			branch := ""
			if len(req.Args) > 0 {
				remote = req.Args[0]
			}
			if len(req.Args) > 1 {
				branch = req.Args[1]
			}
			if branch != "" {
				cmd = []string{"git", "push", remote, branch}
			} else {
				cmd = []string{"git", "push", remote}
			}
		case "pull":
			remote := "origin"
			branch := ""
			if len(req.Args) > 0 {
				remote = req.Args[0]
			}
			if len(req.Args) > 1 {
				branch = req.Args[1]
			}
			if branch != "" {
				cmd = []string{"git", "pull", remote, branch}
			} else {
				cmd = []string{"git", "pull", remote}
			}
		default:
			http.Error(w, "unknown command", http.StatusBadRequest)
			return
		}

		execConfig := container.ExecOptions{
			Cmd:          cmd,
			AttachStdout: true,
			AttachStderr: true,
			User:         "1000",
			WorkingDir:   "/home/coder/project",
		}

		execResp, err := manager.dockerClient.ContainerExecCreate(ctx, containerID, execConfig)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		resp, err := manager.dockerClient.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer resp.Close()

		var output strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := resp.Reader.Read(buf)
			if n > 0 {
				output.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if req.Command == "status" {
			// Parse git status output
			lines := strings.Split(strings.TrimSpace(output.String()), "\n")
			var branch string
			var staged, unstaged, untracked []string
			for _, line := range lines {
				if strings.HasPrefix(line, "## ") {
					branch = strings.TrimSpace(line[3:])
					if idx := strings.Index(branch, "..."); idx != -1 {
						branch = branch[:idx]
					}
				} else if len(line) >= 3 {
					statusCode := line[:2]
					file := strings.TrimSpace(line[3:])
					switch {
					case statusCode[0] != ' ' && statusCode[0] != '?':
						staged = append(staged, file)
					case statusCode[1] != ' ':
						unstaged = append(unstaged, file)
					case statusCode[0] == '?' && statusCode[1] == '?':
						untracked = append(untracked, file)
					}
				}
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"branch":     branch,
				"staged":     staged,
				"unstaged":   unstaged,
				"untracked":  untracked,
				"raw_output": output.String(),
			})
		} else {
			json.NewEncoder(w).Encode(map[string]string{
				"output": output.String(),
			})
		}
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