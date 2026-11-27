// Package worker implements contributed worker identity, enrollment,
// resource-sharing policies and the durable report channel between worker
// agents and the coordinator engine.
package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
)

const (
	// Enrollment credentials are single-use and short-lived.
	EnrollmentPrefix = "kwe"
	// Worker API tokens authenticate every worker operation and are revocable.
	TokenPrefix = "kwt"

	MinEnrollmentTTL = time.Second
	MaxEnrollmentTTL = 24 * time.Hour
)

var (
	ErrNotFound     = errors.New("worker not found")
	ErrConflict     = errors.New("worker already exists or the enrollment token was already used")
	ErrUnauthorized = errors.New("worker credentials are invalid")
	ErrForbidden    = errors.New("worker is not permitted to perform this operation")
	ErrBadInput     = errors.New("invalid worker input")
)

type Enrollment struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	AutoApprove bool       `json:"auto_approve"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	UsedAt      *time.Time `json:"used_at,omitempty"`
	UsedBy      string     `json:"used_by,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

type WorkerInfo struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Hostname      string             `json:"hostname,omitempty"`
	Status        string             `json:"status"`
	EnrolledAt    time.Time          `json:"enrolled_at"`
	EnrolledBy    string             `json:"enrolled_by,omitempty"`
	ApprovedAt    *time.Time         `json:"approved_at,omitempty"`
	RevokedAt     *time.Time         `json:"revoked_at,omitempty"`
	LastHeartbeat *time.Time         `json:"last_heartbeat,omitempty"`
	Capabilities  *core.Capabilities `json:"capabilities,omitempty"`
	Telemetry     *core.Telemetry    `json:"telemetry,omitempty"`
	Policy        core.WorkerPolicy  `json:"policy"`
	LatencyMS     float64            `json:"latency_ms"`
	Reliability   float64            `json:"reliability"`
	TokenLastUsed *time.Time         `json:"token_last_used,omitempty"`
}

// State maps lifecycle status and heartbeat freshness onto the worker states
// the scheduler and API expose.
func (w WorkerInfo) State(now time.Time) string {
	switch w.Status {
	case core.WorkerStateRevoked:
		return core.WorkerStateRevoked
	case core.WorkerStateDisabled:
		return core.WorkerStateDisabled
	case core.WorkerStatePending:
		return core.WorkerStatePending
	}
	if w.LastHeartbeat == nil || now.Sub(*w.LastHeartbeat) > core.OfflineAfter {
		return core.WorkerStateOffline
	}
	if w.Status == core.WorkerStateDraining {
		return core.WorkerStateDraining
	}
	return core.WorkerStateOnline
}

// AsCoreWorker projects the stored worker onto the placement snapshot.
func (w WorkerInfo) AsCoreWorker(now time.Time) core.Worker {
	state := w.State(now)
	out := core.Worker{
		ID:          w.ID,
		State:       state,
		Name:        w.Name,
		Heartbeat:   now,
		Reliability: w.Reliability,
		LatencyMS:   w.LatencyMS,
		Policy:      &w.Policy,
		Telemetry:   w.Telemetry,
	}
	if w.Capabilities != nil {
		caps := *w.Capabilities
		out.Device = &core.Device{ID: w.ID, Capabilities: caps}
		if caps.DockerAvailable != nil && *caps.DockerAvailable && caps.LogicalCPUs != nil &&
			caps.MemoryTotalBytes != nil && *caps.LogicalCPUs > 0 && *caps.MemoryTotalBytes > 0 {
			out.Runtime = &core.DockerCapacity{
				ID:               "worker:" + w.ID,
				OS:               caps.OS,
				Arch:             caps.Arch,
				LogicalCPUs:      *caps.LogicalCPUs,
				MemoryTotalBytes: *caps.MemoryTotalBytes,
			}
		}
	}
	switch state {
	case core.WorkerStateOnline, core.WorkerStateDraining:
		if out.Runtime != nil {
			out.Health = "healthy"
		} else {
			out.Health = "degraded"
		}
	default:
		out.Health = "unavailable"
	}
	if w.LastHeartbeat != nil {
		out.Heartbeat = *w.LastHeartbeat
	}
	return out
}

type Store struct {
	db     *sql.DB
	local  *core.Store
	Logger *slog.Logger
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db, Logger: slog.Default()}
}

// UseLocalStore lets the cluster surface include the coordinator's own worker.
func (s *Store) UseLocalStore(local *core.Store) { s.local = local }

func normalizeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == ' ':
			return r
		default:
			return '-'
		}
	}, name)
	name = strings.Join(strings.Fields(name), " ")
	if len(name) > 64 {
		return name[:64]
	}
	return name
}

// CreateEnrollment issues a single-use bearer credential for one device.
func (s *Store) CreateEnrollment(ctx context.Context, createdBy, name string, ttl time.Duration, autoApprove bool) (Enrollment, string, error) {
	if ttl < MinEnrollmentTTL || ttl > MaxEnrollmentTTL {
		return Enrollment{}, "", fmt.Errorf("%w: enrollment TTL must be between 1 second and 24 hours", ErrBadInput)
	}
	name = normalizeName(name)
	token, err := identity.NewSecretToken(EnrollmentPrefix, 24)
	if err != nil {
		return Enrollment{}, "", err
	}
	id, err := identity.NewSecretToken("enr", 8)
	if err != nil {
		return Enrollment{}, "", err
	}
	now := time.Now().UTC()
	enrollment := Enrollment{
		ID: id, Name: name, AutoApprove: autoApprove, CreatedBy: createdBy,
		CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO worker_enrollments(id,token_hash,name,auto_approve,created_by,created_at,expires_at) VALUES(?,?,?,?,?,?,?)",
		enrollment.ID, identity.HashToken(token), enrollment.Name, enrollment.AutoApprove, enrollment.CreatedBy,
		enrollment.CreatedAt.UnixNano(), enrollment.ExpiresAt.UnixNano())
	if err != nil {
		return Enrollment{}, "", err
	}
	return enrollment, token, nil
}

func scanEnrollment(row interface{ Scan(...any) error }) (Enrollment, error) {
	var e Enrollment
	var createdAt, expiresAt int64
	var usedAt, revokedAt *int64
	var usedBy *string
	err := row.Scan(&e.ID, &e.Name, &e.AutoApprove, &e.CreatedBy, &createdAt, &expiresAt, &usedAt, &usedBy, &revokedAt)
	if usedBy != nil {
		e.UsedBy = *usedBy
	}
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	if err != nil {
		return e, err
	}
	e.CreatedAt = time.Unix(0, createdAt).UTC()
	e.ExpiresAt = time.Unix(0, expiresAt).UTC()
	if usedAt != nil {
		t := time.Unix(0, *usedAt).UTC()
		e.UsedAt = &t
	}
	if revokedAt != nil {
		t := time.Unix(0, *revokedAt).UTC()
		e.RevokedAt = &t
	}
	return e, nil
}

func (s *Store) ListEnrollments(ctx context.Context) ([]Enrollment, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id,name,auto_approve,created_by,created_at,expires_at,used_at,used_by,revoked_at FROM worker_enrollments ORDER BY created_at DESC LIMIT 100")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Enrollment, 0)
	for rows.Next() {
		e, err := scanEnrollment(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, e)
	}
	return items, rows.Err()
}

