package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func (a *app) identityCommand() *cobra.Command {
	group := &cobra.Command{Use: "identity", Short: "Manage user accounts, projects, and team membership"}
	group.AddCommand(
		a.loginCommand(),
		a.logoutCommand(),
		a.whoamiCommand(),
		a.accountCommand(),
		a.projectCommand(),
		a.teamCommand(),
	)
	return group
}

func (a *app) loginCommand() *cobra.Command {
	var username, password, kind, label, projectID string
	var ttlSeconds int
	cmd := &cobra.Command{
		Use: "login", Short: "Authenticate and obtain a session token",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.client()
			if err != nil {
				return err
			}
			// Interactive prompts if not provided
			if username == "" {
				fmt.Fprint(cmd.OutOrStdout(), "Username: ")
				scanner := &inputScanner{reader: cmd.InOrStdin()}
				username, _ = scanner.readLine()
				username = strings.TrimSpace(username)
			}
			if password == "" {
				fmt.Fprint(cmd.OutOrStdout(), "Password: ")
				passBytes, _ := readPassword(cmd.InOrStdin())
				password = string(passBytes)
				fmt.Fprintln(cmd.OutOrStdout())
			}
			if username == "" || password == "" {
				return failure(2, "usage", "username and password are required")
			}
			if kind == "" {
				kind = "cli"
			}
			body := map[string]any{"username": username, "password": password, "kind": kind}
			if label != "" {
				body["label"] = label
			}
			if projectID != "" {
				body["project_id"] = projectID
			}
			if ttlSeconds > 0 {
				body["ttl_seconds"] = ttlSeconds
			}
			result, err := client.request(cmd.Context(), "POST", "/v1/auth/login", body, "")
			if err != nil {
				return err
			}
			var resp struct {
				SessionToken string `json:"session_token"`
				User         struct{ ID, Username string }
			}
			if err := json.Unmarshal(result, &resp); err != nil {
				return failure(6, "response", "invalid login response")
			}
			// Save token to config if using local stack
			if a.tokenFile == "" && a.url == "" {
				if err := a.saveSessionToken(resp.SessionToken); err != nil {
					return err
				}
			}
			if a.cloud.JSON {
				return printJSON(cmd.OutOrStdout(), result, true)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Logged in as %s (%s)\n", resp.User.Username, resp.User.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&username, "username", "", "Username")
	cmd.Flags().StringVar(&password, "password", "", "Password (omit to prompt)")
	cmd.Flags().StringVar(&kind, "kind", "", "Session kind: browser, cli, paired_device (default: cli)")
	cmd.Flags().StringVar(&label, "label", "", "Session label/device name")
	cmd.Flags().StringVar(&projectID, "project", "", "Initial active project ID")
	cmd.Flags().IntVar(&ttlSeconds, "ttl", 0, "Session TTL in seconds (default: 24h for cli, 8h for browser)")
	return cmd
}

func (a *app) saveSessionToken(token string) error {
	// Update config with the session token as the bearer token
	a.tokenFile = ""
	return a.saveSettings(settings{
		URL:       a.saved.URL,
		CAFile:    a.saved.CAFile,
		TokenFile: "",
		Root:      a.saved.Root,
		Context:   a.saved.Context,
	})
}

func (a *app) saveSettings(s settings) error {
	a.saved = s
	return writeSettings(a.configPath, s)
}

func (a *app) logoutCommand() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use: "logout", Short: "Revoke the current session or all sessions",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.client()
			if err != nil {
				return err
			}
			body := map[string]bool{"all": all}
			result, err := client.request(cmd.Context(), "POST", "/v1/auth/logout", body, "")
			if err != nil {
				return err
			}
			// Clear saved token if using local stack
			if a.tokenFile == "" && a.url == "" {
				_ = a.saveSettings(settings{
					URL: a.saved.URL, CAFile: a.saved.CAFile, Root: a.saved.Root, Context: a.saved.Context,
				})
			}
			if a.cloud.JSON {
				return printJSON(cmd.OutOrStdout(), result, true)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Logged out")
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Revoke all sessions for the current user")
	return cmd
}

func (a *app) whoamiCommand() *cobra.Command {
	return &cobra.Command{
		Use: "whoami", Short: "Show current authenticated user and active project",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.send(cmd, "GET", "/v1/auth/me", nil, "")
		},
	}
}

func (a *app) accountCommand() *cobra.Command {
	group := &cobra.Command{Use: "account", Short: "Manage your account settings"}
	group.AddCommand(
		&cobra.Command{
			Use: "settings", Short: "Show identity settings", Args: usageArgs(cobra.NoArgs),
			RunE: func(cmd *cobra.Command, _ []string) error {
				return a.send(cmd, "GET", "/v1/identity/settings", nil, "")
			},
		},
		&cobra.Command{
			Use: "sessions", Short: "List your active sessions", Args: usageArgs(cobra.NoArgs),
			RunE: func(cmd *cobra.Command, _ []string) error {
				return a.send(cmd, "GET", "/v1/auth/sessions", nil, "")
			},
		},
		&cobra.Command{
			Use: "revoke-session SESSION_ID", Short: "Revoke a specific session", Args: usageArgs(cobra.ExactArgs(1)),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.send(cmd, "DELETE", "/v1/auth/sessions/"+args[0], nil, "")
			},
		},
		&cobra.Command{
			Use: "github-status", Short: "Show GitHub connection status", Args: usageArgs(cobra.NoArgs),
			RunE: func(cmd *cobra.Command, _ []string) error {
				return a.send(cmd, "GET", "/v1/github/status", nil, "")
			},
		},
		&cobra.Command{
			Use: "github-repos", Short: "List accessible GitHub repositories", Args: usageArgs(cobra.NoArgs),
			RunE: func(cmd *cobra.Command, _ []string) error {
				return a.send(cmd, "GET", "/v1/github/repos", nil, "")
			},
		},
		&cobra.Command{
			Use: "github-connect", Short: "Start GitHub OAuth flow to connect your account", Args: usageArgs(cobra.NoArgs),
			RunE: func(cmd *cobra.Command, _ []string) error {
				client, err := a.client()
				if err != nil {
					return err
				}
				result, err := client.request(cmd.Context(), "POST", "/v1/github/oauth/start", map[string]string{"mode": "connect"}, "")
				if err != nil {
					return err
				}
				var resp struct {
					AuthorizeURL string `json:"authorize_url"`
					State        string `json:"state"`
				}
				if err := json.Unmarshal(result, &resp); err != nil {
					return failure(6, "response", "invalid oauth start response")
				}
				if a.cloud.JSON {
					return printJSON(cmd.OutOrStdout(), result, true)
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Open this URL in your browser:\n%s\n", resp.AuthorizeURL)
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "State: %s\n", resp.State)
				_, _ = fmt.Fprint(cmd.OutOrStdout(), "After authorizing, run: kamui-cloud identity github-exchange --code <code> --state <state>\n")
				return nil
			},
		},
		&cobra.Command{
			Use: "github-exchange", Short: "Complete GitHub OAuth flow", Args: usageArgs(cobra.NoArgs),
			RunE: func(cmd *cobra.Command, _ []string) error {
				var code, state string
				cmd.Flags().StringVar(&code, "code", "", "Authorization code from GitHub")
				cmd.Flags().StringVar(&state, "state", "", "State parameter from start")
				if err := cmd.Flags().Parse(os.Args[2:]); err != nil {
					return err
				}
				if code == "" || state == "" {
					return failure(2, "usage", "--code and --state are required")
				}
				client, err := a.client()
				if err != nil {
					return err
				}
				result, err := client.request(cmd.Context(), "POST", "/v1/github/oauth/exchange", map[string]string{"code": code, "state": state}, "")
				if err != nil {
					return err
				}
				return printJSON(cmd.OutOrStdout(), result, a.cloud.JSON)
			},
		},
		&cobra.Command{
			Use: "github-disconnect", Short: "Disconnect GitHub account", Args: usageArgs(cobra.NoArgs),
			RunE: func(cmd *cobra.Command, _ []string) error {
				return a.send(cmd, "DELETE", "/v1/github/connection", nil, "")
			},
		},
	)
	return group
}

