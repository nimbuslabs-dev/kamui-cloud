package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// registerServices attaches every service command tree to the root command.
func (a *app) registerServices(root *cobra.Command) {
	root.AddCommand(a.deployCommand(), a.storeCommand(), a.backupCommand(),
		a.trainCommand(), a.inferCommand(), a.databaseCommand(), a.monitorCommand(), a.dropCommand())
}

// sendQuiet performs a request returning parsed JSON or an error.
func (a *app) sendQuiet(cmd *cobra.Command, method, path string, body any) (json.RawMessage, error) {
	client, err := a.client()
	if err != nil {
		return nil, err
	}
	return client.request(cmd.Context(), method, path, body, "")
}

// --- Deploy ---------------------------------------------------------------

func (a *app) deployCommand() *cobra.Command {
	group := &cobra.Command{Use: "deploy", Short: "Build and host web applications"}
	project := group.PersistentFlags()
	project.StringVar(&svcProjectFlag, "project", "", "Project ID")
	create := &cobra.Command{
		Use:   "create NAME",
		Short: "Declare a deployment", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			source, _ := cmd.Flags().GetString("source")
			sourceRef, _ := cmd.Flags().GetString("source-ref")
			placement, _ := cmd.Flags().GetString("placement")
			node, _ := cmd.Flags().GetString("node")
			cpu, _ := cmd.Flags().GetInt64("cpu-millis")
			memory, _ := cmd.Flags().GetInt64("memory-bytes")
			envList, _ := cmd.Flags().GetStringArray("env")
			env := map[string]string{}
			for _, entry := range envList {
				key, value, found := strings.Cut(entry, "=")
				if !found {
					return failure(2, "usage", "--env entries must be KEY=VALUE")
				}
				env[key] = value
			}
			body := map[string]any{
				"name": args[0], "source": source, "source_ref": sourceRef,
				"placement": placement, "worker_id": node, "env": env,
				"resources": map[string]any{"cpu_millis": cpu, "memory_bytes": memory},
			}
			return a.serviceSend(cmd, "POST", svcProjectPath("/deployments"), body)
		},
	}
	create.Flags().String("source", "", "Deployment source: git, dockerfile or image")
	create.Flags().String("source-ref", "", "Git URL for git source; image reference for image source")
	create.Flags().String("placement", "auto", "auto, manual or preferred")
	create.Flags().String("node", "", "Worker ID for manual placement")
	create.Flags().Int64("cpu-millis", 0, "CPU request in millicores")
	create.Flags().Int64("memory-bytes", 0, "Memory request in bytes")
	create.Flags().StringArray("env", nil, "Environment entry KEY=VALUE (values may reference secret:NAME)")
	group.AddCommand(create)
	group.AddCommand(&cobra.Command{
		Use: "list", Short: "List deployments", Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.serviceSend(cmd, "GET", svcProjectPath("/deployments"), nil)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "inspect DEPLOYMENT", Short: "Show revisions, builds and runtime state", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "GET", svcProjectPath("/deployments/"+args[0]), nil)
		},
	})
	for _, op := range []struct{ name, method, path string }{
		{"start", "POST", "/start"}, {"stop", "POST", "/stop"}, {"restart", "POST", "/start"},
	} {
		op := op
		group.AddCommand(&cobra.Command{
			Use: op.name + " DEPLOYMENT", Short: strings.Title(op.name) + " a deployment runtime", Args: usageArgs(cobra.ExactArgs(1)),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.serviceSend(cmd, op.method, svcProjectPath("/deployments/"+args[0]+op.path), map[string]any{})
			},
		})
	}
	group.AddCommand(&cobra.Command{
		Use: "build DEPLOYMENT [--commit SHA] [--dockerfile CONTENT]", Short: "Trigger a build",
		RunE: func(cmd *cobra.Command, args []string) error {
			commit, _ := cmd.Flags().GetString("commit")
			dockerfile, _ := cmd.Flags().GetString("dockerfile")
			body := map[string]any{}
			if commit != "" {
				body["commit"] = commit
			}
			if dockerfile != "" {
				body["dockerfile"] = dockerfile
			}
			return a.serviceSend(cmd, "POST", svcProjectPath("/deployments/"+args[0]+"/builds"), body)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "release DEPLOYMENT REVISION", Short: "Release a built revision", Args: usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "POST", svcProjectPath("/deployments/"+args[0]+"/revisions/"+args[1]+"/release"), map[string]any{})
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "rollback DEPLOYMENT", Short: "Roll back to the previous healthy revision", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "POST", svcProjectPath("/deployments/"+args[0]+"/rollback"), map[string]any{})
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "logs DEPLOYMENT", Short: "Read the runtime log tail", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "POST", svcProjectPath("/deployments/"+args[0]+"/logs"), map[string]any{})
		},
	})
	group.AddCommand(&cobra.Command{
		Use:   "proxy DEPLOYMENT [--method GET --path /path --data JSON]",
		Short: "Send an authenticated request through the deployment service proxy",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			method, _ := cmd.Flags().GetString("method")
			path, _ := cmd.Flags().GetString("path")
			data, _ := cmd.Flags().GetString("data")
			if method == "" {
				method = "GET"
			}
			if method != "GET" && method != "POST" && method != "PUT" && method != "DELETE" && method != "PATCH" && method != "HEAD" && method != "OPTIONS" {
				return failure(2, "usage", "--method must be a valid HTTP method")
			}
			if !validDeployProxyPath(path) {
				return failure(2, "usage", "--path must be an absolute service-relative path without traversal")
			}
			client, err := a.client()
			if err != nil {
				return err
			}
			var body any
			if data != "" {
				body = json.RawMessage(data)
			}
			result, err := client.request(cmd.Context(), method, svcProjectPath("/deployments/"+args[0]+"/proxy"+path), body, "")
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), result, a.cloud.JSON)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "delete DEPLOYMENT", Short: "Stop and delete a deployment", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "DELETE", svcProjectPath("/deployments/"+args[0]), nil)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "webhook", Short: "Manage deployment webhooks",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.cloud.JSON {
				return failure(2, "usage", "select a webhook subcommand when using --json")
			}
			return cmd.Help()
		},
	})
	webhookGroup := group.Commands()[len(group.Commands())-1]
	webhookGroup.AddCommand(&cobra.Command{
		Use: "create DEPLOYMENT", Short: "Create a webhook for a deployment", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			branchFilter, _ := cmd.Flags().GetString("branch-filter")
			body := map[string]any{"branch_filter": branchFilter}
			return a.serviceSend(cmd, "POST", svcProjectPath("/deployments/"+args[0]+"/webhooks"), body)
		},
	})
	webhookGroup.Commands()[0].Flags().String("branch-filter", "", "Only trigger on this branch")
	webhookGroup.AddCommand(&cobra.Command{
		Use: "list DEPLOYMENT", Short: "List webhooks for a deployment", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "GET", svcProjectPath("/deployments/"+args[0]+"/webhooks"), nil)
		},
	})
	webhookGroup.AddCommand(&cobra.Command{
		Use: "delete DEPLOYMENT WEBHOOK", Short: "Delete a webhook", Args: usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "DELETE", svcProjectPath("/deployments/"+args[0]+"/webhooks/"+args[1]), nil)
		},
	})
	return group
}

