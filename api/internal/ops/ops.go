// Package ops implements Kamui SRE and collaboration operations: project
// quotas with scheduler enforcement, durable usage accounting, alert rules
// with deduplication, and managed database lifecycle through the shared
// execution and storage abstractions.
package ops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
)

// ErrConflict marks already-existing resources.
var ErrConflict = errors.New("already exists")

func newAlertID() string {
	token, err := identity.NewSecretToken("alr", 10)
	if err != nil {
		return fmt.Sprintf("alr-%d", time.Now().UTC().UnixNano())
	}
	return token
}

var (
	ErrNotFound = errors.New("not found")
	ErrBadInput = errors.New("invalid input")
)

type Store struct {
	db   *sql.DB
	core *core.Store
}

func NewStore(db *sql.DB, coreStore *core.Store) *Store {
	return &Store{db: db, core: coreStore}
}

// --- Project quotas ------------------------------------------------------------

type Quota struct {
	ProjectID          string    `json:"project_id"`
	MaxActiveWorkloads int       `json:"max_active_workloads"`
	MaxCPUMillis       int64     `json:"max_cpu_millis"`
	MaxMemoryBytes     int64     `json:"max_memory_bytes"`
	MaxStorageBytes    int64     `json:"max_storage_bytes"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func (s *Store) SetQuota(ctx context.Context, projectID string, quota Quota, actor string) (Quota, error) {
	if quota.MaxActiveWorkloads < 0 || quota.MaxActiveWorkloads > 128 ||
		quota.MaxCPUMillis < 0 || quota.MaxCPUMillis > 1024000 ||
		quota.MaxMemoryBytes < 0 || quota.MaxMemoryBytes > 8<<40 ||
		quota.MaxStorageBytes < 0 || quota.MaxStorageBytes > 64<<40 {
		return Quota{}, fmt.Errorf("%w: quota values are outside the supported range", ErrBadInput)
	}
	now := time.Now().UTC().UnixNano()
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO project_quotas(project_id,max_active_workloads,max_cpu_millis,max_memory_bytes,max_storage_bytes,updated_by,updated_at) VALUES(?,?,?,?,?,?,?) "+
			"ON CONFLICT(project_id) DO UPDATE SET max_active_workloads=excluded.max_active_workloads, max_cpu_millis=excluded.max_cpu_millis, max_memory_bytes=excluded.max_memory_bytes, max_storage_bytes=excluded.max_storage_bytes, updated_by=excluded.updated_by, updated_at=excluded.updated_at",
		projectID, quota.MaxActiveWorkloads, quota.MaxCPUMillis, quota.MaxMemoryBytes, quota.MaxStorageBytes, actor, now)
	if err != nil {
		return Quota{}, err
	}
	return s.GetQuota(ctx, projectID)
}

func (s *Store) GetQuota(ctx context.Context, projectID string) (Quota, error) {
	var q Quota
	var updatedAt int64
	err := s.db.QueryRowContext(ctx,
		"SELECT project_id,max_active_workloads,max_cpu_millis,max_memory_bytes,max_storage_bytes,updated_at FROM project_quotas WHERE project_id=?", projectID).
		Scan(&q.ProjectID, &q.MaxActiveWorkloads, &q.MaxCPUMillis, &q.MaxMemoryBytes, &q.MaxStorageBytes, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Quota{ProjectID: projectID}, nil
	}
	q.UpdatedAt = time.Unix(0, updatedAt).UTC()
	return q, err
}

// QuotaUsage is the current consumption picture for one project, derived from
// active workload reservations.
type QuotaUsage struct {
	ActiveWorkloads int   `json:"active_workloads"`
	ReservedCPU     int64 `json:"reserved_cpu_millis"`
	ReservedMemory  int64 `json:"reserved_memory_bytes"`
}

func (s *Store) UsageSnapshot(ctx context.Context, projectID string) (QuotaUsage, error) {
	items, err := s.core.ListForProjects(ctx, []string{projectID})
	if err != nil {
		return QuotaUsage{}, err
	}
	usage := QuotaUsage{}
	for _, item := range items {
		if item.Assignment != nil && !item.State.Terminal() {
			usage.ActiveWorkloads++
			usage.ReservedCPU += item.Assignment.Reservation.CPUMillis
			usage.ReservedMemory += item.Assignment.Reservation.MemoryBytes
		}
	}
	return usage, nil
}

// --- Usage accounting ------------------------------------------------------------

type UsageRecord struct {
	WorkloadID         string     `json:"workload_id"`
	ProjectID          string     `json:"project_id"`
	WorkerID           string     `json:"worker_id,omitempty"`
	Service            string     `json:"service"`
	AllocatedCPUMillis int64      `json:"allocated_cpu_millis"`
	AllocatedMemory    int64      `json:"allocated_memory_bytes"`
	DurationSeconds    int64      `json:"duration_seconds"`
	StartedAt          time.Time  `json:"started_at"`
	FinishedAt         *time.Time `json:"finished_at,omitempty"`
}

// RecordStart opens a usage record when an assignment begins. It is called by
// the engine's cluster loop through the UsageSink hook.
func (s *Store) RecordStart(ctx context.Context, workloadID, projectID, workerID, service string, cpu, memory int64) {
	_, _ = s.db.ExecContext(ctx,
		"INSERT INTO usage_records(workload_id,project_id,worker_id,service,allocated_cpu_millis,allocated_memory_bytes,started_at) VALUES(?,?,?,?,?,?,?) "+
			"ON CONFLICT DO NOTHING",
		workloadID, projectID, workerID, service, cpu, memory, time.Now().UTC().UnixNano())
}

// RecordFinish closes the usage record when the workload reaches a terminal
// state. Allocation is what was reserved; measured utilization is recorded
// only when telemetry provides it — never fabricated.
func (s *Store) RecordFinish(ctx context.Context, workloadID string, measuredCPUPct *float64) {
	now := time.Now().UTC().UnixNano()
	_, _ = s.db.ExecContext(ctx,
		"UPDATE usage_records SET finished_at=?, duration_seconds=CAST((?-started_at)/1000000000 AS INTEGER), measured_cpu_pct=? WHERE workload_id=? AND finished_at IS NULL",
		now, now, measuredCPUPct, workloadID)
}

func (s *Store) ListUsage(ctx context.Context, projectID string, limit int) ([]UsageRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT workload_id,project_id,worker_id,service,allocated_cpu_millis,allocated_memory_bytes,duration_seconds,started_at,finished_at FROM usage_records WHERE project_id=? ORDER BY started_at DESC LIMIT ?",
		projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]UsageRecord, 0)
	for rows.Next() {
		var record UsageRecord
		var started int64
		var finished sql.NullInt64
		if err := rows.Scan(&record.WorkloadID, &record.ProjectID, &record.WorkerID, &record.Service,
			&record.AllocatedCPUMillis, &record.AllocatedMemory, &record.DurationSeconds, &started, &finished); err != nil {
			return nil, err
		}
		record.StartedAt = time.Unix(0, started).UTC()
		if finished.Valid {
			t := time.Unix(0, finished.Int64).UTC()
			record.FinishedAt = &t
		}
		items = append(items, record)
	}
	return items, rows.Err()
}

// --- Alerts ------------------------------------------------------------------------

type AlertRule struct {
	ID         string    `json:"id"`
	ProjectID  string    `json:"project_id"`
	Kind       string    `json:"kind"`
	Target     string    `json:"target,omitempty"`
	Threshold  int       `json:"threshold"`
	WebhookURL string    `json:"webhook_url,omitempty"`
	Enabled    bool      `json:"enabled"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
}

