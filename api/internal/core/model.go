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

type Worker struct {
	ID        string          `json:"id"`
	Device    *Device         `json:"device,omitempty"`
	Runtime   *DockerCapacity `json:"docker,omitempty"`
	Health    string          `json:"health"`
	Heartbeat time.Time       `json:"heartbeat"`
}

const LocalWorker = "local"

type Resources struct {
	CPUMillis   int64  `json:"cpu_millis"`
	MemoryBytes int64  `json:"memory_bytes"`
	OS          string `json:"os,omitempty"`
	Arch        string `json:"arch,omitempty"`
	GPUCount    int    `json:"gpu_count,omitempty"`
}

type Reservation struct {
	CPUMillis   int64 `json:"cpu_millis"`
	MemoryBytes int64 `json:"memory_bytes"`
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

type Submission struct {
	Service   string    `json:"service"`
	Kind      string    `json:"kind"`
	Image     string    `json:"image"`
	Args      []string  `json:"args,omitempty"`
	Resources Resources `json:"resources"`
	Policy    Policy    `json:"policy"`
	WorkerID  string    `json:"worker_id,omitempty"`
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

func (s State) terminal() bool {
	return s == Succeeded || s == Failed || s == Cancelled || s == Stopped
}

type Assignment struct {
	RuntimeID   string      `json:"runtime_id"`
	WorkerID    string      `json:"worker_id"`
	Attempt     int         `json:"attempt"`
	LeaseUntil  time.Time   `json:"lease_until"`
	Deadline    time.Time   `json:"deadline"`
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
)

func ValidKey(key string) bool { return keyPattern.MatchString(key) }

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
	if s.WorkerID != "" && s.WorkerID != LocalWorker {
		return errors.New("manual worker must be local")
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
		r.GPUCount < 0 || r.GPUCount > 64 {
		return errors.New("CPU, memory or GPU count is outside the supported range")
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