func validDeployProxyPath(path string) bool {
	if path == "" {
		return true // root path is valid
	}
	u, err := url.ParseRequestURI(path)
	if err != nil || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") || strings.Contains(u.Path, "\\") {
		return false
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// --- Store -----------------------------------------------------------------

func (a *app) storeCommand() *cobra.Command {
	group := addProjectFlag(&cobra.Command{Use: "store", Short: "Project object storage"})
	bucket := &cobra.Command{Use: "bucket", Short: "Manage buckets"}
	bucket.AddCommand(&cobra.Command{
		Use: "create NAME", Short: "Create a bucket", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "POST", svcProjectPath("/buckets"), map[string]any{"name": args[0]})
		},
	})
	bucket.AddCommand(&cobra.Command{
		Use: "list", Short: "List buckets", Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.serviceSend(cmd, "GET", svcProjectPath("/buckets"), nil)
		},
	})
	group.AddCommand(bucket)
	object := &cobra.Command{Use: "object", Short: "Manage objects"}
	object.AddCommand(&cobra.Command{
		Use: "upload BUCKET KEY FILE", Short: "Upload a file", Args: usageArgs(cobra.ExactArgs(3)),
		RunE: func(cmd *cobra.Command, args []string) error {
			file, err := os.Open(args[2])
			if err != nil {
				return failure(2, "invalid_file", "cannot open the file to upload")
			}
			defer file.Close()
			client, err := a.client()
			if err != nil {
				return err
			}
			data, err := io.ReadAll(file)
			if err != nil {
				return failure(1, "upload_failed", "cannot read the file")
			}
			result, err := client.rawRequest(cmd.Context(), "PUT",
				svcProjectPath("/buckets/"+args[0]+"/objects/"+args[1]), data)
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), result, a.cloud.JSON)
		},
	})
	object.AddCommand(&cobra.Command{
		Use: "download BUCKET KEY [FILE]", Short: "Download an object", Args: usageArgs(cobra.RangeArgs(2, 3)),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := a.client()
			if err != nil {
				return err
			}
			data, err := client.rawRequest(cmd.Context(), "GET",
				svcProjectPath("/buckets/"+args[0]+"/objects/"+args[1]), nil)
			if err != nil {
				return err
			}
			if len(args) == 3 {
				return os.WriteFile(args[2], data, 0600)
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err
		},
	})
	object.AddCommand(&cobra.Command{
		Use: "list BUCKET", Short: "List objects", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "GET", svcProjectPath("/buckets/"+args[0]+"/objects"), nil)
		},
	})
	object.AddCommand(&cobra.Command{
		Use: "delete BUCKET KEY", Short: "Delete an object", Args: usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "DELETE", svcProjectPath("/buckets/"+args[0]+"/objects/"+args[1]), nil)
		},
	})
	group.AddCommand(object)
	return group
}

