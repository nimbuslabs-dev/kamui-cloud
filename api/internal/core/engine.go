package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"
)

const (
	heartbeatInterval = 10 * time.Second
	leaseDuration     = 45 * time.Second
)

type Probe func(context.Context) (*Device, error)

type Engine struct {
	Repo      Repository
	Executor  Executor
	Placement Placement
	Probe     Probe
	Logger    *slog.Logger
}

func newID() string {
	var bytes [16]byte
	rand.Read(bytes[:])
	return "wl-" + hex.EncodeToString(bytes[:])
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

func (e *Engine) reconcile(ctx context.Context) {
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
			err := e.Executor.Create(ctx, ref, ContainerSpec{
				Image: w.Submission.Image, Args: w.Submission.Args, CPUMillis: r.CPUMillis, MemoryBytes: r.MemoryBytes,
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
		if err != nil || w.State.terminal() || w.CancelRequested {
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
