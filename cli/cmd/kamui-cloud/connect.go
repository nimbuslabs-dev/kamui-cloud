package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
)

func (a *app) connectCommand() *cobra.Command {
	group := &cobra.Command{Use: "connect", Short: "Manage client connectivity and device pairing"}
	group.AddCommand(
		a.connectInfoCommand(),
		a.connectAutoCommand(),
		a.connectLocalCommand(),
		a.connectRemoteSelectCommand(),
		a.pairCommand(),
		a.pairedDeviceCommand(),
		a.remoteCommand(),
	)
	return group
}

func (a *app) connectInfoCommand() *cobra.Command {
	return &cobra.Command{
		Use: "info", Short: "Show available connectivity endpoints and configuration", Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.send(cmd, "GET", "/v1/connect/info", nil, "")
		},
	}
}

func (a *app) connectAutoCommand() *cobra.Command {
	return &cobra.Command{
		Use: "auto", Short: "Automatically select the best available endpoint (local preferred)", Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.selectEndpoint(cmd, "auto")
		},
	}
}

func (a *app) connectLocalCommand() *cobra.Command {
	return &cobra.Command{
		Use: "local", Short: "Select a verified local (localhost/LAN/mDNS) endpoint", Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.selectEndpoint(cmd, "local")
		},
	}
}

func (a *app) connectRemoteSelectCommand() *cobra.Command {
	return &cobra.Command{
		Use: "select-remote", Short: "Select a remote (Tailscale/tunnel) endpoint", Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.selectEndpoint(cmd, "remote")
		},
	}
}

func (a *app) selectEndpoint(cmd *cobra.Command, mode string) error {
	client, err := a.client()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	// First get the connectivity info with candidates
	info, err := client.request(ctx, "GET", "/v1/connect/info?mdns=1", nil, "")
	if err != nil {
		return err
	}
	var connInfo struct {
		Candidates []struct {
			URL       string `json:"url"`
			Kind      string `json:"kind"`
			Address   string `json:"address,omitempty"`
			Port      int    `json:"port,omitempty"`
			TLS       bool   `json:"tls"`
			Hostname  string `json:"hostname,omitempty"`
			Reachable bool   `json:"reachable"`
			LatencyMs int64  `json:"latency_ms,omitempty"`
		} `json:"candidates"`
		CASHA256 string `json:"ca_sha256,omitempty"`
	}
	if err := json.Unmarshal(info, &connInfo); err != nil {
		return failure(6, "response", "invalid connectivity info response")
	}
	if len(connInfo.Candidates) == 0 {
		return failure(4, "no_endpoints", "no candidate endpoints available")
	}
	// Probe each candidate
	type candidate struct {
		URL       string `json:"url"`
		Kind      string `json:"kind"`
		Address   string `json:"address,omitempty"`
		Port      int    `json:"port,omitempty"`
		TLS       bool   `json:"tls"`
		Hostname  string `json:"hostname,omitempty"`
		Reachable bool   `json:"reachable"`
		LatencyMs int64  `json:"latency_ms,omitempty"`
	}
	type probeResult struct {
		url       string
		latency   int64
		reachable bool
		err       error
	}
	results := make(chan probeResult, len(connInfo.Candidates))
	for _, c := range connInfo.Candidates {
		go func(ep candidate) {
			start := time.Now()
			req, _ := http.NewRequestWithContext(ctx, "GET", ep.URL+"/healthz", nil)
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.Proxy = nil
			probeClient := &http.Client{Transport: tr, Timeout: 3 * time.Second}
			resp, err := probeClient.Do(req)
			lat := time.Since(start).Milliseconds()
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					results <- probeResult{ep.URL, lat, true, nil}
					return
				}
			}
			results <- probeResult{ep.URL, lat, false, err}
		}(c)
	}
	probed := make([]probeResult, 0, len(connInfo.Candidates))
	for range connInfo.Candidates {
		probed = append(probed, <-results)
	}
	close(results)
	// Select based on mode and priority
	var best *probeResult
	bestPrio := 999
	for i := range probed {
		if !probed[i].reachable {
			continue
		}
		var kind string
		for _, c := range connInfo.Candidates {
			if c.URL == probed[i].url {
				kind = c.Kind
				break
			}
		}
		p := priority(kind)
		switch mode {
		case "local":
			if !isLocal(kind) {
				continue
			}
		case "remote":
			if !isRemote(kind) {
				continue
			}
		case "auto":
		default:
			return failure(2, "usage", "invalid mode")
		}
		if best == nil || p < bestPrio || (p == bestPrio && probed[i].latency < best.latency) {
			best = &probed[i]
			bestPrio = p
		}
	}
	if best == nil {
		return failure(4, "no_reachable", fmt.Sprintf("no reachable endpoint found for mode %s", mode))
	}
	if a.cloud.JSON {
		return printJSON(cmd.OutOrStdout(), map[string]any{
			"selected": best.url,
			"mode":     mode,
			"latency":  best.latency,
		}, true)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Selected endpoint: %s (mode: %s, latency: %dms)\n", best.url, mode, best.latency)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Use --url %s to target this endpoint\n", best.url)
	return nil
}

func isLocal(kind string) bool {
	return kind == "localhost" || kind == "lan" || kind == "mdns"
}

func isRemote(kind string) bool {
	return kind == "tailscale_direct" || kind == "tailscale_relay" || kind == "tunnel"
}

func priority(kind string) int {
	switch kind {
	case "localhost":
		return 1
	case "lan", "mdns":
		return 2
	case "tailscale_direct":
		return 3
	case "tailscale_relay":
		return 4
	case "tunnel":
		return 5
	default:
		return 6
	}
}

