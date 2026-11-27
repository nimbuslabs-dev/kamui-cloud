package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testStack(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	ops := filepath.Join(root, "ops")
	if err := os.Mkdir(ops, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ops, "docker-compose.yml"), []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCloudInitPreservesExistingCredentials(t *testing.T) {
	stack, err := findLocalStack(testStack(t))
	if err != nil {
		t.Fatal(err)
	}
	if created, err := stack.init(); err != nil || !created {
		t.Fatalf("first init = %v, %v", created, err)
	}
	token, err := localToken(stack.env)
	if err != nil || len(token) != 64 {
		t.Fatalf("generated token has invalid size or could not be read: %v", err)
	}
	password, err := lookupDotenv(stack.env, "KAMUI_ADMIN_PASSWORD")
	if err != nil || len(password) != 64 || token == password {
		t.Fatalf("generated credentials are invalid: %v", err)
	}
	info, err := os.Stat(stack.env)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("new credentials mode = %o", info.Mode().Perm())
	}
	if created, err := stack.init(); err != nil || created {
		t.Fatalf("second init = %v, %v", created, err)
	}
	if after, err := localToken(stack.env); err != nil || after != token {
		t.Fatalf("second init changed credentials: %v", err)
	}
}

func TestLocalEndpointUsesEnvPrecedenceAndResolvedTLSPath(t *testing.T) {
	root := testStack(t)
	stack, err := findLocalStack(root)
	if err != nil {
		t.Fatal(err)
	}
	raw := "KAMUI_TOKEN='abc#123' # comment\r\nKAMUI_TLS_DIR=\"my certificates\"\r\nKAMUI_CLI_PORT=19841 # comment\r\n"
	if err := os.WriteFile(stack.env, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"KAMUI_CLI_PORT", "KAMUI_TLS_DIR"} {
		t.Setenv(key, "temporary")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	endpoint, ca, envPath, err := localEndpoint(root)
	if err != nil || endpoint != "https://127.0.0.1:19841" || ca != filepath.Join(stack.ops, "my certificates", "ca.pem") || envPath != stack.env {
		t.Fatalf("endpoint = %q, %q, %q, %v", endpoint, ca, envPath, err)
	}
	token, err := localToken(stack.env)
	if err != nil || token != "abc#123" {
		t.Fatal("single-quoted credential did not parse")
	}
	t.Setenv("KAMUI_CLI_PORT", "19842")
	endpoint, _, _, err = localEndpoint(root)
	if err != nil || endpoint != "https://127.0.0.1:19842" {
		t.Fatalf("shell override = %q, %v", endpoint, err)
	}
}

func TestComposeServicesHandlesComposeJSONArrayAndStreams(t *testing.T) {
	entry := `{"Service":"cli-gateway","State":"running","Health":"healthy","Command":"should never appear"}`
	for _, raw := range []string{"[" + entry + "]", entry + "\n"} {
		services, err := composeServices([]byte(raw))
		want := composeService{Service: "cli-gateway", State: "running", Health: "healthy"}
		if err != nil || len(services) != 1 || services[0] != want {
			t.Fatalf("services = %+v, %v", services, err)
		}
	}
}

func TestCloudUsesComposeProfileAndNeverSwitchesContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake Docker executable uses a POSIX shell")
	}
	root := testStack(t)
	stack, err := findLocalStack(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stack.init(); err != nil {
		t.Fatal(err)
	}
	fakePath := t.TempDir()
	trace := filepath.Join(fakePath, "trace")
	t.Setenv("KAMUI_TEST_DOCKER_TRACE", trace)
	t.Setenv("PATH", fakePath+string(os.PathListSeparator)+os.Getenv("PATH"))
	script := `#!/bin/sh
if [ "$3" = context ]; then
  printf '%s\n' 'unix:///var/run/docker.sock'
  exit 0
fi
printf '%s\n' "$@" > "$KAMUI_TEST_DOCKER_TRACE"
`
	if err := os.WriteFile(filepath.Join(fakePath, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	command := cloudCommand(&cloudOptions{Root: root, Context: "qa-context", JSON: true})
	command.SetArgs([]string{"stop"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--context", "qa-context", "compose", "--env-file", stack.env,
		"-f", filepath.Join(stack.ops, "docker-compose.yml"), "--profile", "cli", "stop", ""}
	if string(args) != strings.Join(want, "\n") {
		t.Fatalf("Docker arguments = %q", args)
	}
}

func TestCloudRejectsRemoteDockerHostWithoutSurfacingAddress(t *testing.T) {
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "ssh://private-user@private-host")
	err := requireLocalDocker(context.Background(), &cloudOptions{})
	if err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("remote Docker endpoint validation = %v", err)
	}
}

func TestCloudStartRequiresGatewayVerificationAfterComposeIsReady(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake Docker executable uses a POSIX shell")
	}
	root := testStack(t)
	fakePath := t.TempDir()
	trace := filepath.Join(fakePath, "trace")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("KAMUI_TEST_DOCKER_TRACE", trace)
	t.Setenv("PATH", fakePath+string(os.PathListSeparator)+os.Getenv("PATH"))
	script := `#!/bin/sh
case " $* " in
  *" config --format json "*) printf '%s' '{"services":{"console":{"environment":{"KAMUI_TOKEN":"test-token-12345678901234567890123456789012","KAMUI_ADMIN_PASSWORD":"test-password-12345678901234567890"}}}}' ;;
  *" up "*) printf ready > "$KAMUI_TEST_DOCKER_TRACE" ;;
esac
`
	if err := os.WriteFile(filepath.Join(fakePath, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	verificationError := errors.New("gateway verification failed")
	options := cloudOptions{Root: root, VerifyStart: func(context.Context) error {
		data, err := os.ReadFile(trace)
		if err != nil || string(data) != "ready" {
			t.Fatalf("verified gateway before Compose up completed: %v", err)
		}
		return verificationError
	}}
	command := cloudCommand(&options)
	command.SetArgs([]string{"start"})
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	if err := command.Execute(); !errors.Is(err, verificationError) {
		t.Fatalf("start failed to return verification error: %v", err)
	}
}
