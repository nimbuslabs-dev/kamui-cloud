package core

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/distribution/reference"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
)

const (
	scopeLabel    = "dev.nimbuslabs.kamui.executor-scope"
	workloadLabel = "dev.nimbuslabs.kamui.workload"
	attemptLabel  = "dev.nimbuslabs.kamui.attempt"
	maxLogBytes   = 32 << 10
)

var ErrInvalidContainer = errors.New("invalid workload container")

type ExecutionRef struct {
	WorkloadID string `json:"workload_id"`
	Attempt    int    `json:"attempt"`
}

type ContainerSpec struct {
	Image       string   `json:"image"`
	Args        []string `json:"args"`
	CPUMillis   int64    `json:"cpu_millis"`
	MemoryBytes int64    `json:"memory_bytes"`
	Outputs     []string `json:"outputs,omitempty"`
}

type ExecutionStatus struct {
	Exists     bool      `json:"exists"`
	Started    bool      `json:"started"`
	Running    bool      `json:"running"`
	Unhealthy  bool      `json:"unhealthy"`
	ExitCode   int       `json:"exit_code"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

type DockerCapacity struct {
	ID               string `json:"id"`
	OS               string `json:"os"`
	Arch             string `json:"arch"`
	LogicalCPUs      int    `json:"logical_cpus"`
	MemoryTotalBytes int64  `json:"memory_total_bytes"`
}

type DockerExecutor struct {
	client *client.Client
	scope  string
	// Serialize inspect-and-mutate operations so a second Start cannot restart a completed attempt.
	mutations sync.Mutex
}

func NewDockerExecutor(cli *client.Client, scope string) (*DockerExecutor, error) {
	if cli == nil || scope == "" || len(scope) > 128 || strings.ContainsRune(scope, 0) {
		return nil, errors.New("Docker client and execution scope are required")
	}
	return &DockerExecutor{client: cli, scope: scope}, nil
}

func (d *DockerExecutor) Capacity(ctx context.Context) (DockerCapacity, error) {
	info, err := d.client.Info(ctx)
	if err != nil {
		return DockerCapacity{}, fmt.Errorf("query Docker Engine capacity: %w", err)
	}
	if info.OSType != "linux" {
		return DockerCapacity{}, errors.New("the local runtime requires a Linux Docker Engine")
	}
	if !info.CPUCfsQuota || !info.MemoryLimit || !info.PidsLimit {
		return DockerCapacity{}, errors.New("Docker Engine must enforce CPU, memory and PID limits")
	}
	if info.ID == "" {
		return DockerCapacity{}, errors.New("Docker Engine identity is unavailable")
	}
	arch := info.Architecture
	switch arch {
	case "x86_64":
		arch = "amd64"
	case "aarch64":
		arch = "arm64"
	}
	return DockerCapacity{
		ID: info.ID, OS: info.OSType, Arch: arch, LogicalCPUs: info.NCPU, MemoryTotalBytes: info.MemTotal,
	}, nil
}

func (d *DockerExecutor) name(ref ExecutionRef) string {
	hash := sha256.Sum256([]byte(d.scope + "\x00" + ref.WorkloadID))
	return fmt.Sprintf("kamui-workload-%x-%d", hash[:16], ref.Attempt)
}

func (d *DockerExecutor) find(ctx context.Context, ref ExecutionRef) (container.InspectResponse, error) {
	if ref.WorkloadID == "" || len(ref.WorkloadID) > 128 || strings.ContainsRune(ref.WorkloadID, 0) || ref.Attempt < 1 {
		return container.InspectResponse{}, errors.New("invalid execution reference")
	}
	inspected, err := d.client.ContainerInspect(ctx, d.name(ref))
	if errdefs.IsNotFound(err) {
		return container.InspectResponse{}, nil
	}
	if err != nil {
		return container.InspectResponse{}, fmt.Errorf("inspect workload container: %w", err)
	}
	if inspected.Config == nil || inspected.ContainerJSONBase == nil || inspected.ID == "" || inspected.State == nil {
		return container.InspectResponse{}, errors.New("Docker returned an incomplete workload inspection")
	}
	labels := inspected.Config.Labels
	if labels[scopeLabel] != d.scope || labels[workloadLabel] != ref.WorkloadID || labels[attemptLabel] != strconv.Itoa(ref.Attempt) {
		return container.InspectResponse{}, errors.New("container ownership does not match the workload attempt")
	}
	return inspected, nil
}

func (d *DockerExecutor) Create(ctx context.Context, ref ExecutionRef, spec ContainerSpec) error {
	d.mutations.Lock()
	defer d.mutations.Unlock()
	existing, err := d.find(ctx, ref)
	if err != nil || existing.ContainerJSONBase != nil {
		return err
	}
	if len(spec.Image) > 255 || spec.CPUMillis <= 0 || spec.CPUMillis > 1_024_000 || spec.MemoryBytes < 6<<20 {
		return fmt.Errorf("%w: invalid Docker image or resource limits", ErrInvalidContainer)
	}
	if _, err := reference.ParseAnyReference(spec.Image); err != nil {
		return fmt.Errorf("%w: invalid Docker image reference", ErrInvalidContainer)
	}
	// Resolve an operator-provisioned image without initiating registry requests; pin the ID
	// before checking metadata so a tag replacement cannot change the resulting container.
	image, err := d.client.ImageInspect(ctx, spec.Image)
	if errdefs.IsNotFound(err) {
		return fmt.Errorf("%w: image is not available locally", ErrInvalidContainer)
	}
	if err != nil {
		return fmt.Errorf("inspect workload image: %w", err)
	}
	if image.Os != "linux" || image.Config == nil || len(image.Config.Volumes) != 0 || image.ID == "" {
		return fmt.Errorf("%w: workload requires a Linux image without declared volumes", ErrInvalidContainer)
	}
	cfg, host := d.containerConfig(ref, spec, image.ID)
	_, err = d.client.ContainerCreate(ctx, cfg, host, nil, nil, d.name(ref))
	if errdefs.IsConflict(err) {
		existing, findErr := d.find(ctx, ref)
		if findErr != nil || existing.ContainerJSONBase != nil {
			return findErr
		}
	}
	if err != nil {
		return fmt.Errorf("create workload container: %w", err)
	}
	return nil
}

func (d *DockerExecutor) containerConfig(ref ExecutionRef, spec ContainerSpec, imageID string) (*container.Config, *container.HostConfig) {
	pidLimit := int64(128)
	init := true
	cfg := &container.Config{
		Image: imageID,
		Cmd:   spec.Args,
		User:  "65534:65534",
		Labels: map[string]string{
			scopeLabel: d.scope, workloadLabel: ref.WorkloadID, attemptLabel: strconv.Itoa(ref.Attempt),
		},
	}
	outputBinds := []string{}
	if len(spec.Outputs) > 0 {
		// An assignment-scoped scratch volume collects declared outputs; the
		// workload itself never sees a host path.
		outputBinds = append(outputBinds, fmt.Sprintf("kamui-out-%s-%d:/out", ref.WorkloadID, ref.Attempt))
	}
	host := &container.HostConfig{
		Init:           &init,
		NetworkMode:    "none",
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges:true"},
		Binds:          outputBinds,
		Tmpfs:          map[string]string{"/tmp": "rw,nosuid,nodev,noexec,size=32m,mode=1777"},
		Resources: container.Resources{
			NanoCPUs: spec.CPUMillis * 1_000_000, Memory: spec.MemoryBytes, MemorySwap: spec.MemoryBytes,
			PidsLimit: &pidLimit,
		},
		LogConfig: container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "1m", "max-file": "2"}},
	}
	return cfg, host
}

func (d *DockerExecutor) Start(ctx context.Context, ref ExecutionRef) error {
	d.mutations.Lock()
	defer d.mutations.Unlock()
	inspected, err := d.find(ctx, ref)
	if err != nil {
		return err
	}
	if inspected.ContainerJSONBase == nil {
		return errors.New("workload container does not exist")
	}
	if inspected.State.Running || inspected.State.Status != "created" {
		return nil
	}
	if err := d.client.ContainerStart(ctx, inspected.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start workload container: %w", err)
	}
	return nil
}

func (d *DockerExecutor) Inspect(ctx context.Context, ref ExecutionRef) (ExecutionStatus, error) {
	inspected, err := d.find(ctx, ref)
	if err != nil || inspected.ContainerJSONBase == nil {
		return ExecutionStatus{}, err
	}
	state := inspected.State
	startedAt, _ := time.Parse(time.RFC3339Nano, state.StartedAt)
	finishedAt, _ := time.Parse(time.RFC3339Nano, state.FinishedAt)
	return ExecutionStatus{
		Exists: true, Started: state.Status != "created", Running: state.Running,
		Unhealthy: state.Paused || state.Restarting || (state.Health != nil && state.Health.Status == container.Unhealthy),
		ExitCode:  state.ExitCode, StartedAt: startedAt, FinishedAt: finishedAt,
	}, nil
}

func (d *DockerExecutor) Stop(ctx context.Context, ref ExecutionRef) error {
	d.mutations.Lock()
	defer d.mutations.Unlock()
	inspected, err := d.find(ctx, ref)
	if err != nil || inspected.ContainerJSONBase == nil {
		return err
	}
	if inspected.State.Status == "created" {
		// A stopped, never-started Docker container remains startable. Delete this attempt instead.
		return d.remove(ctx, inspected.ID)
	}
	if !inspected.State.Running && !inspected.State.Restarting {
		return nil
	}
	timeout := 3
	if err := d.client.ContainerStop(ctx, inspected.ID, container.StopOptions{Timeout: &timeout}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("stop workload container: %w", err)
	}
	return nil
}

func (d *DockerExecutor) Remove(ctx context.Context, ref ExecutionRef) error {
	d.mutations.Lock()
	defer d.mutations.Unlock()
	inspected, err := d.find(ctx, ref)
	if err != nil || inspected.ContainerJSONBase == nil {
		return err
	}
	return d.remove(ctx, inspected.ID)
}

func (d *DockerExecutor) remove(ctx context.Context, id string) error {
	if err := d.client.ContainerRemove(ctx, id, container.RemoveOptions{}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove workload container: %w", err)
	}
	return nil
}

func (d *DockerExecutor) Logs(ctx context.Context, ref ExecutionRef) (string, error) {
	inspected, err := d.find(ctx, ref)
	if err != nil || inspected.ContainerJSONBase == nil {
		return "", err
	}
	logs, err := d.client.ContainerLogs(ctx, inspected.ID, container.LogsOptions{ShowStdout: true, ShowStderr: true, Tail: "200"})
	if err != nil {
		return "", fmt.Errorf("read workload logs: %w", err)
	}
	defer logs.Close()
	return readDockerLogs(logs)
}

// Decode Docker's multiplexed non-TTY output without allocating for a daemon-reported frame size.
func readDockerLogs(logs io.Reader) (string, error) {
	var output strings.Builder
	var header [8]byte
	for output.Len() < maxLogBytes {
		if _, err := io.ReadFull(logs, header[:]); errors.Is(err, io.EOF) {
			return output.String(), nil
		} else if err != nil {
			return "", fmt.Errorf("read workload log frame: %w", err)
		}
		if header[0] != 1 && header[0] != 2 {
			return "", errors.New("Docker returned an invalid workload log stream")
		}
		size := int64(binary.BigEndian.Uint32(header[4:]))
		if _, err := io.CopyN(&output, logs, min(int64(maxLogBytes-output.Len()), size)); err != nil {
			return "", fmt.Errorf("read workload log payload: %w", err)
		}
	}
	return output.String() + "\n[log output truncated]\n", nil
}

// CollectOutput reads one declared output file from an exited attempt's
// output volume through the container's /out mount.
func (d *DockerExecutor) CollectOutput(ctx context.Context, workloadID string, attempt int, containerPath string) ([]byte, error) {
	if workloadID == "" || len(workloadID) > 128 || strings.ContainsRune(workloadID, 0) || attempt < 1 {
		return nil, errors.New("invalid output lookup")
	}
	if !strings.HasPrefix(containerPath, "/out/") && containerPath != "/out" {
		return nil, errors.New("outputs must live under /out/")
	}
	ref := ExecutionRef{WorkloadID: workloadID, Attempt: attempt}
	inspected, err := d.find(ctx, ref)
	if err != nil || inspected.ContainerJSONBase == nil {
		return nil, errors.New("the attempt container is no longer present")
	}
	reader, _, err := d.client.CopyFromContainer(ctx, inspected.ID, containerPath)
	if err != nil {
		return nil, fmt.Errorf("collect output %s: %w", containerPath, err)
	}
	defer reader.Close()
	// CopyFromContainer yields a tar stream; extract the first regular file.
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return nil, errors.New("output is empty")
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag == tar.TypeReg {
			data, err := io.ReadAll(io.LimitReader(tarReader, maxLogBytes))
			if err != nil {
				return nil, err
			}
			return data, nil
		}
	}
}

// ContainerServiceAddress resolves the IP a workload's container holds on its
// project network, for same-daemon service routing. It inspects by ownership
// labels so a replaced assignment always wins over stale addresses.
func (d *DockerExecutor) ContainerServiceAddress(ctx context.Context, workloadID string, attempt int) (string, error) {
	if workloadID == "" || len(workloadID) > 128 || strings.ContainsRune(workloadID, 0) || attempt < 1 {
		return "", errors.New("invalid service lookup")
	}
	containers, err := d.client.ContainerList(ctx, container.ListOptions{
		All: true,
		Filters: filters.NewArgs(
			filters.Arg("label", scopeLabel+"="+d.scope),
			filters.Arg("label", workloadLabel+"="+workloadID),
			filters.Arg("label", attemptLabel+"="+strconv.Itoa(attempt)),
		),
	})
	if err != nil {
		return "", fmt.Errorf("list workload containers: %w", err)
	}
	for _, summary := range containers {
		for _, network := range summary.NetworkSettings.Networks {
			if network.IPAddress != "" {
				return network.IPAddress, nil
			}
		}
	}
	if len(containers) == 0 {
		return "", errors.New("service container is not running")
	}
	return "", errors.New("service container has no address on a reachable network")
}
