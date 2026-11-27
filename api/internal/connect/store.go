package connect

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
)

var (
	ErrNotFound       = errors.New("not found")
	ErrForbidden      = errors.New("forbidden")
	ErrBadInput       = errors.New("invalid input")
	ErrPairingExpired = errors.New("pairing credential expired")
	ErrPairingUsed    = errors.New("pairing credential already used")
	ErrPairingRevoked = errors.New("pairing credential revoked")
)

type PairingChallenge struct {
	ID              string    `json:"id"`
	UserID          string    `json:"user_id"`
	ProjectID       string    `json:"project_id"`
	Code            string    `json:"code"`
	EndpointURL     string    `json:"endpoint_url"`
	CASHA256        string    `json:"ca_sha256,omitempty"`
	PairingURI      string    `json:"pairing_uri"`
	QRSVG           string    `json:"qr_svg,omitempty"`
	QRTerminal      string    `json:"qr_terminal,omitempty"`
	RequireApproval bool      `json:"require_approval"`
	ExpiresAt       time.Time `json:"expires_at"`
	CreatedAt       time.Time `json:"created_at"`
}

type PairedDevice struct {
	ID            string     `json:"id"`
	UserID        string     `json:"user_id"`
	Name          string     `json:"name"`
	Platform      string     `json:"platform,omitempty"`
	Status        string     `json:"status"` // pending, approved, revoked
	SessionID     string     `json:"session_id,omitempty"`
	LastEndpoint  string     `json:"last_endpoint,omitempty"`
	LastTransport string     `json:"last_transport,omitempty"`
	ApprovedBy    string     `json:"approved_by,omitempty"`
	ApprovedAt    *time.Time `json:"approved_at,omitempty"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	LastSeenAt    time.Time  `json:"last_seen_at"`
	CreatedAt     time.Time  `json:"created_at"`
}

type ConnectivityInfo struct {
	CloudMode       string       `json:"cloud_mode"`
	MDNSServiceType string       `json:"mdns_service_type"`
	LocalEndpoints  []Endpoint   `json:"local_endpoints"`
	RemoteConfig    RemoteConfig `json:"remote_config"`
	RemoteStatus    RemoteStatus `json:"remote_status"`
	Candidates      []Endpoint   `json:"candidates"`
	CASHA256        string       `json:"ca_sha256,omitempty"`
}

type Store struct {
	db        *sql.DB
	id        *identity.Store
	tailscale RemoteProvider
	tunnel    RemoteProvider
	caPath    string
	Now       func() time.Time
}

func NewStore(db *sql.DB, id *identity.Store, caPath string) *Store {
	return &Store{
		db:        db,
		id:        id,
		tailscale: NewTailscaleProvider(),
		tunnel:    &TunnelProvider{},
		caPath:    caPath,
		Now:       func() time.Time { return time.Now().UTC() },
	}
}

func (s *Store) SetTailscaleProvider(p RemoteProvider) {
	s.tailscale = p
}

func (s *Store) now() time.Time { return s.Now().UTC() }

func (s *Store) CAFingeprint() string {
	if s.caPath == "" {
		return ""
	}
	raw, err := os.ReadFile(s.caPath)
	if err != nil {
		return ""
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return ""
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

type CreatePairingInput struct {
	EndpointURL     string `json:"endpoint_url"`
	DeviceName      string `json:"device_name"`
	ProjectID       string `json:"project_id"`
	TTLSeconds      int    `json:"ttl_seconds"`
	RequireApproval bool   `json:"require_approval"`
}

func (s *Store) CreatePairing(ctx context.Context, p identity.Principal, in CreatePairingInput) (PairingChallenge, error) {
	projectID := in.ProjectID
	if projectID == "" {
		projectID = p.ActiveProjectID
	}
	if projectID == "" {
		projectID = identity.DefaultProject
	}
	if _, err := s.id.Authorize(ctx, p, projectID, identity.PermProjectRead); err != nil {
		return PairingChallenge{}, ErrForbidden
	}
	ttl := time.Duration(in.TTLSeconds) * time.Second
	if ttl == 0 {
		ttl = 5 * time.Minute
	}
	if ttl < 10*time.Second || ttl > 15*time.Minute {
		return PairingChallenge{}, fmt.Errorf("%w: ttl_seconds must be between 10 and 900", ErrBadInput)
	}
	endpoint := strings.TrimRight(strings.TrimSpace(in.EndpointURL), "/")
	if endpoint != "" {
		if err := identity.ValidateHTTPSOrLoopbackURL(endpoint); err != nil {
			return PairingChallenge{}, fmt.Errorf("%w: endpoint_url: %v", ErrBadInput, err)
		}
	}
	id, err := identity.NewSecretToken("pair-", 12)
	if err != nil {
		return PairingChallenge{}, err
	}
	code, err := identity.NewSecretToken("kpair_", 24)
	if err != nil {
		return PairingChallenge{}, err
	}
	now := s.now()
	expires := now.Add(ttl)
	reqApproval := 0
	if in.RequireApproval {
		reqApproval = 1
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO pairing_tokens(id, user_id, project_id, code_hash, endpoint_url, device_name, require_approval, expires_at, created_at)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		id, p.User.ID, projectID, identity.HashToken(code), endpoint, strings.TrimSpace(in.DeviceName), reqApproval, expires.UnixNano(), now.UnixNano(),
	)
	if err != nil {
		return PairingChallenge{}, err
	}
	_ = s.id.RecordAudit(ctx, p.User.ID, projectID, "device.pair_create", "pairing", id, "success", endpoint)

	fp := s.CAFingeprint()
	uri := fmt.Sprintf("kamui://pair?code=%s", code)
	if endpoint != "" {
		uri += "&endpoint=" + endpoint
	}
	if fp != "" {
		uri += "&ca=" + fp[:16]
	}
	var qrSVG, qrTerm string
	if mat, err := EncodeQR(uri); err == nil {
		qrSVG = RenderQRSVG(mat)
		qrTerm = RenderQRTerminal(mat)
	}
	return PairingChallenge{
		ID: id, UserID: p.User.ID, ProjectID: projectID, Code: code,
		EndpointURL: endpoint, CASHA256: fp, PairingURI: uri,
		QRSVG: qrSVG, QRTerminal: qrTerm,
		RequireApproval: in.RequireApproval, ExpiresAt: expires, CreatedAt: now,
	}, nil
}

type RedeemPairingInput struct {
	Code       string `json:"code"`
	DeviceName string `json:"device_name"`
	Platform   string `json:"platform"`
	Endpoint   string `json:"endpoint"`
	Transport  string `json:"transport"`
}

type RedeemPairingResult struct {
	Device       PairedDevice `json:"device"`
	SessionToken string       `json:"session_token"`
	ExpiresAt    time.Time    `json:"expires_at"`
}

func (s *Store) RedeemPairing(ctx context.Context, in RedeemPairingInput) (RedeemPairingResult, error) {
	code := strings.TrimSpace(in.Code)
	if code == "" {
		return RedeemPairingResult{}, fmt.Errorf("%w: pairing code is required", ErrBadInput)
	}
	name := strings.TrimSpace(in.DeviceName)
	if name == "" {
		name = "Paired Client"
	}
	if len(name) > 100 || strings.ContainsRune(name, 0) {
		return RedeemPairingResult{}, fmt.Errorf("%w: invalid device_name", ErrBadInput)
	}
	now := s.now()
	ts := now.UnixNano()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RedeemPairingResult{}, err
	}
	defer tx.Rollback()

	var pairID, userID, projectID, endpointURL string
	var reqApproval int
	var expiresAt int64
	var usedAt, revokedAt sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT id, user_id, project_id, endpoint_url, require_approval, expires_at, used_at, revoked_at
		 FROM pairing_tokens WHERE code_hash=?`,
		identity.HashToken(code),
	).Scan(&pairID, &userID, &projectID, &endpointURL, &reqApproval, &expiresAt, &usedAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RedeemPairingResult{}, ErrNotFound
	}
	if err != nil {
		return RedeemPairingResult{}, err
	}
	if revokedAt.Valid {
		return RedeemPairingResult{}, ErrPairingRevoked
	}
	if usedAt.Valid {
		return RedeemPairingResult{}, ErrPairingUsed
	}
	if ts >= expiresAt {
		return RedeemPairingResult{}, ErrPairingExpired
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE pairing_tokens SET used_at=? WHERE id=? AND used_at IS NULL AND revoked_at IS NULL AND expires_at>?`,
		ts, pairID, ts,
	)
	if err != nil {
		return RedeemPairingResult{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return RedeemPairingResult{}, ErrPairingUsed
	}

	devID, err := identity.NewSecretToken("dev-", 12)
	if err != nil {
		return RedeemPairingResult{}, err
	}
	status := "approved"
	var approvedBy string
	var approvedAt *time.Time
	var approvedNS sql.NullInt64
	if reqApproval != 0 {
		status = "pending"
	} else {
		approvedBy = userID
		approvedAt = &now
		approvedNS = sql.NullInt64{Int64: ts, Valid: true}
	}
	lastEndpoint := strings.TrimSpace(in.Endpoint)
	if lastEndpoint == "" {
		lastEndpoint = endpointURL
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO paired_devices(id, user_id, name, platform, status, last_endpoint, last_transport, approved_by, approved_at, last_seen_at, created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		devID, userID, name, strings.TrimSpace(in.Platform), status, lastEndpoint, strings.TrimSpace(in.Transport), approvedBy, approvedNS, ts, ts,
	); err != nil {
		return RedeemPairingResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return RedeemPairingResult{}, err
	}

	sess, token, err := s.id.IssueSession(ctx, userID, identity.SessionPairedDevice, name, devID, projectID, 7*24*time.Hour)
	if err != nil {
		return RedeemPairingResult{}, err
	}
	_, _ = s.db.ExecContext(ctx, "UPDATE paired_devices SET session_id=? WHERE id=?", sess.ID, devID)
	_ = s.id.RecordAudit(ctx, userID, projectID, "device.pair_complete", "device", devID, "success", status)

	return RedeemPairingResult{
		Device: PairedDevice{
			ID: devID, UserID: userID, Name: name, Platform: strings.TrimSpace(in.Platform),
			Status: status, SessionID: sess.ID, LastEndpoint: lastEndpoint, LastTransport: strings.TrimSpace(in.Transport),
			ApprovedBy: approvedBy, ApprovedAt: approvedAt, LastSeenAt: now, CreatedAt: now,
		},
		SessionToken: token,
		ExpiresAt:    sess.ExpiresAt,
	}, nil
}

func (s *Store) RevokePairing(ctx context.Context, actorID string, isAdmin bool, pairID string) error {
	now := s.now().UnixNano()
	query := "UPDATE pairing_tokens SET revoked_at=? WHERE id=? AND revoked_at IS NULL"
	args := []any{now, pairID}
	if !isAdmin {
		query += " AND user_id=?"
		args = append(args, actorID)
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	_ = s.id.RecordAudit(ctx, actorID, "", "device.pair_revoke", "pairing", pairID, "success", "")
	return nil
}

func (s *Store) ListDevices(ctx context.Context, userID string, includeAll bool) ([]PairedDevice, error) {
	query := `SELECT id, user_id, name, platform, status, COALESCE(session_id,''), last_endpoint, last_transport, approved_by, approved_at, revoked_at, last_seen_at, created_at
		 FROM paired_devices`
	var args []any
	if !includeAll {
		query += " WHERE user_id=?"
		args = append(args, userID)
	}
	query += " ORDER BY created_at DESC"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]PairedDevice, 0)
	for rows.Next() {
		var d PairedDevice
		var app, rev sql.NullInt64
		var seen, created int64
		if err := rows.Scan(&d.ID, &d.UserID, &d.Name, &d.Platform, &d.Status, &d.SessionID, &d.LastEndpoint, &d.LastTransport, &d.ApprovedBy, &app, &rev, &seen, &created); err != nil {
			return nil, err
		}
		if app.Valid {
			t := time.Unix(0, app.Int64).UTC()
			d.ApprovedAt = &t
		}
		if rev.Valid {
			t := time.Unix(0, rev.Int64).UTC()
			d.RevokedAt = &t
		}
		d.LastSeenAt = time.Unix(0, seen).UTC()
		d.CreatedAt = time.Unix(0, created).UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) ApproveDevice(ctx context.Context, actorID string, isAdmin bool, deviceID string) (PairedDevice, error) {
	now := s.now()
	ts := now.UnixNano()
	query := "UPDATE paired_devices SET status='approved', approved_by=?, approved_at=? WHERE id=? AND status='pending'"
	args := []any{actorID, ts, deviceID}
	if !isAdmin {
		query += " AND user_id=?"
		args = append(args, actorID)
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return PairedDevice{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return PairedDevice{}, ErrNotFound
	}
	_ = s.id.RecordAudit(ctx, actorID, "", "device.approve", "device", deviceID, "success", "")
	devs, err := s.ListDevices(ctx, actorID, true)
	if err != nil {
		return PairedDevice{}, err
	}
	for _, d := range devs {
		if d.ID == deviceID {
			return d, nil
		}
	}
	return PairedDevice{}, ErrNotFound
}

func (s *Store) RevokeDevice(ctx context.Context, actorID string, isAdmin bool, deviceID string) error {
	now := s.now().UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ownerID string
	var sessionID sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT user_id, session_id FROM paired_devices WHERE id=?", deviceID).Scan(&ownerID, &sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !isAdmin && ownerID != actorID {
		return ErrForbidden
	}
	if _, err := tx.ExecContext(ctx, "UPDATE paired_devices SET status='revoked', revoked_at=? WHERE id=?", now, deviceID); err != nil {
		return err
	}
	if sessionID.Valid && sessionID.String != "" {
		if _, err := tx.ExecContext(ctx, "UPDATE sessions SET revoked_at=? WHERE id=? AND revoked_at IS NULL", now, sessionID.String); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_ = s.id.RecordAudit(ctx, actorID, "", "device.revoke", "device", deviceID, "success", "")
	return nil
}

func (s *Store) GetRemoteConfig(ctx context.Context) (RemoteConfig, error) {
	var cfg RemoteConfig
	var enabled int
	err := s.db.QueryRowContext(ctx,
		"SELECT remote_enabled, provider, tailscale_hostname, tunnel_url, tunnel_provider FROM connectivity_settings WHERE id=1",
	).Scan(&enabled, &cfg.Provider, &cfg.TailscaleHostname, &cfg.TunnelURL, &cfg.TunnelProvider)
	if err != nil {
		return cfg, err
	}
	cfg.Enabled = enabled != 0
	return cfg, nil
}

func (s *Store) UpdateRemoteConfig(ctx context.Context, actorID string, cfg RemoteConfig) (RemoteConfig, error) {
	switch cfg.Provider {
	case "", "none", "tailscale", "tunnel":
	default:
		return RemoteConfig{}, fmt.Errorf("%w: provider must be none, tailscale, or tunnel", ErrBadInput)
	}
	if cfg.Provider == "" {
		cfg.Provider = "none"
	}
	if cfg.Enabled && cfg.Provider == "none" {
		return RemoteConfig{}, fmt.Errorf("%w: select a remote provider (tailscale or tunnel) when enabling remote connectivity", ErrBadInput)
	}
	if cfg.Provider == "tunnel" && cfg.Enabled {
		if err := ValidateTunnelURL(cfg.TunnelURL); err != nil {
			return RemoteConfig{}, fmt.Errorf("%w: %v", ErrBadInput, err)
		}
	}
	enabledInt := 0
	if cfg.Enabled {
		enabledInt = 1
	}
	now := s.now().UnixNano()
	var authBy string
	var authAt sql.NullInt64
	if cfg.Enabled {
		authBy = actorID
		authAt = sql.NullInt64{Int64: now, Valid: true}
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE connectivity_settings
		 SET remote_enabled=?, provider=?, tailscale_hostname=?, tunnel_url=?, tunnel_provider=?, authorized_by=?, authorized_at=?, updated_at=?
		 WHERE id=1`,
		enabledInt, cfg.Provider, strings.TrimSpace(cfg.TailscaleHostname), strings.TrimSpace(cfg.TunnelURL), strings.TrimSpace(cfg.TunnelProvider),
		authBy, authAt, now,
	)
	if err != nil {
		return RemoteConfig{}, err
	}
	_ = s.id.RecordAudit(ctx, actorID, "", "connect.remote_config", "system", cfg.Provider, "success", fmt.Sprintf("enabled=%v", cfg.Enabled))
	return s.GetRemoteConfig(ctx)
}

