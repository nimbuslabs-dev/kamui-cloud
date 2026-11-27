package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"
)

const (
	projectLabel   = "dev.nimbuslabs.kamui.project"
	workspaceLabel = "dev.nimbuslabs.kamui.workspace"
)

func (wm *WorkspaceManager) checkDocker(ctx context.Context) error {
	if _, err := wm.docker.Ping(ctx); err != nil {
		return fmt.Errorf("ping Docker daemon: %w", err)
	}
	ide, err := wm.docker.NetworkInspect(ctx, wm.network, network.InspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect IDE network: %w", err)
	}
	if ide.Driver != "bridge" || ide.Labels["com.docker.compose.project"] != wm.project {
		return errors.New("IDE network is not the project-owned bridge")
	}
	if _, err := wm.docker.ImageInspect(ctx, wm.image); err != nil {
		return fmt.Errorf("workspace image %q must be built before startup: %w", wm.image, err)
	}
	return nil
}

func (wm *WorkspaceManager) containerName(id string) string {
	return wm.project + "-workspace-" + id
}

func (wm *WorkspaceManager) findContainer(ctx context.Context, id string) (container.InspectResponse, error) {
	inspected, err := wm.docker.ContainerInspect(ctx, wm.containerName(id))
	if errdefs.IsNotFound(err) {
		return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{}}, nil
	}
	if err != nil {
		return container.InspectResponse{}, fmt.Errorf("inspect workspace container: %w", err)
	}
	if inspected.Config == nil || inspected.Config.Labels[projectLabel] != wm.project || inspected.Config.Labels[workspaceLabel] != id {
		return container.InspectResponse{}, errors.New("refusing to operate on a container without matching workspace ownership labels")
	}
	return inspected, nil
}

type workspaceState struct {
	Status      string `json:"status"`
	ContainerID string `json:"container_id,omitempty"`
}

func (wm *WorkspaceManager) workspaceStatus(ctx context.Context, id string) (workspaceState, error) {
	inspected, err := wm.findContainer(ctx, id)
	if err != nil {
		return workspaceState{}, err
	}
	return dockerState(inspected), nil
}

func dockerState(inspected container.InspectResponse) workspaceState {
	if inspected.ContainerJSONBase == nil {
		return workspaceState{Status: "stopped"}
	}
	status := "stopped"
	state := inspected.State
	if state != nil {
		switch {
		case state.Dead || state.OOMKilled || state.Paused:
			status = "error"
		case state.Restarting:
			status = "starting"
		case state.Running:
			status = "starting"
			if state.Health != nil {
				switch state.Health.Status {
				case container.Healthy:
					status = "running"
				case container.Unhealthy:
					status = "error"
				}
			}
		case state.Status == "exited" && state.ExitCode != 0 && state.ExitCode != 137 && state.ExitCode != 143:
			status = "error"
		}
	}
	return workspaceState{Status: status, ContainerID: inspected.ID}
}

func (wm *WorkspaceManager) ensureVolume(ctx context.Context, id string) (string, error) {
	name := wm.containerName(id) + "-project"
	labels := map[string]string{projectLabel: wm.project, workspaceLabel: id}
	stored, err := wm.docker.VolumeInspect(ctx, name)
	if errdefs.IsNotFound(err) {
		stored, err = wm.docker.VolumeCreate(ctx, volume.CreateOptions{Name: name, Driver: "local", Labels: labels})
	}
	if err != nil {
		return "", fmt.Errorf("open workspace volume: %w", err)
	}
	if stored.Driver != "local" || len(stored.Options) != 0 || stored.Labels[projectLabel] != wm.project || stored.Labels[workspaceLabel] != id {
		return "", errors.New("refusing to attach a volume without matching workspace ownership labels")
	}
	return name, nil
}

func (wm *WorkspaceManager) workspaceSpec(id, volumeName string) (*container.Config, *container.HostConfig, *network.NetworkingConfig) {
	init := true
	pids := int64(256)
	cfg := &container.Config{
		Image:  wm.image,
		User:   "1000:1000",
		Labels: map[string]string{projectLabel: wm.project, workspaceLabel: id},
		Cmd:    []string{"--bind-addr", "0.0.0.0:8080", "--auth", "none", "--disable-proxy", projectDir},
		Healthcheck: &container.HealthConfig{
			Test:     []string{"CMD-SHELL", "test -f /home/coder/.kamui-initialized && /usr/bin/curl --fail --silent --max-time 2 http://127.0.0.1:8080/healthz"},
			Interval: 2 * time.Second, Timeout: 3 * time.Second, StartPeriod: 60 * time.Second, Retries: 4,
		},
	}
	host := &container.HostConfig{
		Init:        &init,
		NetworkMode: container.NetworkMode(wm.network),
		SecurityOpt: []string{"no-new-privileges:true"},
		CapDrop:     []string{"ALL"},
		Tmpfs:       map[string]string{"/tmp": "rw,nosuid,nodev,size=256m,mode=1777"},
		Mounts:      []mount.Mount{{Type: mount.TypeVolume, Source: volumeName, Target: projectDir}},
		Resources:   container.Resources{NanoCPUs: wm.nanoCPUs, Memory: wm.memoryBytes, MemorySwap: wm.memoryBytes, PidsLimit: &pids},
		LogConfig:   container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "10m", "max-file": "3"}},
	}
	networks := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
		wm.network: {Aliases: []string{"kamui-workspace-" + id}},
	}}
	return cfg, host, networks
}