// --- Backup ------------------------------------------------------------------

func (a *app) backupCommand() *cobra.Command {
	group := addProjectFlag(&cobra.Command{Use: "backup", Short: "Back up and restore Kamui state"})
	group.AddCommand(&cobra.Command{
		Use: "create metadata", Short: "Snapshot coordinator metadata (admin)", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "POST", "/v1/backups/metadata", map[string]any{})
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "create objects", Short: "Bundle this project's stored objects", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "POST", svcProjectPath("/backups/objects"), map[string]any{})
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "create volume --volume NAME", Short: "Export a Docker volume backup",
		RunE: func(cmd *cobra.Command, _ []string) error {
			volume, _ := cmd.Flags().GetString("volume")
			if volume == "" {
				return failure(2, "usage", "--volume is required")
			}
			return a.serviceSend(cmd, "POST", svcProjectPath("/backups/volume"), map[string]any{"volume": volume})
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "list", Short: "List backups", Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.serviceSend(cmd, "GET", "/v1/backups"+svcProjectSuffix(), nil)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "inspect BACKUP", Short: "Show backup metadata", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "GET", "/v1/backups/"+args[0], nil)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "restore BACKUP --destination DIR", Short: "Restore into an isolated destination",
		RunE: func(cmd *cobra.Command, args []string) error {
			destination, _ := cmd.Flags().GetString("destination")
			if destination == "" {
				return failure(2, "usage", "--destination is required and must be an isolated directory")
			}
			return a.serviceSend(cmd, "POST", "/v1/backups/"+args[0]+"/restore", map[string]any{"destination": destination})
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "restore-objects BACKUP --destination DIR", Short: "Restore object backup into an isolated directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			destination, _ := cmd.Flags().GetString("destination")
			if destination == "" {
				return failure(2, "usage", "--destination is required and must be an isolated directory")
			}
			return a.serviceSend(cmd, "POST", "/v1/backups/"+args[0]+"/restore-objects", map[string]any{"destination": destination})
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "restore-volume BACKUP --volume NAME", Short: "Restore volume backup into a new Docker volume",
		RunE: func(cmd *cobra.Command, args []string) error {
			volume, _ := cmd.Flags().GetString("volume")
			if volume == "" {
				return failure(2, "usage", "--volume is required")
			}
			return a.serviceSend(cmd, "POST", "/v1/backups/"+args[0]+"/restore-volume", map[string]any{"volume": volume})
		},
	})
	return group
}

