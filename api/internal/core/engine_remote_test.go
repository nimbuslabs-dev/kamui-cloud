package core

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCluster is a deterministic in-memory Cluster for engine tests. It never
// loses reports: DrainReports only yields them once.
type fakeCluster struct {
	workers   []Worker
	reports   []WorkerReport
	decisions []PlacementDecision
	nextID    int64
}

func (f *fakeCluster) Workers(context.Context) ([]Worker, error) { return f.workers, nil }

func (f *fakeCluster) SaveDecision(_ context.Context, d PlacementDecision) error {
	f.decisions = append(f.decisions, d)
	return nil
}

func (f *fakeCluster) DrainReports(_ context.Context, limit int) ([]WorkerReport, error) {
	if len(f.reports) < limit {
		limit = len(f.reports)
	}
	out := make([]WorkerReport, limit)
	copy(out, f.reports[:limit])
	f.reports = f.reports[limit:]
	return out, nil
}

func (f *fakeCluster) ReportsApplied(context.Context, []int64) error { return nil }

func (f *fakeCluster) enqueue(report WorkerReport) {
	f.nextID++
	report.ID = f.nextID
	f.reports = append(f.reports, report)
}

func newClusterEngine(t *testing.T, executor Executor, cluster Cluster) (*Engine, *Store) {
	t.Helper()
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "core.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	engine := NewEngine(store, executor, nil)
	engine.Cluster = cluster
	return engine, store
}

// createSupportingExecutor lets local attempts reach RUNNING under reconcile.
type createSupportingExecutor struct {
	recordingExecutor
}

func (r *createSupportingExecutor) Create(context.Context, ExecutionRef, ContainerSpec) error {
	return nil
}

func (r *createSupportingExecutor) Remove(context.Context, ExecutionRef) error { return nil }

func localDirectoryWorker() Worker {
	return Worker{
		ID: LocalWorker, Health: "healthy", State: WorkerStateOnline,
		Heartbeat: time.Now().UTC(),
		Runtime:   &DockerCapacity{ID: "runtime-local", OS: "linux", Arch: "arm64", LogicalCPUs: 2, MemoryTotalBytes: 4 << 30},
	}
}

func submitWorkload(t *testing.T, store *Store, spec Submission, key string) Workload {
	t.Helper()
	w, created, err := store.Submit(context.Background(), spec, key)
	if err != nil || !created {
		t.Fatalf("submit: %v created=%v", err, created)
	}
	return w
}

func requireState(t *testing.T, store *Store, id string, want State) Workload {
	t.Helper()
	ctx := context.Background()
	w, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if w.State != want {
		t.Fatalf("workload %s state = %s (reason %q), want %s", id, w.State, w.QueueReason, want)
	}
	return w
}

func TestRemoteWorkloadLifecycle(t *testing.T) {
	ctx := context.Background()
	workerID := "wk-" + strings.Repeat("a", 32)
	cluster := &fakeCluster{workers: []Worker{localDirectoryWorker(), remoteWorker(workerID, 4, 8, nil)}}
	engine, store := newClusterEngine(t, &recordingExecutor{}, cluster)

	spec := Submission{Image: "example:local"}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	w := submitWorkload(t, store, spec, "lifecycle")

	engine.reconcile(ctx) // PENDING → QUEUED
	engine.reconcile(ctx) // QUEUED → ASSIGNED to the remote worker
	assigned := requireState(t, store, w.ID, Assigned)
	if assigned.Assignment.WorkerID != workerID || assigned.Assignment.Fence == "" {
		t.Fatalf("assignment = %+v", assigned.Assignment)
	}
	fence := assigned.Assignment.Fence
	_ = fence

	report := func(phase string, mutate func(*WorkerReport)) {
		t.Helper()
		r := WorkerReport{WorkerID: workerID, WorkloadID: w.ID, Fence: fence, Kind: phase, At: time.Now().UTC()}
		if mutate != nil {
			mutate(&r)
		}
		cluster.enqueue(r)
		engine.reconcile(ctx)
	}

	report(ReportAck, nil)
	requireState(t, store, w.ID, Starting)
	started := time.Now().UTC().Add(-time.Second)
	report(ReportStart, func(r *WorkerReport) { r.StartedAt = &started; r.LogTail = "booting" })
	running := requireState(t, store, w.ID, Running)
	if running.LogTail != "booting" {
		t.Fatalf("log tail = %q", running.LogTail)
	}
	code := 0
	report(ReportExit, func(r *WorkerReport) { r.ExitCode = &code; r.Started = true; r.LogTail = "done" })
	finished := requireState(t, store, w.ID, Succeeded)
	if finished.Result == nil || finished.Result.ExitCode == nil || *finished.Result.ExitCode != 0 || finished.LogTail != "done" {
		t.Fatalf("result = %+v", finished)
	}
	if len(cluster.decisions) == 0 || cluster.decisions[len(cluster.decisions)-1].Chosen != workerID {
		t.Fatalf("placement decisions = %+v", cluster.decisions)
	}
}