func (s *Store) RevokeEnrollment(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx,
		"UPDATE worker_enrollments SET revoked_at=? WHERE id=? AND revoked_at IS NULL AND used_at IS NULL",
		time.Now().UTC().UnixNano(), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

// Enroll exchanges a valid single-use enrollment token for a persistent
// worker identity and API token. The token is consumed in the same transaction
// that creates the worker, so it can never be used twice.
func (s *Store) Enroll(ctx context.Context, rawToken, name, hostname string) (string, string, bool, error) {
	name = normalizeName(name)
	hostname = normalizeName(hostname)
	if hostname == "" {
		hostname = name
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", false, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	var enrollmentID string
	var autoApprove bool
	err = tx.QueryRowContext(ctx,
		"SELECT id, auto_approve FROM worker_enrollments WHERE token_hash=? AND revoked_at IS NULL AND used_at IS NULL AND expires_at>?",
		identity.HashToken(rawToken), now.UnixNano()).Scan(&enrollmentID, &autoApprove)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, ErrUnauthorized
	}
	if err != nil {
		return "", "", false, err
	}
	workerID, err := identity.NewSecretToken("wk-", 16)
	if err != nil {
		return "", "", false, err
	}
	workerToken, err := identity.NewSecretToken(TokenPrefix, 32)
	if err != nil {
		return "", "", false, err
	}
	status := core.WorkerStatePending
	if autoApprove {
		status = core.WorkerStateApproved
	}
	policy, err := json.Marshal(core.DefaultWorkerPolicy())
	if err != nil {
		return "", "", false, err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO remote_workers(id,name,hostname,status,enrolled_at,enrolled_by,policy) VALUES(?,?,?,?,?,?,?)",
		workerID, name, hostname, status, now.UnixNano(), enrollmentID, policy); err != nil {
		return "", "", false, err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO worker_tokens(token_hash,worker_id,created_at) VALUES(?,?,?)",
		identity.HashToken(workerToken), workerID, now.UnixNano()); err != nil {
		return "", "", false, err
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE worker_enrollments SET used_at=?, used_by=? WHERE id=?", now.UnixNano(), workerID, enrollmentID); err != nil {
		return "", "", false, err
	}
	if err := tx.Commit(); err != nil {
		return "", "", false, err
	}
	return workerID, workerToken, autoApprove, nil
}

// AuthenticateWorker resolves a bearer token to a live worker ID.
func (s *Store) AuthenticateWorker(ctx context.Context, rawToken string) (string, error) {
	if !strings.HasPrefix(rawToken, TokenPrefix) {
		return "", ErrUnauthorized
	}
	tokenHash := identity.HashToken(rawToken)
	var workerID string
	err := s.db.QueryRowContext(ctx,
		"SELECT worker_id FROM worker_tokens WHERE token_hash=? AND revoked_at IS NULL",
		tokenHash).Scan(&workerID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUnauthorized
	}
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, "UPDATE worker_tokens SET last_used=? WHERE token_hash=?",
		time.Now().UTC().UnixNano(), tokenHash)
	if err != nil {
		return "", err
	}
	return workerID, nil
}

func (s *Store) RequireWorker(ctx context.Context, id string) (WorkerInfo, error) {
	info, err := s.Get(ctx, id)
	if err != nil {
		return WorkerInfo{}, err
	}
	switch info.Status {
	case core.WorkerStateRevoked:
		return WorkerInfo{}, fmt.Errorf("%w: worker is revoked", ErrForbidden)
	case core.WorkerStateDisabled:
		return WorkerInfo{}, fmt.Errorf("%w: worker is disabled", ErrForbidden)
	}
	return info, nil
}

const workerColumns = "SELECT id,name,hostname,status,enrolled_at,enrolled_by,approved_at,revoked_at,last_heartbeat,capabilities,telemetry,latency_ms,reliability,policy," +
	"(SELECT last_used FROM worker_tokens t WHERE t.worker_id=remote_workers.id AND t.revoked_at IS NULL ORDER BY created_at DESC LIMIT 1)"

func (s *Store) Get(ctx context.Context, id string) (WorkerInfo, error) {
	row := s.db.QueryRowContext(ctx, workerColumns+" FROM remote_workers WHERE id=?", id)
	info, err := scanWorker(row)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkerInfo{}, ErrNotFound
	}
	return info, err
}

func (s *Store) List(ctx context.Context) ([]WorkerInfo, error) {
	rows, err := s.db.QueryContext(ctx, workerColumns+" FROM remote_workers ORDER BY enrolled_at")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]WorkerInfo, 0)
	for rows.Next() {
		info, err := scanWorker(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, info)
	}
	return items, rows.Err()
}

func scanWorker(row interface{ Scan(...any) error }) (WorkerInfo, error) {
	var w WorkerInfo
	var enrolledAt int64
	var approvedAt, revokedAt, lastHeartbeat, tokenLastUsed *int64
	var capabilities, telemetry, policy *string
	err := row.Scan(&w.ID, &w.Name, &w.Hostname, &w.Status, &enrolledAt, &w.EnrolledBy, &approvedAt, &revokedAt,
		&lastHeartbeat, &capabilities, &telemetry, &w.LatencyMS, &w.Reliability, &policy, &tokenLastUsed)
	if err != nil {
		return w, err
	}
	w.EnrolledAt = time.Unix(0, enrolledAt).UTC()
	timePtr := func(raw *int64) *time.Time {
		if raw == nil {
			return nil
		}
		t := time.Unix(0, *raw).UTC()
		return &t
	}
	w.ApprovedAt, w.RevokedAt, w.LastHeartbeat, w.TokenLastUsed = timePtr(approvedAt), timePtr(revokedAt), timePtr(lastHeartbeat), timePtr(tokenLastUsed)
	if capabilities != nil {
		var caps core.Capabilities
		if json.Unmarshal([]byte(*capabilities), &caps) == nil {
			w.Capabilities = &caps
		}
	}
	if telemetry != nil {
		var value core.Telemetry
		if json.Unmarshal([]byte(*telemetry), &value) == nil {
			w.Telemetry = &value
		}
	}
	if policy != nil {
		_ = json.Unmarshal([]byte(*policy), &w.Policy)
	}
	return w, nil
}