// --- Train --------------------------------------------------------------------

func (a *app) trainCommand() *cobra.Command {
	group := addProjectFlag(&cobra.Command{Use: "train", Short: "Run training workloads"})
	trainSubmit := &cobra.Command{
		Use:   "submit NAME IMAGE [-- args...]",
		Short: "Submit a training job",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 2 {
				return failure(2, "usage", "provide a name and an image")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			dataset, _ := cmd.Flags().GetString("dataset")
			cpu, _ := cmd.Flags().GetInt64("cpu-millis")
			memory, _ := cmd.Flags().GetInt64("memory-bytes")
			gpus, _ := cmd.Flags().GetInt("gpus")
			placement, _ := cmd.Flags().GetString("placement")
			timeout, _ := cmd.Flags().GetInt("timeout")
			imageArgs := []string{}
			if len(args) > 2 {
				imageArgs = args[2:]
			}
			body := map[string]any{
				"name": args[0], "image": args[1], "args": imageArgs,
				"dataset_artifact": dataset, "placement": placement, "timeout_seconds": timeout,
				"resources": map[string]any{"cpu_millis": cpu, "memory_bytes": memory, "gpu_count": gpus},
			}
			return a.serviceSend(cmd, "POST", svcProjectPath("/training"), body)
		},
	}
	trainSubmit.Flags().String("dataset", "", "Dataset artifact ID mounted at /kamui/dataset")
	trainSubmit.Flags().Int64("cpu-millis", 0, "CPU request in millicores")
	trainSubmit.Flags().Int64("memory-bytes", 0, "Memory request in bytes")
	trainSubmit.Flags().Int("gpus", 0, "GPU count requested")
	trainSubmit.Flags().String("placement", "auto", "auto, manual or preferred")
	trainSubmit.Flags().Int("timeout", 0, "Training timeout in seconds")
	group.AddCommand(trainSubmit)
	group.AddCommand(&cobra.Command{
		Use: "list", Short: "List training jobs", Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.serviceSend(cmd, "GET", svcProjectPath("/training"), nil)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "inspect JOB", Short: "Show a training job and its logs", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "GET", svcProjectPath("/training/"+args[0]), nil)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "logs JOB", Short: "Read the training log tail", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "GET", svcProjectPath("/training/"+args[0]), nil)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "cancel JOB", Short: "Cancel a training job", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "POST", svcProjectPath("/training/"+args[0]+"/cancel"), map[string]any{})
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "checkpoint list JOB", Short: "List observed metrics for a job", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "GET", svcProjectPath("/training/"+args[0]+"/metrics"), nil)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "resume JOB", Short: "Resume is not implemented yet; training jobs are re-submitted instead",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return failure(4, "not_implemented", "checkpoint resume requires explicit framework support; submit a new job with the checkpoint artifact instead")
		},
	})
	model := &cobra.Command{Use: "model", Short: "Registered model artifacts"}
	model.AddCommand(&cobra.Command{
		Use: "list", Short: "List models", Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.serviceSend(cmd, "GET", svcProjectPath("/models"), nil)
		},
	})
	model.AddCommand(&cobra.Command{
		Use: "inspect MODEL", Short: "Show a model", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "GET", svcProjectPath("/models/"+args[0]), nil)
		},
	})
	group.AddCommand(model)
	return group
}

