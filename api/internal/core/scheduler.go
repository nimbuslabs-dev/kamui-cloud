package core

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Scheduling weights are fixed constants so scores are reproducible and
// explainable. Every component is normalized to 0..1 before weighting.
type scoringWeight struct {
	name   string
	weight float64
}

const (
	weightCPUUtilization    = 1.5
	weightMemoryUse         = 1.0
	weightGPUScarcity       = 1.5
	weightPressure          = 1.0
	weightLatency           = 1.0
	weightReliability       = 2.0
	weightPower             = 0.5
	weightThermal           = 0.5
	weightDataLocality      = 0.0
	weightOwnerPreference   = 1.0
	latencyPenaltyMS        = 250.0
	thermalWarmC            = 70.0
	thermalCriticalC        = 90.0
	heavyCPUMillis          = 2000
	heavyMemoryBytes        = int64(2) << 30
	defaultMaxConcurrentRef = 4
)

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

func isHeavy(s Submission) bool {
	r := s.Resources
	return r.CPUMillis >= heavyCPUMillis || r.MemoryBytes >= heavyMemoryBytes || r.GPUCount > 0
}

// gpuMatches reports whether one GPU satisfies the requested capability set.
func gpuMatches(gpu GPU, required []string) bool {
	for _, want := range required {
		found := false
		for _, have := range gpu.Capabilities {
			if strings.EqualFold(have, want) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func compatibleGPUs(w Worker, required []string) int {
	if w.Device == nil {
		return 0
	}
	count := 0
	for _, gpu := range w.Device.Capabilities.GPUs {
		if gpuMatches(gpu, required) {
			count++
		}
	}
	return count
}

// workerRejections returns every reason the worker cannot host the submission.
// An empty slice means eligible. Reasons are stable strings suitable for users.
func workerRejections(w Worker, s Submission, reserved Reservation, pressure int, now time.Time) []string {
	var reasons []string
	add := func(format string, args ...any) {
		reasons = append(reasons, fmt.Sprintf(format, args...))
	}

	if w.ID == LocalWorker {
		if _, reason := (LocalPlacement{}).Select(s, w, reserved); reason != "" {
			add("%s", reason)
		}
		return reasons
	}

	switch w.State {
	case WorkerStatePending:
		add("worker is awaiting owner approval")
	case WorkerStateRevoked:
		add("worker is revoked")
	case WorkerStateDisabled:
		add("worker is disabled by its owner")
	case WorkerStateDraining:
		add("worker is draining and accepts no new assignments")
	}
	if w.State != WorkerStateOnline && w.State != WorkerStateDraining || now.Sub(w.Heartbeat) > OfflineAfter {
		if w.State != WorkerStatePending && w.State != WorkerStateRevoked && w.State != WorkerStateDisabled {
			add("worker is offline")
		}
	}
	if w.Runtime == nil || w.Runtime.LogicalCPUs <= 0 || w.Runtime.MemoryTotalBytes <= 0 {
		add("worker has no usable Docker runtime capacity")
		return reasons
	}
	caps := WorkerPolicy{}
	if w.Policy != nil {
		caps = *w.Policy
	}
	r := s.Resources
	if (r.OS != "" && r.OS != w.Runtime.OS) || (r.Arch != "" && r.Arch != w.Runtime.Arch) {
		add("requested platform is incompatible with the worker runtime")
	}
	if r.GPUCount > 0 {
		if !caps.GPUAllowed {
			add("worker owner does not permit GPU workloads")
		}
		available := compatibleGPUs(w, r.GPUCapabilities)
		if available < r.GPUCount {
			if len(r.GPUCapabilities) > 0 {
				add("worker offers %d GPU(s) matching %s but %d are requested", available, strings.Join(r.GPUCapabilities, ", "), r.GPUCount)
			} else {
				add("worker offers %d GPU(s) but %d are requested", available, r.GPUCount)
			}
		}
	}
	if caps.CPUMillisMax > 0 && r.CPUMillis > caps.CPUMillisMax {
		add("requested CPU exceeds the owner's per-workload allocation of %d millicores", caps.CPUMillisMax)
	}
	if caps.MemoryBytesMax > 0 && r.MemoryBytes > caps.MemoryBytesMax {
		add("requested RAM exceeds the owner's per-workload allocation")
	}
	if caps.StorageBytesMax > 0 && r.StorageBytes > caps.StorageBytesMax {
		add("requested storage exceeds the owner's per-workload allocation")
	}
	if r.StorageBytes > 0 && w.Device != nil && w.Device.Capabilities.StorageAvailable != nil &&
		r.StorageBytes > *w.Device.Capabilities.StorageAvailable {
		add("worker has insufficient free storage")
	}
	if len(caps.WorkloadKinds) > 0 {
		allowed := false
		for _, kind := range caps.WorkloadKinds {
			if kind == s.Kind {
				allowed = true
				break
			}
		}
		if !allowed {
			add("worker owner restricts workloads to kinds: %s", strings.Join(caps.WorkloadKinds, ", "))
		}
	}
	if len(caps.Projects) > 0 {
		projectScope, _ := s.Scope()
		permitted := false
		for _, project := range caps.Projects {
			if project == projectScope {
				permitted = true
				break
			}
		}
		if !permitted {
			add("worker is not shared with project %s", projectScope)
		}
	}
	if len(caps.Users) > 0 && s.submitter != "" {
		permitted := false
		for _, user := range caps.Users {
			if user == s.submitter {
				permitted = true
				break
			}
		}
		if !permitted {
			add("worker owner has not authorized user %s", s.submitter)
		}
	}
	if len(caps.Schedule) > 0 && !scheduleAllows(caps, now) {
		add("worker is outside the owner's availability schedule")
	}
	if w.Telemetry != nil && w.Telemetry.OnBattery != nil && *w.Telemetry.OnBattery &&
		!caps.AllowHeavyOnBattery && isHeavy(s) {
		add("owner disallows heavy workloads while the device is on battery")
	}
	if w.Telemetry != nil && w.Telemetry.TemperatureC != nil && *w.Telemetry.TemperatureC > thermalCriticalC {
		add("worker reported %.0f°C which exceeds the safe threshold", *w.Telemetry.TemperatureC)
	}
	cpuCapacity := int64(w.Runtime.LogicalCPUs) * 1000
	if r.CPUMillis > cpuCapacity-reserved.CPUMillis || r.MemoryBytes > w.Runtime.MemoryTotalBytes-reserved.MemoryBytes {
		add("waiting for available CPU or RAM reservations on this worker")
	}
	if caps.MaxConcurrent > 0 && pressure >= caps.MaxConcurrent {
		add("worker is at its concurrent workload limit of %d", caps.MaxConcurrent)
	}
	return reasons
}

// scheduleAllows checks a worker's availability windows in the policy timezone.
func scheduleAllows(policy WorkerPolicy, now time.Time) bool {
	location := time.UTC
	if policy.Timezone != "" {
		parsed, err := time.LoadLocation(policy.Timezone)
		if err != nil {
			return false
		}
		location = parsed
	}
	local := now.In(location)
	minuteOfDay := local.Hour()*60 + local.Minute()
	weekday := int(local.Weekday())
	for _, window := range policy.Schedule {
		dayMatch := len(window.Days) == 0
		for _, day := range window.Days {
			if day == weekday {
				dayMatch = true
				break
			}
		}
		if !dayMatch {
			continue
		}
		start, startOK := ParseClock(window.Start)
		end, endOK := ParseClock(window.End)
		if !startOK || !endOK {
			continue
		}
		if end >= start {
			if minuteOfDay >= start && minuteOfDay <= end {
				return true
			}
			continue
		}
		// Overnight window (e.g. 22:00–06:00).
		if minuteOfDay >= start || minuteOfDay <= end {
			return true
		}
	}
	return false
}

func ParseClock(raw string) (int, bool) {
	parts := strings.Split(raw, ":")
	if len(parts) != 2 {
		return 0, false
	}
	var hour, minute int
	if _, err := fmt.Sscanf(parts[0]+":"+parts[1], "%d:%d", &hour, &minute); err != nil {
		return 0, false
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, false
	}
	return hour*60 + minute, true
}

// scoreWorker computes the deterministic placement score with explainable
// components. Callers must have established eligibility first.
func scoreWorker(w Worker, s Submission, reserved Reservation, pressure int) (float64, []ScoreComponent) {
	var components []ScoreComponent
	add := func(name string, value, weight float64, detail string) {
		components = append(components, ScoreComponent{Name: name, Value: clamp01(value), Weight: weight, Detail: detail})
	}

	cpuCapacity := float64(int64(w.Runtime.LogicalCPUs) * 1000)
	cpuUtil := (float64(reserved.CPUMillis) + float64(s.Resources.CPUMillis)) / cpuCapacity
	add("cpu_utilization", cpuUtil, weightCPUUtilization,
		fmt.Sprintf("reservation %d/%d millicores after placement", reserved.CPUMillis+s.Resources.CPUMillis, int64(cpuCapacity)))

	memCapacity := float64(w.Runtime.MemoryTotalBytes)
	memUtil := (float64(reserved.MemoryBytes) + float64(s.Resources.MemoryBytes)) / memCapacity
	add("memory_utilization", memUtil, weightMemoryUse,
		fmt.Sprintf("reservation %d/%d bytes after placement", reserved.MemoryBytes+s.Resources.MemoryBytes, int64(memCapacity)))

	requestsGPU := s.Resources.GPUCount > 0
	hasGPU := compatibleGPUs(w, nil) > 0
	switch {
	case requestsGPU:
		add("gpu_scarcity", 1.0, weightGPUScarcity, "workload requires a compatible GPU")
	case hasGPU:
		add("gpu_scarcity", 0.2, weightGPUScarcity, "conserves scarce GPU capacity for workloads that need it")
	default:
		add("gpu_scarcity", 1.0, weightGPUScarcity, "CPU-only worker hosts CPU-only workloads efficiently")
	}

	ref := float64(defaultMaxConcurrentRef)
	if w.Policy != nil && w.Policy.MaxConcurrent > 0 {
		ref = float64(w.Policy.MaxConcurrent)
	}
	add("workload_pressure", 1-clamp01(float64(pressure)/ref), weightPressure,
		fmt.Sprintf("%d active assignment(s) on this worker", pressure))

	latency := 0.5
	detail := "latency unknown; neutral"
	if w.LatencyMS > 0 {
		latency = 1 - clamp01(w.LatencyMS/latencyPenaltyMS)
		detail = fmt.Sprintf("%.0f ms round trip", w.LatencyMS)
	}
	add("latency", latency, weightLatency, detail)

	add("reliability", clamp01(w.Reliability), weightReliability,
		fmt.Sprintf("%.0f%% heartbeat reliability", w.Reliability*100))

	power := 1.0
	powerDetail := "on mains power or unknown"
	if w.Telemetry != nil && w.Telemetry.OnBattery != nil && *w.Telemetry.OnBattery {
		power = 0.4
		powerDetail = "device is on battery"
	}
	add("power", power, weightPower, powerDetail)

	thermal := 1.0
	thermalDetail := "temperature unknown; not penalized"
	if w.Telemetry != nil && w.Telemetry.TemperatureC != nil {
		temp := *w.Telemetry.TemperatureC
		switch {
		case temp <= thermalWarmC:
			thermalDetail = fmt.Sprintf("%.0f°C is within safe range", temp)
		default:
			thermal = clamp01((thermalCriticalC - temp) / (thermalCriticalC - thermalWarmC))
			thermalDetail = fmt.Sprintf("%.0f°C approaching the thermal threshold", temp)
		}
	}
	add("thermal", thermal, weightThermal, thermalDetail)

	add("data_locality", 0.5, weightDataLocality, "no data locality signals yet")

	preference := 0.5
	preferenceDetail := "not in the submitter's preferred list"
	for i, id := range s.PreferredWorkers {
		if id == w.ID {
			preference = 1 - float64(i)/float64(len(s.PreferredWorkers)+1)
			preferenceDetail = fmt.Sprintf("preferred position %d", i+1)
			break
		}
	}
	add("owner_preference", preference, weightOwnerPreference, preferenceDetail)

	total, weight := 0.0, 0.0
	for _, component := range components {
		total += component.Value * component.Weight
		weight += component.Weight
	}
	if weight == 0 {
		return 0.5, components
	}
	return total / weight, components
}

// queuedPriority orders queued workloads: highest priority first, then the
// project with the least active share (fairness), then arrival order.
func OrderQueue(items []Workload) []Workload {
	share := map[string]int{}
	for _, w := range items {
		if w.Assignment != nil {
			share[w.ProjectID]++
		}
	}
	queued := make([]Workload, 0, len(items))
	for _, w := range items {
		if w.State == Queued {
			queued = append(queued, w)
		}
	}
	sort.SliceStable(queued, func(i, j int) bool {
		a, b := queued[i], queued[j]
		if a.Submission.Priority != b.Submission.Priority {
			return a.Submission.Priority > b.Submission.Priority
		}
		if share[a.ProjectID] != share[b.ProjectID] {
			return share[a.ProjectID] < share[b.ProjectID]
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	return queued
}

// SelectPlacement evaluates workers for one queued submission. It returns the
// chosen worker (empty when nothing is schedulable), a queue reason, and a
// durable decision for the explanation API. The result is deterministic for a
// given set of inputs.
func SelectPlacement(s Submission, projectID, submitterID string, workers []Worker, reserved map[string]Reservation, pressure map[string]int, now time.Time) (string, string, PlacementDecision) {
	scope := Submission{Service: s.Service, Kind: s.Kind, Resources: s.Resources, PreferredWorkers: s.PreferredWorkers}
	scope.SetScope(projectID, submitterID)
	decision := PlacementDecision{Mode: s.Placement, Candidates: []DecisionCandidate{}, At: now}

	evaluate := func(w Worker) DecisionCandidate {
		candidate := DecisionCandidate{WorkerID: w.ID}
		res := reserved[w.ID]
		reasons := workerRejections(w, scope, res, pressure[w.ID], now)
		if len(reasons) == 0 {
			candidate.Eligible = true
			score, components := scoreWorker(w, scope, res, pressure[w.ID])
			candidate.Score = &score
			candidate.Components = components
			candidate.Reasons = topComponents(components, 3)
		} else {
			candidate.Reasons = reasons
		}
		return candidate
	}

	byID := make(map[string]Worker, len(workers))
	for _, w := range workers {
		byID[w.ID] = w
	}

	switch s.Placement {
	case PlacementManual:
		w, found := byID[s.WorkerID]
		if !found {
			decision.Candidates = append(decision.Candidates, DecisionCandidate{WorkerID: s.WorkerID, Reasons: []string{"worker is not registered"}})
			return "", fmt.Sprintf("selected worker %s is not registered", s.WorkerID), decision
		}
		candidate := evaluate(w)
		decision.Candidates = append(decision.Candidates, candidate)
		if !candidate.Eligible {
			return "", fmt.Sprintf("selected worker %s is unavailable: %s", s.WorkerID, candidate.Reasons[0]), decision
		}
		decision.Chosen = w.ID
		decision.Explanation = fmt.Sprintf("manual placement on %s", w.ID)
		return w.ID, "", decision

	case PlacementPreferred:
		var blocked []string
		for _, id := range s.PreferredWorkers {
			w, found := byID[id]
			if !found {
				blocked = append(blocked, fmt.Sprintf("%s is not registered", id))
				continue
			}
			candidate := evaluate(w)
			decision.Candidates = append(decision.Candidates, candidate)
			if candidate.Eligible {
				decision.Chosen = w.ID
				decision.Explanation = fmt.Sprintf("preferred worker %s accepted the workload", w.ID)
				return w.ID, "", decision
			}
			blocked = append(blocked, fmt.Sprintf("%s: %s", id, candidate.Reasons[0]))
		}
		if s.FallbackEnabled() {
			fallback := s
			fallback.Placement = PlacementAuto
			fallback.SetScope(projectID, submitterID)
			choice, reason, autoDecision := SelectPlacement(fallback, projectID, submitterID, workers, reserved, pressure, now)
			autoDecision.Mode = PlacementPreferred
			if choice != "" {
				autoDecision.Explanation = "preferred workers were unavailable; automatic fallback selected " + choice
			} else {
				autoDecision.Explanation = "preferred workers were unavailable and automatic fallback found no eligible worker: " + reason
			}
			autoDecision.Candidates = append(decision.Candidates, autoDecision.Candidates...)
			return choice, reason, autoDecision
		}
		return "", "preferred workers unavailable and fallback is disabled: " + strings.Join(blocked, "; "), decision

	default: // PlacementAuto
		for _, w := range workers {
			decision.Candidates = append(decision.Candidates, evaluate(w))
		}
		best, bestScore := "", math.Inf(-1)
		var bestReasons []string
		for _, candidate := range decision.Candidates {
			if !candidate.Eligible {
				bestReasons = append(bestReasons, fmt.Sprintf("%s: %s", candidate.WorkerID, candidate.Reasons[0]))
				continue
			}
			if *candidate.Score > bestScore+1e-9 || (math.Abs(*candidate.Score-bestScore) <= 1e-9 && candidate.WorkerID < best) {
				best, bestScore = candidate.WorkerID, *candidate.Score
			}
		}
		if best == "" {
			const maxReasons = 3
			if len(bestReasons) > maxReasons {
				bestReasons = bestReasons[:maxReasons]
			}
			return "", "no eligible worker: " + strings.Join(bestReasons, "; "), decision
		}
		decision.Chosen = best
		decision.Explanation = fmt.Sprintf("automatic placement selected %s with score %.3f", best, bestScore)
		return best, "", decision
	}
}

// CheckManualSubmission rejects a manual placement request at submit time when
// the selected worker is unknown or structurally ineligible. A busy worker is
// acceptable: the workload queues until capacity frees up.
func CheckManualSubmission(s Submission, projectID, submitterID string, workers []Worker, now time.Time) error {
	if s.Placement != PlacementManual {
		return nil
	}
	scope := Submission{Service: s.Service, Kind: s.Kind, Resources: s.Resources, PreferredWorkers: s.PreferredWorkers}
	scope.SetScope(projectID, submitterID)
	for _, w := range workers {
		if w.ID != s.WorkerID {
			continue
		}
		reasons := workerRejections(w, scope, Reservation{}, 0, now)
		for _, reason := range reasons {
			if reason != "waiting for available CPU or RAM reservations on this worker" {
				return fmt.Errorf("worker %s cannot host this workload: %s", w.ID, reason)
			}
		}
		return nil
	}
	return fmt.Errorf("worker %s is not registered", s.WorkerID)
}

func topComponents(components []ScoreComponent, n int) []string {
	sorted := make([]ScoreComponent, len(components))
	copy(sorted, components)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Value*sorted[i].Weight > sorted[j].Value*sorted[j].Weight })
	out := make([]string, 0, n)
	for _, component := range sorted {
		if component.Weight == 0 {
			continue
		}
		out = append(out, fmt.Sprintf("%s=%.2f", component.Name, component.Value))
		if len(out) == n {
			break
		}
	}
	return out
}