// SetStatus applies owner lifecycle decisions. Revocation also invalidates the
// worker's API token so it cannot poll, report or receive assignments.
func (s *Store) SetStatus(ctx context.Context, id, status string) (WorkerInfo, error) {
	now := time.Now().UTC().UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkerInfo{}, err
	}
	defer tx.Rollback()
	switch status {
	case core.WorkerStateApproved:
		_, err = tx.ExecContext(ctx,
			"UPDATE remote_workers SET status=?, approved_at=? WHERE id=? AND status IN ('pending','disabled','draining')", status, now, id)
	case core.WorkerStateDisabled:
		_, err = tx.ExecContext(ctx,
			"UPDATE remote_workers SET status=? WHERE id=? AND status IN ('approved','draining')", status, id)
	case core.WorkerStateDraining:
		_, err = tx.ExecContext(ctx,
			"UPDATE remote_workers SET status=? WHERE id=? AND status IN ('approved','disabled')", status, id)
	case core.WorkerStateRevoked:
		_, err = tx.ExecContext(ctx,
			"UPDATE remote_workers SET status=?, revoked_at=? WHERE id=? AND status!='revoked'", status, now, id)
		if err == nil {
			_, err = tx.ExecContext(ctx,
				"UPDATE worker_tokens SET revoked_at=? WHERE worker_id=? AND revoked_at IS NULL", now, id)
		}
	default:
		return WorkerInfo{}, fmt.Errorf("%w: unknown worker status", ErrBadInput)
	}
	if err != nil {
		return WorkerInfo{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkerInfo{}, err
	}
	return s.Get(ctx, id)
}

// Heartbeat records fresh capabilities and telemetry. It is the worker's liveness signal.
func (s *Store) Heartbeat(ctx context.Context, workerID string, capabilities *core.Capabilities, telemetry *core.Telemetry, rttMS float64) error {
	info, err := s.RequireWorker(ctx, workerID)
	if err != nil {
		return err
	}
	now := time.Now().UTC().UnixNano()
	latency := info.LatencyMS
	if rttMS > 0 {
		if latency <= 0 {
			latency = rttMS
		} else {
			latency = 0.7*latency + 0.3*rttMS
		}
	}
	reliability := info.Reliability
	if info.LastHeartbeat != nil {
		interval := now - info.LastHeartbeat.UnixNano()
		onTime := 0.2
		if interval > 0 && interval <= int64(22*time.Second) {
			onTime = 1
		}
		reliability = 0.9*reliability + 0.1*onTime
	} else {
		reliability = 0.5
	}
	capabilitiesJSON := "null"
	if capabilities != nil {
		if data, err := json.Marshal(capabilities); err == nil {
			capabilitiesJSON = string(data)
		}
	}
	telemetryJSON := "null"
	if telemetry != nil {
		if data, err := json.Marshal(telemetry); err == nil {
			telemetryJSON = string(data)
		}
	}
	_, err = s.db.ExecContext(ctx,
		"UPDATE remote_workers SET last_heartbeat=?, capabilities=?, telemetry=?, latency_ms=?, reliability=? WHERE id=?",
		now, capabilitiesJSON, telemetryJSON, latency, reliability, workerID)
	return err
}