// --- Infer ---------------------------------------------------------------------

func (a *app) inferCommand() *cobra.Command {
	group := addProjectFlag(&cobra.Command{Use: "infer", Short: "Serve models through runtime adapters"})
	inferDeploy := &cobra.Command{
		Use:   "deploy NAME",
		Short: "Deploy an inference service",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			runtimeKind, _ := cmd.Flags().GetString("runtime")
			image, _ := cmd.Flags().GetString("image")
			model, _ := cmd.Flags().GetString("model")
			upstream, _ := cmd.Flags().GetString("upstream")
			if runtimeKind == "" || image == "" {
				return failure(2, "usage", "--runtime and --image are required")
			}
			body := map[string]any{"name": name, "runtime": runtimeKind, "image": image, "model_id": model, "upstream_url": upstream}
			return a.serviceSend(cmd, "POST", svcProjectPath("/inference"), body)
		},
	}
	inferDeploy.Flags().String("runtime", "", "ollama, llamacpp, vllm, openai-compatible or proxy")
	inferDeploy.Flags().String("image", "", "Runtime server image")
	inferDeploy.Flags().String("model", "", "Registered model ID")
	inferDeploy.Flags().String("upstream", "", "Upstream endpoint for the proxy runtime")
	group.AddCommand(inferDeploy)
	group.AddCommand(&cobra.Command{
		Use: "list", Short: "List inference services", Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.serviceSend(cmd, "GET", svcProjectPath("/inference"), nil)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "inspect SERVICE", Short: "Show an inference service", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "GET", svcProjectPath("/inference/"+args[0]), nil)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "endpoint SERVICE", Short: "Request the service root through the authenticated proxy", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "GET", svcProjectPath("/inference/"+args[0]+"/proxy"), nil)
		},
	})
	group.AddCommand(&cobra.Command{
		Use:   "request SERVICE --method GET --path /v1/models [--data JSON]",
		Short: "Send an authenticated request through the service proxy",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc := args[0]
			method, _ := cmd.Flags().GetString("method")
			path, _ := cmd.Flags().GetString("path")
			data, _ := cmd.Flags().GetString("data")
			if method != "GET" && method != "POST" {
				return failure(2, "usage", "--method must be GET or POST")
			}
			if !validInferencePath(path) {
				return failure(2, "usage", "--path must be an absolute service-relative path without traversal")
			}
			client, err := a.client()
			if err != nil {
				return err
			}
			var body any
			if data != "" {
				body = json.RawMessage(data)
			}
			result, err := client.request(cmd.Context(), method, svcProjectPath("/inference/"+svc+"/proxy"+path), body, "")
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), result, a.cloud.JSON)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "stop SERVICE", Short: "Stop an inference service", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "POST", svcProjectPath("/inference/"+args[0]+"/stop"), map[string]any{})
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "restart SERVICE", Short: "Restart re-releases the same workload; use stop then deploy",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return failure(4, "not_implemented", "inference restart reuses stop and a fresh deploy")
		},
	})
	return group
}

// --- Databases -------------------------------------------------------------------

