package main

import (
	"regexp"

	"github.com/spf13/cobra"
)

const workspacesPath = "/v1/workspaces"

var workspaceID = regexp.MustCompile(`^ws-[0-9a-f]{8,32}$`)

func (a *app) workspaceCommand() *cobra.Command {
	group := &cobra.Command{Use: "workspace", Short: "Manage Workspace containers and Git operations"}
	group.AddCommand(&cobra.Command{
		Use: "list", Short: "List workspaces", Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.send(cmd, "GET", workspacesPath, nil, "")
		},
	})
	var branch string
	create := &cobra.Command{
		Use: "create NAME REPOSITORY", Short: "Create a workspace from a GitHub HTTPS repository",
		Args: usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.send(cmd, "POST", workspacesPath, map[string]string{"name": args[0], "repository_url": args[1], "branch": branch}, "")
		},
	}
	create.Flags().StringVar(&branch, "branch", "", "Git branch to check out")
	group.AddCommand(create)
	for _, op := range []struct{ use, short, method, suffix string }{
		{"inspect", "Inspect workspace status", "GET", ""},
		{"start", "Start the workspace", "POST", "/start"},
		{"stop", "Stop the workspace and retain its volume", "POST", "/stop"},
		{"restart", "Restart the workspace", "POST", "/restart"},
		{"delete", "Remove the workspace record and container; retain its volume", "DELETE", ""},
	} {
		group.AddCommand(&cobra.Command{
			Use: op.use + " ID", Short: op.short, Args: usageArgs(cobra.ExactArgs(1)),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.workspaceAction(cmd, args[0], op.method, op.suffix, nil)
			},
		})
	}
	group.AddCommand(a.workspaceGitCommand())
	return group
}

func (a *app) workspaceAction(cmd *cobra.Command, id, method, suffix string, input any) error {
	if !workspaceID.MatchString(id) {
		return failure(2, "invalid_id", "provide a valid workspace ID")
	}
	return a.send(cmd, method, workspacesPath+"/"+id+suffix, input, "")
}

func (a *app) workspaceGitCommand() *cobra.Command {
	group := &cobra.Command{Use: "git", Short: "Manage Git in a running workspace"}
	group.AddCommand(
		&cobra.Command{
			Use: "status ID", Short: "Show branch, divergence and changed files", Args: usageArgs(cobra.ExactArgs(1)),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.workspaceAction(cmd, args[0], "GET", "/git/status", nil)
			},
		},
		&cobra.Command{
			Use: "add ID FILE...", Short: "Stage the specified repository-relative files", Args: usageArgs(cobra.MinimumNArgs(2)),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.workspaceAction(cmd, args[0], "POST", "/git/add", map[string][]string{"files": args[1:]})
			},
		},
		&cobra.Command{
			Use: "branch ID BRANCH", Short: "Create or switch to a Git branch", Args: usageArgs(cobra.ExactArgs(2)),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.workspaceAction(cmd, args[0], "POST", "/git/branch", map[string]string{"branch": args[1]})
			},
		},
	)
	var message string
	commit := &cobra.Command{
		Use: "commit ID", Short: "Commit staged files", Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.workspaceAction(cmd, args[0], "POST", "/git/commit", map[string]string{"message": message})
		},
	}
	commit.Flags().StringVarP(&message, "message", "m", "", "Commit message")
	_ = commit.MarkFlagRequired("message")
	group.AddCommand(commit)
	for _, action := range []string{"push", "pull"} {
		var remote, branch string
		command := &cobra.Command{
			Use: action + " ID", Short: "Git " + action + " for a running workspace", Args: usageArgs(cobra.ExactArgs(1)),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.workspaceAction(cmd, args[0], "POST", "/git/"+action, map[string]string{"remote": remote, "branch": branch})
			},
		}
		command.Flags().StringVar(&remote, "remote", "origin", "Git remote")
		command.Flags().StringVar(&branch, "branch", "", "Git branch (defaults to the current branch)")
		group.AddCommand(command)
	}
	return group
}
