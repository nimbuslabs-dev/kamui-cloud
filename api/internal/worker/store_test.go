package worker

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
)

func openStores(t *testing.T) (*core.Store, *Store) {
	t.Helper()
	ctx := context.Background()
	coreStore, err := core.Open(ctx, filepath.Join(t.TempDir(), "core.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { coreStore.Close() })
	store := NewStore(coreStore.DB())
	store.UseLocalStore(coreStore)
	return coreStore, store
}

func TestEnrollmentLifecycle(t *testing.T) {
	ctx := context.Background()
	_, store := openStores(t)

	enrollment, token, err := store.CreateEnrollment(ctx, "local-admin", "Laptop arm64", time.Minute, false)
	if err != nil || token == "" || !strings.HasPrefix(token, EnrollmentPrefix) {
		t.Fatalf("create enrollment: %v token=%q", err, token)
	}
	listed, err := store.ListEnrollments(ctx)
	if err != nil || len(listed) != 1 || listed[0].ID != enrollment.ID {
		t.Fatalf("list enrollments: %v %+v", err, listed)
	}
	if strings.Contains(listed[0].Name, "  ") || listed[0].Name != "Laptop arm64" {
		t.Fatalf("name normalization: %q", listed[0].Name)
	}

	workerID, workerToken, approved, err := store.Enroll(ctx, token, "laptop", "laptop.local")
	if err != nil || approved {
		t.Fatalf("enroll: %v approved=%v", err, approved)
	}
	if !strings.HasPrefix(workerID, "wk") || !strings.HasPrefix(workerToken, TokenPrefix) {
		t.Fatalf("identity = %q token prefix ok=%v", workerID, strings.HasPrefix(workerToken, TokenPrefix+"-"))
	}

	// Single use: the token is consumed by the first enrollment.
	if _, _, _, err := store.Enroll(ctx, token, "laptop", "laptop.local"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("replayed enrollment token must be rejected: %v", err)
	}

	info, err := store.Get(ctx, workerID)
	if err != nil || info.Status != core.WorkerStatePending {
		t.Fatalf("pending worker: %v %+v", err, info)
	}
	approvedInfo, err := store.SetStatus(ctx, workerID, core.WorkerStateApproved)
	if err != nil || approvedInfo.Status != core.WorkerStateApproved || approvedInfo.ApprovedAt == nil {
		t.Fatalf("approve: %v %+v", err, approvedInfo)
	}
	if state := approvedInfo.State(time.Now().UTC()); state != core.WorkerStateOffline {
		t.Fatalf("fresh worker without heartbeat = %s", state)
	}

	// Worker token authentication.
	if id, err := store.AuthenticateWorker(ctx, workerToken); err != nil || id != workerID {
		t.Fatalf("authenticate: %v %q", err, id)
	}
	if _, err := store.AuthenticateWorker(ctx, "kwtforged"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("forged token: %v", err)
	}

	// Revocation invalidates the token and the worker.
	if _, err := store.SetStatus(ctx, workerID, core.WorkerStateRevoked); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AuthenticateWorker(ctx, workerToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("revoked worker token must be invalid")
	}
	if _, err := store.RequireWorker(ctx, workerID); !errors.Is(err, ErrForbidden) {
		t.Fatal("revoked worker must be forbidden")
	}
}

func TestEnrollmentExpiryAndRevocation(t *testing.T) {
	ctx := context.Background()
	_, store := openStores(t)

	if _, _, err := store.CreateEnrollment(ctx, "local-admin", "x", 0, false); !errors.Is(err, ErrBadInput) {
		t.Fatalf("zero TTL must be rejected: %v", err)
	}
	if _, _, err := store.CreateEnrollment(ctx, "local-admin", "x", 25*time.Hour, false); !errors.Is(err, ErrBadInput) {
		t.Fatalf("over-long TTL must be rejected: %v", err)
	}

	expired, token, err := store.CreateEnrollment(ctx, "local-admin", "expired", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	// Force expiry.
	if _, err := store.db.ExecContext(ctx, "UPDATE worker_enrollments SET expires_at=? WHERE id=?",
		time.Now().UTC().Add(-time.Second).UnixNano(), expired.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.Enroll(ctx, token, "late", ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired token must be rejected: %v", err)
	}

	revoked, token2, err := store.CreateEnrollment(ctx, "local-admin", "revoked", time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeEnrollment(ctx, revoked.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.Enroll(ctx, token2, "late", ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked token must be rejected: %v", err)
	}
	if err := store.RevokeEnrollment(ctx, "enr-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing enrollment: %v", err)
	}
}

func TestAutoApproveEnrollment(t *testing.T) {
	ctx := context.Background()
	_, store := openStores(t)
	_, token, err := store.CreateEnrollment(ctx, "local-admin", "trusted", time.Minute, true)
	if err != nil {
		t.Fatal(err)
	}
	workerID, _, approved, err := store.Enroll(ctx, token, "trusted-laptop", "")
	if err != nil || !approved {
		t.Fatalf("auto approve: %v %v", err, approved)
	}
	info, err := store.Get(ctx, workerID)
	if err != nil || info.Status != core.WorkerStateApproved {
		t.Fatalf("status: %v %+v", err, info)
	}
	if info.Policy.GPUAllowed {
		t.Fatal("GPU must stay forbidden by default")
	}
}

func TestHeartbeatAndTelemetry(t *testing.T) {
	ctx := context.Background()
	_, store := openStores(t)
	_, token, _ := store.CreateEnrollment(ctx, "local-admin", "hb", time.Minute, true)
	workerID, workerToken, _, err := store.Enroll(ctx, token, "hb-laptop", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AuthenticateWorker(ctx, workerToken); err != nil {
		t.Fatal(err)
	}

	cpus := 8
	total := int64(16 << 30)
	available := int64(8 << 30)
	storage := int64(200 << 30)
	caps := &core.Capabilities{
		Hostname: "hb-laptop", OS: "linux", Arch: "arm64",
		LogicalCPUs: &cpus, MemoryTotalBytes: &total, MemoryAvailableBytes: &available,
		StorageAvailable: &storage, DockerAvailable: boolPtr(true), Runtimes: []string{"docker"},
	}
	onBattery := false
	temp := 55.0
	telemetry := &core.Telemetry{CPUPct: floatPtr(12.5), OnBattery: &onBattery, TemperatureC: &temp}
	if err := store.Heartbeat(ctx, workerID, caps, telemetry, 42.0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := store.Heartbeat(ctx, workerID, caps, telemetry, 50.0); err != nil {
		t.Fatal(err)
	}
	info, err := store.Get(ctx, workerID)
	if err != nil {
		t.Fatal(err)
	}
	if state := info.State(time.Now().UTC()); state != core.WorkerStateOnline {
		t.Fatalf("state = %s", state)
	}
	if info.LatencyMS <= 40 || info.LatencyMS >= 51 {
		t.Fatalf("latency EWMA = %.1f", info.LatencyMS)
	}
	if info.Reliability <= 0.5 {
		t.Fatalf("reliability = %.2f", info.Reliability)
	}
	if info.Capabilities == nil || info.Capabilities.OS != "linux" {
		t.Fatalf("capabilities: %+v", info.Capabilities)
	}
	if info.Telemetry == nil || info.Telemetry.TemperatureC == nil || *info.Telemetry.TemperatureC != 55 {
		t.Fatalf("telemetry: %+v", info.Telemetry)
	}

	worker := info.AsCoreWorker(time.Now().UTC())
	if worker.Runtime == nil || worker.Runtime.LogicalCPUs != 8 || worker.State != core.WorkerStateOnline {
		t.Fatalf("core worker projection: %+v", worker)
	}
	if worker.Health != "healthy" {
		t.Fatalf("health = %q", worker.Health)
	}
}

func boolPtr(v bool) *bool { return &v }

func floatPtr(v float64) *float64 { return &v }

func TestPolicyValidation(t *testing.T) {
	ctx := context.Background()
	_, store := openStores(t)
	_, token, _ := store.CreateEnrollment(ctx, "local-admin", "policy", time.Minute, true)
	workerID, _, _, err := store.Enroll(ctx, token, "policy-laptop", "")
	if err != nil {
		t.Fatal(err)
	}

	valid := core.WorkerPolicy{
		CPUMillisMax: 4000, MemoryBytesMax: 8 << 30, GPUAllowed: false,
		WorkloadKinds: []string{"job"}, Projects: []string{"default"},
		Schedule: []core.ScheduleWindow{{Days: []int{1, 2, 3, 4, 5}, Start: "09:00", End: "17:00"}},
		Timezone: "Europe/Berlin", MaxConcurrent: 2,
	}
	if err := store.UpdatePolicy(ctx, workerID, valid); err != nil {
		t.Fatalf("valid policy: %v", err)
	}
	store.Get(ctx, workerID)
	info, _ := store.Get(ctx, workerID)
	if info.Policy.CPUMillisMax != 4000 || len(info.Policy.Schedule) != 1 {
		t.Fatalf("stored policy: %+v", info.Policy)
	}

	for name, bad := range map[string]core.WorkerPolicy{
		"negative cpu":   {CPUMillisMax: -1},
		"huge memory":    {MemoryBytesMax: 1 << 41},
		"bad kind":       {WorkloadKinds: []string{"cron"}},
		"bad timezone":   {Timezone: "Mars/Olympus"},
		"bad day":        {Schedule: []core.ScheduleWindow{{Days: []int{7}, Start: "00:00", End: "23:59"}}},
		"bad clock":      {Schedule: []core.ScheduleWindow{{Start: "24:00", End: "01:00"}}},
		"too concurrent": {MaxConcurrent: 65},
		"empty project":  {Projects: []string{""}},
	} {
		if err := store.UpdatePolicy(ctx, workerID, bad); !errors.Is(err, ErrBadInput) {
			t.Fatalf("%s must be rejected: %v", name, err)
		}
	}
	if err := store.UpdatePolicy(ctx, "wk-missing", valid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing worker policy: %v", err)
	}
}

func TestReportsAndAssignments(t *testing.T) {
	ctx := context.Background()
	coreStore, store := openStores(t)
	_, token, _ := store.CreateEnrollment(ctx, "local-admin", "reports", time.Minute, true)
	workerID, _, _, err := store.Enroll(ctx, token, "reports-laptop", "")
	if err != nil {
		t.Fatal(err)
	}

	spec := core.Submission{Image: "example:local"}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	workload, _, err := coreStore.Submit(ctx, spec, "report-test")
	if err != nil {
		t.Fatal(err)
	}

	// No assignment yet.
	items, err := store.WorkerAssignments(ctx, workerID)
	if err != nil || len(items) != 0 {
		t.Fatalf("assignments before scheduling: %v %+v", err, items)
	}

	// Simulate an assignment directly in the store (the engine does this).
	now := time.Now().UTC()
	workload.Assignment = &core.Assignment{
		WorkerID: workerID, RuntimeID: "worker:" + workerID, Attempt: 1, Fence: "f-test",
		LeaseUntil: now.Add(45 * time.Second), Deadline: now.Add(time.Minute),
		Reservation: core.Reservation{CPUMillis: 250, MemoryBytes: 128 << 20},
	}
	workload.State = core.Assigned
	if err := coreStore.Save(ctx, workload, false); err != nil {
		t.Fatal(err)
	}

	items, err = store.WorkerAssignments(ctx, workerID)
	if err != nil || len(items) != 1 || items[0].Assignment.Fence != "f-test" {
		t.Fatalf("assignments: %v %+v", err, items)
	}
	workers, err := store.Workers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, w := range workers {
		if w.ID == workerID {
			active = w.Active
		}
	}
	if active != 1 {
		t.Fatalf("active assignments = %d", active)
	}

	// Queue a report; draining peeks without deleting until acknowledged.
	if err := store.RecordReport(ctx, workerID, workload.ID, "f-test", core.ReportAck, []byte(`{"exit_code":null}`)); err != nil {
		t.Fatal(err)
	}
	reports, err := store.DrainReports(ctx, 10)
	if err != nil || len(reports) != 1 || reports[0].Kind != core.ReportAck || reports[0].Fence != "f-test" {
		t.Fatalf("drain: %v %+v", err, reports)
	}
	again, err := store.DrainReports(ctx, 10)
	if err != nil || len(again) != 1 {
		t.Fatalf("drain must not delete before acknowledgement: %v %+v", err, again)
	}
	if err := store.ReportsApplied(ctx, []int64{reports[0].ID}); err != nil {
		t.Fatal(err)
	}
	final, err := store.DrainReports(ctx, 10)
	if err != nil || len(final) != 0 {
		t.Fatalf("acknowledged reports must be deleted: %v %+v", err, final)
	}

	if err := store.RecordReport(ctx, workerID, workload.ID, "f-test", "bogus", nil); !errors.Is(err, ErrBadInput) {
		t.Fatalf("unknown report kind: %v", err)
	}
}

func TestProjectPrefs(t *testing.T) {
	ctx := context.Background()
	_, store := openStores(t)

	prefs, err := store.ProjectPrefs(ctx, "default")
	if err != nil || prefs.Mode != core.PlacementAuto {
		t.Fatalf("default prefs: %v %+v", err, prefs)
	}
	manual := "local"
	saved, err := store.SaveProjectPrefs(ctx, "default", core.PlacementManual, []string{manual}, nil, "local-admin")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Mode != core.PlacementManual || saved.PreferredWorkers[0] != manual {
		t.Fatalf("saved prefs: %+v", saved)
	}
	preferred := false
	if _, err := store.SaveProjectPrefs(ctx, "default", core.PlacementPreferred, []string{"local"}, &preferred, "local-admin"); err != nil {
		t.Fatal(err)
	}
	prefs, _ = store.ProjectPrefs(ctx, "default")
	if prefs.Fallback {
		t.Fatal("explicit fallback=false must persist")
	}
	if _, err := store.SaveProjectPrefs(ctx, "default", core.PlacementManual, []string{"a", "b"}, nil, "x"); !errors.Is(err, ErrBadInput) {
		t.Fatalf("manual mapping requires one worker: %v", err)
	}
	if _, err := store.SaveProjectPrefs(ctx, "default", core.PlacementPreferred, nil, nil, "x"); !errors.Is(err, ErrBadInput) {
		t.Fatalf("preferred mapping requires workers: %v", err)
	}
	if _, err := store.SaveProjectPrefs(ctx, "default", "chaos", nil, nil, "x"); !errors.Is(err, ErrBadInput) {
		t.Fatalf("unknown mode: %v", err)
	}
}

func TestWorkerTokenHashing(t *testing.T) {
	ctx := context.Background()
	_, store := openStores(t)
	_, token, _ := store.CreateEnrollment(ctx, "local-admin", "hash", time.Minute, true)
	_, workerToken, _, err := store.Enroll(ctx, token, "hash-laptop", "")
	if err != nil {
		t.Fatal(err)
	}
	// The stored credential must never be the raw token.
	rows, err := store.db.QueryContext(ctx, "SELECT token_hash FROM worker_tokens")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			t.Fatal(err)
		}
		if hash == workerToken {
			t.Fatal("worker token must be stored hashed")
		}
		if hash != identity.HashToken(workerToken) {
			t.Fatal("token hash must match the identity scheme")
		}
	}
}
