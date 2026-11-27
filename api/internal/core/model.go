package core

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type User struct {
	ID string `json:"id"`
}

type Project struct {
	ID      string `json:"id"`
	OwnerID string `json:"owner_id"`
}

type Device struct {
	ID           string       `json:"id"`
	Capabilities Capabilities `json:"capabilities"`
}

type Capabilities struct {
	Hostname             string   `json:"hostname,omitempty"`
	OS                   string   `json:"os"`
	Arch                 string   `json:"arch"`
	LogicalCPUs          *int     `json:"logical_cpus"`
	MemoryTotalBytes     *int64   `json:"memory_total_bytes"`
	MemoryAvailableBytes *int64   `json:"memory_available_bytes"`
	StorageAvailable     *int64   `json:"storage_available_bytes"`
	GPUs                 []GPU    `json:"gpus"`
	DockerAvailable      *bool    `json:"docker_available"`
	Runtimes             []string `json:"runtimes"`
}

type GPU struct {
	Vendor               string   `json:"vendor"`
	Model                string   `json:"model,omitempty"`
	AvailableMemoryBytes *int64   `json:"available_memory_bytes"`
	Capabilities         []string `json:"capabilities,omitempty"`
}

// Telemetry carries sampled host measurements a worker may be unable to observe.
// nil fields mean unknown; they are never fabricated.
type Telemetry struct {
	CPUPct       *float64 `json:"cpu_pct,omitempty"`
	OnBattery    *bool    `json:"on_battery,omitempty"`
	TemperatureC *float64 `json:"temperature_c,omitempty"`
}

// Placement modes for workload scheduling.
const (
	PlacementAuto      = "auto"
	PlacementManual    = "manual"
	PlacementPreferred = "preferred"
)

// Worker lifecycle states for contributed devices. The local worker has no lifecycle state.
const (
	WorkerStatePending  = "pending"
	WorkerStateApproved = "approved"
	WorkerStateDraining = "draining"
	WorkerStateDisabled = "disabled"
	WorkerStateRevoked  = "revoked"
	WorkerStateOnline   = "online"
	WorkerStateOffline  = "offline"
)

// DefaultWorkerPolicy is applied when a device enrolls before an owner narrows it.
// It shares compute with the owner's default project and never permits GPU passthrough.
func DefaultWorkerPolicy() WorkerPolicy {
	return WorkerPolicy{GPUAllowed: false, Projects: []string{"default"}, AllowHeavyOnBattery: true}
}

// ScheduleWindow restricts sharing to recurring time ranges in the owner's timezone.
type ScheduleWindow struct {
	Days  []int  `json:"days,omitempty"` // 0=Sunday..6=Saturday; empty means every day
	Start string `json:"start"`          // "HH:MM"
	End   string `json:"end"`            // "HH:MM"
}

// WorkerPolicy is the resource-sharing contract the device owner controls.
type WorkerPolicy struct {
	CPUMillisMax        int64            `json:"cpu_millis_max,omitempty"`
	MemoryBytesMax      int64            `json:"memory_bytes_max,omitempty"`
	StorageBytesMax     int64            `json:"storage_bytes_max,omitempty"`
	GPUAllowed          bool             `json:"gpu_allowed"`
	WorkloadKinds       []string         `json:"workload_kinds,omitempty"`
	Projects            []string         `json:"projects,omitempty"`
	Users               []string         `json:"users,omitempty"`
	Schedule            []ScheduleWindow `json:"schedule,omitempty"`
	Timezone            string           `json:"timezone,omitempty"`
	AllowHeavyOnBattery bool             `json:"allow_heavy_on_battery"`
	MaxConcurrent       int              `json:"max_concurrent,omitempty"`
}

// ScoreComponent is one weighted factor of an explainable placement score.
type ScoreComponent struct {
	Name   string  `json:"name"`
	Value  float64 `json:"value"`
	Weight float64 `json:"weight"`
	Detail string  `json:"detail,omitempty"`
}

// DecisionCandidate records how one worker was evaluated for a workload.
type DecisionCandidate struct {
	WorkerID   string           `json:"worker_id"`
	Eligible   bool             `json:"eligible"`
	Score      *float64         `json:"score,omitempty"`
	Reasons    []string         `json:"reasons,omitempty"`
	Components []ScoreComponent `json:"components,omitempty"`
}