func (s *Store) Info(ctx context.Context, port int, scheme string) (ConnectivityInfo, error) {
	settings, err := s.id.GetSettings(ctx)
	if err != nil {
		return ConnectivityInfo{}, err
	}
	remoteCfg, err := s.GetRemoteConfig(ctx)
	if err != nil {
		return ConnectivityInfo{}, err
	}
	local := DiscoverLocalEndpoints(port, scheme)
	var remoteStatus RemoteStatus
	switch remoteCfg.Provider {
	case "tunnel":
		remoteStatus = s.tunnel.Status(ctx, remoteCfg, port)
	case "tailscale":
		remoteStatus = s.tailscale.Status(ctx, remoteCfg, port)
	default:
		remoteStatus = RemoteStatus{
			Provider:     "none",
			Enabled:      false,
			State:        "disabled",
			Connectivity: "unavailable",
			Message:      "Remote connectivity is disabled until explicitly configured and authorized.",
		}
	}
	candidates := make([]Endpoint, 0, len(local)+len(remoteStatus.Endpoints))
	candidates = append(candidates, local...)
	if remoteStatus.Enabled && remoteStatus.Available {
		candidates = append(candidates, remoteStatus.Endpoints...)
	}
	return ConnectivityInfo{
		CloudMode:       settings.CloudMode,
		MDNSServiceType: MDNSServiceType,
		LocalEndpoints:  local,
		RemoteConfig:    remoteCfg,
		RemoteStatus:    remoteStatus,
		Candidates:      candidates,
		CASHA256:        s.CAFingeprint(),
	}, nil
}

