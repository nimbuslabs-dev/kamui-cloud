package connect

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

type Endpoint struct {
	URL           string `json:"url"`
	Kind          string `json:"kind"` // localhost, lan, mdns, tailscale_direct, tailscale_relay, tunnel, manual
	InterfaceName string `json:"interface_name,omitempty"`
	Address       string `json:"address,omitempty"`
	Port          int    `json:"port,omitempty"`
	TLS           bool   `json:"tls"`
	Reachable     bool   `json:"reachable"`
	LatencyMs     int64  `json:"latency_ms,omitempty"`
	Hostname      string `json:"hostname,omitempty"`
	Note          string `json:"note,omitempty"`
}

func isVirtualInterface(name string) bool {
	lower := strings.ToLower(name)
	for _, prefix := range []string{"docker", "br-", "veth", "virbr", "cni", "flannel", "tunl", "lo"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func ClassifyIPv4(addr netip.Addr) (string, bool) {
	if !addr.Is4() {
		return "", false
	}
	if addr.IsLoopback() {
		return "localhost", true
	}
	if addr.IsLinkLocalUnicast() || addr.IsMulticast() || addr.IsUnspecified() {
		return "", false
	}
	// RFC 6598 CGNAT range 100.64.0.0/10 is used by Tailscale.
	tsPrefix := netip.MustParsePrefix("100.64.0.0/10")
	if tsPrefix.Contains(addr) {
		return "tailscale", true
	}
	if addr.IsPrivate() {
		return "lan", true
	}
	return "", false
}

func DiscoverLocalEndpoints(port int, scheme string) []Endpoint {
	if port <= 0 {
		port = 8443
	}
	if scheme == "" {
		scheme = "https"
	}
	endpoints := []Endpoint{
		{
			URL:           fmt.Sprintf("%s://127.0.0.1:%d", scheme, port),
			Kind:          "localhost",
			InterfaceName: "loopback",
			Address:       "127.0.0.1",
			Port:          port,
			TLS:           scheme == "https",
		},
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return endpoints
	}
	seen := map[string]bool{"127.0.0.1": true}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || isVirtualInterface(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			addr, ok := netip.AddrFromSlice(ipNet.IP)
			if !ok {
				continue
			}
			addr = addr.Unmap()
			kind, eligible := ClassifyIPv4(addr)
			if !eligible || kind == "localhost" || kind == "tailscale" {
				continue
			}
			ipStr := addr.String()
			if seen[ipStr] {
				continue
			}
			seen[ipStr] = true
			endpoints = append(endpoints, Endpoint{
				URL:           fmt.Sprintf("%s://%s:%d", scheme, ipStr, port),
				Kind:          kind,
				InterfaceName: iface.Name,
				Address:       ipStr,
				Port:          port,
				TLS:           scheme == "https",
			})
		}
	}
	return endpoints
}
