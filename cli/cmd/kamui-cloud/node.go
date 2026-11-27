package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

type workerSummary struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	State  string `json:"state"`
	Status string `json:"status"`
}

func workerIDMatches(id string) bool {
	return id == "local" || (len(id) == 35 && strings.HasPrefix(id, "wk-"))
}

func (a *app) nodeCommand() *cobra.Command {
	group := &cobra.Command{Use: "node", Short: "Manage contributed worker devices"}
	group.AddCommand(a.nodeJoinCommand())
	group.AddCommand(a.nodeListCommand())
	group.AddCommand(a.nodeInspectCommand())
	for _, action := range []struct{ name, short, endpoint string }{
		{"approve", "Approve a pending worker", "approve"},
		{"revoke", "Revoke a worker and disable its credentials", "revoke"},
		{"drain", "Stop new assignments; running workloads finish", "drain"},
		{"enable", "Re-enable a disabled or draining worker", "enable"},
		{"disable", "Temporarily disable a worker", "disable"},
	} {
		action := action
		group.AddCommand(&cobra.Command{
			Use:   action.name + " WORKER_ID",
			Short: action.short,
			Args:  usageArgs(cobra.ExactArgs(1)),
			RunE: func(cmd *cobra.Command, args []string) error {
				if !workerIDMatches(args[0]) {
					return failure(2, "invalid_id", "provide a worker ID like wk-…")
				}
				return a.send(cmd, "POST", "/v1/workers/"+args[0]+"/"+action.endpoint, map[string]any{}, "")
			},
		})
	}
	group.AddCommand(a.nodeShareCommand())
	group.AddCommand(a.nodeMapCommand())
	return group
}

func (a *app) nodeJoinCommand() *cobra.Command {
	var name string
	var ttl int
	var autoApprove bool
	cmd := &cobra.Command{
		Use:   "join",
		Short: "Create a single-use enrollment credential for a new device",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			input := map[string]any{"name": name, "auto_approve": autoApprove}
			if ttl != 0 {
				input["ttl_seconds"] = ttl
			}
			client, err := a.client()
			if err != nil {
				return err
			}
			data, err := client.request(cmd.Context(), "POST", "/v1/workers/enrollments", input, "")
			if err != nil {
				return err
			}
			var reply struct {
				Token     string `json:"token"`
				ExpiresAt string `json:"expires_at"`
			}
			if err := json.Unmarshal(data, &reply); err != nil || reply.Token == "" {
				return failure(6, "response", "invalid enrollment response")
			}
			if a.cloud.JSON {
				return printJSON(cmd.OutOrStdout(), data, true)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "Enrollment credential created (single use, expires "+reply.ExpiresAt+").")
			fmt.Fprintln(out, "Run this on the worker device:")
			join := "kamui-agent enroll --url " + a.effectiveURL() + " --token " + reply.Token
			if a.ca != "" {
				join += " --ca-file " + a.ca
			}
			fmt.Fprintln(out, "  "+join)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Human-readable device name")
	cmd.Flags().IntVar(&ttl, "ttl", 0, "Credential lifetime in seconds (1–86400, default 900)")
	cmd.Flags().BoolVar(&autoApprove, "auto-approve", false, "Approve the device on enrollment")
	return cmd
}

func (a *app) effectiveURL() string {
	if a.url != "" {
		return a.url
	}
	if endpoint, _, _, err := localEndpoint(a.cloud.Root); err == nil {
		return endpoint
	}
	return ""
}

func (a *app) nodeListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List contributed workers",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.client()
			if err != nil {
				return err
			}
			data, err := client.request(cmd.Context(), "GET", "/v1/workers", nil, "")
			if err != nil {
				return err
			}
			if a.cloud.JSON {
				return printJSON(cmd.OutOrStdout(), data, true)
			}
			var items []workerSummary
			if err := json.Unmarshal(data, &items); err != nil {
				return failure(6, "response", "invalid worker list")
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%-38s  %-8s  %-10s  %s\n", "WORKER", "STATE", "STATUS", "NAME")
			for _, item := range items {
				fmt.Fprintf(out, "%-38s  %-8s  %-10s  %s\n", item.ID, item.State, item.Status, item.Name)
			}
			return nil
		},
	}
}

func (a *app) nodeInspectCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect WORKER_ID",
		Short: "Show a worker's capabilities, telemetry and sharing policy",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !workerIDMatches(args[0]) {
				return failure(2, "invalid_id", "provide a worker ID like wk-…")
			}
			return a.send(cmd, "GET", "/v1/workers/"+args[0], nil, "")
		},
	}
}