var validAlertKinds = map[string]bool{
	"worker_offline": true, "service_unhealthy": true, "deployment_failed": true,
	"workload_failed": true, "storage_threshold": true, "quota_exceeded": true, "job_timeout": true,
}

func (s *Store) CreateAlertRule(ctx context.Context, projectID, kind, target string, threshold int, webhookURL, actor string) (AlertRule, error) {
	if !validAlertKinds[kind] {
		return AlertRule{}, fmt.Errorf("%w: unknown alert kind", ErrBadInput)
	}
	if webhookURL != "" && !validWebhookURL(webhookURL) {
		return AlertRule{}, fmt.Errorf("%w: webhook must be an https URL", ErrBadInput)
	}
	id := newAlertID()
	now := time.Now().UTC().UnixNano()
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO alert_rules(id,project_id,kind,target,threshold,webhook_url,enabled,created_by,created_at) VALUES(?,?,?,?,?,?,1,?,?)",
		id, projectID, kind, target, threshold, webhookURL, actor, now); err != nil {
		return AlertRule{}, err
	}
	return s.GetAlertRule(ctx, id)
}

func validWebhookURL(raw string) bool {
	return strings.HasPrefix(raw, "https://") && len(raw) < 2048 && !strings.ContainsAny(raw, "\r\n")
}