// PlacementDecision is the durable, explainable record of a scheduling evaluation.
type PlacementDecision struct {
	WorkloadID  string              `json:"workload_id"`
	Attempt     int                 `json:"attempt"`
	Mode        string              `json:"mode"`
	Chosen      string              `json:"chosen_worker,omitempty"`
	Explanation string              `json:"explanation"`
	Candidates  []DecisionCandidate `json:"candidates"`
	At          time.Time           `json:"at"`
}

// WorkerReport is a durable message from a worker about one assignment.
// Reports are applied by the coordinator with assignment fencing.
const (
	ReportAck   = "ack"
	ReportStart = "running"
	ReportExit  = "exit"
	ReportRenew = "renew"
)

type WorkerReport struct {
	ID         int64      `json:"id"`
	WorkerID   string     `json:"worker_id"`
	WorkloadID string     `json:"workload_id"`
	Fence      string     `json:"fence"`
	Kind       string     `json:"kind"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Started    bool       `json:"started,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	LogTail    string     `json:"log_tail,omitempty"`
	At         time.Time  `json:"at"`
}

// WorkerState is a worker snapshot used for placement. State is populated for
// contributed workers; the local worker relies on Health alone.
type Worker struct {
	ID          string          `json:"id"`
	Device      *Device         `json:"device,omitempty"`
	Runtime     *DockerCapacity `json:"docker,omitempty"`
	Health      string          `json:"health"`
	Heartbeat   time.Time       `json:"heartbeat"`
	State       string          `json:"state,omitempty"`
	Name        string          `json:"name,omitempty"`
	Telemetry   *Telemetry      `json:"telemetry,omitempty"`
	Policy      *WorkerPolicy   `json:"policy,omitempty"`
	LatencyMS   float64         `json:"latency_ms,omitempty"`
	Reliability float64         `json:"reliability,omitempty"`
	Active      int             `json:"active_assignments,omitempty"`
}

const LocalWorker = "local"

// OfflineAfter is how long a contributed worker may stay silent before the
// coordinator considers it offline. It is roughly three heartbeat intervals.
const OfflineAfter = 45 * time.Second

type Resources struct {
	CPUMillis       int64    `json:"cpu_millis"`
	MemoryBytes     int64    `json:"memory_bytes"`
	StorageBytes    int64    `json:"storage_bytes,omitempty"`
	OS              string   `json:"os,omitempty"`
	Arch            string   `json:"arch,omitempty"`
	GPUCount        int      `json:"gpu_count,omitempty"`
	GPUCapabilities []string `json:"gpu_capabilities,omitempty"`
}

type Reservation struct {
	CPUMillis   int64 `json:"cpu_millis"`
	MemoryBytes int64 `json:"memory_bytes"`
}

// Validate checks standalone resource requests for service layers that submit
// without going through the full Submission path.
func (r Resources) Validate() error {
	if r.CPUMillis < 10 || r.CPUMillis > 256000 || r.MemoryBytes < 16<<20 || r.MemoryBytes > 1<<40 ||
		r.StorageBytes < 0 || r.StorageBytes > 16<<40 || r.GPUCount < 0 || r.GPUCount > 64 {
		return errors.New("CPU, memory, storage or GPU request is outside the supported range")
	}
	if r.Arch != "" && r.Arch != "amd64" && r.Arch != "arm64" {
		return errors.New("architecture must be amd64 or arm64")
	}
	if r.OS != "" && r.OS != "linux" && r.OS != "windows" && r.OS != "macos" {
		return errors.New("OS must be linux, windows or macos")
	}
	return nil
}

type Placement interface {
	Select(Submission, Worker, Reservation) (workerID, reason string)
}

type LocalPlacement struct{}

