package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

const (
	heartbeatInterval = 10 * time.Second
	leaseDuration     = 45 * time.Second
	// remoteGrace is how long an acknowledged but silent worker assignment
	// survives before the coordinator gives up waiting for a report.
	remoteGrace = 45 * time.Second
)

// Cluster is the optional multi-worker scheduling surface. When nil the engine
// keeps the single-host behavior unchanged.
type Cluster interface {
	// Workers returns the local worker plus every contributed worker with
	// fresh lifecycle state, capabilities and telemetry.
	Workers(context.Context) ([]Worker, error)
	// SaveDecision persists an explainable placement decision.
	SaveDecision(context.Context, PlacementDecision) error
	// DrainReports reads durable worker reports in submission order.
	DrainReports(context.Context, int) ([]WorkerReport, error)
	// ReportsApplied acknowledges processed reports so they are not replayed.
	ReportsApplied(context.Context, []int64) error
}

type Probe func(context.Context) (*Device, error)

type Engine struct {
	Repo      Repository
	Executor  Executor
	Placement Placement
	Probe     Probe
	Logger    *slog.Logger
	Cluster   Cluster
	// Volumes optionally provides persistent volume ownership for stateful
	// placement constraints. nil keeps stateless scheduling behavior.
	Volumes VolumeDirectory
	// EnvResolver expands `secret:NAME` environment references at container
	// create time. nil keeps environment entries as-is.
	EnvResolver func(ctx context.Context, projectID string, env map[string]string) (map[string]string, error)
}

// VolumeDirectory is the persistent storage locality surface consulted by the
// scheduler. A stateful workload may only run where its volumes already live;
// the coordinator never recreates an empty volume on another worker.
type VolumeDirectory interface {
	VolumeOwner(ctx context.Context, projectID, name string) (string, error)
	RecordVolumePlacement(ctx context.Context, projectID, name, workerID string, sizeBytes int64) error
}

func newID() string {
	var bytes [16]byte
	rand.Read(bytes[:])
	return "wl-" + hex.EncodeToString(bytes[:])
}

func newFence() string {
	var bytes [16]byte
	rand.Read(bytes[:])
	return "f-" + hex.EncodeToString(bytes[:])
}

func NewEngine(repo Repository, executor Executor, probe Probe) *Engine {
	return &Engine{Repo: repo, Executor: executor, Placement: LocalPlacement{}, Probe: probe, Logger: slog.Default()}
}

