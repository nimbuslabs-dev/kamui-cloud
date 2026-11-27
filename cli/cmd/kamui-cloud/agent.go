package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// The CLI shares the Rust agent's state file so `agent enroll` prepares exactly
// what `kamui-agent worker` consumes.
type agentState struct {
	Coordinator string `json:"coordinator"`
	WorkerID    string `json:"worker_id"`
	WorkerToken string `json:"worker_token"`
	Name        string `json:"name"`
}

func agentHome() (string, error) {
	if dir := os.Getenv("KAMUI_AGENT_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", failure(2, "config", "cannot locate the home directory; set KAMUI_AGENT_HOME")
	}
	return filepath.Join(home, ".kamui-agent"), nil
}

func agentStatePath() (string, error) {
	dir, err := agentHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "worker.json"), nil
}

func (a *app) agentCommand() *cobra.Command {
	group := &cobra.Command{Use: "agent", Short: "Enroll this device and run the worker agent"}
	group.AddCommand(a.agentEnrollCommand())
	group.AddCommand(a.agentStartCommand())
	group.AddCommand(a.agentStatusCommand())
	group.AddCommand(a.agentStopCommand())
	return group
}

func (a *app) agentEnrollCommand() *cobra.Command {
	var endpoint, token, name, ca string
	cmd := &cobra.Command{
		Use:   "enroll --url URL --token TOKEN",
		Short: "Exchange a node join credential for worker identity",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if endpoint == "" {
				endpoint = a.effectiveURL()
			}
			if endpoint == "" || token == "" {
				return failure(2, "usage", "--url and --token are required (run `kamui-cloud node join` on the coordinator)")
			}
			parsed, err := endpointURL(endpoint)
			if err != nil {
				return err
			}
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.Proxy = nil
			tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
			if ca != "" {
				data, err := os.ReadFile(ca)
				if err != nil {
					return failure(2, "tls_ca", "cannot read the coordinator CA certificate")
				}
				tlsConfig.RootCAs = x509.NewCertPool()
				if !tlsConfig.RootCAs.AppendCertsFromPEM(data) {
					return failure(2, "tls_ca", "the coordinator CA file contains no valid PEM certificate")
				}
			}
			transport.TLSClientConfig = tlsConfig
			client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
			body := map[string]any{"token": token}
			if name != "" {
				body["name"] = name
			}
			payload, _ := json.Marshal(body)
			req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, parsed.Scheme+"://"+parsed.Host+"/v1/workers/enroll", io.NopCloser(bytes.NewReader(payload)))
			if err != nil {
				return failure(2, "invalid_url", err.Error())
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				return failure(6, "connection", "could not reach the coordinator")
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			if err != nil {
				return failure(6, "response", "cannot read the enrollment response")
			}
			var reply struct {
				WorkerID    string `json:"worker_id"`
				WorkerToken string `json:"worker_token"`
				Approved    bool   `json:"approved"`
				Error       *struct {
					Code, Message string
				} `json:"error"`
			}
			if err := json.Unmarshal(data, &reply); err != nil || reply.WorkerID == "" {
				if reply.Error != nil {
					return failure(3, reply.Error.Code, reply.Error.Message)
				}
				return failure(6, "response", "invalid enrollment response")
			}
			display := name
			if display == "" {
				hostname, _ := os.Hostname()
				display = hostname
			}
			state := agentState{
				Coordinator: parsed.Scheme + "://" + parsed.Host,
				WorkerID:    reply.WorkerID,
				WorkerToken: reply.WorkerToken,
				Name:        display,
			}
			dir, err := agentHome()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(dir, 0700); err != nil {
				return failure(1, "config", "cannot create the agent state directory")
			}
			path := filepath.Join(dir, "worker.json")
			data2, _ := json.MarshalIndent(state, "", "  ")
			if err := os.WriteFile(path, data2, 0600); err != nil {
				return failure(1, "config", "cannot write the agent state file")
			}
			if a.cloud.JSON {
				return printJSON(cmd.OutOrStdout(), map[string]any{
					"worker_id": reply.WorkerID, "approved": reply.Approved, "state_file": path,
				}, true)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Enrolled as %s (%s)\n", reply.WorkerID, display)
			if reply.Approved {
				fmt.Fprintln(out, "Approved; start the worker with: kamui-cloud agent start")
			} else {
				fmt.Fprintln(out, "Pending owner approval; assignments are withheld until an owner approves this device.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&endpoint, "url", "", "Coordinator URL")
	cmd.Flags().StringVar(&token, "token", "", "Single-use enrollment token from `kamui-cloud node join`")
	cmd.Flags().StringVar(&name, "name", "", "Human-readable device name")
	cmd.Flags().StringVar(&ca, "ca-file", "", "PEM certificate authority for the coordinator")
	return cmd
}

func (a *app) agentStartCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the local worker agent (kamui-agent worker)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			statePath, err := agentStatePath()
			if err != nil {
				return err
			}
			if _, err := os.Stat(statePath); err != nil {
				return failure(2, "config", "this device is not enrolled; run `kamui-cloud agent enroll` first")
			}
			if pid, running := agentProcess(); running {
				return failure(5, "already_running", fmt.Sprintf("the worker agent is already running (pid %d)", pid))
			}
			binary := os.Getenv("KAMUI_AGENT_PATH")
			if binary == "" {
				binary, err = exec.LookPath("kamui-agent")
				if err != nil {
					return failure(2, "config", "cannot find kamui-agent; install it or set KAMUI_AGENT_PATH")
				}
			}
			dir, _ := agentHome()
			logFile, err := os.OpenFile(filepath.Join(dir, "agent.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return failure(1, "config", "cannot open the agent log file")
			}
			defer logFile.Close()
			process := exec.Command(binary, "worker")
			process.Stdout, process.Stderr = logFile, logFile
			process.Stdin = nil
			process.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := process.Start(); err != nil {
				return failure(1, "start_failed", "cannot start kamui-agent: "+err.Error())
			}
			pidFile := filepath.Join(dir, "agent.pid")
			if err := os.WriteFile(pidFile, []byte(strconv.Itoa(process.Process.Pid)), 0600); err != nil {
				return failure(1, "config", "cannot write the agent pid file")
			}
			return printJSON(cmd.OutOrStdout(), map[string]any{
				"pid": process.Process.Pid, "log": filepath.Join(dir, "agent.log"),
			}, a.cloud.JSON)
		},
	}
}

func agentProcess() (int, bool) {
	dir, err := agentHome()
	if err != nil {
		return 0, false
	}
	data, err := os.ReadFile(filepath.Join(dir, "agent.pid"))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(string(trimSpace(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}
	if err := process.Signal(syscall.Signal(0)); err != nil {
		return 0, false
	}
	return pid, true
}

func trimSpace(data []byte) []byte {
	start, end := 0, len(data)
	for start < end && (data[start] == ' ' || data[start] == '\n' || data[start] == '\t' || data[start] == '\r') {
		start++
	}
	for end > start && (data[end-1] == ' ' || data[end-1] == '\n' || data[end-1] == '\t' || data[end-1] == '\r') {
		end--
	}
	return data[start:end]
}

func (a *app) agentStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the local agent process and coordinator view of this device",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			statePath, err := agentStatePath()
			if err != nil {
				return err
			}
			data, err := os.ReadFile(statePath)
			if err != nil {
				return failure(2, "config", "this device is not enrolled; run `kamui-cloud agent enroll` first")
			}
			var state agentState
			if err := json.Unmarshal(data, &state); err != nil {
				return failure(2, "config", "the agent state file is invalid; enroll again")
			}
			pid, running := agentProcess()
			result := map[string]any{
				"worker_id":   state.WorkerID,
				"name":        state.Name,
				"coordinator": state.Coordinator,
				"pid":         pid,
				"running":     running,
			}
			if a.tokenAvailable() {
				client, err := a.client()
				if err == nil {
					if raw, err := client.request(cmd.Context(), "GET", "/v1/workers/"+state.WorkerID, nil, ""); err == nil {
						var worker map[string]any
						if json.Unmarshal(raw, &worker) == nil {
							result["coordinator_state"] = worker["state"]
							result["policy"] = worker["policy"]
						}
					}
				}
			}
			return printJSON(cmd.OutOrStdout(), result, a.cloud.JSON)
		},
	}
}

func (a *app) tokenAvailable() bool {
	if a.tokenFile != "" || os.Getenv("KAMUI_TOKEN") != "" {
		return true
	}
	return a.url == "" // local stack resolves the operator token itself
}

func (a *app) agentStopCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the local worker agent",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			pid, running := agentProcess()
			if !running {
				return printJSON(cmd.OutOrStdout(), map[string]any{"stopped": false}, a.cloud.JSON)
			}
			process, err := os.FindProcess(pid)
			if err != nil {
				return failure(1, "stop_failed", "cannot find the agent process")
			}
			if err := process.Signal(syscall.SIGTERM); err != nil {
				return failure(1, "stop_failed", "cannot signal the agent process")
			}
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				if _, stillRunning := agentProcess(); !stillRunning {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			return printJSON(cmd.OutOrStdout(), map[string]any{"stopped": true, "pid": pid}, a.cloud.JSON)
		},
	}
}