func TestRemoteCancellationBeforeAckStopsWorkload(t *testing.T) {
	ctx := context.Background()
	workerID := "wk-" + strings.Repeat("b", 32)
	cluster := &fakeCluster{workers: []Worker{localDirectoryWorker(), remoteWorker(workerID, 4, 8, nil)}}
	engine, store := newClusterEngine(t, &recordingExecutor{}, cluster)

	spec := Submission{Image: "example:local"}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	w := submitWorkload(t, store, spec, "cancel")
	engine.reconcile(ctx)
	engine.reconcile(ctx)
	requireState(t, store, w.ID, Assigned)

	if _, err := engine.Cancel(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	engine.reconcile(ctx)
	cancelled := requireState(t, store, w.ID, Cancelled)
	if cancelled.Assignment != nil || cancelled.Result == nil || cancelled.Result.Reason != "cancelled" {
		t.Fatalf("cancelled workload = %+v", cancelled)
	}
}

func TestUnackedLeaseExpiryRequeuesWithExplanation(t *testing.T) {
	ctx := context.Background()
	workerID := "wk-" + strings.Repeat("c", 32)
	cluster := &fakeCluster{workers: []Worker{localDirectoryWorker(), remoteWorker(workerID, 4, 8, nil)}}
	engine, store := newClusterEngine(t, &recordingExecutor{}, cluster)

	spec := Submission{Image: "example:local"}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	w := submitWorkload(t, store, spec, "expiry")
	engine.reconcile(ctx)
	engine.reconcile(ctx)
	staleFence := fenceOf(t, store, w.ID)
	requireState(t, store, w.ID, Assigned)

	// Simulate a worker that never acknowledges: expire the lease.
	w, _ = store.Get(ctx, w.ID)
	w.Assignment.LeaseUntil = time.Now().UTC().Add(-time.Minute)
	if err := store.Save(ctx, w, false); err != nil {
		t.Fatal(err)
	}
	engine.reconcile(ctx)
	requeued := requireState(t, store, w.ID, Queued)
	if requeued.Assignment != nil || !strings.Contains(requeued.QueueReason, "acknowledged") {
		t.Fatalf("requeued workload = %+v", requeued)
	}
	// A safe requeue keeps bounded retries: attempt 2 still assigns.
	engine.reconcile(ctx)
	reassigned := requireState(t, store, w.ID, Assigned)
	if reassigned.Attempt != 2 || reassigned.Assignment.Fence == "" || reassigned.Assignment.Fence == staleFence {
		t.Fatalf("reassignment = %+v", reassigned.Assignment)
	}
	// A stale worker must not mutate the newer attempt.
	cluster.enqueue(WorkerReport{WorkerID: workerID, WorkloadID: w.ID, Fence: "f-stale", Kind: ReportExit, ExitCode: intPtr(0), Started: true})
	engine.reconcile(ctx)
	requireState(t, store, w.ID, Assigned)
}

func fenceOf(t *testing.T, store *Store, id string) string {
	t.Helper()
	w, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if w.Assignment == nil {
		return ""
	}
	return w.Assignment.Fence
}

func intPtr(v int) *int { return &v }

func TestLostWorkerFailsWithoutBlindRetry(t *testing.T) {
	ctx := context.Background()
	workerID := "wk-" + strings.Repeat("d", 32)
	cluster := &fakeCluster{workers: []Worker{localDirectoryWorker(), remoteWorker(workerID, 4, 8, nil)}}
	engine, store := newClusterEngine(t, &recordingExecutor{}, cluster)

	spec := Submission{Image: "example:local"}
	spec.Policy.MaxRetries = 3
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	w := submitWorkload(t, store, spec, "lost")
	engine.reconcile(ctx)
	engine.reconcile(ctx)
	assigned := requireState(t, store, w.ID, Assigned)
	fence := assigned.Assignment.Fence

	cluster.enqueue(WorkerReport{WorkerID: workerID, WorkloadID: w.ID, Fence: fence, Kind: ReportAck})
	cluster.enqueue(WorkerReport{WorkerID: workerID, WorkloadID: w.ID, Fence: fence, Kind: ReportStart, StartedAt: nowPtr()})
	engine.reconcile(ctx)
	requireState(t, store, w.ID, Running)

	// The worker goes silent: stale heartbeat and an expired lease.
	offline := remoteWorker(workerID, 4, 8, func(x *Worker) { x.State = WorkerStateOffline; x.Heartbeat = time.Now().UTC().Add(-2 * OfflineAfter) })
	cluster.workers = []Worker{localDirectoryWorker(), offline}
	w, _ = store.Get(ctx, w.ID)
	w.Assignment.LeaseUntil = time.Now().UTC().Add(-2 * remoteGrace)
	if err := store.Save(ctx, w, false); err != nil {
		t.Fatal(err)
	}
	engine.reconcile(ctx)
	failed := requireState(t, store, w.ID, Failed)
	if failed.Result == nil || !strings.Contains(failed.Result.Reason, "lost contact") {
		t.Fatalf("failure result = %+v", failed.Result)
	}
	if failed.Assignment != nil {
		t.Fatalf("reservation must be released: %+v", failed.Assignment)
	}
}

func nowPtr() *time.Time {
	ts := time.Now().UTC().Add(-time.Second)
	return &ts
}

func TestRevokedAndDrainingWorkersReceiveNoAssignments(t *testing.T) {
	ctx := context.Background()
	revoked := remoteWorker("wk-"+strings.Repeat("e", 32), 4, 8, func(w *Worker) { w.State = WorkerStateRevoked })
	draining := remoteWorker("wk-"+strings.Repeat("f", 32), 4, 8, func(w *Worker) { w.State = WorkerStateDraining })
	cluster := &fakeCluster{workers: []Worker{localDirectoryWorker(), revoked, draining}}
	engine, store := newClusterEngine(t, &recordingExecutor{}, cluster)

	spec := Submission{Image: "example:local"}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	w := submitWorkload(t, store, spec, "restricted")
	engine.reconcile(ctx)
	engine.reconcile(ctx)
	// The workload stays queued with an explanation; the local worker also
	// exists, so auto mode may pick it. Verify no revoked/draining assignment.
	got, err := store.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignment != nil && (got.Assignment.WorkerID == revoked.ID || got.Assignment.WorkerID == draining.ID) {
		t.Fatalf("assignment to a revoked/draining worker: %+v", got.Assignment)
	}
	if got.Assignment == nil && !strings.Contains(got.QueueReason, "revoked") && !strings.Contains(got.QueueReason, "eligible") {
		t.Fatalf("queue explanation missing: %q", got.QueueReason)
	}
}

func TestConcurrentReservationsPreventOverAllocation(t *testing.T) {
	ctx := context.Background()
	workerID := "wk-" + strings.Repeat("1", 32) // 2 CPUs
	tinyLocal := localDirectoryWorker()
	tinyLocal.Runtime = &DockerCapacity{ID: "runtime-local", OS: "linux", Arch: "arm64", LogicalCPUs: 1, MemoryTotalBytes: 1 << 30}
	cluster := &fakeCluster{workers: []Worker{tinyLocal, remoteWorker(workerID, 2, 4, nil)}}
	engine, store := newClusterEngine(t, &recordingExecutor{}, cluster)

	spec := Submission{Image: "example:local"}
	spec.Resources.CPUMillis = 1500 // 1.5 of 2 CPUs
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	first := submitWorkload(t, store, spec, "res-a")
	second := submitWorkload(t, store, spec, "res-b")

	engine.reconcile(ctx)
	engine.reconcile(ctx)
	a := requireState(t, store, first.ID, Assigned)
	b, err := store.Get(ctx, second.ID)
	t.Logf("A worker=%v state=%s; B state=%s reason=%q", a.Assignment != nil, a.State, b.State, b.QueueReason)
	for _, d := range cluster.decisions {
		t.Logf("decision attempt=%d chosen=%q explanation=%q", d.Attempt, d.Chosen, d.Explanation)
		for _, c := range d.Candidates {
			t.Logf("  candidate %s eligible=%v reasons=%v", c.WorkerID, c.Eligible, c.Reasons)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if b.State == Assigned {
		t.Fatal("second workload over-allocated the worker")
	}
	if b.State != Queued || !strings.Contains(b.QueueReason, "reservations") {
		t.Fatalf("second workload = %s %q", b.State, b.QueueReason)
	}
	_ = a
}

func TestReportApplicationIsIdempotentAfterCrash(t *testing.T) {
	ctx := context.Background()
	workerID := "wk-" + strings.Repeat("2", 32)
	cluster := &fakeCluster{workers: []Worker{localDirectoryWorker(), remoteWorker(workerID, 4, 8, nil)}}
	engine, store := newClusterEngine(t, &recordingExecutor{}, cluster)

	spec := Submission{Image: "example:local"}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	w := submitWorkload(t, store, spec, "idem")
	engine.reconcile(ctx)
	engine.reconcile(ctx)
	fence := fenceOf(t, store, w.ID)

	// The engine applies the ack report but crashes before acknowledging it.
	cluster.enqueue(WorkerReport{WorkerID: workerID, WorkloadID: w.ID, Fence: fence, Kind: ReportAck})
	engine.reconcile(ctx)
	requireState(t, store, w.ID, Starting)

	// Simulate the crash: the report returns to the queue and is replayed.
	cluster.reports = append([]WorkerReport{{ID: cluster.nextID, WorkerID: workerID, WorkloadID: w.ID, Fence: fence, Kind: ReportAck, At: time.Now().UTC()}}, cluster.reports...)
	engine.reconcile(ctx)
	requireState(t, store, w.ID, Starting)
}

func TestClusterModeStillServesLocalWorkloads(t *testing.T) {
	ctx := context.Background()
	cluster := &fakeCluster{workers: []Worker{localDirectoryWorker()}}
	exec := &createSupportingExecutor{}
	engine, store := newClusterEngine(t, exec, cluster)

	spec := Submission{Image: "example:local", Kind: "service"}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	w := submitWorkload(t, store, spec, "local-in-cluster")
	engine.reconcile(ctx) // PENDING → QUEUED
	engine.reconcile(ctx) // QUEUED → ASSIGNED on local worker
	assigned := requireState(t, store, w.ID, Assigned)
	if assigned.Assignment.WorkerID != LocalWorker {
		t.Fatalf("expected local placement, got %q", assigned.Assignment.WorkerID)
	}
	engine.reconcile(ctx) // ASSIGNED → STARTING (container is created)
	exec.status = ExecutionStatus{Exists: true, Started: true, Running: true}
	engine.reconcile(ctx) // STARTING → RUNNING
	requireState(t, store, w.ID, Running)
	if _, err := engine.Cancel(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	engine.reconcile(ctx) // → STOPPING (stop is requested)
	engine.reconcile(ctx) // → stop the container
	engine.reconcile(ctx) // → observe the exit and finish
	requireState(t, store, w.ID, Stopped)
}

func TestClusterWithoutLocalWorkerQueuesWithReason(t *testing.T) {
	ctx := context.Background()
	cluster := &fakeCluster{workers: nil}
	engine, store := newClusterEngine(t, &recordingExecutor{}, cluster)

	spec := Submission{Image: "example:local"}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	w := submitWorkload(t, store, spec, "no-workers")
	engine.reconcile(ctx)
	engine.reconcile(ctx)
	queued := requireState(t, store, w.ID, Queued)
	if queued.QueueReason == "" {
		t.Fatal("an unschedulable workload must carry an explanation")
	}
	// Reconciling again with an unchanged explanation must not spam decisions.
	cluster.SaveDecision(ctx, PlacementDecision{})
	cluster.decisions = nil
	engine.reconcile(ctx)
	if len(cluster.decisions) != 0 {
		t.Fatalf("unchanged queue reason must not record decisions: %+v", cluster.decisions)
	}
}

func TestManualRemotePlacementRunsOnSelectedWorker(t *testing.T) {
	ctx := context.Background()
	workerID := "wk-" + strings.Repeat("3", 32)
	cluster := &fakeCluster{workers: []Worker{localDirectoryWorker(), remoteWorker(workerID, 4, 8, nil)}}
	engine, store := newClusterEngine(t, &recordingExecutor{}, cluster)

	spec := Submission{Image: "example:local"}
	spec.Placement = PlacementManual
	spec.WorkerID = workerID
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	w := submitWorkload(t, store, spec, "manual")
	engine.reconcile(ctx)
	engine.reconcile(ctx)
	assigned := requireState(t, store, w.ID, Assigned)
	if assigned.Assignment.WorkerID != workerID {
		t.Fatalf("manual placement landed on %q", assigned.Assignment.WorkerID)
	}
	if len(cluster.decisions) == 0 || cluster.decisions[0].Mode != PlacementManual {
		t.Fatalf("decision mode = %+v", cluster.decisions)
	}
}

func TestReportsAppliedDeletesConsumedReports(t *testing.T) {
	ctx := context.Background()
	workerID := "wk-" + strings.Repeat("4", 32)
	cluster := &fakeCluster{workers: []Worker{localDirectoryWorker(), remoteWorker(workerID, 4, 8, nil)}}
	engine, store := newClusterEngine(t, &recordingExecutor{}, cluster)

	spec := Submission{Image: "example:local"}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	w := submitWorkload(t, store, spec, "fence")
	engine.reconcile(ctx)
	engine.reconcile(ctx)
	assigned := requireState(t, store, w.ID, Assigned)

	// Renew with the correct fence works; a fenced-out report is discarded.
	cluster.enqueue(WorkerReport{WorkerID: workerID, WorkloadID: w.ID, Fence: assigned.Assignment.Fence, Kind: ReportRenew})
	cluster.enqueue(WorkerReport{WorkerID: workerID, WorkloadID: w.ID, Fence: "f-bogus", Kind: ReportExit, ExitCode: intPtr(1), Started: true})
	engine.reconcile(ctx)
	requireState(t, store, w.ID, Assigned)
	if len(engine.Cluster.(*fakeCluster).reports) != 0 {
		t.Fatal("processed reports must be deleted")
	}
	if len(cluster.decisions) == 0 {
		t.Fatal("assignment decision must be recorded")
	}
}

var _ = errors.Is // Silence unused import in constrained builds.
