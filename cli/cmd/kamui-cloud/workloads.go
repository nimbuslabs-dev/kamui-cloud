package main

import (
	"crypto/rand"
	"encoding/json"
	"io"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

var workloadID = regexp.MustCompile(`^wl-[0-9a-f]{32}$`)

type workloadSummary struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	Submission struct {
		Kind  string `json:"kind"`
		Image string `json:"image"`
	} `json:"submission"`
}

func (a *app) workloadCommand(kind string) *cobra.Command {
	action, cancel, short := "submit", "cancel", "Manage batch jobs"
	if kind == "service" {
		action, cancel, short = "start", "stop", "Manage long-running local services"
	}
	group := &cobra.Command{Use: kind, Short: short}
	group.AddCommand(a.submitCommand(kind, action))
	group.AddCommand(&cobra.Command{
		Use: "list", Short: "List recent workloads of this kind", Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.client()
			if err != nil {
				return err
			}
			data, err := client.request(cmd.Context(), "GET", "/v1/workloads", nil, "")
			if err != nil {
				return err
			}
			items, err := filterWorkloads(data, kind)
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), items, a.cloud.JSON)
		},
	})
	for _, operation := range []string{"inspect", "logs", cancel} {
		group.AddCommand(&cobra.Command{
			Use: operation + " ID", Short: workloadHelp(operation), Args: usageArgs(cobra.ExactArgs(1)),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.workloadAction(cmd, kind, operation, args[0])
			},
		})
	}
	group.AddCommand(&cobra.Command{
		Use: "explain ID", Short: "Show the placement decision history for this workload", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !workloadID.MatchString(args[0]) {
				return failure(2, "invalid_id", "provide a valid workload ID")
			}
			return a.send(cmd, "GET", "/v1/workloads/"+args[0]+"/explanation", nil, "")
		},
	})
	return group
}

func workloadHelp(operation string) string {
	switch operation {
	case "inspect":
		return "Show status, assignment and result"
	case "logs":
		return "Read the current or most recent attempt's log tail"
	default:
		return "Request cancellation of this workload"
	}
}

func filterWorkloads(data json.RawMessage, kind string) ([]json.RawMessage, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, failure(6, "response", "invalid workload list")
	}
	selected := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		var summary workloadSummary
		if err := json.Unmarshal(item, &summary); err != nil {
			return nil, failure(6, "response", "invalid workload snapshot")
		}
		if summary.Submission.Kind == kind {
			selected = append(selected, item)
		}
	}
	return selected, nil
}

func (a *app) workloadAction(cmd *cobra.Command, kind, operation, id string) error {
	if !workloadID.MatchString(id) {
		return failure(2, "invalid_id", "provide a valid workload ID")
	}
	client, err := a.client()
	if err != nil {
		return err
	}
	path := "/v1/workloads/" + id
	current, err := client.request(cmd.Context(), "GET", path, nil, "")
	if err != nil {
		return err
	}
	var job workloadSummary
	if err := json.Unmarshal(current, &job); err != nil || job.ID != id {
		return failure(6, "response", "invalid workload snapshot")
	}
	if job.Submission.Kind != kind {
		return failure(4, "not_found", "no workload of this kind matches the ID")
	}
	switch operation {
	case "inspect":
		return printJSON(cmd.OutOrStdout(), current, a.cloud.JSON)
	case "logs":
		logs, err := client.request(cmd.Context(), "GET", path+"/logs", nil, "")
		if err != nil {
			return err
		}
		if a.cloud.JSON {
			return printJSON(cmd.OutOrStdout(), logs, true)
		}
		var output struct{ Tail string }
		if err := json.Unmarshal(logs, &output); err != nil {
			return failure(6, "response", "invalid workload logs")
		}
		if isTerminal(cmd.OutOrStdout()) {
			output.Tail = terminalText(output.Tail)
		}
		_, err = io.WriteString(cmd.OutOrStdout(), output.Tail)
		return err
	default:
		result, err := client.request(cmd.Context(), "POST", path+"/cancel", nil, "")
		if err != nil {
			return err
		}
		return printJSON(cmd.OutOrStdout(), result, a.cloud.JSON)
	}
}

