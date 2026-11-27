package core

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

type recordingExecutor struct {
	Executor
	status       ExecutionStatus
	inspectError error
	stopped      bool
}

func (r *recordingExecutor) Inspect(context.Context, ExecutionRef) (ExecutionStatus, error) {
	return r.status, r.inspectError
}

func (r *recordingExecutor) Stop(context.Context, ExecutionRef) error {
	r.stopped = true
	r.status.Running = false
	return nil
}

func (*recordingExecutor) Logs(context.Context, ExecutionRef) (string, error) {
	return "last line", nil
}

func TestEngineReconcilesCancelledServiceWithoutReassigning(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "core.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	spec := Submission{Image: "example:local", Kind: "service"}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	workload, _, err := store.Submit(ctx, spec, "service")
	if err != nil {
		t.Fatal(err)
	}
	exec := &recordingExecutor{status: ExecutionStatus{Exists: true, Started: true, Running: true}}
	engine := NewEngine(store, exec, nil)
	worker := Worker{ID: LocalWorker, Health: "healthy", Runtime: &DockerCapacity{ID: "runtime", OS: "linux", Arch: "arm64", LogicalCPUs: 1, MemoryTotalBytes: 1 << 30}}
	reserved := Reservation{}
	next := func(want State) {
		t.Helper()
		w, err := store.Get(ctx, workload.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.advance(ctx, w, worker, &reserved); err != nil {
			t.Fatal(err)
		}
		w, err = store.Get(ctx, workload.ID)
		if err != nil || w.State != want {
			t.Fatalf("state = %s, want %s, err = %v", w.State, want, err)
		}
	}
	next(Queued)
	next(Assigned)
	if reserved.CPUMillis != spec.Resources.CPUMillis {
		t.Fatalf("resource reservation = %+v", reserved)
	}
	next(Starting)
	next(Running)
	exec.status.Unhealthy = true
	next(Unhealthy)
	exec.status.Unhealthy = false
	next(Running)

	if _, err := engine.Cancel(ctx, workload.ID); err != nil {
		t.Fatal(err)
	}
	next(Stopping)
	worker.Runtime.ID = "different-runtime"
	next(Stopping)
	if exec.stopped {
		t.Fatal("must not stop execution on a replacement Docker daemon")
	}
	worker.Runtime.ID = "runtime"
	exec.inspectError = errors.New("daemon unreachable")
	stopping, _ := store.Get(ctx, workload.ID)
	stopping.Assignment.LeaseUntil = time.Now().Add(-time.Minute)
	if err := engine.advance(ctx, stopping, worker, &reserved); !errors.Is(err, exec.inspectError) {
		t.Fatalf("inspect error = %v", err)
	}
	stillStopping, _ := store.Get(ctx, workload.ID)
	if stillStopping.Assignment == nil || stillStopping.Attempt != 1 {
		t.Fatalf("timed-out lease lost its assignment: %+v", stillStopping)
	}
	exec.inspectError = nil
	next(Stopping)
	next(Stopped)
	out, err := engine.Logs(ctx, workload.ID)
	if !exec.stopped || err != nil || out.Tail != "last line" || out.Attempt != 1 {
		t.Fatalf("final logs = %+v, %v", out, err)
	}
	cleanup, err := store.Cleanup(ctx)
	if err != nil || len(cleanup) != 1 || cleanup[0].Attempt != 1 {
		t.Fatalf("cleanup = %+v, %v", cleanup, err)
	}
}

func TestLocalPlacementHonorsPlatformAndReservations(t *testing.T) {
	w := Worker{ID: LocalWorker, Health: "healthy", Runtime: &DockerCapacity{OS: "linux", Arch: "arm64", LogicalCPUs: 1, MemoryTotalBytes: 1 << 30}}
	s := Submission{WorkerID: LocalWorker, Resources: Resources{CPUMillis: 500, MemoryBytes: 1 << 28, Arch: "arm64"}}
	placement := LocalPlacement{}
	if id, reason := placement.Select(s, w, Reservation{}); id != LocalWorker || reason != "" {
		t.Fatalf("compatible workload = %q, %q", id, reason)
	}
	if id, reason := placement.Select(s, w, Reservation{CPUMillis: 600}); id != "" || reason == "" {
		t.Fatalf("reserved capacity = %q, %q", id, reason)
	}
	s.Resources.Arch = "amd64"
	if id, reason := placement.Select(s, w, Reservation{}); id != "" || reason == "" {
		t.Fatalf("invalid platform = %q, %q", id, reason)
	}
}
