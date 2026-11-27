package core

import (
	"strings"
	"testing"
	"time"
)

func remoteWorker(id string, cpus int, memGB int64, mutate func(*Worker)) Worker {
	capacity := DockerCapacity{ID: "worker:" + id, OS: "linux", Arch: "arm64", LogicalCPUs: cpus, MemoryTotalBytes: memGB << 30}
	w := Worker{
		ID: id, Name: id, State: WorkerStateOnline, Health: "healthy",
		Heartbeat:   time.Now().UTC(),
		Runtime:     &capacity,
		Reliability: 0.9,
	}
	policy := DefaultWorkerPolicy()
	w.Policy = &policy
	w.Device = &Device{ID: id}
	logicalCPUs := cpus
	w.Device.Capabilities = Capabilities{OS: "linux", Arch: "arm64", LogicalCPUs: &logicalCPUs, MemoryTotalBytes: &capacity.MemoryTotalBytes}
	if mutate != nil {
		mutate(&w)
	}
	return w
}

func gpuWorker(id string) Worker {
	return remoteWorker(id, 8, 32, func(w *Worker) {
		free := int64(12 << 30)
		w.Device.Capabilities.GPUs = []GPU{{Vendor: "nvidia", Model: "RTX", AvailableMemoryBytes: &free, Capabilities: []string{"cuda"}}}
		w.Policy.GPUAllowed = true
	})
}

func autoSubmission() Submission {
	s := Submission{Image: "example:local", Resources: Resources{CPUMillis: 500, MemoryBytes: 128 << 20}}
	if err := s.Normalize(); err != nil {
		panic(err)
	}
	return s
}

func TestSelectPlacementAutoPrefersReliableCPUWorkerForSmallJobs(t *testing.T) {
	small := remoteWorker("wk-"+strings.Repeat("1", 32), 4, 8, nil)
	gpu := gpuWorker("wk-" + strings.Repeat("2", 32))
	s := autoSubmission()
	now := time.Now().UTC()
	choice, reason, decision := SelectPlacement(s, "default", "local-admin", []Worker{gpu, small}, map[string]Reservation{}, map[string]int{}, now)
	if choice != small.ID {
		t.Fatalf("selected %q (%s); small reliable CPU worker was expected", choice, reason)
	}
	if decision.Chosen != small.ID || len(decision.Candidates) != 2 {
		t.Fatalf("decision = %+v", decision)
	}
	for _, candidate := range decision.Candidates {
		if candidate.WorkerID == gpu.ID && candidate.Eligible && *candidate.Score >= *scoreOf(decision, small.ID) {
			t.Fatalf("GPU worker scored at least as high for a CPU-only job: %+v", candidate)
		}
	}
}

func scoreOf(decision PlacementDecision, workerID string) *float64 {
	for _, candidate := range decision.Candidates {
		if candidate.WorkerID == workerID {
			return candidate.Score
		}
	}
	return nil
}

func TestSelectPlacementAutoPrefersCUDAWorkerForTraining(t *testing.T) {
	cpu := remoteWorker("wk-"+strings.Repeat("1", 32), 16, 64, nil)
	gpu := gpuWorker("wk-" + strings.Repeat("2", 32))
	s := autoSubmission()
	s.Resources.GPUCount = 1
	s.Resources.GPUCapabilities = []string{"cuda"}
	s.Placement = PlacementAuto
	now := time.Now().UTC()
	choice, _, _ := SelectPlacement(s, "default", "local-admin", []Worker{cpu, gpu}, map[string]Reservation{}, map[string]int{}, now)
	if choice != gpu.ID {
		t.Fatalf("selected %q; CUDA worker was expected", choice)
	}
}