func (a *app) databaseCommand() *cobra.Command {
	group := addProjectFlag(&cobra.Command{Use: "database", Short: "Managed databases (PostgreSQL)"})
	dbCreate := &cobra.Command{
		Use:   "create NAME",
		Short: "Create a managed database",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			engine, _ := cmd.Flags().GetString("engine")
			if engine == "" {
				return failure(2, "usage", "--engine is required (postgres)")
			}
			body := map[string]any{"name": args[0], "engine": engine}
			return a.serviceSend(cmd, "POST", svcProjectPath("/databases"), body)
		},
	}
	dbCreate.Flags().String("engine", "postgres", "Database engine")
	group.AddCommand(dbCreate)
	group.AddCommand(&cobra.Command{
		Use: "list", Short: "List databases", Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.serviceSend(cmd, "GET", svcProjectPath("/databases"), nil)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "inspect DB", Short: "Show a database and its logs", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "GET", svcProjectPath("/databases/"+args[0]), nil)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "logs DB", Short: "Read the database log tail", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "GET", svcProjectPath("/databases/"+args[0]), nil)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "stop DB", Short: "Stop a database", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "POST", svcProjectPath("/databases/"+args[0]+"/stop"), map[string]any{})
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "restart DB", Short: "Restart is stop + start; restart is not yet implemented",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return failure(4, "not_implemented", "database restart reuses stop and start; not yet wired")
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "backup DB [--output FILE]", Short: "Create a PostgreSQL dump of the database",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			output, _ := cmd.Flags().GetString("output")
			client, err := a.client()
			if err != nil {
				return err
			}
			// Use raw request to get binary dump
			data, err := client.rawRequest(cmd.Context(), "POST",
				svcProjectPath("/databases/"+args[0]+"/backup"), nil)
			if err != nil {
				return err
			}
			if output != "" {
				return os.WriteFile(output, data, 0600)
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "restore DB [--input FILE] [--force]", Short: "Restore a PostgreSQL dump into the database (defaults to isolated restore; --force overwrites existing)",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			input, _ := cmd.Flags().GetString("input")
			force, _ := cmd.Flags().GetBool("force")
			var data []byte
			var err error
			if input != "" {
				data, err = os.ReadFile(input)
			} else {
				data, err = io.ReadAll(cmd.InOrStdin())
			}
			if err != nil {
				return failure(1, "restore_failed", "cannot read the dump file")
			}
			client, err := a.client()
			if err != nil {
				return err
			}
			path := svcProjectPath("/databases/" + args[0] + "/restore")
			if force {
				path += "?force=true"
			}
			result, err := client.rawRequest(cmd.Context(), "POST", path, data)
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), result, a.cloud.JSON)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "delete DB", Short: "Stop and remove a database record", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "DELETE", svcProjectPath("/databases/"+args[0]), nil)
		},
	})
	return group
}

// --- Monitor / alerts ---------------------------------------------------------------

func (a *app) monitorCommand() *cobra.Command {
	group := addProjectFlag(&cobra.Command{Use: "monitor", Short: "Operational monitoring and alerts"})
	group.AddCommand(&cobra.Command{
		Use: "status", Short: "Queue, workers and recent placement decisions", Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.serviceSend(cmd, "GET", "/v1/scheduler/status", nil)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "events", Short: "Recent workload events", Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.serviceSend(cmd, "GET", "/v1/workload-events", nil)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "metrics", Short: "Prometheus metrics are exposed at /metrics by the coordinator",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.client()
			if err != nil {
				return err
			}
			raw, err := client.request(cmd.Context(), "GET", "/v1/scheduler/status", nil, "")
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), raw, a.cloud.JSON)
		},
	})
	alert := &cobra.Command{Use: "alert", Short: "Durable alert rules"}
	alert.AddCommand(&cobra.Command{
		Use: "list", Short: "List alert rules", Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.serviceSend(cmd, "GET", svcProjectPath("/alerts"), nil)
		},
	})
	alertCreate := &cobra.Command{
		Use:   "create",
		Short: "Create an alert rule",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			kind, _ := cmd.Flags().GetString("kind")
			target, _ := cmd.Flags().GetString("target")
			threshold, _ := cmd.Flags().GetInt("threshold")
			webhook, _ := cmd.Flags().GetString("webhook")
			body := map[string]any{"kind": kind, "target": target, "threshold": threshold, "webhook_url": webhook}
			return a.serviceSend(cmd, "POST", svcProjectPath("/alerts"), body)
		},
	}
	alertCreate.Flags().String("kind", "", "worker_offline, deployment_failed, workload_failed, storage_threshold, quota_exceeded or job_timeout")
	alertCreate.Flags().String("target", "", "Optional target identifier")
	alertCreate.Flags().Int("threshold", 0, "Optional threshold")
	alertCreate.Flags().String("webhook", "", "Optional https webhook URL")
	alert.AddCommand(alertCreate)
	group.AddCommand(alert)
	return group
}

