package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
)

type cloudOptions struct {
	Root, Context string
	JSON          bool
	VerifyStart   func(context.Context) error
}

type localStack struct {
	ops, env string
}

func findLocalStack(root string) (localStack, error) {
	explicit := root != ""
	if !explicit {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return localStack{}, errors.New("cannot determine the current directory")
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return localStack{}, errors.New("cannot resolve the Kamui checkout")
	}
	for {
		ops := filepath.Join(root, "ops")
		if info, err := os.Stat(filepath.Join(ops, "docker-compose.yml")); err == nil && info.Mode().IsRegular() {
			return localStack{ops: ops, env: filepath.Join(ops, ".env")}, nil
		}
		parent := filepath.Dir(root)
		if explicit || root == parent {
			return localStack{}, errors.New("Kamui checkout not found; set --root to the directory containing ops/docker-compose.yml")
		}
		root = parent
	}
}

func lookupDotenv(path, key string) (string, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", errors.New("cannot read ops/.env")
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	var value string
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\uFEFF"))
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		name, raw, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(name) != key {
			continue
		}
		raw = strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(raw, "'"):
			literal, suffix, ok := strings.Cut(raw[1:], "'")
			suffix = strings.TrimSpace(suffix)
			if !ok || (suffix != "" && !strings.HasPrefix(suffix, "#")) {
				return "", fmt.Errorf("use a single-line literal %s in ops/.env", key)
			}
			value = literal
		case strings.HasPrefix(raw, "\""):
			idx := 1
			for idx < len(raw) && raw[idx] != '"' {
				if raw[idx] == '\\' {
					idx++
				}
				idx++
			}
			if idx >= len(raw) {
				return "", fmt.Errorf("use a single-line literal %s in ops/.env", key)
			}
			suffix := strings.TrimSpace(raw[idx+1:])
			literal, err := strconv.Unquote(raw[:idx+1])
			if err != nil || strings.Contains(literal, "$") || (suffix != "" && !strings.HasPrefix(suffix, "#")) {
				return "", fmt.Errorf("use a single-line literal %s in ops/.env", key)
			}
			value = literal
		default:
			if strings.HasPrefix(raw, "#") {
				raw = ""
			}
			for i := 1; i < len(raw); i++ {
				if raw[i] == '#' && (raw[i-1] == ' ' || raw[i-1] == '\t') {
					raw = raw[:i]
					break
				}
			}
			if strings.Contains(raw, "$") {
				return "", fmt.Errorf("use a literal %s in ops/.env or set it in the environment", key)
			}
			value = strings.TrimSpace(raw)
		}
	}
	if scanner.Err() != nil {
		return "", errors.New("cannot read ops/.env")
	}
	return value, nil
}

func (s localStack) setting(key, fallback string) (string, error) {
	value, present := os.LookupEnv(key)
	if !present {
		var err error
		value, err = lookupDotenv(s.env, key)
		if err != nil {
			return "", err
		}
	}
	if value == "" {
		return fallback, nil
	}
	return value, nil
}

func localToken(envPath string) (string, error) {
	return lookupDotenv(envPath, "KAMUI_TOKEN")
}

func localEndpoint(root string) (url, caPath, envPath string, err error) {
	s, err := findLocalStack(root)
	if err != nil {
		return "", "", "", err
	}
	port, err := s.setting("KAMUI_CLI_PORT", "8443")
	if err != nil {
		return "", "", "", err
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return "", "", "", errors.New("KAMUI_CLI_PORT must be a TCP port from 1 to 65535")
	}
	dir, err := s.setting("KAMUI_TLS_DIR", "tls-local")
	if err != nil {
		return "", "", "", err
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(s.ops, dir)
	}
	return "https://127.0.0.1:" + strconv.FormatUint(n, 10), filepath.Join(dir, "ca.pem"), s.env, nil
}

