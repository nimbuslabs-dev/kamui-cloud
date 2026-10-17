package scheduler

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueueLifecycle(t *testing.T) {
	currentTime := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	q := NewQueue(func() time.Time { return currentTime })

	if len(q.List()) != 0 {
		t.Fatalf("expected empty slice from initial List")
	}

	_, ok := q.Claim("node-1")
	if ok {
		t.Fatalf("expected claim on empty queue to return false")
	}

	j1 := q.Create("alpine:3.20", []string{"echo", "1"})
	currentTime = currentTime.Add(time.Second)
	j2 := q.Create("alpine:3.20", []string{"echo", "2"})
	currentTime = currentTime.Add(time.Second)
	j3 := q.Create("alpine:3.20", []string{"echo", "3"})

	list := q.List()
	if len(list) != 3 {
		t.Fatalf("expected 3 jobs, got %d", len(list))
	}
	if list[0].ID != j3.ID || list[1].ID != j2.ID || list[2].ID != j1.ID {
		t.Fatalf("expected newest first order, got [%s, %s, %s]", list[0].ID, list[1].ID, list[2].ID)
	}

	claimed1, ok := q.Claim("node-a")
	if !ok || claimed1.ID != j1.ID {
		t.Fatalf("expected FIFO claim to return j1, got %v (ok=%v)", claimed1, ok)
	}
	if claimed1.Status != StatusRunning || claimed1.NodeID != "node-a" {
		t.Fatalf("expected running status and node_id, got %+v", claimed1)
	}

	claimed2, ok := q.Claim("node-b")
	if !ok || claimed2.ID != j2.ID {
		t.Fatalf("expected FIFO claim to return j2, got %v (ok=%v)", claimed2, ok)
	}

	if _, err := q.Finish(j3.ID, 0, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict finishing queued job, got %v", err)
	}

	finished1, err := q.Finish(j1.ID, 0, "success log")
	if err != nil {
		t.Fatalf("unexpected finish error: %v", err)
	}
	if finished1.Status != StatusSucceeded {
		t.Fatalf("expected status succeeded, got %q", finished1.Status)
	}
	if finished1.ExitCode == nil || *finished1.ExitCode != 0 {
		t.Fatalf("expected exit_code 0, got %v", finished1.ExitCode)
	}
	if finished1.FinishedAt == "" {
		t.Fatalf("expected non-empty finished_at")
	}

	data, err := json.Marshal(finished1)
	if err != nil {
		t.Fatalf("failed to marshal job: %v", err)
	}
	if !strings.Contains(string(data), `"exit_code":0`) {
		t.Fatalf("expected exit_code 0 in json, got %s", string(data))
	}

	finished2, err := q.Finish(j2.ID, 3, "fail log")
	if err != nil {
		t.Fatalf("unexpected finish error: %v", err)
	}
	if finished2.Status != StatusFailed {
		t.Fatalf("expected status failed, got %q", finished2.Status)
	}
	if finished2.ExitCode == nil || *finished2.ExitCode != 3 {
		t.Fatalf("expected exit_code 3, got %v", finished2.ExitCode)
	}

	if _, err := q.Finish(j1.ID, 0, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict finishing already finished job, got %v", err)
	}

	if _, err := q.Finish("j-unknown", 0, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound finishing unknown job, got %v", err)
	}

	if _, err := q.Get("j-unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound getting unknown job, got %v", err)
	}

	got1, err := q.Get(j1.ID)
	if err != nil {
		t.Fatalf("unexpected get error: %v", err)
	}
	if got1.ID != j1.ID || got1.Status != StatusSucceeded {
		t.Fatalf("unexpected job retrieved: %+v", got1)
	}
}

func TestQueueConcurrentClaim(t *testing.T) {
	q := NewQueue(nil)
	j := q.Create("alpine:3.20", []string{"sleep", "1"})

	var winnerCount int32
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			claimed, ok := q.Claim("node-worker")
			if ok {
				if claimed.ID == j.ID {
					atomic.AddInt32(&winnerCount, 1)
				}
			}
		}(i)
	}

	wg.Wait()

	if winnerCount != 1 {
		t.Fatalf("expected exactly 1 winner out of 50 concurrent claims, got %d", winnerCount)
	}
}