func (LocalPlacement) Select(s Submission, worker Worker, reserved Reservation) (string, string) {
	r := s.Resources
	if s.WorkerID != "" && s.WorkerID != worker.ID {
		return "", "waiting for the selected worker"
	}
	if r.GPUCount != 0 {
		return "", "GPU passthrough is unavailable on the local executor"
	}
	if worker.Health != "healthy" || worker.Runtime == nil {
		return "", "waiting for the local Docker runtime"
	}
	c := worker.Runtime
	if (r.OS != "" && r.OS != c.OS) || (r.Arch != "" && r.Arch != c.Arch) {
		return "", "requested platform is incompatible with the Docker runtime"
	}
	cpu := int64(c.LogicalCPUs) * 1000
	if r.CPUMillis > cpu || r.MemoryBytes > c.MemoryTotalBytes {
		return "", "requested CPU or RAM exceeds Docker runtime capacity"
	}
	if r.CPUMillis > cpu-reserved.CPUMillis || r.MemoryBytes > c.MemoryTotalBytes-reserved.MemoryBytes {
		return "", "waiting for available CPU or RAM reservations"
	}
	return worker.ID, ""
}

type Policy struct {
	TimeoutSeconds int `json:"timeout_seconds"`
	MaxRetries     int `json:"max_retries"`
}

// VolumeRequest declares a persistent named volume owned by the project.
// Stateful workloads pin to the worker that already holds the volume.
type VolumeRequest struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

// ArtifactMount attaches a project artifact read-only at an in-container path.
type ArtifactMount struct {
	ArtifactID string `json:"artifact_id"`
	Path       string `json:"path"`
}

// HealthCheck configures the worker-side readiness probe for services.
type HealthCheck struct {
	Kind            string `json:"kind"` // "http" or "tcp"
	Port            int    `json:"port"`
	Path            string `json:"path,omitempty"`
	IntervalSeconds int    `json:"interval_seconds,omitempty"`
}

// Submission is the unified workload contract shared by every service layer.
type Submission struct {
	Service          string            `json:"service"`
	Kind             string            `json:"kind"`
	Image            string            `json:"image"`
	ImageDigest      string            `json:"image_digest,omitempty"`
	Args             []string          `json:"args,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	Resources        Resources         `json:"resources"`
	Policy           Policy            `json:"policy"`
	WorkerID         string            `json:"worker_id,omitempty"`
	Placement        string            `json:"placement,omitempty"`
	PreferredWorkers []string          `json:"preferred_workers,omitempty"`
	Fallback         *bool             `json:"fallback,omitempty"`
	Priority         int               `json:"priority,omitempty"`

	// Network selects the execution profile: "" or "none" keeps the default
	// fully isolated batch profile; "private" joins the project-scoped network.
	Network string `json:"network,omitempty"`

	// Volumes are persistent project-owned volumes; artifacts are read-only
	// inputs; Outputs lists in-container paths collected after completion.
	Volumes   []VolumeRequest `json:"volumes,omitempty"`
	Artifacts []ArtifactMount `json:"artifacts,omitempty"`
	Outputs   []string        `json:"outputs,omitempty"`

	// HealthCheck enables worker-side readiness probing for services.
	HealthCheck *HealthCheck `json:"health_check,omitempty"`

	// Scheduling scope is set by the coordinator, never by clients, and is
	// excluded from idempotency fingerprints because it is part of the request
	// envelope rather than the workload definition.
	projectScopeValue string
	submitter         string
}

// FallbackEnabled reports whether preferred placement may fall back to
// automatic selection. Absence means enabled.
func (s *Submission) FallbackEnabled() bool { return s.Fallback == nil || *s.Fallback }

// Scope returns the project and submitter identity the coordinator attached.
func (s *Submission) Scope() (projectID, submitterID string) {
	return s.projectScopeValue, s.submitter
}

// SetScope attaches scheduling identity. It must be called before Submit.
func (s *Submission) SetScope(projectID, submitterID string) {
	s.projectScopeValue, s.submitter = projectID, submitterID
}

type State string

const (
	Pending   State = "PENDING"
	Queued    State = "QUEUED"
	Assigned  State = "ASSIGNED"
	Starting  State = "STARTING"
	Running   State = "RUNNING"
	Unhealthy State = "UNHEALTHY"
	Stopping  State = "STOPPING"
	Succeeded State = "SUCCEEDED"
	Failed    State = "FAILED"
	Cancelled State = "CANCELLED"
	Stopped   State = "STOPPED"
)

func (s State) Terminal() bool {
	return s == Succeeded || s == Failed || s == Cancelled || s == Stopped
}

type Assignment struct {
	RuntimeID   string      `json:"runtime_id"`
	WorkerID    string      `json:"worker_id"`
	Attempt     int         `json:"attempt"`
	Fence       string      `json:"fence,omitempty"`
	LeaseUntil  time.Time   `json:"lease_until"`
	Deadline    time.Time   `json:"deadline"`
	AckedAt     time.Time   `json:"acked_at,omitempty"`
	Reservation Reservation `json:"reservation"`
}

type Result struct {
	ExitCode *int   `json:"exit_code"`
	Reason   string `json:"reason,omitempty"`
}

type Artifact struct {
	WorkloadID string `json:"workload_id"`
	Name       string `json:"name"`
	URI        string `json:"uri"`
}

type Logs struct {
	Attempt int    `json:"attempt"`
	Tail    string `json:"tail"`
}

type Workload struct {
	ID              string      `json:"id"`
	ProjectID       string      `json:"project_id"`
	SubmitterID     string      `json:"submitter_id,omitempty"`
	Submission      Submission  `json:"submission"`
	State           State       `json:"state"`
	CreatedAt       time.Time   `json:"created_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
	Attempt         int         `json:"attempt"`
	Assignment      *Assignment `json:"assignment,omitempty"`
	Result          *Result     `json:"result,omitempty"`
	QueueReason     string      `json:"queue_reason,omitempty"`
	CancelRequested bool        `json:"cancel_requested"`
	StopReason      string      `json:"stop_reason,omitempty"`
	Revision        int64       `json:"-"`
	LogTail         string      `json:"-"`
}