func (a *app) nodeShareCommand() *cobra.Command {
	var policy struct {
		CPUMillisMax   int64
		MemoryBytesMax int64
		StorageMax     int64
		GPU            bool
		Kinds          []string
		Projects       []string
		Users          []string
		ScheduleDay    []int
		ScheduleStart  string
		ScheduleEnd    string
		Timezone       string
		Battery        bool
		MaxConcurrent  int
	}
	cmd := &cobra.Command{
		Use:   "share WORKER_ID",
		Short: "Set the resource-sharing policy for a worker you own",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !workerIDMatches(args[0]) {
				return failure(2, "invalid_id", "provide a worker ID like wk-…")
			}
			body := map[string]any{
				"gpu_allowed":            policy.GPU,
				"allow_heavy_on_battery": policy.Battery,
			}
			if policy.CPUMillisMax != 0 {
				body["cpu_millis_max"] = policy.CPUMillisMax
			}
			if policy.MemoryBytesMax != 0 {
				body["memory_bytes_max"] = policy.MemoryBytesMax
			}
			if policy.StorageMax != 0 {
				body["storage_bytes_max"] = policy.StorageMax
			}
			if len(policy.Kinds) > 0 {
				body["workload_kinds"] = policy.Kinds
			}
			if len(policy.Projects) > 0 {
				body["projects"] = policy.Projects
			}
			if len(policy.Users) > 0 {
				body["users"] = policy.Users
			}
			if len(policy.ScheduleDay) > 0 || policy.ScheduleStart != "" || policy.ScheduleEnd != "" {
				body["schedule"] = []map[string]any{{
					"days":  policy.ScheduleDay,
					"start": policy.ScheduleStart,
					"end":   policy.ScheduleEnd,
				}}
				if policy.Timezone != "" {
					body["timezone"] = policy.Timezone
				}
			}
			if policy.MaxConcurrent != 0 {
				body["max_concurrent"] = policy.MaxConcurrent
			}
			return a.send(cmd, "PUT", "/v1/workers/"+args[0]+"/policy", body, "")
		},
	}
	flags := cmd.Flags()
	flags.Int64Var(&policy.CPUMillisMax, "cpu-millis-max", 0, "Maximum CPU per workload in millicores (0 = unlimited)")
	flags.Int64Var(&policy.MemoryBytesMax, "memory-bytes-max", 0, "Maximum RAM per workload in bytes (0 = unlimited)")
	flags.Int64Var(&policy.StorageMax, "storage-bytes-max", 0, "Maximum storage per workload in bytes (0 = unlimited)")
	flags.BoolVar(&policy.GPU, "gpu", false, "Permit GPU workloads (passthrough still requires an NVIDIA runtime)")
	flags.StringSliceVar(&policy.Kinds, "kinds", nil, "Allowed workload kinds (job, service)")
	flags.StringSliceVar(&policy.Projects, "projects", nil, "Projects permitted to use this worker")
	flags.StringSliceVar(&policy.Users, "users", nil, "User IDs permitted to use this worker")
	flags.IntSliceVar(&policy.ScheduleDay, "schedule-day", nil, "Availability weekdays, 0=Sunday…6=Saturday")
	flags.StringVar(&policy.ScheduleStart, "schedule-start", "", "Availability window start (HH:MM)")
	flags.StringVar(&policy.ScheduleEnd, "schedule-end", "", "Availability window end (HH:MM)")
	flags.StringVar(&policy.Timezone, "timezone", "", "IANA timezone for the availability window")
	flags.BoolVar(&policy.Battery, "heavy-on-battery", false, "Allow heavy workloads while on battery")
	flags.IntVar(&policy.MaxConcurrent, "max-concurrent", 0, "Maximum concurrent workloads (0 = unlimited)")
	return cmd
}

func (a *app) nodeMapCommand() *cobra.Command {
	var project, mode, worker string
	var allowFallback bool
	var clear bool
	cmd := &cobra.Command{
		Use:   "map --project PROJECT_ID",
		Short: "Set a project's default device mapping (auto, manual or preferred)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if project == "" {
				return failure(2, "usage", "--project is required")
			}
			if clear {
				mode, worker = "auto", ""
			}
			body := map[string]any{"mode": mode}
			switch mode {
			case "manual":
				if worker == "" {
					return failure(2, "usage", "manual mapping requires --worker WORKER_ID")
				}
				body["preferred_workers"] = []string{worker}
			case "preferred":
				if worker == "" {
					return failure(2, "usage", "preferred mapping requires --worker WORKER_ID (comma-separate for an ordered list)")
				}
				body["preferred_workers"] = strings.Split(worker, ",")
				body["fallback"] = allowFallback
			case "auto":
			default:
				return failure(2, "usage", "--mode must be auto, manual or preferred")
			}
			return a.send(cmd, "PUT", "/v1/projects/"+project+"/scheduling-preferences", body, "")
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&project, "project", "", "Project to configure")
	flags.StringVar(&mode, "mode", "auto", "Placement mode: auto, manual or preferred")
	flags.StringVar(&worker, "worker", "", "Worker ID(s); comma-separated for preferred mode")
	flags.BoolVar(&allowFallback, "allow-fallback", true, "Preferred mode may fall back to automatic placement")
	flags.BoolVar(&clear, "clear", false, "Reset the mapping to automatic placement")
	return cmd
}

func (a *app) schedulerCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Queue depth, assignments and recent placement decisions",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.send(cmd, "GET", "/v1/scheduler/status", nil, "")
		},
	}
}