func TestSelectPlacementRejectsIncompatibleArchAndGPURequests(t *testing.T) {
	worker := remoteWorker("wk-"+strings.Repeat("1", 32), 4, 8, nil)
	s := autoSubmission()
	s.Resources.Arch = "amd64"
	if choice, reason, _ := SelectPlacement(s, "default", "user", []Worker{worker}, map[string]Reservation{}, map[string]int{}, time.Now().UTC()); choice != "" || !strings.Contains(reason, "incompatible") {
		t.Fatalf("amd64 request on arm64 worker: %q, %q", choice, reason)
	}
	s = autoSubmission()
	s.Resources.GPUCount = 1
	if _, reason, _ := SelectPlacement(s, "default", "user", []Worker{worker}, map[string]Reservation{}, map[string]int{}, time.Now().UTC()); !strings.Contains(reason, "GPU") {
		t.Fatalf("GPU request on CPU worker: %q", reason)
	}
	s = autoSubmission()
	s.Resources.GPUCount = 1
	s.Resources.GPUCapabilities = []string{"cuda"}
	apple := gpuWorker("wk-" + strings.Repeat("3", 32))
	apple.Device.Capabilities.GPUs = []GPU{{Vendor: "apple", Capabilities: []string{"metal"}}}
	if _, reason, _ := SelectPlacement(s, "default", "user", []Worker{apple}, map[string]Reservation{}, map[string]int{}, time.Now().UTC()); !strings.Contains(reason, "GPU") {
		t.Fatalf("Apple GPU must not satisfy a CUDA request: %q", reason)
	}
}

func TestSelectPlacementHonorsOwnerRestrictions(t *testing.T) {
	worker := remoteWorker("wk-"+strings.Repeat("1", 32), 4, 8, func(w *Worker) {
		w.Policy.Projects = []string{"other-project"}
	})
	s := autoSubmission()
	if _, reason, _ := SelectPlacement(s, "default", "user", []Worker{worker}, map[string]Reservation{}, map[string]int{}, time.Now().UTC()); !strings.Contains(reason, "not shared with project") {
		t.Fatalf("project restriction ignored: %q", reason)
	}
	now := time.Now().UTC()
	scheduled := remoteWorker("wk-"+strings.Repeat("4", 32), 4, 8, func(w *Worker) {
		w.Policy.Schedule = []ScheduleWindow{{Start: "00:00", End: "23:59"}}
	})
	if choice, _, _ := SelectPlacement(s, "default", "user", []Worker{scheduled}, map[string]Reservation{}, map[string]int{}, now); choice != scheduled.ID {
		t.Fatalf("inside schedule window should be eligible: %q", choice)
	}
	scheduled.Policy.Schedule = []ScheduleWindow{{Start: "01:00", End: "02:00"}}
	if _, reason, _ := SelectPlacement(s, "default", "user", []Worker{scheduled}, map[string]Reservation{}, map[string]int{}, now); !strings.Contains(reason, "availability schedule") {
		t.Fatalf("outside schedule window must be rejected: %q", reason)
	}
}

func TestSelectPlacementRespectsReservationsAndConcurrency(t *testing.T) {
	worker := remoteWorker("wk-"+strings.Repeat("1", 32), 2, 4, nil)
	s := autoSubmission() // 500m CPU
	reserved := map[string]Reservation{worker.ID: {CPUMillis: 1600}}
	if _, reason, _ := SelectPlacement(s, "default", "user", []Worker{worker}, reserved, map[string]int{}, time.Now().UTC()); !strings.Contains(reason, "reservations") {
		t.Fatalf("over-allocation must be rejected: %q", reason)
	}
	worker.Policy.MaxConcurrent = 1
	if _, reason, _ := SelectPlacement(s, "default", "user", []Worker{worker}, map[string]Reservation{}, map[string]int{worker.ID: 1}, time.Now().UTC()); !strings.Contains(reason, "concurrent workload limit") {
		t.Fatalf("concurrency limit ignored: %q", reason)
	}
}

func TestSelectPlacementManualAndPreferredModes(t *testing.T) {
	workerA := remoteWorker("wk-"+strings.Repeat("1", 32), 4, 8, nil)
	workerB := remoteWorker("wk-"+strings.Repeat("2", 32), 4, 8, nil)
	now := time.Now().UTC()

	manual := autoSubmission()
	manual.Placement = PlacementManual
	manual.WorkerID = workerB.ID
	choice, _, decision := SelectPlacement(manual, "default", "user", []Worker{workerA, workerB}, map[string]Reservation{}, map[string]int{}, now)
	if choice != workerB.ID || decision.Mode != PlacementManual {
		t.Fatalf("manual placement = %q %+v", choice, decision)
	}

	manual.WorkerID = "wk-" + strings.Repeat("9", 32)
	if _, reason, _ := SelectPlacement(manual, "default", "user", []Worker{workerA}, map[string]Reservation{}, map[string]int{}, now); !strings.Contains(reason, "not registered") {
		t.Fatalf("unknown manual worker: %q", reason)
	}

	preferred := autoSubmission()
	preferred.Placement = PlacementPreferred
	preferred.PreferredWorkers = []string{workerA.ID, workerB.ID}
	choice, _, _ = SelectPlacement(preferred, "default", "user", []Worker{workerB}, map[string]Reservation{}, map[string]int{}, now)
	if choice != workerB.ID {
		t.Fatalf("preferred fallback should reach worker B: %q", choice)
	}
	no := false
	preferred.Fallback = &no
	unrelated := remoteWorker("wk-"+strings.Repeat("3", 32), 4, 8, nil)
	if _, reason, _ := SelectPlacement(preferred, "default", "user", []Worker{unrelated}, map[string]Reservation{}, map[string]int{}, now); !strings.Contains(reason, "fallback is disabled") {
		t.Fatalf("fallback disabled must queue with an explanation: %q", reason)
	}
}