func (a *app) pairCommand() *cobra.Command {
	var endpoint, deviceName, projectID string
	var ttlSeconds int
	var requireApproval bool
	cmd := &cobra.Command{
		Use: "pair", Short: "Create a pairing QR code for a new client device", Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.client()
			if err != nil {
				return err
			}
			body := map[string]any{}
			if endpoint != "" {
				body["endpoint_url"] = endpoint
			}
			if deviceName != "" {
				body["device_name"] = deviceName
			}
			if projectID != "" {
				body["project_id"] = projectID
			}
			if ttlSeconds > 0 {
				body["ttl_seconds"] = ttlSeconds
			}
			body["require_approval"] = requireApproval
			result, err := client.request(cmd.Context(), "POST", "/v1/connect/pairings", body, "")
			if err != nil {
				return err
			}
			var resp struct {
				ID              string `json:"id"`
				Code            string `json:"code"`
				PairingURI      string `json:"pairing_uri"`
				QRSVG           string `json:"qr_svg,omitempty"`
				QRTerminal      string `json:"qr_terminal,omitempty"`
				EndpointURL     string `json:"endpoint_url"`
				RequireApproval bool   `json:"require_approval"`
				ExpiresAt       string `json:"expires_at"`
			}
			if err := json.Unmarshal(result, &resp); err != nil {
				return failure(6, "response", "invalid pairing response")
			}
			if a.cloud.JSON {
				return printJSON(cmd.OutOrStdout(), result, true)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Pairing created: %s\n", resp.ID)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Code: %s\n", resp.Code)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Expires: %s\n", resp.ExpiresAt)
			if resp.QRTerminal != "" {
				_, _ = fmt.Fprint(cmd.OutOrStdout(), "\nQR Code (scan with Kamui mobile app):\n")
				_, _ = fmt.Fprint(cmd.OutOrStdout(), resp.QRTerminal)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nPairing URI: %s\n", resp.PairingURI)
			return nil
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "Endpoint URL to pair (auto-detected if omitted)")
	cmd.Flags().StringVar(&deviceName, "device-name", "", "Name for the paired device")
	cmd.Flags().StringVar(&projectID, "project", "", "Project ID (defaults to active project)")
	cmd.Flags().IntVar(&ttlSeconds, "ttl", 300, "TTL in seconds (10-900)")
	cmd.Flags().BoolVar(&requireApproval, "require-approval", false, "Require manual approval before device can connect")
	return cmd
}

func (a *app) pairedDeviceCommand() *cobra.Command {
	group := &cobra.Command{Use: "device", Short: "Manage paired client devices"}
	group.AddCommand(
		&cobra.Command{
			Use: "list", Short: "List paired devices", Args: usageArgs(cobra.NoArgs),
			RunE: func(cmd *cobra.Command, _ []string) error {
				return a.send(cmd, "GET", "/v1/connect/devices", nil, "")
			},
		},
		&cobra.Command{
			Use: "approve DEVICE_ID", Short: "Approve a pending device", Args: usageArgs(cobra.ExactArgs(1)),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.send(cmd, "POST", "/v1/connect/devices/"+args[0]+"/approve", nil, "")
			},
		},
		&cobra.Command{
			Use: "revoke DEVICE_ID", Short: "Revoke a paired device", Args: usageArgs(cobra.ExactArgs(1)),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.send(cmd, "POST", "/v1/connect/devices/"+args[0]+"/revoke", nil, "")
			},
		},
	)
	return group
}

func (a *app) remoteCommand() *cobra.Command {
	group := &cobra.Command{Use: "remote", Short: "Manage remote connectivity (Tailscale/tunnel)"}
	group.AddCommand(
		&cobra.Command{
			Use: "status", Short: "Show remote connectivity status", Args: usageArgs(cobra.NoArgs),
			RunE: func(cmd *cobra.Command, _ []string) error {
				return a.send(cmd, "GET", "/v1/connect/remote", nil, "")
			},
		},
		&cobra.Command{
			Use: "enable", Short: "Enable remote connectivity", Args: usageArgs(cobra.NoArgs),
			RunE: func(cmd *cobra.Command, _ []string) error {
				var provider, tailscaleHostname, tunnelURL, tunnelProvider string
				cmd.Flags().StringVar(&provider, "provider", "tailscale", "Provider: tailscale or tunnel")
				cmd.Flags().StringVar(&tailscaleHostname, "tailscale-hostname", "", "Tailscale MagicDNS hostname (e.g., my-host.tail123.ts.net)")
				cmd.Flags().StringVar(&tunnelURL, "tunnel-url", "", "Authenticated tunnel URL (https://...)")
				cmd.Flags().StringVar(&tunnelProvider, "tunnel-provider", "", "Tunnel provider name")
				if err := cmd.Flags().Parse(os.Args[3:]); err != nil {
					return err
				}
				if provider != "tailscale" && provider != "tunnel" {
					return failure(2, "usage", "provider must be tailscale or tunnel")
				}
				body := map[string]any{"enabled": true, "provider": provider}
				if tailscaleHostname != "" {
					body["tailscale_hostname"] = tailscaleHostname
				}
				if tunnelURL != "" {
					body["tunnel_url"] = tunnelURL
				}
				if tunnelProvider != "" {
					body["tunnel_provider"] = tunnelProvider
				}
				return a.send(cmd, "PUT", "/v1/connect/remote", body, "")
			},
		},
		&cobra.Command{
			Use: "disable", Short: "Disable remote connectivity", Args: usageArgs(cobra.NoArgs),
			RunE: func(cmd *cobra.Command, _ []string) error {
				return a.send(cmd, "DELETE", "/v1/connect/remote", nil, "")
			},
		},
	)
	return group
}