func (a *app) projectCommand() *cobra.Command {
	group := &cobra.Command{Use: "project", Short: "Manage projects and memberships"}
	group.AddCommand(
		&cobra.Command{
			Use: "list", Short: "List your projects", Args: usageArgs(cobra.NoArgs),
			RunE: func(cmd *cobra.Command, _ []string) error {
				return a.send(cmd, "GET", "/v1/projects", nil, "")
			},
		},
		&cobra.Command{
			Use: "create NAME [DESCRIPTION]", Short: "Create a new project", Args: usageArgs(cobra.RangeArgs(1, 2)),
			RunE: func(cmd *cobra.Command, args []string) error {
				body := map[string]string{"name": args[0]}
				if len(args) > 1 {
					body["description"] = args[1]
				}
				return a.send(cmd, "POST", "/v1/projects", body, "")
			},
		},
		&cobra.Command{
			Use: "inspect PROJECT_ID", Short: "Show project details", Args: usageArgs(cobra.ExactArgs(1)),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.send(cmd, "GET", "/v1/projects/"+args[0], nil, "")
			},
		},
		&cobra.Command{
			Use: "delete PROJECT_ID", Short: "Delete a project (owner only)", Args: usageArgs(cobra.ExactArgs(1)),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.send(cmd, "DELETE", "/v1/projects/"+args[0], nil, "")
			},
		},
		&cobra.Command{
			Use: "members PROJECT_ID", Short: "List project members", Args: usageArgs(cobra.ExactArgs(1)),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.send(cmd, "GET", "/v1/projects/"+args[0]+"/members", nil, "")
			},
		},
		&cobra.Command{
			Use: "audit PROJECT_ID", Short: "List project audit events", Args: usageArgs(cobra.ExactArgs(1)),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.send(cmd, "GET", "/v1/projects/"+args[0]+"/audit-events", nil, "")
			},
		},
	)
	return group
}