type Event struct {
	ID         int64     `json:"id"`
	WorkloadID string    `json:"workload_id"`
	State      State     `json:"state"`
	Attempt    int       `json:"attempt"`
	At         time.Time `json:"at"`
}

type EventReader interface {
	Events(context.Context, int64) ([]Event, error)
}

type Executor interface {
	Capacity(context.Context) (DockerCapacity, error)
	Create(context.Context, ExecutionRef, ContainerSpec) error
	Start(context.Context, ExecutionRef) error
	Inspect(context.Context, ExecutionRef) (ExecutionStatus, error)
	Stop(context.Context, ExecutionRef) error
	Remove(context.Context, ExecutionRef) error
	Logs(context.Context, ExecutionRef) (string, error)
}

var (
	ErrNotFound = errors.New("workload not found")
	ErrConflict = errors.New("workload changed or idempotency key is already in use")
	ErrLimit    = errors.New("active workload limit reached")
	keyPattern  = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,128}$`)
	workerIDRe  = regexp.MustCompile(`^wk-[0-9a-f]{32}$`)
	gpuCaps     = map[string]bool{"cuda": true, "rocm": true, "metal": true, "vulkan": true, "level-zero": true, "oneapi": true}
)

func ValidKey(key string) bool { return keyPattern.MatchString(key) }

func ValidWorkerID(id string) bool { return id == LocalWorker || workerIDRe.MatchString(id) }

// Normalize sets the defaults before hashing a request for idempotency.
func (s *Submission) Normalize() error {
	switch s.Service {
	case "":
		s.Service = "run"
	case "workspace", "run", "deploy", "train", "infer", "store":
	default:
		return errors.New("unknown service")
	}
	if s.Kind == "" {
		s.Kind = "job"
	}
	if s.Kind != "job" && s.Kind != "service" {
		return errors.New("kind must be job or service")
	}
	switch s.Placement {
	case "":
		s.Placement = PlacementAuto
	case PlacementAuto, PlacementManual, PlacementPreferred:
	default:
		return errors.New("placement must be auto, manual or preferred")
	}
	if s.WorkerID != "" && !ValidWorkerID(s.WorkerID) {
		return errors.New("worker_id must be the local worker or a wk- worker identifier")
	}
	if s.Placement == PlacementManual && s.WorkerID == "" {
		return errors.New("manual placement requires worker_id")
	}
	if s.Placement != PlacementManual && s.WorkerID != "" {
		return errors.New("worker_id requires manual placement")
	}
	if s.Placement == PlacementPreferred {
		if len(s.PreferredWorkers) == 0 {
			return errors.New("preferred placement requires at least one preferred worker")
		}
		if len(s.PreferredWorkers) > 8 {
			return errors.New("at most 8 preferred workers are allowed")
		}
		for _, id := range s.PreferredWorkers {
			if !ValidWorkerID(id) {
				return errors.New("preferred workers must be the local worker or wk- worker identifiers")
			}
		}
	}
	if s.Placement != PlacementPreferred && len(s.PreferredWorkers) > 0 {
		return errors.New("preferred_workers require preferred placement")
	}
	if s.Fallback != nil && s.Placement != PlacementPreferred {
		return errors.New("fallback requires preferred placement")
	}
	if s.Priority == 0 {
		s.Priority = 50
	}
	if s.Priority < 1 || s.Priority > 100 {
		return errors.New("priority must be between 1 and 100")
	}
	if err := s.normalizeRuntime(); err != nil {
		return err
	}
	if s.Image == "" || len(s.Image) > 255 || strings.TrimSpace(s.Image) != s.Image ||
		strings.ContainsAny(s.Image, " \r\n\t\x00") {
		return errors.New("a valid local Docker image reference is required")
	}
	if len(s.Args) > 64 {
		return errors.New("at most 64 arguments are allowed")
	}
	for _, arg := range s.Args {
		if len(arg) > 4096 || strings.ContainsRune(arg, 0) {
			return errors.New("each argument must be at most 4096 bytes and contain no NUL")
		}
	}
	r := &s.Resources
	if r.CPUMillis == 0 {
		r.CPUMillis = 250
	}
	if r.MemoryBytes == 0 {
		r.MemoryBytes = 128 << 20
	}
	if r.CPUMillis < 10 || r.CPUMillis > 256000 || r.MemoryBytes < 16<<20 || r.MemoryBytes > 1<<40 ||
		r.GPUCount < 0 || r.GPUCount > 64 || r.StorageBytes < 0 || r.StorageBytes > 16<<40 {
		return errors.New("CPU, memory, storage or GPU count is outside the supported range")
	}
	if len(r.GPUCapabilities) > 4 {
		return errors.New("at most 4 GPU capabilities are allowed")
	}
	for i, capability := range r.GPUCapabilities {
		capability = strings.ToLower(strings.TrimSpace(capability))
		if !gpuCaps[capability] {
			return errors.New("GPU capabilities must be cuda, rocm, metal, vulkan, level-zero or oneapi")
		}
		r.GPUCapabilities[i] = capability
	}
	if r.Arch != "" && r.Arch != "amd64" && r.Arch != "arm64" {
		return errors.New("architecture must be amd64 or arm64")
	}
	if r.OS != "" && r.OS != "linux" && r.OS != "windows" && r.OS != "macos" {
		return errors.New("OS must be linux, windows or macos")
	}
	p := &s.Policy
	if p.TimeoutSeconds == 0 {
		p.TimeoutSeconds = 300
	}
	if p.TimeoutSeconds < 1 || p.TimeoutSeconds > 86400 || p.MaxRetries < 0 || p.MaxRetries > 3 {
		return fmt.Errorf("timeout must be 1–86400 seconds and retries 0–3")
	}
	return nil
}

var (
	volumeNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)
	digestRe     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	envKeyRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]{0,127}$`)
	imageHostRe  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]*(:[0-9]+)?$`)
)

// Stateful reports whether the workload owns persistent volumes. Stateful
// workloads pin to the worker that already holds their volumes.
func (s *Submission) Stateful() bool { return len(s.Volumes) > 0 }

// NetworkIsolated reports whether the workload uses the default isolated
// profile. Batch workloads always stay isolated.
func (s *Submission) NetworkIsolated() bool { return s.Network == "" || s.Network == "none" }

// SecretRefs lists environment entries that reference project secrets.
func (s *Submission) SecretRefs() []string {
	var refs []string
	for _, value := range s.Env {
		if name, ok := strings.CutPrefix(value, "secret:"); ok && name != "" {
			refs = append(refs, name)
		}
	}
	return refs
}

// normalizeRuntime validates the shared runtime contract additions: image
// digest pinning, environment references, network profile, persistent volumes,
// artifact mounts, output collection and health checks.
func (s *Submission) normalizeRuntime() error {
	if s.ImageDigest != "" && !digestRe.MatchString(s.ImageDigest) {
		return errors.New("image_digest must be a sha256 digest")
	}
	if len(s.Env) > 32 {
		return errors.New("at most 32 environment entries are allowed")
	}
	for key, value := range s.Env {
		if !envKeyRe.MatchString(key) {
			return errors.New("environment keys must match [A-Za-z_][A-Za-z0-9_.]*")
		}
		if len(value) > 4096 {
			return errors.New("environment values must be at most 4096 bytes")
		}
		if name, ok := strings.CutPrefix(value, "secret:"); ok {
			if !volumeNameRe.MatchString(name) {
				return errors.New("secret references must name a project secret")
			}
		}
	}
	switch s.Network {
	case "", "none", "private":
	default:
		return errors.New("network must be none or private")
	}
	if s.Network == "private" && s.Kind != "service" {
		return errors.New("the private network profile is available to services")
	}
	if len(s.Volumes) > 4 {
		return errors.New("at most 4 persistent volumes are allowed")
	}
	seen := map[string]bool{}
	for _, volume := range s.Volumes {
		if !volumeNameRe.MatchString(volume.Name) {
			return errors.New("volume names must be lowercase letters, digits and dashes")
		}
		if seen[volume.Name] {
			return errors.New("volume names must be unique")
		}
		seen[volume.Name] = true
		if volume.SizeBytes < 0 || volume.SizeBytes > 1<<40 {
			return errors.New("volume size must be between 0 and 1 TiB")
		}
	}
	if len(s.Artifacts) > 8 {
		return errors.New("at most 8 artifact mounts are allowed")
	}
	for _, mount := range s.Artifacts {
		if !strings.HasPrefix(mount.ArtifactID, "art-") || len(mount.ArtifactID) > 64 {
			return errors.New("artifact mounts must reference project artifact IDs")
		}
		if !validArtifactPath(mount.Path) {
			return errors.New("artifact mount paths must be absolute, normalized paths under /kamui/")
		}
	}
	if len(s.Outputs) > 8 {
		return errors.New("at most 8 output paths are allowed")
	}
	for _, out := range s.Outputs {
		if !strings.HasPrefix(out, "/out/") || !validArtifactPath(out) {
			return errors.New("output paths must be absolute, normalized paths under /out/")
		}
	}
	if s.HealthCheck != nil {
		hc := s.HealthCheck
		if hc.Kind != "http" && hc.Kind != "tcp" {
			return errors.New("health check kind must be http or tcp")
		}
		if hc.Port < 1 || hc.Port > 65535 {
			return errors.New("health check port must be between 1 and 65535")
		}
		if hc.Kind == "http" && !strings.HasPrefix(hc.Path, "/") {
			return errors.New("http health checks require an absolute path")
		}
		if hc.IntervalSeconds < 0 || hc.IntervalSeconds > 300 {
			return errors.New("health check interval must be between 0 and 300 seconds")
		}
	}
	return nil
}

func validArtifactPath(path string) bool {
	if len(path) == 0 || len(path) > 256 || !strings.HasPrefix(path, "/") {
		return false
	}
	clean := path
	for strings.Contains(clean, "//") {
		clean = strings.ReplaceAll(clean, "//", "/")
	}
	if clean != path || strings.HasSuffix(path, "/") {
		return false
	}
	for _, segment := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." || strings.ContainsRune(segment, 0) {
			return false
		}
	}
	return true
}

// RegistryHost extracts the registry host from an image reference for policy
// checks. A reference without an explicit host implies docker.io.
func RegistryHost(image string) string {
	first := image
	if idx := strings.Index(first, "/"); idx > 0 {
		first = first[:idx]
		if strings.ContainsAny(first, ".:") && imageHostRe.MatchString(first) {
			return strings.ToLower(first)
		}
	}
	return "docker.io"
}