func (wm *WorkspaceManager) startWorkspace(ctx context.Context, req WorkspaceConfig) error {
	wm.mutation.Lock()
	defer wm.mutation.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	inspected, err := wm.findContainer(ctx, req.ID)
	if err != nil {
		return err
	}
	id := inspected.ID
	running := inspected.State != nil && inspected.State.Running
	if !running {
		active, err := wm.docker.ContainerList(ctx, container.ListOptions{Filters: filters.NewArgs(filters.Arg("label", projectLabel+"="+wm.project))})
		if err != nil {
			return fmt.Errorf("list active workspaces: %w", err)
		}
		if len(active) != 0 {
			return errNoSlot
		}
	}
	if id == "" {
		name, err := wm.ensureVolume(ctx, req.ID)
		if err != nil {
			return err
		}
		cfg, host, networks := wm.workspaceSpec(req.ID, name)
		created, err := wm.docker.ContainerCreate(ctx, cfg, host, networks, nil, wm.containerName(req.ID))
		if err != nil {
			return fmt.Errorf("create workspace container: %w", err)
		}
		id = created.ID
	}
	if !running {
		if err := wm.docker.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
			return fmt.Errorf("start workspace container: %w", err)
		}
	}
	if err := wm.cloneRepository(ctx, id, req.RepositoryURL, req.Branch, req.GitToken); err != nil {
		return wm.stopFailedStart(id, err)
	}
	mark, err := wm.exec(ctx, id, "/usr/bin/touch", "/home/coder/.kamui-initialized")
	if err != nil {
		return wm.stopFailedStart(id, err)
	}
	if mark.exitCode != 0 {
		return wm.stopFailedStart(id, errors.New("mark workspace initialization complete"))
	}
	if err := wm.waitReady(ctx, id); err != nil {
		return wm.stopFailedStart(id, err)
	}
	return nil
}

func (wm *WorkspaceManager) waitReady(ctx context.Context, id string) error {
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	for {
		inspected, err := wm.docker.ContainerInspect(ctx, id)
		if err != nil {
			return fmt.Errorf("inspect workspace readiness: %w", err)
		}
		state := dockerState(inspected).Status
		if state == "running" {
			return nil
		}
		if state == "error" || state == "stopped" {
			return &managerError{http.StatusBadGateway, "ide_unavailable", "code-server did not become healthy"}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-poll.C:
		}
	}
}

func (wm *WorkspaceManager) stopFailedStart(id string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	timeout := 2
	if err := wm.docker.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("cleanup after workspace start failure (%v): %w", cause, err)
	}
	return cause
}

func (wm *WorkspaceManager) stopWorkspace(ctx context.Context, id string) error {
	wm.mutation.Lock()
	defer wm.mutation.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	inspected, err := wm.findContainer(ctx, id)
	if err != nil || inspected.ID == "" || (inspected.State != nil && !inspected.State.Running && !inspected.State.Restarting) {
		return err
	}
	timeout := 10
	if err := wm.docker.ContainerStop(ctx, inspected.ID, container.StopOptions{Timeout: &timeout}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("stop workspace container: %w", err)
	}
	return nil
}

// workspaceLogs returns the bounded tail of a workspace container's logs.
func (wm *WorkspaceManager) workspaceLogs(ctx context.Context, id string) (string, error) {
	inspected, err := wm.findContainer(ctx, id)
	if err != nil {
		return "", err
	}
	if inspected.ID == "" {
		return "", nil
	}
	logs, err := wm.docker.ContainerLogs(ctx, inspected.ID, container.LogsOptions{
		ShowStdout: true, ShowStderr: true, Tail: "200",
	})
	if err != nil {
		return "", fmt.Errorf("workspace container logs: %w", err)
	}
	defer logs.Close()
	output, err := io.ReadAll(io.LimitReader(logs, 64<<10))
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func (wm *WorkspaceManager) deleteWorkspace(ctx context.Context, id string) error {
	wm.mutation.Lock()
	defer wm.mutation.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	inspected, err := wm.findContainer(ctx, id)
	if err != nil || inspected.ID == "" {
		return err
	}
	if err := wm.docker.ContainerRemove(ctx, inspected.ID, container.RemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove workspace container: %w", err)
	}
	return nil // Keep the named project volume for recovery.
}