func (e *Engine) Run(ctx context.Context) {
	heartbeatDone := make(chan struct{})
	go func() { defer close(heartbeatDone); e.heartbeat(ctx) }()
	defer func() { <-heartbeatDone }()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		e.reconcile(ctx)
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

func (e *Engine) reconcile(ctx context.Context) {
	if e.Cluster != nil {
		e.reconcileCluster(ctx)
		return
	}
	e.reconcileLocal(ctx)
}

// Register before serving requests, even when Docker or hardware probing is unavailable.
func (e *Engine) Register(ctx context.Context) error {
	return e.Repo.SaveWorker(ctx, Worker{ID: LocalWorker, Health: "probing", Heartbeat: time.Now().UTC()})
}

func (e *Engine) heartbeat(ctx context.Context) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		worker := Worker{ID: LocalWorker, Health: "unavailable"}
		capCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		capacity, err := e.Executor.Capacity(capCtx)
		cancel()
		if err == nil && capacity.LogicalCPUs > 0 && capacity.MemoryTotalBytes > 0 {
			worker.Health, worker.Runtime = "healthy", &capacity
		}
		if e.Probe != nil {
			probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			worker.Device, err = e.Probe(probeCtx)
			cancel()
			if err != nil && ctx.Err() == nil {
				e.Logger.Warn("hardware probe unavailable", "error", err)
			}
		}
		worker.Heartbeat = time.Now().UTC()
		if err := e.Repo.SaveWorker(ctx, worker); err != nil && ctx.Err() == nil {
			e.Logger.Error("save worker heartbeat", "error", err)
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

func (e *Engine) reconcileLocal(ctx context.Context) {
	worker, err := e.Repo.Worker(ctx)
	if err != nil {
		return
	}
	if time.Since(worker.Heartbeat) > leaseDuration {
		worker.Health = "unavailable"
	}
	items, err := e.Repo.List(ctx, true)
	if err != nil {
		e.Logger.Error("list active workloads", "error", err)
		return
	}
	var reserved Reservation
	for _, w := range items {
		if w.Assignment != nil {
			r := w.Assignment.Reservation
			reserved.CPUMillis += r.CPUMillis
			reserved.MemoryBytes += r.MemoryBytes
		}
	}
	for _, w := range items {
		if ctx.Err() != nil {
			return
		}
		if time.Since(worker.Heartbeat) > leaseDuration {
			worker.Health = "unavailable"
		}
		op, cancel := context.WithTimeout(ctx, 9*time.Second)
		err := e.advance(op, w, worker, &reserved)
		cancel()
		if err != nil && !errors.Is(err, ErrConflict) && ctx.Err() == nil {
			e.Logger.Warn("reconcile workload", "workload_id", w.ID, "attempt", w.Attempt, "state", w.State, "error", err)
		}
	}
	if worker.Health == "healthy" {
		e.cleanup(ctx)
	}
}

// reconcileCluster schedules across the local worker and contributed workers.
// Reports are applied first so worker progress is durable before scheduling.
func (e *Engine) reconcileCluster(ctx context.Context) {
	e.applyReports(ctx)
	workers, err := e.Cluster.Workers(ctx)
	if err != nil {
		if ctx.Err() == nil {
			e.Logger.Error("read worker directory", "error", err)
		}
		return
	}
	byID := make(map[string]Worker, len(workers))
	for _, w := range workers {
		if w.ID == LocalWorker && time.Since(w.Heartbeat) > leaseDuration {
			w.Health = "unavailable"
		}
		byID[w.ID] = w
	}
	items, err := e.Repo.List(ctx, true)
	if err != nil {
		e.Logger.Error("list active workloads", "error", err)
		return
	}
	now := time.Now().UTC()
	reserved := make(map[string]Reservation, len(workers))
	pressure := make(map[string]int, len(workers))
	for _, w := range items {
		if w.Assignment != nil {
			r := w.Assignment.Reservation
			res := reserved[w.Assignment.WorkerID]
			res.CPUMillis += r.CPUMillis
			res.MemoryBytes += r.MemoryBytes
			reserved[w.Assignment.WorkerID] = res
			pressure[w.Assignment.WorkerID]++
		}
	}

	for _, w := range items {
		if w.State != Pending {
			continue
		}
		op, cancel := context.WithTimeout(ctx, 9*time.Second)
		w.State = Queued
		err := e.Repo.Save(op, w, false)
		cancel()
		if err != nil && !errors.Is(err, ErrConflict) && ctx.Err() == nil {
			e.Logger.Warn("queue workload", "workload_id", w.ID, "error", err)
		}
	}

	// Fair order: priority, then project share, then arrival.
	queued := OrderQueue(items)
	for _, w := range queued {
		if ctx.Err() != nil {
			return
		}
		var choice, reason string
		var decision PlacementDecision
		submission := w.Submission
		if submission.Stateful() && e.Volumes != nil {
			owner := ""
			conflict := false
			for _, volume := range submission.Volumes {
				holder, err := e.Volumes.VolumeOwner(ctx, w.ProjectID, volume.Name)
				if err != nil {
					e.Logger.Warn("read volume placement", "workload_id", w.ID, "error", err)
					break
				}
				if holder != "" && owner != "" && holder != owner {
					conflict = true
					break
				}
				if holder != "" {
					owner = holder
				}
			}
			switch {
			case conflict:
				choice, reason, decision = "", "persistent volumes are pinned to different workers; manual recovery required", PlacementDecision{Mode: submission.Placement, At: now, Candidates: []DecisionCandidate{}}
			case owner != "":
				// Stateful workloads run only where their volumes live.
				if submission.Placement == PlacementManual && submission.WorkerID != owner {
					choice, reason, decision = "", fmt.Sprintf("volume %s is pinned to worker %s; refusing to run stateful data elsewhere", submission.Volumes[0].Name, owner), PlacementDecision{Mode: submission.Placement, At: now, Candidates: []DecisionCandidate{}}
				} else {
					submission.Placement = PlacementManual
					submission.WorkerID = owner
					choice, reason, decision = SelectPlacement(submission, w.ProjectID, w.SubmitterID, workers, reserved, pressure, now)
				}
			default:
				choice, reason, decision = SelectPlacement(submission, w.ProjectID, w.SubmitterID, workers, reserved, pressure, now)
			}
		} else {
			choice, reason, decision = SelectPlacement(w.Submission, w.ProjectID, w.SubmitterID, workers, reserved, pressure, now)
		}
		decision.Attempt = w.Attempt
		if choice == "" {
			if reason == w.QueueReason {
				continue
			}
			w.QueueReason = reason
			op, cancel := context.WithTimeout(ctx, 9*time.Second)
			err := e.Repo.Save(op, w, false)
			cancel()
			if err == nil {
				_ = e.Cluster.SaveDecision(ctx, decision)
			} else if !errors.Is(err, ErrConflict) && ctx.Err() == nil {
				e.Logger.Warn("record queue reason", "workload_id", w.ID, "error", err)
			}
			continue
		}
		worker := byID[choice]
		r := w.Submission.Resources
		w.Attempt++
		w.State, w.QueueReason, w.Result, w.LogTail = Assigned, "", nil, ""
		w.Assignment = &Assignment{
			WorkerID:    choice,
			RuntimeID:   runtimeID(worker),
			Attempt:     w.Attempt,
			Fence:       newFence(),
			LeaseUntil:  now.Add(leaseDuration),
			Reservation: Reservation{CPUMillis: r.CPUMillis, MemoryBytes: r.MemoryBytes},
		}
		decision.WorkloadID = w.ID
		decision.Attempt = w.Attempt
		decision.Chosen = choice
		op, cancel := context.WithTimeout(ctx, 9*time.Second)
		err = e.Repo.Save(op, w, false)
		cancel()
		if err != nil {
			if !errors.Is(err, ErrConflict) && ctx.Err() == nil {
				e.Logger.Warn("assign workload", "workload_id", w.ID, "error", err)
			}
			continue
		}
		if derr := e.Cluster.SaveDecision(ctx, decision); derr != nil {
			e.Logger.Error("save assignment decision", "error", derr)
		}
		res := reserved[choice]
		res.CPUMillis += r.CPUMillis
		res.MemoryBytes += r.MemoryBytes
		reserved[choice] = res
		pressure[choice]++
	}

	for _, w := range items {
		if ctx.Err() != nil {
			return
		}
		if w.Assignment == nil {
			// Cancelled while queued, or still waiting on placement.
			if w.CancelRequested && !w.State.Terminal() {
				state := Cancelled
				if w.Submission.Kind == "service" {
					state = Stopped
				}
				w.State = state
				w.Result = &Result{Reason: "cancelled"}
				op, cancel := context.WithTimeout(ctx, 9*time.Second)
				err := e.Repo.Save(op, w, false)
				cancel()
				if err != nil && !errors.Is(err, ErrConflict) && ctx.Err() == nil {
					e.Logger.Warn("cancel queued workload", "workload_id", w.ID, "error", err)
				}
			}
			continue
		}
		if w.Assignment.WorkerID == LocalWorker {
			worker := byID[LocalWorker]
			local := reserved[LocalWorker]
			op, cancel := context.WithTimeout(ctx, 9*time.Second)
			err := e.advance(op, w, worker, &local)
			cancel()
			reserved[LocalWorker] = local
			if err != nil && !errors.Is(err, ErrConflict) && ctx.Err() == nil {
				e.Logger.Warn("reconcile local workload", "workload_id", w.ID, "attempt", w.Attempt, "state", w.State, "error", err)
			}
			continue
		}
		op, cancel := context.WithTimeout(ctx, 9*time.Second)
		err := e.advanceRemote(op, w, byID[w.Assignment.WorkerID], time.Now().UTC())
		cancel()
		if err != nil && !errors.Is(err, ErrConflict) && ctx.Err() == nil {
			e.Logger.Warn("reconcile remote workload", "workload_id", w.ID, "attempt", w.Attempt, "state", w.State, "error", err)
		}
	}
	if byID[LocalWorker].Health == "healthy" {
		e.cleanup(ctx)
	}
}

func runtimeID(worker Worker) string {
	if worker.ID == LocalWorker && worker.Runtime != nil {
		return worker.Runtime.ID
	}
	return "worker:" + worker.ID
}

func (e *Engine) applyReports(ctx context.Context) {
	reports, err := e.Cluster.DrainReports(ctx, 64)
	if err != nil {
		if ctx.Err() == nil {
			e.Logger.Error("drain worker reports", "error", err)
		}
		return
	}
	if len(reports) == 0 {
		return
	}
	applied := make([]int64, 0, len(reports))
	for _, report := range reports {
		if e.applyReport(ctx, report) {
			applied = append(applied, report.ID)
		}
	}
	if len(applied) > 0 {
		if err := e.Cluster.ReportsApplied(ctx, applied); err != nil && ctx.Err() == nil {
			e.Logger.Error("acknowledge worker reports", "error", err)
		}
	}
}

// applyReport validates fencing before mutating a workload. It reports whether
// the report was consumed (including fenced-out and obsolete reports, which
// must not be replayed).
func (e *Engine) applyReport(ctx context.Context, report WorkerReport) bool {
	w, err := e.Repo.Get(ctx, report.WorkloadID)
	if errors.Is(err, ErrNotFound) {
		return true
	}
	if err != nil {
		return false
	}
	if w.Assignment == nil || w.Assignment.WorkerID != report.WorkerID ||
		w.Assignment.Fence == "" || w.Assignment.Fence != report.Fence || w.State.Terminal() {
		return true // fenced out: a stale worker must never mutate a newer attempt
	}
	now := time.Now().UTC()
	switch report.Kind {
	case ReportRenew:
		w.Assignment.LeaseUntil = now.Add(leaseDuration)
		return e.saveReport(ctx, w)
	case ReportAck:
		if !w.Assignment.AckedAt.IsZero() {
			return true
		}
		w.Assignment.AckedAt = now
		if w.State == Assigned {
			budget := leaseDuration
			if w.Submission.Kind == "service" {
				budget = time.Duration(w.Submission.Policy.TimeoutSeconds) * time.Second
			}
			w.Assignment.Deadline = now.Add(budget)
			w.State = Starting
		}
		return e.saveReport(ctx, w)
	case ReportStart:
		startedAt := now
		if report.StartedAt != nil && !report.StartedAt.IsZero() {
			startedAt = report.StartedAt.UTC()
		}
		if w.Assignment.AckedAt.IsZero() {
			w.Assignment.AckedAt = now
		}
		if w.State == Assigned || w.State == Starting {
			w.State = Running
			if w.Submission.Kind == "job" {
				w.Assignment.Deadline = startedAt.Add(time.Duration(w.Submission.Policy.TimeoutSeconds) * time.Second)
			}
		}
		w.Assignment.LeaseUntil = now.Add(leaseDuration)
		w.LogTail = report.LogTail
		return e.saveReport(ctx, w)
	case ReportExit:
		return e.finishRemote(ctx, w, report, now)
	default:
		return true
	}
}

func (e *Engine) saveReport(ctx context.Context, w Workload) bool {
	if err := e.Repo.Save(ctx, w, false); err != nil && !errors.Is(err, ErrConflict) && ctx.Err() == nil {
		e.Logger.Warn("apply worker report", "workload_id", w.ID, "error", err)
	}
	return true
}

func (e *Engine) finishRemote(ctx context.Context, w Workload, report WorkerReport, now time.Time) bool {
	code := report.ExitCode
	var state State
	var reason string
	retry := false
	switch {
	case w.CancelRequested:
		state, reason = Cancelled, "cancelled"
		if w.Submission.Kind == "service" {
			state = Stopped
		}
	case w.StopReason == "timeout":
		state, reason = Failed, "timeout"
	case !report.Started:
		state, reason = Failed, "worker reported that the attempt never started"
		retry = w.Attempt <= w.Submission.Policy.MaxRetries
	case code == nil:
		state, reason = Failed, "worker reported no exit code"
	case *code == 0:
		state, reason = Succeeded, ""
	default:
		state, reason = Failed, "nonzero exit code"
		retry = w.Attempt <= w.Submission.Policy.MaxRetries
	}
	w.State, w.Assignment, w.StopReason = state, nil, ""
	w.Result = &Result{ExitCode: code, Reason: reason}
	w.LogTail = report.LogTail
	if retry {
		w.State, w.QueueReason = Queued, "retry after "+reason
	}
	if err := e.Repo.Save(ctx, w, false); err != nil && !errors.Is(err, ErrConflict) && ctx.Err() == nil {
		e.Logger.Warn("finish remote attempt", "workload_id", w.ID, "error", err)
	} else if err == nil {
		e.Logger.Info("remote attempt finished", "workload_id", w.ID, "attempt", w.Attempt, "state", w.State, "reason", reason)
	}
	return true
}

// advanceRemote drives the remote half of the assignment lifecycle: cancel
// intent, lease expiry before acknowledgement, deadlines and lost workers.
func (e *Engine) advanceRemote(ctx context.Context, w Workload, worker Worker, now time.Time) error {
	if w.Assignment == nil {
		return nil
	}
	if w.CancelRequested {
		if w.State == Assigned && w.Assignment.AckedAt.IsZero() {
			state := Cancelled
			if w.Submission.Kind == "service" {
				state = Stopped
			}
			w.State, w.Assignment = state, nil
			w.Result = &Result{Reason: "cancelled"}
			return e.Repo.Save(ctx, w, false)
		}
		if w.State != Stopping {
			w.State, w.StopReason = Stopping, "cancelled"
			return e.Repo.Save(ctx, w, false)
		}
	}
	online := workerActive(worker, now)
	switch w.State {
	case Assigned:
		if now.After(w.Assignment.LeaseUntil) {
			if w.Attempt <= w.Submission.Policy.MaxRetries+3 {
				w.State, w.Assignment = Queued, nil
				w.QueueReason = "assignment expired before the worker acknowledged it"
				return e.Repo.Save(ctx, w, false)
			}
			w.State, w.Assignment = Failed, nil
			w.Result = &Result{Reason: "repeatedly failed to reach an acknowledging worker"}
			return e.Repo.Save(ctx, w, false)
		}
		return nil
	case Starting, Running, Unhealthy:
		if w.Submission.Kind == "job" && now.After(w.Assignment.Deadline) ||
			w.Submission.Kind == "service" && w.State == Starting && now.After(w.Assignment.Deadline) {
			w.State, w.StopReason = Stopping, "timeout"
			return e.Repo.Save(ctx, w, false)
		}
		if !online && now.After(w.Assignment.LeaseUntil.Add(remoteGrace)) {
			return e.finishLostWorker(ctx, w)
		}
		return nil
	case Stopping:
		if !online && now.After(w.Assignment.LeaseUntil.Add(remoteGrace)) {
			return e.finishLostWorker(ctx, w)
		}
		return nil
	}
	return nil
}

func workerActive(worker Worker, now time.Time) bool {
	if worker.ID == "" {
		return false
	}
	return now.Sub(worker.Heartbeat) <= OfflineAfter &&
		(worker.State == WorkerStateOnline || worker.State == WorkerStateDraining)
}

// finishLostWorker ends an assignment without confirmation. Lost contact after
// an attempt started is never retried automatically: side effects may have
// happened and duplicating them is worse than reporting the loss.
func (e *Engine) finishLostWorker(ctx context.Context, w Workload) error {
	var state State
	var reason string
	if w.CancelRequested || w.StopReason == "cancelled" {
		state, reason = Cancelled, "cancelled"
		if w.Submission.Kind == "service" {
			state = Stopped
		}
	} else if w.StopReason == "timeout" {
		state, reason = Failed, "timeout"
	} else {
		state, reason = Failed, "worker lost contact during execution; not retried to avoid duplicate side effects"
	}
	w.State, w.Assignment, w.StopReason = state, nil, ""
	w.Result = &Result{Reason: reason}
	if err := e.Repo.Save(ctx, w, false); err != nil {
		return err
	}
	e.Logger.Warn("worker lost contact", "workload_id", w.ID, "attempt", w.Attempt, "state", state)
	return nil
}

func (e *Engine) cleanup(ctx context.Context) {
	refs, err := e.Repo.Cleanup(ctx)
	if err != nil {
		e.Logger.Warn("read execution cleanup queue", "error", err)
		return
	}
	for _, ref := range refs {
		op, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := e.Executor.Remove(op, ref)
		cancel()
		if err == nil {
			err = e.Repo.Cleaned(ctx, ref)
		}
		if err != nil && ctx.Err() == nil {
			e.Logger.Warn("clean up execution", "workload_id", ref.WorkloadID, "attempt", ref.Attempt, "error", err)
		}
	}
}

func (e *Engine) advance(ctx context.Context, w Workload, worker Worker, reserved *Reservation) error {
	now := time.Now().UTC()
	if w.CancelRequested && (w.Assignment == nil || w.State == Assigned) {
		if w.Submission.Kind == "service" {
			w.State = Stopped
		} else {
			w.State = Cancelled
		}
		w.Assignment, w.Result = nil, &Result{Reason: "cancelled"}
		return e.Repo.Save(ctx, w, false)
	}
	switch w.State {
	case Pending:
		w.State = Queued
		return e.Repo.Save(ctx, w, false)
	case Queued:
		id, reason := e.Placement.Select(w.Submission, worker, *reserved)
		if id == "" {
			if reason == w.QueueReason {
				return nil
			}
			w.QueueReason = reason
			return e.Repo.Save(ctx, w, false)
		}
		r := w.Submission.Resources
		w.Attempt++
		w.State, w.QueueReason, w.Result, w.LogTail = Assigned, "", nil, ""
		w.Assignment = &Assignment{
			WorkerID: id, Attempt: w.Attempt, LeaseUntil: now.Add(leaseDuration),
			RuntimeID:   worker.Runtime.ID,
			Reservation: Reservation{CPUMillis: r.CPUMillis, MemoryBytes: r.MemoryBytes},
		}
		if err := e.Repo.Save(ctx, w, false); err != nil {
			return err
		}
		reserved.CPUMillis += r.CPUMillis
		reserved.MemoryBytes += r.MemoryBytes
		return nil
	}

	// Persist intent before attempting a Docker side effect.
	if w.State != Stopping && w.CancelRequested {
		w.State, w.StopReason = Stopping, "cancelled"
		return e.Repo.Save(ctx, w, false)
	}
	if w.Assignment == nil || worker.Health != "healthy" || worker.Runtime == nil || worker.Runtime.ID != w.Assignment.RuntimeID {
		return nil
	}
	if w.State == Assigned {
		startupBudget := leaseDuration
		if w.Submission.Kind == "service" {
			startupBudget = time.Duration(w.Submission.Policy.TimeoutSeconds) * time.Second
		}
		w.Assignment.Deadline = now.Add(startupBudget)
		w.State = Starting
		return e.Repo.Save(ctx, w, false)
	}
	ref := ExecutionRef{WorkloadID: w.ID, Attempt: w.Attempt}
	status, err := e.Executor.Inspect(ctx, ref)
	if err != nil {
		return err
	}
	if w.State == Stopping {
		if status.Exists && (status.Running || !status.Started) {
			return e.Executor.Stop(ctx, ref)
		}
		return e.finishObserved(ctx, w, ref, status)
	}

	deadline := w.Assignment.Deadline
	expired := now.After(deadline) && !status.Started && w.State == Starting
	if status.Started && w.State == Starting {
		expired = status.StartedAt.After(deadline)
	}
	if w.Submission.Kind == "job" && w.State == Running {
		expired = status.FinishedAt.After(deadline) || (status.Running && now.After(deadline))
	}
	if expired {
		w.State, w.StopReason = Stopping, "timeout"
		return e.Repo.Save(ctx, w, false)
	}
	if w.State == Starting {
		switch {
		case status.Started:
			w.State = Running
			w.Assignment.LeaseUntil = now.Add(leaseDuration)
			if w.Submission.Kind == "job" {
				started := status.StartedAt
				if started.IsZero() {
					started = now
				}
				w.Assignment.Deadline = started.Add(time.Duration(w.Submission.Policy.TimeoutSeconds) * time.Second)
			}
			return e.Repo.Save(ctx, w, false)
		case status.Exists:
			return e.Executor.Start(ctx, ref)
		default:
			r := w.Submission.Resources
			bindings := make([]VolumeBinding, 0, len(w.Submission.Volumes))
			for _, volume := range w.Submission.Volumes {
				target := volume.Target
				if target == "" {
					target = "/kamui/volumes/" + volume.Name
				}
				bindings = append(bindings, VolumeBinding{Name: volume.Name, Target: target})
			}
			networkName := ""
			if !w.Submission.NetworkIsolated() && w.Submission.Network == "private" {
				networkName = ProjectNetworkName(w.ProjectID)
			}
			env := w.Submission.Env
			if len(env) > 0 && e.EnvResolver != nil {
				resolved, err := e.EnvResolver(ctx, w.ProjectID, env)
				if err != nil {
					return e.finish(ctx, w, Failed, "secret resolution failed: "+err.Error(), nil, "", false, false)
				}
				env = resolved
			}
			err := e.Executor.Create(ctx, ref, ContainerSpec{
				Image: w.Submission.Image, Args: w.Submission.Args, CPUMillis: r.CPUMillis, MemoryBytes: r.MemoryBytes,
				Env: env, Outputs: w.Submission.Outputs, Volumes: bindings,
				Network:   networkName,
				RunAsUser: w.Submission.ManagedInfraUser(), ExtraTmpfs: w.Submission.ExtraTmpfs,
				CapAdd: w.Submission.ManagedCaps,
			})
			if errors.Is(err, ErrInvalidContainer) {
				return e.finish(ctx, w, Failed, err.Error(), nil, "", false, false)
			}
			return err
		}
	}
	if !status.Running {
		return e.finishObserved(ctx, w, ref, status)
	}
	next := Running
	if w.Submission.Kind == "service" && status.Unhealthy {
		next = Unhealthy
	}
	if next != w.State || w.Assignment.LeaseUntil.Sub(now) < leaseDuration/2 {
		w.State = next
		w.Assignment.LeaseUntil = now.Add(leaseDuration)
		return e.Repo.Save(ctx, w, false)
	}
	return nil
}

func (e *Engine) finishObserved(ctx context.Context, w Workload, ref ExecutionRef, status ExecutionStatus) error {
	var code *int
	var logs string
	if status.Started {
		code = &status.ExitCode
		var err error
		logs, err = e.Executor.Logs(ctx, ref)
		if err != nil {
			return err
		}
	}
	switch {
	case w.CancelRequested:
		state := Cancelled
		if w.Submission.Kind == "service" {
			state = Stopped
		}
		return e.finish(ctx, w, state, "cancelled", code, logs, true, false)
	case w.StopReason == "timeout":
		return e.finish(ctx, w, Failed, "timeout", code, logs, true, true)
	case !status.Exists:
		return e.finish(ctx, w, Failed, "execution container is missing", nil, "", true, true)
	case w.Submission.Kind == "service":
		state := Failed
		if status.ExitCode == 0 {
			state = Stopped
		}
		return e.finish(ctx, w, state, "service process exited", code, logs, true, state == Failed)
	case status.ExitCode == 0:
		return e.finish(ctx, w, Succeeded, "", code, logs, true, false)
	default:
		return e.finish(ctx, w, Failed, "nonzero exit code", code, logs, true, true)
	}
}

func (e *Engine) finish(ctx context.Context, w Workload, state State, reason string, code *int, logs string, cleanup, retry bool) error {
	w.State, w.Assignment, w.StopReason = state, nil, ""
	w.Result, w.LogTail = &Result{ExitCode: code, Reason: reason}, logs
	if retry && w.Attempt <= w.Submission.Policy.MaxRetries {
		w.State, w.QueueReason = Queued, "retry after "+reason
	}
	if err := e.Repo.Save(ctx, w, cleanup); err != nil {
		return err
	}
	e.Logger.Info("workload attempt finished", "workload_id", w.ID, "attempt", w.Attempt, "state", w.State, "reason", reason)
	return nil
}

func (e *Engine) Cancel(ctx context.Context, id string) (Workload, error) {
	for {
		w, err := e.Repo.Get(ctx, id)
		if err != nil || w.State.Terminal() || w.CancelRequested {
			return w, err
		}
		w.CancelRequested = true
		err = e.Repo.Save(ctx, w, false)
		if !errors.Is(err, ErrConflict) {
			return w, err
		}
	}
}

func (e *Engine) Logs(ctx context.Context, id string) (Logs, error) {
	w, err := e.Repo.Get(ctx, id)
	if err != nil {
		return Logs{}, err
	}
	out := Logs{Attempt: w.Attempt, Tail: w.LogTail}
	if w.Assignment != nil {
		out.Tail, err = e.Executor.Logs(ctx, ExecutionRef{WorkloadID: id, Attempt: w.Attempt})
		if err != nil || out.Tail == "" {
			// Completion may have committed its log tail while the adapter removed the container.
			if latest, readErr := e.Repo.Get(ctx, id); readErr == nil && latest.Revision != w.Revision && latest.Assignment == nil {
				return Logs{Attempt: latest.Attempt, Tail: latest.LogTail}, nil
			}
		}
	}
	return out, err
}
