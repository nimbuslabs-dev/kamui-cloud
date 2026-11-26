package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/workspaceinput"
)

const maxGitOutput = 256 << 10

var (
	fullCommit = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)
	githubSSH  = regexp.MustCompile(`^(git@github\.com:|ssh://git@github\.com/)[A-Za-z0-9-]+/[A-Za-z0-9_.-]+$`)
)

type cappedOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *cappedOutput) Write(p []byte) (int, error) {
	n := min(len(p), maxGitOutput-b.Len())
	_, _ = b.Buffer.Write(p[:n])
	b.overflow = b.overflow || n < len(p)
	return len(p), nil // Drain all frames so Docker can observe the process exit.
}

type commandResult struct {
	out      string
	stderr   string
	exitCode int
	overflow bool
}

func (wm *WorkspaceManager) exec(ctx context.Context, containerID string, args ...string) (commandResult, error) {
	// Closing the Docker stream does not kill exec's process. Give commands an
	// independent deadline in the container, including their child processes.
	cmd := append([]string{"/usr/bin/timeout", "--kill-after=2s", "45s"}, args...)
	created, err := wm.docker.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd: cmd, WorkingDir: projectDir, User: "1000:1000", AttachStdout: true, AttachStderr: true,
		Env: []string{
			"GIT_TERMINAL_PROMPT=0", "LC_ALL=C",
			"GIT_SSH_COMMAND=/usr/bin/ssh -F /dev/null -o BatchMode=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=/dev/null",
		},
	})
	if err != nil {
		return commandResult{}, fmt.Errorf("create workspace command: %w", err)
	}
	stream, err := wm.docker.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return commandResult{}, fmt.Errorf("attach workspace command: %w", err)
	}
	defer stream.Close()
	unwatch := context.AfterFunc(ctx, stream.Close)
	defer unwatch()
	var stdout, stderr cappedOutput
	_, err = stdcopy.StdCopy(&stdout, &stderr, stream.Reader)
	if ctx.Err() != nil {
		return commandResult{}, ctx.Err()
	}
	if err != nil {
		return commandResult{}, fmt.Errorf("read workspace command result: %w", err)
	}
	// The last stream frame can arrive slightly before the daemon records the exit.
	for {
		info, err := wm.docker.ContainerExecInspect(ctx, created.ID)
		if err != nil {
			return commandResult{}, fmt.Errorf("inspect workspace command result: %w", err)
		}
		if !info.Running {
			return commandResult{stdout.String(), stderr.String(), info.ExitCode, stdout.overflow}, nil
		}
		select {
		case <-ctx.Done():
			return commandResult{}, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (wm *WorkspaceManager) git(ctx context.Context, id string, args ...string) (commandResult, error) {
	global := []string{"/usr/bin/git", "-c", "credential.helper=", "-c", "core.hooksPath=/dev/null", "-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "protocol.ssh.allow=always", "-c", "http.followRedirects=false"}
	return wm.exec(ctx, id, append(global, args...)...)
}

func gitFailure(op string, result commandResult) error {
	if result.exitCode == 124 || result.exitCode == 137 {
		return &managerError{http.StatusGatewayTimeout, "git_timeout", "Git operation timed out"}
	}
	diagnostic := strings.ToLower(result.stderr)
	if strings.Contains(diagnostic, "permission denied (publickey)") || strings.Contains(diagnostic, "could not read username") ||
		strings.Contains(diagnostic, "authentication failed") || strings.Contains(diagnostic, "repository not found") ||
		strings.Contains(diagnostic, "write access to repository not granted") {
		return &managerError{http.StatusUnprocessableEntity, "git_auth", "GitHub denied access or the repository was not found. Check the repository and workspace credentials."}
	}
	if (op == "push" && pushNeedsReconciliation(result.out)) ||
		(op == "pull" && strings.Contains(diagnostic, "not possible to fast-forward")) {
		return &managerError{http.StatusConflict, "git_conflict", "Reconcile your branch with the remote in the workspace terminal before trying again."}
	}
	if op == "clone" || op == "push" || op == "pull" {
		for _, failure := range []string{"could not resolve host", "failed to connect to", "connection timed out", "connection refused", "connection reset by peer"} {
			if strings.Contains(diagnostic, failure) {
				return &managerError{http.StatusBadGateway, "git_network", "Git could not connect to GitHub. Check the workspace network and try again."}
			}
		}
	}
	if op == "commit" && (strings.Contains(diagnostic, "author identity unknown") || strings.Contains(diagnostic, "unable to auto-detect email")) {
		return &managerError{http.StatusUnprocessableEntity, "git_identity", "Configure Git user.name and user.email in the workspace terminal."}
	}
	if op == "commit" && (strings.Contains(result.out, "nothing to commit") || strings.Contains(result.out, "no changes added to commit")) {
		return &managerError{http.StatusConflict, "git_no_changes", "Stage changes before committing."}
	}
	if op == "clone" && strings.Contains(diagnostic, "remote branch") {
		return &managerError{http.StatusUnprocessableEntity, "git_branch", "The requested branch was not found in the repository."}
	}
	return &managerError{http.StatusUnprocessableEntity, "git_failed", fmt.Sprintf("Git %s failed (exit %d). Check the workspace terminal for repository state.", op, result.exitCode)}
}

func pushNeedsReconciliation(output string) bool {
	// git push --porcelain writes one tab-delimited status line per ref.
	for line := range strings.SplitSeq(output, "\n") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) == 3 && fields[0] == "!" &&
			(fields[2] == "[rejected] (non-fast-forward)" || fields[2] == "[rejected] (fetch first)") {
			return true
		}
	}
	return false
}

func (wm *WorkspaceManager) cloneRepository(ctx context.Context, id, repo, branch string) error {
	const cloneMarker = ".git/kamui-clone-complete"
	probe, err := wm.exec(ctx, id, "/usr/bin/test", "-e", ".git")
	if err != nil {
		return err
	}
	switch probe.exitCode {
	case 0:
		ready, err := wm.exec(ctx, id, "/usr/bin/test", "-f", cloneMarker)
		if err != nil {
			return err
		}
		if ready.exitCode != 0 {
			return &managerError{http.StatusConflict, "git_incomplete", "A previous clone may have been interrupted. Its files were preserved. Create a new workspace to retry."}
		}
		existing, err := wm.git(ctx, id, "rev-parse", "--is-inside-work-tree")
		if err != nil {
			return err
		}
		if existing.exitCode == 0 && strings.TrimSpace(existing.out) == "true" {
			return nil
		}
		return &managerError{http.StatusConflict, "git_existing", "The saved workspace repository could not be opened. Its files were preserved."}
	case 1:
		args := []string{"clone", "--no-recurse-submodules"}
		if branch != "" {
			args = append(args, "--branch", branch)
		}
		result, err := wm.git(ctx, id, append(args, "--", repo, ".")...)
		if err != nil {
			return err
		}
		if result.exitCode != 0 {
			return gitFailure("clone", result)
		}
		// Record completion after a successful clone even if the HTTP caller
		// leaves in the meantime. Interrupted clones never gain this marker.
		markerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		mark, err := wm.exec(markerCtx, id, "/usr/bin/touch", cloneMarker)
		if err != nil {
			return err
		}
		if mark.exitCode != 0 {
			return errors.New("record clone completion in workspace volume")
		}
		return nil
	default:
		return errors.New("could not inspect saved workspace project")
	}
}

type gitRequest struct {
	WorkspaceID string   `json:"workspace_id"`
	Command     string   `json:"command"`
	Args        []string `json:"args"`
}

func buildGitArgs(req gitRequest) ([]string, error) {
	invalid := &managerError{http.StatusBadRequest, "bad_request", "invalid arguments for Git operation"}
	args := req.Args
	switch req.Command {
	case "status":
		if len(args) == 0 {
			return []string{"status", "--porcelain=v2", "--branch", "-z"}, nil
		}
	case "add":
		if len(args) == 0 || (len(args) == 1 && args[0] == ".") {
			return []string{"add", "-A", "--"}, nil
		}
		if len(args) > 64 {
			return nil, invalid
		}
		for _, path := range args {
			if path == "" || len(path) > 4096 || strings.IndexByte(path, 0) >= 0 {
				return nil, invalid
			}
		}
		return append([]string{"--literal-pathspecs", "add", "--"}, args...), nil
	case "commit":
		if len(args) == 1 && strings.TrimSpace(args[0]) != "" && len(args[0]) <= 4096 && strings.IndexByte(args[0], 0) < 0 {
			return []string{"commit", "-m", args[0]}, nil
		}
	case "branch":
		if len(args) == 1 && workspaceinput.ValidBranch(args[0]) {
			return []string{"switch", "--no-guess", "--", args[0]}, nil
		}
	case "push", "pull":
		if len(args) > 2 || (len(args) > 0 && args[0] != "origin" && args[0] != "") || (len(args) == 2 && args[1] != "" && !workspaceinput.ValidBranch(args[1])) {
			return nil, invalid
		}
		if req.Command == "pull" {
			cmd := []string{"pull", "--ff-only", "--no-recurse-submodules", "--", "origin"}
			if len(args) == 2 && args[1] != "" {
				cmd = append(cmd, args[1])
			}
			return cmd, nil
		}
		ref := "HEAD"
		if len(args) == 2 && args[1] != "" {
			ref += ":refs/heads/" + args[1]
		}
		return []string{"push", "--porcelain", "--set-upstream", "--recurse-submodules=no", "--", "origin", ref}, nil
	}
	return nil, invalid
}

func (wm *WorkspaceManager) validateOrigin(ctx context.Context, id, op string) error {
	args := []string{"remote", "get-url", "--all"}
	if op == "push" {
		args = append(args, "--push")
	}
	result, err := wm.git(ctx, id, append(args, "origin")...)
	if err != nil {
		return err
	}
	url := strings.TrimSpace(result.out)
	if result.exitCode != 0 || result.overflow || !(workspaceinput.ValidRepositoryURL(url) || githubSSH.MatchString(url)) {
		return &managerError{http.StatusUnprocessableEntity, "git_remote", "Configure a single GitHub HTTPS or SSH URL for the workspace origin."}
	}
	return nil
}

func (wm *WorkspaceManager) workspaceGit(ctx context.Context, req gitRequest) (any, error) {
	args, err := buildGitArgs(req)
	if err != nil {
		return nil, err
	}
	wm.mutation.Lock()
	defer wm.mutation.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	workspace, err := wm.workspaceStatus(ctx, req.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if workspace.Status != "running" {
		return nil, errStopped
	}
	id := workspace.ContainerID
	switch req.Command {
	case "push", "pull":
		if err := wm.validateOrigin(ctx, id, req.Command); err != nil {
			return nil, err
		}
	case "branch":
		branch := req.Args[0]
		ref, err := wm.git(ctx, id, "show-ref", "--verify", "--quiet", "--", "refs/heads/"+branch)
		if err != nil {
			return nil, err
		}
		switch ref.exitCode {
		case 0:
		case 1:
			args = []string{"switch", "-c", branch}
		default:
			return nil, gitFailure("branch", ref)
		}
	}
	result, err := wm.git(ctx, id, args...)
	if err != nil {
		return nil, err
	}
	if result.exitCode != 0 {
		return nil, gitFailure(req.Command, result)
	}
	switch req.Command {
	case "status":
		if result.overflow {
			return nil, &managerError{http.StatusUnprocessableEntity, "git_status_limit", "Git status is too large. Inspect it in the workspace terminal."}
		}
		return parseStatus(result.out), nil
	case "commit":
		head, err := wm.git(ctx, id, "rev-parse", "--verify", "HEAD")
		if err != nil {
			return nil, err
		}
		hash := strings.TrimSpace(head.out)
		if head.exitCode != 0 || !fullCommit.MatchString(hash) {
			return nil, errors.New("Git committed changes but HEAD could not be read")
		}
		return map[string]string{"hash": hash}, nil
	case "branch":
		return map[string]string{"branch": req.Args[0]}, nil
	default:
		return map[string]string{"status": "ok"}, nil
	}
}

type gitStatus struct {
	Branch    string   `json:"branch"`
	Ahead     int      `json:"ahead"`
	Behind    int      `json:"behind"`
	Staged    []string `json:"staged"`
	Unstaged  []string `json:"unstaged"`
	Untracked []string `json:"untracked"`
}

func parseStatus(output string) gitStatus {
	status := gitStatus{Staged: []string{}, Unstaged: []string{}, Untracked: []string{}}
	// Porcelain v2 -z retains spaces and line breaks in filenames. A rename record
	// carries a second NUL-terminated name, which we skip after recording the new path.
	next, stop := iter.Pull(strings.SplitSeq(output, "\x00"))
	defer stop()
	for record, ok := next(); ok; record, ok = next() {
		switch {
		case strings.HasPrefix(record, "# branch.head "):
			status.Branch = strings.TrimPrefix(record, "# branch.head ")
		case strings.HasPrefix(record, "# branch.ab "):
			counts := strings.Fields(strings.TrimPrefix(record, "# branch.ab "))
			if len(counts) == 2 {
				status.Ahead, _ = strconv.Atoi(strings.TrimPrefix(counts[0], "+"))
				status.Behind, _ = strconv.Atoi(strings.TrimPrefix(counts[1], "-"))
			}
		case strings.HasPrefix(record, "? "):
			status.Untracked = append(status.Untracked, record[2:])
		case strings.HasPrefix(record, "1 "):
			status.addTracked(strings.SplitN(record, " ", 9))
		case strings.HasPrefix(record, "2 "):
			status.addTracked(strings.SplitN(record, " ", 10))
			_, _ = next()
		case strings.HasPrefix(record, "u "):
			fields := strings.SplitN(record, " ", 11)
			if len(fields) == 11 {
				status.Unstaged = append(status.Unstaged, fields[10])
			}
		}
	}
	return status
}

func (s *gitStatus) addTracked(fields []string) {
	if len(fields) < 9 || len(fields[1]) != 2 {
		return
	}
	name := fields[len(fields)-1]
	if fields[1][0] != '.' {
		s.Staged = append(s.Staged, name)
	}
	if fields[1][1] != '.' {
		s.Unstaged = append(s.Unstaged, name)
	}
}