type workloadSubmission struct {
	Kind             string   `json:"kind"`
	Service          string   `json:"service,omitempty"`
	Image            string   `json:"image"`
	Args             []string `json:"args,omitempty"`
	WorkerID         string   `json:"worker_id,omitempty"`
	Placement        string   `json:"placement,omitempty"`
	PreferredWorkers []string `json:"preferred_workers,omitempty"`
	Fallback         *bool    `json:"fallback,omitempty"`
	Priority         int      `json:"priority,omitempty"`
	Resources        struct {
		CPUMillis       int64    `json:"cpu_millis,omitempty"`
		Memory          int64    `json:"memory_bytes,omitempty"`
		StorageBytes    int64    `json:"storage_bytes,omitempty"`
		OS              string   `json:"os,omitempty"`
		Arch            string   `json:"arch,omitempty"`
		GPUCount        int      `json:"gpu_count,omitempty"`
		GPUCapabilities []string `json:"gpu_capabilities,omitempty"`
	} `json:"resources"`
	Policy struct {
		TimeoutSeconds int `json:"timeout_seconds,omitempty"`
		MaxRetries     int `json:"max_retries,omitempty"`
	} `json:"policy"`
}

func (a *app) submitCommand(kind, action string) *cobra.Command {
	var input workloadSubmission
	var key string
	var node string
	var preferred string
	var allowFallback bool
	cmd := &cobra.Command{
		Use: action + " IMAGE [-- ARG ...]", Short: "Submit a locally provisioned Linux Docker image",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 || (cmd.ArgsLenAtDash() != -1 && cmd.ArgsLenAtDash() != 1) ||
				(len(args) > 1 && cmd.ArgsLenAtDash() == -1) {
				return failure(2, "usage", "provide exactly one image and put container arguments after --")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			input.Kind, input.Image, input.Args = kind, args[0], args[1:]
			switch {
			case node != "":
				if node != "local" && !strings.HasPrefix(node, "wk-") {
					return failure(2, "invalid_id", "--node must be a worker ID like wk-…")
				}
				input.Placement, input.WorkerID = "manual", node
			case preferred != "":
				input.Placement = "preferred"
				input.PreferredWorkers = strings.Split(preferred, ",")
				input.Fallback = &allowFallback
			default:
				input.Placement = "auto"
			}
			if key == "" {
				key = rand.Text()
			}
			return a.send(cmd, "POST", "/v1/workloads", input, key)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&key, "idempotency-key", "", "Reuse this key to safely retry a submission")
	flags.StringVar(&input.Service, "service", "", "Workload service label")
	flags.StringVar(&node, "node", "", "Run on this worker (manual placement)")
	flags.StringVar(&preferred, "preferred", "", "Comma-separated worker preference order (preferred placement)")
	flags.BoolVar(&allowFallback, "allow-fallback", true, "Preferred placement may fall back to automatic selection")
	flags.StringVar(&input.Placement, "placement", "", "Override placement mode: auto, manual or preferred")
	flags.IntVar(&input.Priority, "priority", 0, "Queue priority 1–100 (default 50)")
	flags.Int64Var(&input.Resources.CPUMillis, "cpu-millis", 0, "CPU request in millicores")
	flags.Int64Var(&input.Resources.Memory, "memory-bytes", 0, "Memory request in bytes")
	flags.Int64Var(&input.Resources.StorageBytes, "storage-bytes", 0, "Storage request in bytes")
	flags.StringVar(&input.Resources.OS, "os", "", "Required Docker runtime OS")
	flags.StringVar(&input.Resources.Arch, "arch", "", "Required Docker runtime architecture")
	flags.IntVar(&input.Resources.GPUCount, "gpus", 0, "Required GPU count")
	flags.StringSliceVar(&input.Resources.GPUCapabilities, "gpu-capability", nil, "Required GPU capabilities (cuda, rocm, …)")
	flags.IntVar(&input.Policy.TimeoutSeconds, "timeout", 0, "Batch runtime or service startup timeout in seconds")
	flags.IntVar(&input.Policy.MaxRetries, "retries", 0, "Maximum retries after a failed attempt")
	return cmd
}