// --- Drop -------------------------------------------------------------------------

func (a *app) dropCommand() *cobra.Command {
	group := addProjectFlag(&cobra.Command{Use: "drop", Short: "Secure file transfers"})
	group.AddCommand(&cobra.Command{
		Use: "send FILE --recipient USER_ID", Short: "Send a file to a principal",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return failure(2, "usage", "provide exactly one file")
			}
			recipient, _ := cmd.Flags().GetString("recipient")
			if recipient == "" {
				return failure(2, "usage", "--recipient is required")
			}
			file, err := os.Open(args[0])
			if err != nil {
				return failure(2, "invalid_file", "cannot open the file to send")
			}
			defer file.Close()
			data, err := io.ReadAll(file)
			if err != nil {
				return failure(1, "send_failed", "cannot read the file")
			}
			client, err := a.client()
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v1/drop/transfers?recipient=%s&filename=%s", recipient, urlQueryEscape(args[0]))
			result, err := client.rawRequest(cmd.Context(), "POST", path, data)
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), result, a.cloud.JSON)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "receive TRANSFER --output FILE", Short: "Download an approved transfer",
		RunE: func(cmd *cobra.Command, args []string) error {
			output, _ := cmd.Flags().GetString("output")
			if output == "" {
				return failure(2, "usage", "--output is required")
			}
			client, err := a.client()
			if err != nil {
				return err
			}
			data, err := client.rawRequest(cmd.Context(), "GET", "/v1/drop/transfers/"+args[0]+"/content", nil)
			if err != nil {
				return err
			}
			return os.WriteFile(output, data, 0600)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "list", Short: "List your transfers", Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.serviceSend(cmd, "GET", "/v1/drop/transfers", nil)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "inspect TRANSFER", Short: "Show a transfer", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "GET", "/v1/drop/transfers/"+args[0], nil)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "cancel TRANSFER", Short: "Cancel a transfer you sent", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.serviceSend(cmd, "POST", "/v1/drop/transfers/"+args[0]+"/cancel", map[string]any{})
		},
	})
	return group
}

// --- shared helpers -----------------------------------------------------------------

// addProjectFlag attaches the shared --project selector to a service tree.
func addProjectFlag(cmd *cobra.Command) *cobra.Command {
	cmd.PersistentFlags().StringVar(&svcProjectFlag, "project", "", "Project ID")
	return cmd
}

func urlQueryEscape(raw string) string {
	return url.QueryEscape(raw)
}

func (a *app) serviceSend(cmd *cobra.Command, method, path string, body any) error {
	result, err := a.sendQuiet(cmd, method, path, body)
	if err != nil {
		return err
	}
	return printJSON(cmd.OutOrStdout(), result, a.cloud.JSON)
}

var svcProjectFlag string

func svcProject() string { return svcProjectFlag }

func svcProjectSuffix() string {
	if svcProject() == "" {
		return ""
	}
	return "&project_id=" + svcProject()
}

func svcProjectPath(suffix string) string {
	if svcProject() == "" {
		// Without a project the request cannot be routed; the coordinator
		// answers with a clear authorization error.
		return "/v1/projects/UNSET" + suffix
	}
	return "/v1/projects/" + svcProject() + suffix
}

func validInferencePath(path string) bool {
	u, err := url.ParseRequestURI(path)
	if err != nil || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") || strings.Contains(u.Path, "\\") {
		return false
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}
