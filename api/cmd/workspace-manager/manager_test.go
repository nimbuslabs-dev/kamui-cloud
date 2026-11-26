package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

func TestManagerRoutesRequireToken(t *testing.T) {
	token := strings.Repeat("t", 64)
	handler := managerRouter(&WorkspaceManager{}, token)
	for _, route := range []struct{ method, path string }{
		{"GET", "/workspace/status?id=ws-abcd1234"}, {"POST", "/workspace/start"}, {"POST", "/workspace/stop"},
		{"POST", "/workspace/delete"}, {"POST", "/workspace/git"},
	} {
		t.Run(route.path, func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path, nil)
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", res.Code)
			}
		})
	}
	req := httptest.NewRequest("POST", "/workspace/start", strings.NewReader(`{"id":"ws-abcd1234","name":"test","repository_url":"https://example.com/owner/repo"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Code)
	}
}

func TestWorkspaceSpecIsBoundedAndPrivate(t *testing.T) {
	wm := &WorkspaceManager{project: "kamui-test", network: "kamui-test-ide", image: "kamui-test-workspace", nanoCPUs: 1e9, memoryBytes: 2 << 30}
	cfg, host, networks := wm.workspaceSpec("ws-abcd1234", "named-project")
	if cfg.User != "1000:1000" || cfg.Labels[projectLabel] != "kamui-test" || cfg.Labels[workspaceLabel] != "ws-abcd1234" {
		t.Fatalf("workspace lacks expected user or ownership: %#v", cfg)
	}
	if host.Privileged || host.PublishAllPorts || len(host.PortBindings) != 0 || len(host.Binds) != 0 ||
		len(host.Mounts) != 1 || host.Mounts[0].Type != mount.TypeVolume || host.Mounts[0].Target != projectDir ||
		len(networks.EndpointsConfig) != 1 || networks.EndpointsConfig[wm.network].Aliases[0] != "kamui-workspace-ws-abcd1234" {
		t.Fatalf("workspace isolation: %#v %#v", host, networks)
	}
	if host.NanoCPUs != wm.nanoCPUs || host.Memory != wm.memoryBytes || host.MemorySwap != wm.memoryBytes || *host.PidsLimit <= 0 {
		t.Fatalf("workspace resources: %#v", host.Resources)
	}
}

func TestDockerStateWaitsForIDEHealth(t *testing.T) {
	if got := dockerState(container.InspectResponse{}); got.Status != "stopped" || got.ContainerID != "" {
		t.Fatalf("missing container state = %+v", got)
	}
	inspected := container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{
		ID: "container-id", State: &container.State{Running: true, Health: &container.Health{Status: container.Starting}},
	}}
	for health, want := range map[string]string{container.Starting: "starting", container.Unhealthy: "error", container.Healthy: "running"} {
		inspected.State.Health.Status = health
		if got := dockerState(inspected); got.Status != want || got.ContainerID != inspected.ID {
			t.Errorf("state = %+v, want %s", got, want)
		}
	}
}

func TestGitArguments(t *testing.T) {
	stage, err := buildGitArgs(gitRequest{Command: "add", Args: []string{"-odd file.txt"}})
	if err != nil || !reflect.DeepEqual(stage, []string{"--literal-pathspecs", "add", "--", "-odd file.txt"}) {
		t.Fatalf("git add arguments %q, error %v", stage, err)
	}
	for _, req := range []gitRequest{
		{Command: "branch", Args: []string{"-branch"}},
		{Command: "push", Args: []string{"other", "main"}},
		{Command: "pull", Args: []string{"origin", "../bad"}},
		{Command: "commit", Args: []string{""}},
	} {
		if args, err := buildGitArgs(req); err == nil {
			t.Errorf("%s unexpectedly accepted as %q", req.Command, args)
		}
	}
}

func TestParseRealGitStatusAndPushArguments(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable unavailable")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "work")
	remote := filepath.Join(dir, "remote.git")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "--bare", "-q", remote)
	git("init", "-q", "-b", "test-branch", repo)
	git("-C", repo, "config", "user.name", "Kamui Tests")
	git("-C", repo, "config", "user.email", "kamui-tests@example.invalid")
	git("-C", repo, "remote", "add", "origin", remote)
	write("original.txt", "original\n")
	git("-C", repo, "add", "original.txt")
	git("-C", repo, "commit", "-qm", "initial commit")
	push, err := buildGitArgs(gitRequest{Command: "push"})
	if err != nil {
		t.Fatal(err)
	}
	git(append([]string{"-C", repo}, push...)...)
	localSHA := git("-C", repo, "rev-parse", "HEAD")
	if remoteSHA := git("--git-dir", remote, "rev-parse", "refs/heads/test-branch"); remoteSHA != localSHA {
		t.Fatalf("remote SHA %q, local SHA %q", remoteSHA, localSHA)
	}
	git("-C", repo, "mv", "original.txt", "renamed file.txt")
	write("renamed file.txt", "working tree edit\n")
	write("new file.txt", "untracked\n")
	status := parseStatus(git("-C", repo, "status", "--porcelain=v2", "--branch", "-z"))
	want := gitStatus{Branch: "test-branch", Staged: []string{"renamed file.txt"}, Unstaged: []string{"renamed file.txt"}, Untracked: []string{"new file.txt"}}
	if !reflect.DeepEqual(status, want) {
		t.Fatalf("status = %#v, want %#v", status, want)
	}

	peer := filepath.Join(dir, "peer")
	git("clone", "-q", "-b", "test-branch", remote, peer)
	git("-C", peer, "-c", "user.name=Kamui Tests", "-c", "user.email=kamui-tests@example.invalid", "commit", "--allow-empty", "-qm", "peer update")
	git("-C", peer, "push", "-q", "origin", "HEAD")
	peerSHA := git("-C", peer, "rev-parse", "HEAD")
	git("-C", repo, "commit", "-qm", "local update")
	localSHA = git("-C", repo, "rev-parse", "HEAD")
	for _, operation := range []string{"push", "pull"} {
		args, err := buildGitArgs(gitRequest{Command: operation, Args: []string{"origin", "test-branch"}})
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "LC_ALL=C")
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		var exitError *exec.ExitError
		if err := cmd.Run(); !errors.As(err, &exitError) {
			t.Fatalf("diverged Git %s: expected subprocess rejection, got %v", operation, err)
		}
		err = gitFailure(operation, commandResult{out: stdout.String(), stderr: stderr.String(), exitCode: exitError.ExitCode()})
		var gitErr *managerError
		if !errors.As(err, &gitErr) || gitErr.status != http.StatusConflict || gitErr.code != "git_conflict" {
			t.Errorf("diverged Git %s: got %#v, want conflict", operation, err)
		}
	}
	if got := git("--git-dir", remote, "rev-parse", "refs/heads/test-branch"); got != peerSHA {
		t.Fatalf("remote SHA = %q, want peer SHA %q", got, peerSHA)
	}
	if got := git("-C", repo, "rev-parse", "HEAD"); got != localSHA {
		t.Fatalf("local SHA = %q, want local update %q", got, localSHA)
	}
}

func TestGitTransportFailuresAreSanitized(t *testing.T) {
	for _, stderr := range []string{
		"ssh: connect to host private-host.invalid port 22: Connection timed out",
		"fatal: unable to access 'https://private-host.invalid/repo/': Could not resolve host: private-host.invalid",
	} {
		err := gitFailure("push", commandResult{exitCode: 128, stderr: stderr})
		var gitErr *managerError
		if !errors.As(err, &gitErr) || gitErr.status != http.StatusBadGateway || gitErr.code != "git_network" {
			t.Errorf("network failure: got %#v, want bad gateway", err)
		}
		if strings.Contains(err.Error(), "private-host.invalid") {
			t.Fatal("Git diagnostics exposed the raw remote address")
		}
	}
}