// UpdatePolicy stores the owner's resource-sharing rules.
func (s *Store) UpdatePolicy(ctx context.Context, workerID string, policy core.WorkerPolicy) error {
	if err := ValidatePolicy(policy); err != nil {
		return err
	}
	data, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, "UPDATE remote_workers SET policy=? WHERE id=? AND status!='revoked'", data, workerID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordReport queues a durable worker message for the engine. The engine is
// the single writer of workload state, so reports are applied in order there.
func (s *Store) RecordReport(ctx context.Context, workerID, workloadID, fence, kind string, payload []byte) error {
	if kind != core.ReportAck && kind != core.ReportStart && kind != core.ReportExit && kind != core.ReportRenew {
		return fmt.Errorf("%w: unknown report kind", ErrBadInput)
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO worker_reports(worker_id,workload_id,fence,kind,payload,at) VALUES(?,?,?,?,?,?)",
		workerID, workloadID, fence, kind, payload, time.Now().UTC().UnixNano())
	return err
}

// DrainReports peeks at queued reports in submission order without deleting
// them; ReportsApplied removes them after the engine has persisted the
// resulting state, so a crash replays reports that were not applied.
func (s *Store) DrainReports(ctx context.Context, limit int) ([]core.WorkerReport, error) {
	if limit <= 0 {
		limit = 64
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT id,worker_id,workload_id,fence,kind,payload,at FROM worker_reports ORDER BY id LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reports := make([]core.WorkerReport, 0, limit)
	for rows.Next() {
		var report core.WorkerReport
		var payload []byte
		var at int64
		if err := rows.Scan(&report.ID, &report.WorkerID, &report.WorkloadID, &report.Fence, &report.Kind, &payload, &at); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &report); err != nil {
			return nil, fmt.Errorf("decode worker report %d: %w", report.ID, err)
		}
		report.At = time.Unix(0, at).UTC()
		reports = append(reports, report)
	}
	return reports, rows.Err()
}

// ReportsApplied deletes reports the engine has applied. Application is
// idempotent per report kind, so a crash between apply and delete is safe.
func (s *Store) ReportsApplied(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, "DELETE FROM worker_reports WHERE id=?", id); err != nil {
			return err
		}
	}
	return nil
}

// SaveDecision persists a placement decision for the explanation API.
func (s *Store) SaveDecision(ctx context.Context, decision core.PlacementDecision) error {
	candidates, err := json.Marshal(decision.Candidates)
	if err != nil {
		return err
	}
	var chosen any
	if decision.Chosen != "" {
		chosen = decision.Chosen
	}
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO placement_decisions(workload_id,attempt,mode,chosen_worker,explanation,candidates,at) VALUES(?,?,?,?,?,?,?)",
		decision.WorkloadID, decision.Attempt, decision.Mode, chosen, decision.Explanation, candidates, decision.At.UnixNano())
	return err
}

func scanDecision(row interface{ Scan(...any) error }, workloadID string) (core.PlacementDecision, error) {
	var decision core.PlacementDecision
	var chosen *string
	var at int64
	var candidates []byte
	if err := row.Scan(&decision.WorkloadID, &decision.Attempt, &decision.Mode, &chosen, &decision.Explanation, &candidates, &at); err != nil {
		return decision, err
	}
	if chosen != nil {
		decision.Chosen = *chosen
	}
	if err := json.Unmarshal(candidates, &decision.Candidates); err != nil {
		return decision, err
	}
	decision.At = time.Unix(0, at).UTC()
	return decision, nil
}

func (s *Store) Decisions(ctx context.Context, workloadID string) ([]core.PlacementDecision, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT workload_id,attempt,mode,chosen_worker,explanation,candidates,at FROM placement_decisions WHERE workload_id=? ORDER BY id DESC LIMIT 50", workloadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	decisions := make([]core.PlacementDecision, 0)
	for rows.Next() {
		decision, err := scanDecision(rows, workloadID)
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, decision)
	}
	return decisions, rows.Err()
}

func (s *Store) RecentDecisions(ctx context.Context, limit int) ([]core.PlacementDecision, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT workload_id,attempt,mode,chosen_worker,explanation,candidates,at FROM placement_decisions ORDER BY id DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	decisions := make([]core.PlacementDecision, 0)
	for rows.Next() {
		decision, err := scanDecision(rows, "")
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, decision)
	}
	return decisions, rows.Err()
}

// WorkerAssignments returns the worker's active assignments for poll responses
// and worker-side reconciliation.
func (s *Store) WorkerAssignments(ctx context.Context, workerID string) ([]core.Workload, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT snapshot, revision, log_tail FROM workloads WHERE worker_id=? AND "+
			"state NOT IN ('SUCCEEDED','FAILED','CANCELLED','STOPPED') ORDER BY created_at, id", workerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]core.Workload, 0)
	for rows.Next() {
		var w core.Workload
		var data []byte
		if err := rows.Scan(&data, &w.Revision, &w.LogTail); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &w); err != nil {
			return nil, err
		}
		items = append(items, w)
	}
	return items, rows.Err()
}