func (s localStack) init() (bool, error) {
	f, err := os.OpenFile(s.env, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		if info, statErr := os.Stat(s.env); statErr != nil || !info.Mode().IsRegular() {
			return false, errors.New("ops/.env must be a readable regular file")
		}
		return false, nil
	}
	if err != nil {
		return false, errors.New("cannot create ops/.env")
	}
	secret := func() string {
		var raw [32]byte
		rand.Read(raw[:])
		return hex.EncodeToString(raw[:])
	}
	_, writeErr := fmt.Fprintf(f, "KAMUI_TOKEN=%s\nKAMUI_ADMIN_PASSWORD=%s\n", secret(), secret())
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return false, errors.New("cannot write ops/.env")
	}
	return true, nil
}

func dockerOutput(ctx context.Context, options *cloudOptions, dir string, args ...string) ([]byte, error) {
	if options.Context != "" {
		args = append([]string{"--context", options.Context}, args...)
	}
	invocation := exec.CommandContext(ctx, "docker", args...)
	invocation.Dir = dir
	// Compose diagnostics may include interpolated configuration. Report only the failing action.
	result, err := invocation.Output()
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.Is(err, exec.ErrNotFound):
		return nil, errors.New("Docker CLI is required and was not found in PATH")
	default:
		return result, err
	}
}

func requireLocalDocker(ctx context.Context, options *cloudOptions) error {
	host := os.Getenv("DOCKER_HOST")
	contextName := options.Context
	if contextName == "" {
		contextName = os.Getenv("DOCKER_CONTEXT")
	}
	if contextName != "" || host == "" {
		args := []string{"context", "inspect", "--format", "{{.Endpoints.docker.Host}}"}
		if contextName != "" {
			args = append(args, contextName)
		}
		raw, err := dockerOutput(ctx, options, "", args...)
		if err != nil {
			return fmt.Errorf("inspect the selected Docker context: %w", err)
		}
		host = strings.TrimSpace(string(raw))
	}
	if !strings.HasPrefix(host, "unix:///") && !strings.HasPrefix(strings.ToLower(host), "npipe:////./pipe/") {
		return errors.New("cloud lifecycle requires a local Docker Unix socket or Windows named pipe")
	}
	return nil
}

func (s localStack) compose(ctx context.Context, options *cloudOptions, args ...string) ([]byte, error) {
	base := []string{"compose", "--env-file", s.env, "-f", filepath.Join(s.ops, "docker-compose.yml"), "--profile", "cli"}
	data, err := dockerOutput(ctx, options, s.ops, append(base, args...)...)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("docker compose %s failed (exit %d); check the selected Docker endpoint and Compose configuration", args[0], exit.ExitCode())
		}
		return nil, err
	}
	return data, nil
}

func (s localStack) validateCredentials(ctx context.Context, options *cloudOptions) error {
	raw, err := s.compose(ctx, options, "config", "--format", "json")
	if err != nil {
		return err
	}
	var resolved struct {
		Services map[string]struct {
			Environment map[string]string
		}
	}
	if err := json.Unmarshal(raw, &resolved); err != nil {
		return errors.New("cannot read resolved Compose configuration")
	}
	env := resolved.Services["console"].Environment
	token, password := env["KAMUI_TOKEN"], env["KAMUI_ADMIN_PASSWORD"]
	if len(token) < 32 || len(password) < 24 || token == password ||
		strings.ContainsFunc(token, unicode.IsSpace) ||
		strings.ContainsFunc(token, unicode.IsControl) || strings.ContainsFunc(password, unicode.IsControl) {
		return errors.New("configure distinct strong KAMUI_TOKEN and KAMUI_ADMIN_PASSWORD values")
	}
	return nil
}

type composeService struct {
	Service string `json:"service"`
	State   string `json:"state"`
	Health  string `json:"health,omitempty"`
}

func composeServices(raw []byte) ([]composeService, error) {
	result := make([]composeService, 0)
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return result, nil
	}
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, err
		}
		return result, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		var item composeService
		err := dec.Decode(&item)
		if errors.Is(err, io.EOF) {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
}

