package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/docker/docker/client"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/apierr"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/auth"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/workspaceinput"
)

const projectDir = "/home/coder/project"

type WorkspaceConfig struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	RepositoryURL string `json:"repository_url"`
	Branch        string `json:"branch"`
}

type WorkspaceManager struct {
	docker      *client.Client
	project     string
	network     string
	image       string
	nanoCPUs    int64
	memoryBytes int64
	mutation    sync.Mutex
}

type managerError struct {
	status  int
	code    string
	message string
}

func (e *managerError) Error() string { return e.message }

var (
	errInvalidID   = &managerError{http.StatusBadRequest, "bad_request", "invalid workspace ID"}
	errNoSlot      = &managerError{http.StatusConflict, "workspace_limit", "stop another workspace before starting this one"}
	errStopped     = &managerError{http.StatusConflict, "workspace_stopped", "start the workspace before running Git"}
	projectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
)

func writeFailure(w http.ResponseWriter, err error) {
	var expected *managerError
	switch {
	case errors.As(err, &expected):
		apierr.WriteError(w, expected.status, expected.code, expected.message)
	case errors.Is(err, context.DeadlineExceeded):
		apierr.WriteError(w, http.StatusGatewayTimeout, "timeout", "workspace operation timed out")
	default:
		log.Printf("Workspace operation failed: %v", err)
		apierr.WriteError(w, http.StatusServiceUnavailable, "docker_error", "workspace operation failed, check Workspace Manager logs")
	}
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if d.Decode(dst) != nil || d.Decode(new(any)) != io.EOF {
		apierr.WriteError(w, http.StatusBadRequest, "bad_request", "invalid JSON request")
		return false
	}
	return true
}

func managerRouter(wm *WorkspaceManager, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		if err := wm.checkDocker(ctx); err != nil {
			writeFailure(w, err)
			return
		}
		apierr.WriteJSON(w, http.StatusOK, map[string]string{"status": "healthy"})
	})
	privileged := http.NewServeMux()
	privileged.HandleFunc("GET /workspace/status", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if !workspaceinput.ValidID(id) {
			writeFailure(w, errInvalidID)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		status, err := wm.workspaceStatus(ctx, id)
		if err != nil {
			writeFailure(w, err)
			return
		}
		apierr.WriteJSON(w, http.StatusOK, status)
	})
	privileged.HandleFunc("POST /workspace/start", func(w http.ResponseWriter, r *http.Request) {
		var cfg WorkspaceConfig
		if !decode(w, r, &cfg) {
			return
		}
		if !workspaceinput.ValidID(cfg.ID) {
			writeFailure(w, errInvalidID)
			return
		}
		if !workspaceinput.ValidRepositoryURL(cfg.RepositoryURL) || (cfg.Branch != "" && !workspaceinput.ValidBranch(cfg.Branch)) {
			apierr.WriteError(w, http.StatusBadRequest, "bad_request", "a GitHub HTTPS repository URL and valid optional branch are required")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 70*time.Second)
		defer cancel()
		if err := wm.startWorkspace(ctx, cfg); err != nil {
			writeFailure(w, err)
			return
		}
		apierr.WriteJSON(w, http.StatusOK, map[string]string{"status": "running"})
	})
	handleAction := func(action func(context.Context, string) error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var input struct {
				ID string `json:"workspace_id"`
			}
			if !decode(w, r, &input) {
				return
			}
			if !workspaceinput.ValidID(input.ID) {
				writeFailure(w, errInvalidID)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
			defer cancel()
			if err := action(ctx, input.ID); err != nil {
				writeFailure(w, err)
				return
			}
			apierr.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		}
	}
	privileged.HandleFunc("POST /workspace/stop", handleAction(wm.stopWorkspace))
	privileged.HandleFunc("POST /workspace/delete", handleAction(wm.deleteWorkspace))
	privileged.HandleFunc("POST /workspace/git", func(w http.ResponseWriter, r *http.Request) {
		var req gitRequest
		if !decode(w, r, &req) {
			return
		}
		if !workspaceinput.ValidID(req.WorkspaceID) {
			writeFailure(w, errInvalidID)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 58*time.Second)
		defer cancel()
		result, err := wm.workspaceGit(ctx, req)
		if err != nil {
			writeFailure(w, err)
			return
		}
		apierr.WriteJSON(w, http.StatusOK, result)
	})
	mux.Handle("/workspace/", auth.Middleware(token)(privileged))
	if wm.docker != nil {
		executor, err := core.NewDockerExecutor(wm.docker, wm.project)
		if err != nil {
			panic(err)
		}
		mux.Handle("/internal/core/docker/", auth.Middleware(token)(core.BridgeHandler(executor)))
	}
	return mux
}

func setting(name string, fallback, min, max int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < min || n > max {
		log.Fatalf("%s must be between %d and %d", name, min, max)
	}
	return n
}

func main() {
	token := os.Getenv("KAMUI_TOKEN")
	project, network, image := os.Getenv("KAMUI_PROJECT"), os.Getenv("DOCKER_NETWORK_NAME"), os.Getenv("WORKSPACE_IMAGE")
	if len(token) < 32 || strings.IndexFunc(token, func(c rune) bool { return unicode.IsSpace(c) || unicode.IsControl(c) }) >= 0 || !projectPattern.MatchString(project) || network == "" || image == "" {
		log.Fatal("A strong KAMUI_TOKEN and KAMUI_PROJECT, DOCKER_NETWORK_NAME and WORKSPACE_IMAGE are required")
	}
	docker, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatal(err)
	}
	defer docker.Close()
	wm := &WorkspaceManager{
		docker: docker, project: project, network: network, image: image,
		nanoCPUs:    int64(setting("KAMUI_WORKSPACE_CPUS", 1, 1, 8)) * 1e9,
		memoryBytes: int64(setting("KAMUI_WORKSPACE_MEMORY_MIB", 2048, 768, 16384)) * (1 << 20),
	}
	server := &http.Server{
		Addr: ":8082", Handler: managerRouter(wm, token),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 75 * time.Second, IdleTimeout: 30 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			log.Printf("Manager shutdown: %v", err)
		}
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(fmt.Errorf("Workspace Manager: %w", err))
	}
}