func TestSelectPlacementIsDeterministicAndExplainable(t *testing.T) {
	workers := []Worker{remoteWorker("wk-"+strings.Repeat("1", 32), 4, 8, nil), gpuWorker("wk-" + strings.Repeat("2", 32))}
	s := autoSubmission()
	now := time.Now().UTC()
	_, _, first := SelectPlacement(s, "default", "user", workers, map[string]Reservation{}, map[string]int{}, now)
	_, _, second := SelectPlacement(s, "default", "user", workers, map[string]Reservation{}, map[string]int{}, now)
	if first.Explanation != second.Explanation || len(first.Candidates) != len(second.Candidates) {
		t.Fatalf("placement is not deterministic: %+v vs %+v", first, second)
	}
	for _, candidate := range first.Candidates {
		if !candidate.Eligible {
			continue
		}
		if len(candidate.Components) == 0 || candidate.Score == nil {
			t.Fatalf("eligible candidate lacks explainable components: %+v", candidate)
		}
		total, weight := 0.0, 0.0
		for _, component := range candidate.Components {
			total += component.Value * component.Weight
			weight += component.Weight
		}
		if weight > 0 && abs(*candidate.Score-total/weight) > 1e-9 {
			t.Fatalf("score does not match its components: %+v", candidate)
		}
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func TestOrderQueueFairnessAndPriority(t *testing.T) {
	high := Workload{ID: "wl-high", ProjectID: "default", State: Queued}
	high.Submission.Priority = 90
	low := Workload{ID: "wl-low", ProjectID: "default", State: Queued}
	low.Submission.Priority = 10
	otherProject := Workload{ID: "wl-other", ProjectID: "proj-b", State: Queued}
	otherProject.Submission.Priority = 50
	low.Submission.Priority = 50
	assigned := Workload{ID: "wl-running", ProjectID: "proj-b", State: Running, Assignment: &Assignment{WorkerID: "wk-x"}}
	assigned.Submission.Priority = 50

	ordered := OrderQueue([]Workload{low, otherProject, high, assigned})
	if len(ordered) != 3 {
		t.Fatalf("terminal and running workloads must not be queued: %+v", ordered)
	}
	if ordered[0].ID != high.ID {
		t.Fatalf("highest priority first: %+v", ordered)
	}
	// proj-b already runs one workload; fairness prefers the default project.
	if ordered[1].ID != low.ID || ordered[2].ID != otherProject.ID {
		t.Fatalf("fairness ordering violated: %+v", ordered)
	}
}

func TestCheckManualSubmission(t *testing.T) {
	worker := remoteWorker("wk-"+strings.Repeat("1", 32), 4, 8, nil)
	s := autoSubmission()
	s.Placement = PlacementManual
	s.WorkerID = worker.ID
	if err := CheckManualSubmission(s, "default", "user", []Worker{worker}, time.Now().UTC()); err != nil {
		t.Fatalf("compatible manual placement rejected: %v", err)
	}
	s.Resources.Arch = "amd64"
	if err := CheckManualSubmission(s, "default", "user", []Worker{worker}, time.Now().UTC()); err == nil {
		t.Fatal("incompatible manual placement must be rejected")
	}
	revoked := remoteWorker("wk-"+strings.Repeat("2", 32), 4, 8, func(w *Worker) { w.State = WorkerStateRevoked })
	s.Resources.Arch = ""
	s.WorkerID = revoked.ID
	if err := CheckManualSubmission(s, "default", "user", []Worker{revoked}, time.Now().UTC()); err == nil {
		t.Fatal("revoked manual placement must be rejected")
	}
}