func cloudCommand(options *cloudOptions) *cobra.Command {
	parent := &cobra.Command{Use: "cloud", Short: "Manage the local Kamui deployment"}
	initStack := func(cmd *cobra.Command, stack localStack) error {
		created, err := stack.init()
		if err != nil {
			return err
		}
		if options.JSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]bool{"created": created})
		}
		if created {
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "Created ops/.env. Open it privately to read the Console password.")
		} else {
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "Using existing ops/.env.")
		}
		return err
	}

	wrap := func(use, short string, requiresDocker bool, action func(*cobra.Command, localStack) error) *cobra.Command {
		return &cobra.Command{Use: use, Short: short, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			stack, err := findLocalStack(options.Root)
			if err != nil {
				return err
			}
			if requiresDocker {
				if err := requireLocalDocker(cmd.Context(), options); err != nil {
					return err
				}
			}
			return action(cmd, stack)
		}}
	}
	parent.AddCommand(
		wrap("init", "Initialize local credentials", false, initStack),
		wrap("start", "Build and start the local Compose stack", true, func(cmd *cobra.Command, stack localStack) error {
			endpoint, _, _, err := localEndpoint(options.Root)
			if err != nil {
				return err
			}
			if _, err := stack.init(); err != nil {
				return err
			}
			ctx := cmd.Context()
			if err := stack.validateCredentials(ctx, options); err != nil {
				return err
			}
			for _, args := range [][]string{
				{"build", "workspace-image", "workspace-proxy"},
				{"run", "--rm", "--no-deps", "tls-init"},
				{"up", "--build", "--detach", "--wait", "--wait-timeout", "120"},
			} {
				if _, err := stack.compose(ctx, options, args...); err != nil {
					return err
				}
			}
			if options.VerifyStart != nil {
				if err := options.VerifyStart(ctx); err != nil {
					return fmt.Errorf("verify started Kamui gateway: %w", err)
				}
			}
			if options.JSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"state": "started", "url": endpoint})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Kamui is running. CLI endpoint %s\n", endpoint)
			return err
		}),
		wrap("stop", "Stop Compose services and retain persisted data", true, func(cmd *cobra.Command, stack localStack) error {
			if _, err := stack.compose(cmd.Context(), options, "stop"); err != nil {
				return err
			}
			if options.JSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"state": "stopped"})
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "Kamui Compose services stopped.")
			return err
		}),
		wrap("status", "List Compose service states", true, func(cmd *cobra.Command, stack localStack) error {
			raw, err := stack.compose(cmd.Context(), options, "ps", "--all", "--format", "json")
			if err != nil {
				return err
			}
			services, err := composeServices(raw)
			if err != nil {
				return errors.New("cannot read Docker Compose service status")
			}
			if options.JSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(services)
			}
			if len(services) == 0 {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "No Kamui Compose containers found.")
				return err
			}
			for _, service := range services {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", service.Service, service.State, service.Health); err != nil {
					return err
				}
			}
			return nil
		}),
	)
	return parent
}

func dockerCommand(options *cloudOptions) *cobra.Command {
	parent := &cobra.Command{Use: "docker", Short: "Inspect Docker CLI configuration"}
	parent.AddCommand(&cobra.Command{Use: "context", Short: "Show Docker context selection and available contexts", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			show, err := dockerOutput(cmd.Context(), options, "", "context", "show")
			if err != nil {
				return fmt.Errorf("read Docker context selection: %w", err)
			}
			list, err := dockerOutput(cmd.Context(), options, "", "context", "ls", "--format", "{{.Name}}")
			if err != nil {
				return fmt.Errorf("list Docker contexts: %w", err)
			}
			selected := strings.TrimSpace(string(show))
			source := "docker configuration"
			switch {
			case options.Context != "":
				source = "--context"
			case os.Getenv("DOCKER_CONTEXT") != "":
				source = "DOCKER_CONTEXT"
			case os.Getenv("DOCKER_HOST") != "":
				source = "DOCKER_HOST override"
			}
			contexts := strings.Fields(string(list))
			if options.JSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"selected": selected, "source": source, "contexts": contexts})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Selected Docker context %s (%s)\nAvailable contexts: %s\n", selected, source, strings.Join(contexts, ", "))
			return err
		}})
	return parent
}
