package connect

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
)

func openTestConnect(t *testing.T) (*core.Store, *identity.Store, *Store) {
	t.Helper()
	ctx := context.Background()
	cs, err := core.Open(ctx, filepath.Join(t.TempDir(), "core.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	id := identity.NewStore(cs.DB(), "master-secret-0123456789abcdef0123456", "admin-pass-0123456789abcdef012345")
	_ = id.EnsureAdminBootstrap(ctx)
	conn := NewStore(cs.DB(), id, "")
	return cs, id, conn
}

func TestClassifyAndMDNSAndQRCode(t *testing.T) {
	cases := []struct {
		ip   string
		kind string
		ok   bool
	}{
		{"127.0.0.1", "localhost", true},
		{"192.168.1.42", "lan", true},
		{"172.20.10.2", "lan", true},
		{"10.0.0.5", "lan", true},
		{"100.88.12.4", "tailscale", true},
		{"169.254.10.1", "", false},
		{"8.8.8.8", "", false},
	}
	for _, tc := range cases {
		kind, ok := ClassifyIPv4(netip.MustParseAddr(tc.ip))
		if kind != tc.kind || ok != tc.ok {
			t.Fatalf("ClassifyIPv4(%s) = (%q, %v), want (%q, %v)", tc.ip, kind, ok, tc.kind, tc.ok)
		}
	}

	q, err := BuildMDNSQueryPacket(MDNSServiceType)
	if err != nil || len(q) < 16 {
		t.Fatalf("query packet: %v", err)
	}
	resp, err := BuildMDNSResponsePacket(MDNSService{
		Instance: "kamui-local",
		Host:     "kamui-host",
		Address:  "192.168.1.25",
		Port:     8443,
		TXT:      map[string]string{"tls": "1", "mode": "personal"},
	})
	if err != nil {
		t.Fatal(err)
	}
	svcs, err := ParseMDNSResponse(resp)
	if err != nil || len(svcs) != 1 || svcs[0].Address != "192.168.1.25" || svcs[0].Port != 8443 || svcs[0].TXT["tls"] != "1" {
		t.Fatalf("parsed mDNS = %+v, %v", svcs, err)
	}

	mat, err := EncodeQR("kamui://pair?code=kpair_0123456789abcdef&endpoint=https://192.168.1.25:8081")
	if err != nil || len(mat) < 21 {
		t.Fatalf("EncodeQR failed: %v", err)
	}
	if term := RenderQRTerminal(mat); !strings.Contains(term, "█") {
		t.Fatalf("unexpected terminal QR output")
	}
	if svg := RenderQRSVG(mat); !strings.Contains(svg, "<svg") {
		t.Fatalf("unexpected SVG QR output")
	}
}

func TestPairingLifecycleExpiryApprovalRevocationAndNoWorkerEnrollment(t *testing.T) {
	ctx := context.Background()
	cs, id, conn := openTestConnect(t)
	now := time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC)
	id.Now = func() time.Time { return now }
	conn.Now = func() time.Time { return now }

	admin, err := id.GetUser(ctx, identity.LocalAdminID)
	if err != nil {
		t.Fatal(err)
	}
	p := identity.Principal{User: admin, ActiveProjectID: identity.DefaultProject}

	// Expired pairing code is rejected.
	expPair, err := conn.CreatePairing(ctx, p, CreatePairingInput{EndpointURL: "https://192.168.1.5:8081", TTLSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := conn.RedeemPairing(ctx, RedeemPairingInput{Code: expPair.Code, DeviceName: "Phone"}); !errors.Is(err, ErrPairingExpired) {
		t.Fatalf("expected ErrPairingExpired, got %v", err)
	}

	// Revoked pairing code is rejected.
	revPair, err := conn.CreatePairing(ctx, p, CreatePairingInput{EndpointURL: "https://192.168.1.5:8081", TTLSeconds: 120})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.RevokePairing(ctx, admin.ID, true, revPair.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.RedeemPairing(ctx, RedeemPairingInput{Code: revPair.Code, DeviceName: "Phone"}); !errors.Is(err, ErrPairingRevoked) {
		t.Fatalf("expected ErrPairingRevoked, got %v", err)
	}

	// Pending approval pairing blocks session until approved, then allows, then revocation blocks.
	pendingPair, err := conn.CreatePairing(ctx, p, CreatePairingInput{
		EndpointURL: "https://192.168.1.5:8081", TTLSeconds: 120, RequireApproval: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := conn.RedeemPairing(ctx, RedeemPairingInput{
		Code: pendingPair.Code, DeviceName: "iPhone 16", Platform: "ios", Transport: "lan",
	})
	if err != nil || res.Device.Status != "pending" || res.SessionToken == "" {
		t.Fatalf("redeem pending: %+v, %v", res, err)
	}
	// Single-use: second redeem must fail.
	if _, err := conn.RedeemPairing(ctx, RedeemPairingInput{Code: pendingPair.Code, DeviceName: "Replay"}); !errors.Is(err, ErrPairingUsed) {
		t.Fatalf("expected ErrPairingUsed, got %v", err)
	}
	// Pending device cannot authenticate yet.
	if _, err := id.AuthenticateToken(ctx, res.SessionToken); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("expected pending device token rejected, got %v", err)
	}
	// Approve device.
	approved, err := conn.ApproveDevice(ctx, admin.ID, true, res.Device.ID)
	if err != nil || approved.Status != "approved" {
		t.Fatalf("approve device: %+v, %v", approved, err)
	}
	principal, err := id.AuthenticateToken(ctx, res.SessionToken)
	if err != nil || principal.DeviceID != res.Device.ID {
		t.Fatalf("approved device auth: %+v, %v", principal, err)
	}
	// Verify pairing did NOT enroll any worker in core DB!
	var workerCount int
	if err := cs.DB().QueryRowContext(ctx, "SELECT count(*) FROM workers").Scan(&workerCount); err != nil || workerCount != 0 {
		t.Fatalf("device pairing unexpectedly modified workers table: count=%d err=%v", workerCount, err)
	}
	// Revoke device -> immediately revokes session.
	if err := conn.RevokeDevice(ctx, admin.ID, true, res.Device.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := id.AuthenticateToken(ctx, res.SessionToken); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("expected revoked device session rejected, got %v", err)
	}
}

func TestTailscaleProvidersAndEndpointSelection(t *testing.T) {
	ctx := context.Background()
	_, _, conn := openTestConnect(t)

	// Remote connectivity starts disabled.
	info, err := conn.Info(ctx, 8443, "https")
	if err != nil {
		t.Fatal(err)
	}
	if info.RemoteConfig.Enabled || info.RemoteStatus.Enabled {
		t.Fatalf("remote connectivity must be disabled by default: %+v", info)
	}

	// Mock Tailscale with direct peer.
	conn.SetTailscaleProvider(&TailscaleProvider{
		Runner: func(context.Context, ...string) ([]byte, error) {
			return []byte(`{
				"BackendState":"Running",
				"Self":{"DNSName":"kamui-mac.tail123.ts.net.","TailscaleIPs":["100.64.0.10"],"Online":true},
				"Peer":{"p1":{"Online":true,"CurAddr":"203.0.113.5:41641","Relay":"tok"}}
			}`), nil
		},
	})
	if _, err := conn.UpdateRemoteConfig(ctx, identity.LocalAdminID, RemoteConfig{Enabled: true, Provider: "tailscale"}); err != nil {
		t.Fatal(err)
	}
	info, err = conn.Info(ctx, 8443, "https")
	if err != nil {
		t.Fatal(err)
	}
	if !info.RemoteStatus.Available || info.RemoteStatus.Connectivity != "direct" || info.RemoteStatus.DNSName != "kamui-mac.tail123.ts.net" {
		t.Fatalf("unexpected direct tailscale status: %+v", info.RemoteStatus)
	}

	// Relayed peer only.
	conn.SetTailscaleProvider(&TailscaleProvider{
		Runner: func(context.Context, ...string) ([]byte, error) {
			return []byte(`{
				"BackendState":"Running",
				"Self":{"DNSName":"kamui-mac.tail123.ts.net.","TailscaleIPs":["100.64.0.10"],"Online":true,"Relay":"tok"},
				"Peer":{"p1":{"Online":true,"CurAddr":"","Relay":"tok"}}
			}`), nil
		},
	})
	info, err = conn.Info(ctx, 8443, "https")
	if err != nil {
		t.Fatal(err)
	}
	if info.RemoteStatus.Connectivity != "relayed" {
		t.Fatalf("expected relayed connectivity, got %+v", info.RemoteStatus)
	}

	// Selection prefers verified local endpoint over remote in auto mode.
	candidates := []Endpoint{
		{URL: "https://kamui-mac.tail123.ts.net:8443", Kind: "tailscale_direct", TLS: true},
		{URL: "https://192.168.1.50:8443", Kind: "lan", TLS: true},
	}
	probe := func(_ context.Context, u string) (int64, error) {
		if strings.Contains(u, "192.168.1.50") {
			return 12, nil
		}
		return 5, nil
	}
	best, _, err := SelectEndpoint(ctx, "auto", candidates, probe)
	if err != nil || best.Kind != "lan" {
		t.Fatalf("auto selection = %+v, %v", best, err)
	}
	bestRemote, _, err := SelectEndpoint(ctx, "remote", candidates, probe)
	if err != nil || bestRemote.Kind != "tailscale_direct" {
		t.Fatalf("remote selection = %+v, %v", bestRemote, err)
	}
}