type ProjectPrefs struct {
	ProjectID        string    `json:"project_id"`
	Mode             string    `json:"mode"`
	PreferredWorkers []string  `json:"preferred_workers,omitempty"`
	Fallback         bool      `json:"fallback"`
	UpdatedAt        time.Time `json:"updated_at"`
	UpdatedBy        string    `json:"updated_by,omitempty"`
}

// ProjectPrefs returns the scheduling defaults for one project. Missing rows
// mean automatic placement.
func (s *Store) ProjectPrefs(ctx context.Context, projectID string) (ProjectPrefs, error) {
	var prefs ProjectPrefs
	var preferred string
	var updatedAt int64
	var fallback int
	err := s.db.QueryRowContext(ctx,
		"SELECT project_id,mode,preferred_workers,fallback,updated_at,updated_by FROM project_scheduling_prefs WHERE project_id=?",
		projectID).Scan(&prefs.ProjectID, &prefs.Mode, &preferred, &fallback, &updatedAt, &prefs.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectPrefs{ProjectID: projectID, Mode: core.PlacementAuto, Fallback: true}, nil
	}
	if err != nil {
		return prefs, err
	}
	prefs.Fallback = fallback == 1
	prefs.UpdatedAt = time.Unix(0, updatedAt).UTC()
	if preferred != "" {
		_ = json.Unmarshal([]byte(preferred), &prefs.PreferredWorkers)
	}
	return prefs, nil
}

// SaveProjectPrefs stores the project's scheduling defaults.
func (s *Store) SaveProjectPrefs(ctx context.Context, projectID, mode string, preferred []string, fallback *bool, actorID string) (ProjectPrefs, error) {
	switch mode {
	case core.PlacementAuto:
	case core.PlacementManual:
		if len(preferred) != 1 {
			return ProjectPrefs{}, fmt.Errorf("%w: manual mapping requires exactly one worker", ErrBadInput)
		}
	case core.PlacementPreferred:
		if len(preferred) == 0 {
			return ProjectPrefs{}, fmt.Errorf("%w: preferred mapping requires at least one worker", ErrBadInput)
		}
		if len(preferred) > 8 {
			return ProjectPrefs{}, fmt.Errorf("%w: at most 8 preferred workers are allowed", ErrBadInput)
		}
	default:
		return ProjectPrefs{}, fmt.Errorf("%w: mode must be auto, manual or preferred", ErrBadInput)
	}
	for _, id := range preferred {
		if !core.ValidWorkerID(id) {
			return ProjectPrefs{}, fmt.Errorf("%w: workers must be the local worker or wk- worker identifiers", ErrBadInput)
		}
	}
	setFallback := fallback == nil || *fallback
	preferredJSON := "[]"
	if len(preferred) > 0 {
		if data, err := json.Marshal(preferred); err == nil {
			preferredJSON = string(data)
		}
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO project_scheduling_prefs(project_id,mode,preferred_workers,fallback,updated_at,updated_by) VALUES(?,?,?,?,?,?) "+
			"ON CONFLICT(project_id) DO UPDATE SET mode=excluded.mode, preferred_workers=excluded.preferred_workers, "+
			"fallback=excluded.fallback, updated_at=excluded.updated_at, updated_by=excluded.updated_by",
		projectID, mode, preferredJSON, setFallbackInt(setFallback), now.UnixNano(), actorID)
	if err != nil {
		return ProjectPrefs{}, err
	}
	return s.ProjectPrefs(ctx, projectID)
}

func setFallbackInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// Workers implements core.Cluster: the local worker plus every contributed
// worker with fresh lifecycle state, capabilities and telemetry.
func (s *Store) Workers(ctx context.Context) ([]core.Worker, error) {
	now := time.Now().UTC()
	workers := make([]core.Worker, 0, 4)
	if s.local != nil {
		if localWorker, err := s.local.Worker(ctx); err == nil {
			workers = append(workers, localWorker)
		}
	}
	remote, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	assignments, err := s.countAssignments(ctx)
	if err != nil {
		return nil, err
	}
	for _, info := range remote {
		worker := info.AsCoreWorker(now)
		worker.Active = assignments[info.ID]
		workers = append(workers, worker)
	}
	return workers, nil
}

func (s *Store) countAssignments(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT worker_id, count(*) FROM workloads WHERE worker_id!='' AND "+
			"state NOT IN ('SUCCEEDED','FAILED','CANCELLED','STOPPED') GROUP BY worker_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var id string
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return nil, err
		}
		counts[id] = count
	}
	return counts, rows.Err()
}