func (a *app) teamCommand() *cobra.Command {
	group := &cobra.Command{Use: "team", Short: "Manage team invitations and membership"}
	group.AddCommand(
		&cobra.Command{
			Use: "invite PROJECT_ID", Short: "Invite a user to a project", Args: usageArgs(cobra.ExactArgs(1)),
			RunE: func(cmd *cobra.Command, args []string) error {
				var email, username, role string
				var ttlSeconds int
				cmd.Flags().StringVar(&email, "email", "", "Invitee email")
				cmd.Flags().StringVar(&username, "username", "", "Invitee username (if they already have an account)")
				cmd.Flags().StringVar(&role, "role", "developer", "Role: admin, developer, viewer")
				cmd.Flags().IntVar(&ttlSeconds, "ttl", 86400, "Invitation TTL in seconds (max 7 days)")
				if err := cmd.Flags().Parse(os.Args[3:]); err != nil {
					return err
				}
				if email == "" && username == "" {
					return failure(2, "usage", "either --email or --username is required")
				}
				body := map[string]any{"role": role, "ttl_seconds": ttlSeconds}
				if email != "" {
					body["email"] = email
				}
				if username != "" {
					body["username"] = username
				}
				return a.send(cmd, "POST", "/v1/projects/"+args[0]+"/invitations", body, "")
			},
		},
		&cobra.Command{
			Use: "invitations PROJECT_ID", Short: "List pending invitations for a project", Args: usageArgs(cobra.ExactArgs(1)),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.send(cmd, "GET", "/v1/projects/"+args[0]+"/invitations", nil, "")
			},
		},
		&cobra.Command{
			Use: "revoke-invitation PROJECT_ID INVITATION_ID", Short: "Revoke a pending invitation", Args: usageArgs(cobra.ExactArgs(2)),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.send(cmd, "DELETE", "/v1/projects/"+args[0]+"/invitations/"+args[1], nil, "")
			},
		},
		&cobra.Command{
			Use: "accept TOKEN", Short: "Accept an invitation (creates account if needed)", Args: usageArgs(cobra.ExactArgs(1)),
			RunE: func(cmd *cobra.Command, args []string) error {
				var username, password, displayName string
				cmd.Flags().StringVar(&username, "username", "", "Username for new account")
				cmd.Flags().StringVar(&password, "password", "", "Password for new account")
				cmd.Flags().StringVar(&displayName, "display-name", "", "Display name for new account")
				if err := cmd.Flags().Parse(os.Args[3:]); err != nil {
					return err
				}
				body := map[string]string{"token": args[0]}
				if username != "" {
					body["username"] = username
				}
				if password != "" {
					body["password"] = password
				}
				if displayName != "" {
					body["display_name"] = displayName
				}
				return a.send(cmd, "POST", "/v1/invitations/accept", body, "")
			},
		},
		&cobra.Command{
			Use: "set-role PROJECT_ID USER_ID ROLE", Short: "Set a member's role", Args: usageArgs(cobra.ExactArgs(3)),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.send(cmd, "PATCH", "/v1/projects/"+args[0]+"/members/"+args[1], map[string]string{"role": args[2]}, "")
			},
		},
		&cobra.Command{
			Use: "remove PROJECT_ID USER_ID", Short: "Remove a member from a project", Args: usageArgs(cobra.ExactArgs(2)),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.send(cmd, "DELETE", "/v1/projects/"+args[0]+"/members/"+args[1], nil, "")
			},
		},
	)
	return group
}

// inputScanner reads a single line from stdin
type inputScanner struct {
	reader io.Reader
}

func (s *inputScanner) readLine() (string, error) {
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 1)
	for {
		n, err := s.reader.Read(tmp)
		if err != nil || n == 0 {
			return string(buf), err
		}
		if tmp[0] == '\n' {
			break
		}
		buf = append(buf, tmp[0])
	}
	return string(buf), nil
}

func readPassword(reader io.Reader) ([]byte, error) {
	if f, ok := reader.(*os.File); ok {
		return term.ReadPassword(int(f.Fd()))
	}
	// Fallback for non-terminal input
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 1)
	for {
		n, err := reader.Read(tmp)
		if err != nil || n == 0 {
			return buf, err
		}
		if tmp[0] == '\n' {
			break
		}
		buf = append(buf, tmp[0])
	}
	return buf, nil
}