func (s *Store) GetAlertRule(ctx context.Context, id string) (AlertRule, error) {
	var rule AlertRule
	var enabled, createdAt int64
	err := s.db.QueryRowContext(ctx,
		"SELECT id,project_id,kind,target,threshold,webhook_url,enabled,created_by,created_at FROM alert_rules WHERE id=?", id).
		Scan(&rule.ID, &rule.ProjectID, &rule.Kind, &rule.Target, &rule.Threshold, &rule.WebhookURL, &enabled, &rule.CreatedBy, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AlertRule{}, ErrNotFound
	}
	rule.Enabled = enabled == 1
	rule.CreatedAt = time.Unix(0, createdAt).UTC()
	return rule, err
}

func (s *Store) ListAlertRules(ctx context.Context, projectID string) ([]AlertRule, error) {
	args := []any{}
	query := "SELECT id,project_id,kind,target,threshold,webhook_url,enabled,created_by,created_at FROM alert_rules"
	if projectID != "" {
		query += " WHERE project_id=? OR project_id=''"
		args = append(args, projectID)
	}
	query += " ORDER BY created_at DESC LIMIT 100"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AlertRule, 0)
	for rows.Next() {
		var rule AlertRule
		var enabled, createdAt int64
		if err := rows.Scan(&rule.ID, &rule.ProjectID, &rule.Kind, &rule.Target, &rule.Threshold, &rule.WebhookURL, &enabled, &rule.CreatedBy, &createdAt); err != nil {
			return nil, err
		}
		rule.Enabled = enabled == 1
		rule.CreatedAt = time.Unix(0, createdAt).UTC()
		items = append(items, rule)
	}
	return items, rows.Err()
}

