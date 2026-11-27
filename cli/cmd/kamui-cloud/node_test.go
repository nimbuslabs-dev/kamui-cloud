package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out strings.Builder
	code := execute(context.Background(), args, strings.NewReader(""), &out, &out)
	return code, out.String()
}

func testCoordinator(t *testing.T, handler http.HandlerFunc) (*httptest.Server, string, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(strings.Repeat("t", 40)), 0600); err != nil {
		t.Fatal(err)
	}
	return server, server.URL, tokenFile
}

func TestNodeCommandValidation(t *testing.T) {
	code, out := runCLI(t, "node", "approve", "not-a-worker")
	if code != 2 || !strings.Contains(out, "invalid_id") {
		t.Fatalf("node approve with a bad ID = %d %s", code, out)
	}
	code, out = runCLI(t, "node", "map")
	if code != 2 || !strings.Contains(out, "--project") {
		t.Fatalf("node map without project = %d %s", code, out)
	}
	code, out = runCLI(t, "node", "map", "--project", "default", "--mode", "manual")
	if code != 2 || !strings.Contains(out, "--worker") {
		t.Fatalf("manual map without worker = %d %s", code, out)
	}
	code, out = runCLI(t, "node", "map", "--project", "default", "--mode", "preferred")
	if code != 2 || !strings.Contains(out, "--worker") {
		t.Fatalf("preferred map without worker = %d %s", code, out)
	}
}

func TestNodeMapSendsSchedulingPreferences(t *testing.T) {
	var captured map[string]any
	_, url, tokenFile := testCoordinator(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/projects/default/scheduling-preferences" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_ = json.NewEncoder(w).Encode(map[string]any{"mode": "preferred"})
	})
	code, out := runCLI(t, "--url", url, "--token-file", tokenFile,
		"node", "map", "--project", "default", "--mode", "preferred",
		"--worker", "local,wk-"+strings.Repeat("a", 32), "--allow-fallback=false")
	if code != 0 {
		t.Fatalf("node map = %d %s", code, out)
	}
	if captured["mode"] != "preferred" || captured["fallback"] != false {
		t.Fatalf("preferences = %+v", captured)
	}
	workers, ok := captured["preferred_workers"].([]any)
	if !ok || len(workers) != 2 {
		t.Fatalf("preferred workers = %+v", captured["preferred_workers"])
	}
}

func TestJobSubmitPlacementFlags(t *testing.T) {
	var captured map[string]any
	_, url, tokenFile := testCoordinator(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/workloads" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "wl-x", "state": "PENDING"})
	})

	code, out := runCLI(t, "--url", url, "--token-file", tokenFile,
		"job", "submit", "--node", "wk-"+strings.Repeat("b", 32), "--priority", "80", "example:local", "--", "echo", "hi")
	if code != 0 {
		t.Fatalf("manual submit = %d %s", code, out)
	}
	if captured["placement"] != "manual" || captured["worker_id"] != "wk-"+strings.Repeat("b", 32) {
		t.Fatalf("manual submission = %+v", captured)
	}
	if captured["priority"] != float64(80) {
		t.Fatalf("priority = %+v", captured["priority"])
	}

	code, out = runCLI(t, "--url", url, "--token-file", tokenFile,
		"job", "submit", "--preferred", "local,wk-"+strings.Repeat("c", 32), "--allow-fallback=false", "example:local")
	if code != 0 {
		t.Fatalf("preferred submit = %d %s", code, out)
	}
	if captured["placement"] != "preferred" || captured["fallback"] != false {
		t.Fatalf("preferred submission = %+v", captured)
	}

	code, out = runCLI(t, "--url", url, "--token-file", tokenFile, "job", "submit", "--node", "bogus", "example:local")
	if code != 2 || !strings.Contains(out, "invalid_id") {
		t.Fatalf("invalid node ID = %d %s", code, out)
	}
}

func TestAgentEnrollWritesStateFile(t *testing.T) {
	var enrolled map[string]any
	_, url, _ := testCoordinator(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/workers/enroll" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&enrolled)
		if enrolled["token"] != "kwe123" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "enrollment_rejected", "message": "unknown token"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"worker_id": "wk-" + strings.Repeat("d", 32), "worker_token": "kwtest", "approved": true,
		})
	})
	home := t.TempDir()
	t.Setenv("KAMUI_AGENT_HOME", home)
	t.Setenv("KAMUI_AGENT_HOSTNAME", "test-laptop")

	code, out := runCLI(t, "agent", "enroll", "--url", url, "--token", "kwe123", "--name", "test laptop")
	if code != 0 {
		t.Fatalf("agent enroll = %d %s", code, out)
	}
	if enrolled["token"] != "kwe123" || enrolled["name"] != "test laptop" {
		t.Fatalf("enrollment request = %+v", enrolled)
	}
	data, err := os.ReadFile(filepath.Join(home, "worker.json"))
	if err != nil {
		t.Fatalf("state file: %v", err)
	}
	var state agentState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.WorkerID != "wk-"+strings.Repeat("d", 32) || state.Coordinator != url || state.Name != "test laptop" {
		t.Fatalf("state = %+v", state)
	}

	// Status reports the enrolled device even when the coordinator is unreachable.
	code, out = runCLI(t, "--url", url, "--token-file", strings.Repeat("t", 40)+"-missing-file",
		"agent", "status")
	if code != 0 || !strings.Contains(out, state.WorkerID) {
		t.Fatalf("agent status = %d %s", code, out)
	}

	// A second enrollment attempt on a bad token surfaces the coordinator error.
	code, out = runCLI(t, "agent", "enroll", "--url", url, "--token", "kwebad")
	if code == 0 {
		t.Fatalf("bad enrollment succeeded: %s", out)
	}
}

func TestAgentStartRequiresEnrollment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KAMUI_AGENT_HOME", home)
	code, out := runCLI(t, "agent", "start")
	if code != 2 || !strings.Contains(out, "not enrolled") {
		t.Fatalf("agent start without enrollment = %d %s", code, out)
	}
}