func SelectEndpoint(ctx context.Context, mode string, candidates []Endpoint, probe func(context.Context, string) (int64, error)) (Endpoint, []Endpoint, error) {
	if mode == "" {
		mode = "auto"
	}
	isLocal := func(kind string) bool {
		return kind == "localhost" || kind == "lan" || kind == "mdns"
	}
	isRemote := func(kind string) bool {
		return kind == "tailscale_direct" || kind == "tailscale_relay" || kind == "tunnel"
	}
	priority := func(kind string) int {
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

	filtered := make([]Endpoint, 0, len(candidates))
	for _, c := range candidates {
		switch mode {
		case "local":
			if !isLocal(c.Kind) {
				continue
			}
		case "remote":
			if !isRemote(c.Kind) {
				continue
			}
		case "auto":
		default:
			return Endpoint{}, nil, fmt.Errorf("%w: mode must be auto, local, or remote", ErrBadInput)
		}
		filtered = append(filtered, c)
	}
	if len(filtered) == 0 {
		return Endpoint{}, nil, fmt.Errorf("%w: no candidate endpoints available for mode %s", ErrNotFound, mode)
	}

	probed := make([]Endpoint, len(filtered))
	var best *Endpoint
	bestPrio := 999
	for i, c := range filtered {
		lat, err := probe(ctx, c.URL)
		if err == nil {
			c.Reachable = true
			c.LatencyMs = lat
			p := priority(c.Kind)
			if best == nil || p < bestPrio || (p == bestPrio && lat < best.LatencyMs) {
				copyC := c
				best = &copyC
				bestPrio = p
			}
		}
		probed[i] = c
	}
	if best == nil {
		return Endpoint{}, probed, fmt.Errorf("%w: no reachable endpoint found for mode %s", ErrNotFound, mode)
	}
	return *best, probed, nil
}
