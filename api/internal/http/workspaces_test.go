package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceCreateValidatesRepositoryAndUsesDefaultBranch(t *testing.T) {
	handler, err := newWorkspaceHandler("", "", "token")
	if err != nil {
		t.Fatal(err)
	}
	rejected := httptest.NewRecorder()
	handler.Create(rejected, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"name":"Workspace","repository_url":"https://user:secret@github.com/octocat/Hello-World.git"}`,
	)))
	if rejected.Code != http.StatusBadRequest || strings.Contains(rejected.Body.String(), "secret") {
		t.Fatalf("want sanitized 400, got %d %s", rejected.Code, rejected.Body)
	}
	accepted := httptest.NewRecorder()
	handler.Create(accepted, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"name":"Workspace","repository_url":"https://github.com/octocat/Hello-World.git"}`,
	)))
	var workspace Workspace
	if accepted.Code != http.StatusCreated || json.Unmarshal(accepted.Body.Bytes(), &workspace) != nil {
		t.Fatalf("want workspace, got %d %s", accepted.Code, accepted.Body)
	}
	if workspace.Branch != "" || workspace.Status != "stopped" {
		t.Fatalf("want repository default and stopped status, got %#v", workspace)
	}
}

type managerTransport func(*http.Request) (*http.Response, error)

func (f managerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func managerResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func workspaceRequest(method, id, body string) *http.Request {
	r := httptest.NewRequest(method, "/v1/workspaces/"+id, strings.NewReader(body))
	r.SetPathValue("id", id)
	return r
}

func TestWorkspaceStatusComesFromManagerAfterCoordinatorRestart(t *testing.T) {
	dataPath := filepath.Join(t.TempDir(), "workspaces.json")
	handler, err := newWorkspaceHandler(dataPath, "http://manager", "test-token")
	if err != nil {
		t.Fatal(err)
	}

	created := httptest.NewRecorder()
	handler.Create(created, workspaceRequest(http.MethodPost, "", `{"name":"Test","repository_url":"https://github.com/octocat/Hello-World.git","branch":"test/mobile"}`))
	var original Workspace
	if created.Code != 201 || json.Unmarshal(created.Body.Bytes(), &original) != nil {
		t.Fatalf("create failed: %d %s", created.Code, created.Body)
	}

	reopened, err := newWorkspaceHandler(dataPath, "http://manager", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	reopened.client.Transport = managerTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.Path != "/workspace/status" || r.URL.Query().Get("id") != original.ID || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("unexpected manager request: %s %s", r.Method, r.URL)
		}
		return managerResponse(200, `{"status":"running","container_id":"container-1"}`), nil
	})
	got := httptest.NewRecorder()
	reopened.Get(got, workspaceRequest(http.MethodGet, original.ID, ""))
	var workspace Workspace
	if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &workspace) != nil {
		t.Fatalf("lookup failed: %d %s", got.Code, got.Body)
	}
	if workspace.ID != original.ID || workspace.Branch != "test/mobile" || workspace.Status != "running" ||
		workspace.ContainerID != "container-1" || workspace.CodeServerURL != "/workspace/"+workspace.ID+"/" {
		t.Fatalf("unexpected loaded workspace: %+v", workspace)
	}

	reopened.client.Transport = managerTransport(func(*http.Request) (*http.Response, error) {
		return managerResponse(503, ""), nil
	})
	got = httptest.NewRecorder()
	reopened.List(got, workspaceRequest(http.MethodGet, "", ""))
	var workspaces []Workspace
	if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &workspaces) != nil || len(workspaces) != 1 ||
		workspaces[0].Status != "unavailable" || workspaces[0].CodeServerURL != "" {
		t.Fatalf("stale status after manager outage: %d %s", got.Code, got.Body)
	}
}

func TestWorkspaceRejectsCorruptStoreWithoutOverwritingIt(t *testing.T) {
	dataPath := filepath.Join(t.TempDir(), "workspaces.json")
	original := []byte(`{"version":1,"workspaces":`)
	if err := os.WriteFile(dataPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	if handler, err := newWorkspaceHandler(dataPath, "", "test-token"); handler != nil || err == nil {
		t.Fatalf("expected initialization failure, got %v", err)
	}
	data, err := os.ReadFile(dataPath)
	if err != nil || string(data) != string(original) {
		t.Fatalf("metadata changed: %v", err)
	}
}

func TestWorkspaceLifecycleAndGitHonorManagerResults(t *testing.T) {
	dataPath := filepath.Join(t.TempDir(), "workspaces.json")
	handler, err := newWorkspaceHandler(dataPath, "http://manager", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := handler.store.create(WorkspaceCreate{
		Name: "Test", RepositoryURL: "https://github.com/octocat/Hello-World.git",
	})
	if err != nil {
		t.Fatal(err)
	}

	state := "stopped"
	hash := strings.Repeat("ab", 20)
	handler.client.Transport = managerTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/workspace/start":
			state = "running"
		case "/workspace/stop", "/workspace/delete":
			state = "stopped"
		case "/workspace/status":
			return managerResponse(200, `{"status":"`+state+`"}`), nil
		case "/workspace/git":
			var command gitCommand
			if err := json.NewDecoder(r.Body).Decode(&command); err != nil || command.WorkspaceID != workspace.ID {
				t.Fatalf("unexpected Git request: %+v, %v", command, err)
			}
			switch command.Command {
			case "commit":
				return managerResponse(200, `{"hash":"`+hash+`"}`), nil
			case "push":
				return managerResponse(409, `{"error":{"code":"git_failed","message":"Git push failed"}}`), nil
			}
		default:
			t.Fatalf("unexpected manager path: %s", r.URL.Path)
		}
		return managerResponse(200, ""), nil
	})

	for _, tc := range []struct {
		name   string
		fn     http.HandlerFunc
		body   string
		status int
		want   string
	}{
		{"start", handler.Start, "", 200, `"status":"running"`},
		{"start again", handler.Start, "", 200, `"status":"running"`},
		{"commit", handler.GitCommit, `{"message":"MVP changes"}`, 200, hash},
		{"push error", handler.GitPush, `{"remote":"origin"}`, 409, "git_failed"},
		{"malformed push", handler.GitPush, `{"remote":`, 400, "bad_request"},
		{"stop", handler.Stop, "", 200, `"status":"stopped"`},
		{"restart", handler.Restart, "", 200, `"status":"running"`},
		{"delete", handler.Delete, "", 204, ""},
		{"start deleted", handler.Start, "", 404, "not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := httptest.NewRecorder()
			tc.fn(got, workspaceRequest(http.MethodPost, workspace.ID, tc.body))
			if got.Code != tc.status || !strings.Contains(got.Body.String(), tc.want) {
				t.Fatalf("unexpected %s response %d %s", tc.name, got.Code, got.Body)
			}
		})
	}
	reopened, err := newWorkspaceHandler(dataPath, "", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, found := reopened.store.get(workspace.ID); found {
		t.Fatal("deleted metadata recovered from disk")
	}
}
