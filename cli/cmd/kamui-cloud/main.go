package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type app struct {
	cloud              cloudOptions
	url, ca, tokenFile string
	configPath         string
	saved              settings
}

type cliError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"status,omitempty"`
	exit    int
}

func (e *cliError) Error() string { return e.Message }

func failure(exit int, code, message string) error {
	return &cliError{Code: code, Message: message, exit: exit}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	os.Exit(execute(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func execute(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer) int {
	a := &app{}
	cmd := a.command()
	if args == nil {
		args = []string{}
	}
	cmd.SetArgs(args)
	cmd.SetIn(in)
	cmd.SetOut(out)
	cmd.SetErr(stderr)
	if err := cmd.ExecuteContext(ctx); err != nil {
		var detail *cliError
		switch {
		case ctx.Err() != nil:
			detail = &cliError{Code: "interrupted", Message: "interrupted", exit: 130}
		case errors.As(err, &detail):
		default:
			detail = &cliError{Code: "operation_failed", Message: err.Error(), exit: 1}
		}
		if a.cloud.JSON || jsonRequested(args) {
			_ = json.NewEncoder(stderr).Encode(map[string]any{"error": detail})
		} else {
			_, _ = fmt.Fprintf(stderr, "%s: %s\n", terminalText(detail.Code), terminalText(detail.Message))
		}
		return detail.exit
	}
	return 0
}

func jsonRequested(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--json" || arg == "--json=true" {
			return true
		}
	}
	return false
}

func (a *app) command() *cobra.Command {
	cmd := &cobra.Command{
		Use: "kamui-cloud", Short: "Manage a Kamui Cloud installation",
		SilenceErrors: true, SilenceUsage: true, Args: usageArgs(cobra.NoArgs),
		PersistentPreRunE: a.loadSettings,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.cloud.JSON {
				return failure(2, "usage", "select a command when using --json")
			}
			if !isTerminal(cmd.InOrStdin()) || !isTerminal(cmd.OutOrStdout()) {
				return cmd.Help()
			}
			return a.dashboard(cmd)
		},
	}
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return failure(2, "usage", err.Error()) })
	flags := cmd.PersistentFlags()
	flags.StringVar(&a.cloud.Root, "root", "", "Kamui checkout (defaults to the current directory or a parent)")
	flags.StringVar(&a.cloud.Context, "context", "", "Docker context for local lifecycle commands")
	flags.StringVar(&a.url, "url", "", "Coordinator URL")
	flags.StringVar(&a.ca, "ca-file", "", "PEM certificate authority for the coordinator")
	flags.StringVar(&a.tokenFile, "token-file", "", "File containing the coordinator bearer token")
	flags.BoolVar(&a.cloud.JSON, "json", false, "Print machine-readable output")
	a.cloud.VerifyStart = a.verifyLocalStart
	cloud := cloudCommand(&a.cloud)
	cloud.AddCommand(a.healthCommand())
	cmd.AddCommand(cloud, dockerCommand(&a.cloud), a.configCommand(), a.deviceCommand(), a.workloadCommand("job"), a.workloadCommand("service"), a.workspaceCommand())
	return cmd
}

func usageArgs(check cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := check(cmd, args); err != nil {
			return failure(2, "usage", err.Error())
		}
		return nil
	}
}

func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func terminalText(text string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || (r >= 32 && (r < 127 || r >= 160)) {
			return r
		}
		return '�'
	}, text)
}

type settings struct {
	URL       string `json:"url,omitempty"`
	CAFile    string `json:"ca_file,omitempty"`
	TokenFile string `json:"token_file,omitempty"`
	Root      string `json:"root,omitempty"`
	Context   string `json:"context,omitempty"`
}

func (a *app) loadSettings(cmd *cobra.Command, _ []string) error {
	a.configPath = os.Getenv("KAMUI_CONFIG")
	if a.configPath == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return failure(2, "config", "cannot locate the user configuration directory; set KAMUI_CONFIG")
		}
		a.configPath = filepath.Join(dir, "kamui-cloud", "config.json")
	}
	data, err := os.ReadFile(a.configPath)
	if err == nil {
		if err := json.Unmarshal(data, &a.saved); err != nil {
			return failure(2, "config", "invalid CLI configuration JSON")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return failure(2, "config", "cannot read the CLI configuration file")
	}
	values := []struct {
		flag, env, stored string
		dst               *string
	}{
		{"url", "KAMUI_URL", a.saved.URL, &a.url},
		{"ca-file", "KAMUI_CA_FILE", a.saved.CAFile, &a.ca},
		{"token-file", "KAMUI_TOKEN_FILE", a.saved.TokenFile, &a.tokenFile},
		{"root", "KAMUI_ROOT", a.saved.Root, &a.cloud.Root},
		{"context", "KAMUI_DOCKER_CONTEXT", a.saved.Context, &a.cloud.Context},
	}
	for _, v := range values {
		if !cmd.Flags().Changed(v.flag) {
			*v.dst = os.Getenv(v.env)
			if *v.dst == "" {
				*v.dst = v.stored
			}
		}
	}
	return nil
}

func (a *app) configCommand() *cobra.Command {
	group := &cobra.Command{Use: "config", Short: "Inspect or save CLI settings"}
	group.AddCommand(&cobra.Command{
		Use: "show", Short: "Show active settings without credentials", Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := settings{URL: a.url, CAFile: a.ca, TokenFile: a.tokenFile, Root: a.cloud.Root, Context: a.cloud.Context}
			if a.url == "" {
				endpoint, ca, _, err := localEndpoint(a.cloud.Root)
				if err == nil {
					info.URL = endpoint
					if info.CAFile == "" {
						info.CAFile = ca
					}
				}
			}
			if info.URL != "" {
				if _, err := endpointURL(info.URL); err != nil {
					return err
				}
			}
			return printJSON(cmd.OutOrStdout(), struct {
				settings
				ConfigFile string `json:"config_file"`
			}{info, a.configPath}, a.cloud.JSON)
		},
	})
	group.AddCommand(&cobra.Command{
		Use: "set", Short: "Persist --url, --ca-file, --token-file, --root or --context", Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			update := a.saved
			values := []struct {
				flag, value string
				dst         *string
			}{
				{"url", a.url, &update.URL}, {"ca-file", a.ca, &update.CAFile},
				{"token-file", a.tokenFile, &update.TokenFile}, {"root", a.cloud.Root, &update.Root},
				{"context", a.cloud.Context, &update.Context},
			}
			changed := false
			for _, v := range values {
				if !cmd.Flags().Changed(v.flag) {
					continue
				}
				*v.dst = v.value
				if v.value != "" && (v.flag == "root" || strings.HasSuffix(v.flag, "-file")) {
					absolute, err := filepath.Abs(v.value)
					if err != nil {
						return failure(2, "config", "cannot resolve settings path")
					}
					*v.dst = absolute
				}
				changed = true
			}
			if !changed {
				return failure(2, "usage", "provide at least one settings flag")
			}
			if update.URL != "" {
				if _, err := endpointURL(update.URL); err != nil {
					return err
				}
			}
			if err := writeSettings(a.configPath, update); err != nil {
				return failure(1, "config_write", "cannot save the CLI configuration file")
			}
			return printJSON(cmd.OutOrStdout(), map[string]string{"config_file": a.configPath}, a.cloud.JSON)
		},
	})
	return group
}

func writeSettings(path string, value settings) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".kamui-config-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	err = printJSON(file, value, false)
	if err := errors.Join(err, file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func printJSON(out io.Writer, value any, compact bool) error {
	enc := json.NewEncoder(out)
	if !compact {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(value)
}
