package connect

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"net/url"
	"os/exec"
	"strings"
)

type RemoteStatus struct {
	Provider     string     `json:"provider"`
	Installed    bool       `json:"installed"`
	Available    bool       `json:"available"`
	Enabled      bool       `json:"enabled"`
	State        string     `json:"state"`
	Connectivity string     `json:"connectivity,omitempty"` // direct, relayed, unavailable
	TailscaleIPs []string   `json:"tailscale_ips,omitempty"`
	DNSName      string     `json:"dns_name,omitempty"`
	DirectPeers  int        `json:"direct_peers"`
	RelayedPeers int        `json:"relayed_peers"`
	TunnelURL    string     `json:"tunnel_url,omitempty"`
	Endpoints    []Endpoint `json:"endpoints,omitempty"`
	Message      string     `json:"message,omitempty"`
}

type RemoteProvider interface {
	Name() string
	Status(ctx context.Context, cfg RemoteConfig, port int) RemoteStatus
}

type RemoteConfig struct {
	Enabled           bool   `json:"enabled"`
	Provider          string `json:"provider"` // none, tailscale, tunnel
	TailscaleHostname string `json:"tailscale_hostname,omitempty"`
	TunnelURL         string `json:"tunnel_url,omitempty"`
	TunnelProvider    string `json:"tunnel_provider,omitempty"`
}

type TailscaleProvider struct {
	Runner func(ctx context.Context, args ...string) ([]byte, error)
}

func NewTailscaleProvider() *TailscaleProvider {
	return &TailscaleProvider{
		Runner: func(ctx context.Context, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, "tailscale", args...).Output()
		},
	}
}

func (p *TailscaleProvider) Name() string { return "tailscale" }

func (p *TailscaleProvider) Status(ctx context.Context, cfg RemoteConfig, port int) RemoteStatus {
	out := RemoteStatus{
		Provider: "tailscale",
		Enabled:  cfg.Enabled && cfg.Provider == "tailscale",
		State:    "disabled",
	}
	if !out.Enabled {
		out.Message = "Remote Tailscale connectivity is disabled until explicitly configured and authorized."
		return out
	}
	raw, err := p.Runner(ctx, "status", "--json")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			out.Installed = false
			out.State = "unavailable"
			out.Connectivity = "unavailable"
			out.Message = "tailscale CLI/daemon is not installed on this host"
			return out
		}
		out.Installed = true
		out.State = "unavailable"
		out.Connectivity = "unavailable"
		out.Message = "tailscale daemon is not running or not authenticated"
		return out
	}
	out.Installed = true
	var ts struct {
		BackendState string `json:"BackendState"`
		Self         struct {
			DNSName      string   `json:"DNSName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
			Online       bool     `json:"Online"`
			Relay        string   `json:"Relay"`
		} `json:"Self"`
		Peer map[string]struct {
			Online  bool   `json:"Online"`
			CurAddr string `json:"CurAddr"`
			Relay   string `json:"Relay"`
		} `json:"Peer"`
	}
	if err := json.Unmarshal(raw, &ts); err != nil {
		out.State = "error"
		out.Connectivity = "unavailable"
		out.Message = "invalid tailscale status response"
		return out
	}
	out.State = strings.ToLower(ts.BackendState)
	out.DNSName = strings.TrimSuffix(ts.Self.DNSName, ".")
	out.TailscaleIPs = ts.Self.TailscaleIPs
	for _, peer := range ts.Peer {
		if !peer.Online {
			continue
		}
		if peer.CurAddr != "" {
			out.DirectPeers++
		} else if peer.Relay != "" {
			out.RelayedPeers++
		}
	}
	if ts.BackendState == "Running" && ts.Self.Online {
		out.Available = true
		kind := "tailscale_relay"
		out.Connectivity = "relayed"
		if out.DirectPeers > 0 || (ts.Self.Relay == "" && len(ts.Peer) == 0) {
			kind = "tailscale_direct"
			out.Connectivity = "direct"
		}
		if port <= 0 {
			port = 8443
		}
		if out.DNSName != "" {
			out.Endpoints = append(out.Endpoints, Endpoint{
				URL:      "https://" + out.DNSName + ":" + itoa(port),
				Kind:     kind,
				Hostname: out.DNSName,
				Port:     port,
				TLS:      true,
			})
		}
		for _, ipStr := range out.TailscaleIPs {
			addr, err := netip.ParseAddr(ipStr)
			if err == nil && addr.Is4() {
				out.Endpoints = append(out.Endpoints, Endpoint{
					URL:     "https://" + addr.String() + ":" + itoa(port),
					Kind:    kind,
					Address: addr.String(),
					Port:    port,
					TLS:     true,
				})
			}
		}
	} else {
		out.Connectivity = "unavailable"
		out.Message = "tailscale backend state is " + ts.BackendState
	}
	return out
}

type TunnelProvider struct{}

func (p *TunnelProvider) Name() string { return "tunnel" }

func (p *TunnelProvider) Status(_ context.Context, cfg RemoteConfig, _ int) RemoteStatus {
	out := RemoteStatus{
		Provider: "tunnel",
		Enabled:  cfg.Enabled && cfg.Provider == "tunnel",
		State:    "disabled",
	}
	if !out.Enabled {
		out.Message = "Authenticated browser-access tunnel is disabled until explicitly configured and authorized."
		return out
	}
	if err := ValidateTunnelURL(cfg.TunnelURL); err != nil {
		out.State = "misconfigured"
		out.Connectivity = "unavailable"
		out.Message = err.Error()
		return out
	}
	out.Installed = true
	out.Available = true
	out.State = "configured"
	out.Connectivity = "relayed"
	out.TunnelURL = strings.TrimRight(cfg.TunnelURL, "/")
	out.Endpoints = []Endpoint{{
		URL:  out.TunnelURL,
		Kind: "tunnel",
		TLS:  true,
		Note: "Authenticated browser-access tunnel (" + fallback(cfg.TunnelProvider, "https") + ")",
	}}
	return out
}

func ValidateTunnelURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("tunnel URL must be an https:// origin with no embedded credentials, query, or fragment")
	}
	return nil
}

func fallback(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