func (s *Store) DeleteAlertRule(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM alert_rules WHERE id=?", id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

type AlertEvent struct {
	ID         int64      `json:"id"`
	RuleID     string     `json:"rule_id"`
	State      string     `json:"state"`
	Target     string     `json:"target,omitempty"`
	Detail     string     `json:"detail,omitempty"`
	At         time.Time  `json:"at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// RaiseAlert records a firing event with deduplication: an unresolved event
// for the same rule+target is refreshed instead of duplicated.
func (s *Store) RaiseAlert(ctx context.Context, ruleID, target, detail string) (AlertEvent, error) {
	now := time.Now().UTC().UnixNano()
	var existing int64
	err := s.db.QueryRowContext(ctx,
		"SELECT id FROM alert_events WHERE rule_id=? AND target=? AND state='firing' AND resolved_at IS NULL", ruleID, target).Scan(&existing)
	if err == nil {
		_, _ = s.db.ExecContext(ctx, "UPDATE alert_events SET detail=?, at=? WHERE id=?", detail, now, existing)
		return s.GetAlertEvent(ctx, existing)
	}
	result, err := s.db.ExecContext(ctx,
		"INSERT INTO alert_events(rule_id,state,target,detail,at) VALUES(?, 'firing', ?, ?, ?)", ruleID, target, detail, now)
	if err != nil {
		return AlertEvent{}, err
	}
	id, _ := result.LastInsertId()
	return s.GetAlertEvent(ctx, id)
}

func (s *Store) GetAlertEvent(ctx context.Context, id int64) (AlertEvent, error) {
	var event AlertEvent
	var at int64
	var resolved sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		"SELECT id,rule_id,state,target,detail,at,resolved_at FROM alert_events WHERE id=?", id).
		Scan(&event.ID, &event.RuleID, &event.State, &event.Target, &event.Detail, &at, &resolved)
	if errors.Is(err, sql.ErrNoRows) {
		return AlertEvent{}, ErrNotFound
	}
	event.At = time.Unix(0, at).UTC()
	if resolved.Valid {
		t := time.Unix(0, resolved.Int64).UTC()
		event.ResolvedAt = &t
	}
	return event, err
}

func (s *Store) ResolveAlert(ctx context.Context, ruleID, target string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE alert_events SET state='resolved', resolved_at=? WHERE rule_id=? AND target=? AND state='firing' AND resolved_at IS NULL",
		time.Now().UTC().UnixNano(), ruleID, target)
	return err
}

func (s *Store) ListAlertEvents(ctx context.Context, ruleID string, limit int) ([]AlertEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT id,rule_id,state,target,detail,at,resolved_at FROM alert_events WHERE rule_id=? ORDER BY at DESC LIMIT ?", ruleID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AlertEvent, 0)
	for rows.Next() {
		var event AlertEvent
		var at int64
		var resolved sql.NullInt64
		if err := rows.Scan(&event.ID, &event.RuleID, &event.State, &event.Target, &event.Detail, &at, &resolved); err != nil {
			return nil, err
		}
		event.At = time.Unix(0, at).UTC()
		if resolved.Valid {
			t := time.Unix(0, resolved.Int64).UTC()
			event.ResolvedAt = &t
		}
		items = append(items, event)
	}
	return items, rows.Err()
}

// --- Managed databases -----------------------------------------------------------

type Database struct {
	ID         string    `json:"id"`
	ProjectID  string    `json:"project_id"`
	Name       string    `json:"name"`
	Engine     string    `json:"engine"`
	Version    string    `json:"version,omitempty"`
	Image      string    `json:"image,omitempty"`
	WorkloadID string    `json:"workload_id,omitempty"`
	Volume     string    `json:"volume,omitempty"`
	State      string    `json:"state"`
	Reason     string    `json:"reason,omitempty"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

var engineImages = map[string]string{
	"postgres": "postgres:17-alpine",
}

// EngineProbe derives the readiness check for a database engine.
func EngineProbe(engine string) core.HealthCheck {
	switch engine {
	case "postgres":
		return core.HealthCheck{Kind: "tcp", Port: 5432, IntervalSeconds: 5}
	default:
		return core.HealthCheck{Kind: "tcp", Port: 5432, IntervalSeconds: 5}
	}
}

func (s *Store) CreateDatabase(ctx context.Context, projectID, name, engine, version string, resources core.Resources, actor string) (Database, error) {
	if !validDBName(name) {
		return Database{}, fmt.Errorf("%w: database names must be lowercase letters, digits and dashes", ErrBadInput)
	}
	image, ok := engineImages[engine]
	if !ok {
		return Database{}, fmt.Errorf("%w: engine %s is not supported yet", ErrBadInput, engine)
	}
	volume := "kamui-db-" + name
	submission := core.Submission{
		Service: "store",
		Kind:    "service",
		Image:   image,
		Env: map[string]string{
			"POSTGRES_DB":          name,
			"POSTGRES_PASSWORD":    "secret:db-" + name + "-password",
			"POSTGRES_INITDB_ARGS": "--data-checksums",
		},
		Network:     "private",
		Volumes:     []core.VolumeRequest{{Name: volume}},
		HealthCheck: ptr(EngineProbe(engine)),
	}
	submission.Resources = resources
	if submission.Resources.CPUMillis == 0 {
		submission.Resources.CPUMillis = 500
	}
	if submission.Resources.MemoryBytes == 0 {
		submission.Resources.MemoryBytes = 512 << 20
	}
	submission.SetScope(projectID, actor)
	if err := submission.Normalize(); err != nil {
		return Database{}, fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	workload, _, err := s.core.SubmitForProjectAs(ctx, projectID, actor, submission, "db:"+name)
	if err != nil {
		if errors.Is(err, core.ErrConflict) {
			return Database{}, fmt.Errorf("%w: a database with this name exists in the project", ErrConflict)
		}
		return Database{}, err
	}
	now := time.Now().UTC().UnixNano()
	id := "db-" + strings.TrimPrefix(workload.ID, "wl-")
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO databases(id,project_id,name,engine,version,image,workload_id,volume,state,created_by,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,'starting',?,?,?)",
		id, projectID, name, engine, version, image, workload.ID, volume, actor, now, now); err != nil {
		return Database{}, err
	}
	return s.GetDatabase(ctx, projectID, id)
}

func ptr(hc core.HealthCheck) *core.HealthCheck { return &hc }

func validDBName(name string) bool {
	if name == "" || len(name) > 62 || !strings.Contains(name, "-") {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func (s *Store) GetDatabase(ctx context.Context, projectID, id string) (Database, error) {
	var d Database
	var createdAt, updatedAt int64
	err := s.db.QueryRowContext(ctx,
		"SELECT id,project_id,name,engine,version,image,workload_id,volume,state,reason,created_by,created_at,updated_at FROM databases WHERE id=? AND project_id=?",
		id, projectID).Scan(&d.ID, &d.ProjectID, &d.Name, &d.Engine, &d.Version, &d.Image, &d.WorkloadID, &d.Volume, &d.State, &d.Reason, &d.CreatedBy, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Database{}, ErrNotFound
	}
	d.CreatedAt = time.Unix(0, createdAt).UTC()
	d.UpdatedAt = time.Unix(0, updatedAt).UTC()
	return d, err
}

func (s *Store) ListDatabases(ctx context.Context, projectID string) ([]Database, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id,project_id,name,engine,version,image,workload_id,volume,state,reason,created_by,created_at,updated_at FROM databases WHERE project_id=? ORDER BY created_at DESC LIMIT 100", projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Database, 0)
	for rows.Next() {
		var d Database
		var createdAt, updatedAt int64
		if err := rows.Scan(&d.ID, &d.ProjectID, &d.Name, &d.Engine, &d.Version, &d.Image, &d.WorkloadID, &d.Volume, &d.State, &d.Reason, &d.CreatedBy, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		d.CreatedAt = time.Unix(0, createdAt).UTC()
		d.UpdatedAt = time.Unix(0, updatedAt).UTC()
		items = append(items, d)
	}
	return items, rows.Err()
}

// SyncDatabaseState derives the database state from its runtime workload.
func (s *Store) SyncDatabaseState(ctx context.Context, database *Database) {
	if database.WorkloadID == "" {
		return
	}
	workload, err := s.core.Get(ctx, database.WorkloadID)
	if err != nil {
		return
	}
	states := map[core.State]string{
		core.Running: "running", core.Starting: "starting", core.Assigned: "starting",
		core.Queued: "queued", core.Pending: "queued", core.Stopped: "stopped",
		core.Failed: "failed", core.Cancelled: "stopped", core.Stopping: "stopping",
		core.Unhealthy: "unhealthy",
	}
	if state, ok := states[workload.State]; ok && state != database.State {
		_, _ = s.db.ExecContext(ctx, "UPDATE databases SET state=?, updated_at=? WHERE id=?", state, time.Now().UTC().UnixNano(), database.ID)
		database.State = state
	}
}

func (s *Store) StopDatabase(ctx context.Context, projectID, id string) error {
	database, err := s.GetDatabase(ctx, projectID, id)
	if err != nil {
		return err
	}
	for {
		workload, err := s.core.Get(ctx, database.WorkloadID)
		if err != nil || workload.State.Terminal() || workload.CancelRequested {
			return err
		}
		workload.CancelRequested = true
		if err := s.core.Save(ctx, workload, false); !errors.Is(err, core.ErrConflict) {
			return err
		}
	}
}

func (s *Store) DeleteDatabase(ctx context.Context, projectID, id string) error {
	if _, err := s.GetDatabase(ctx, projectID, id); err != nil {
		return err
	}
	_ = s.StopDatabase(ctx, projectID, id)
	result, err := s.db.ExecContext(ctx, "DELETE FROM databases WHERE id=? AND project_id=?", id, projectID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}
