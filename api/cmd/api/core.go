package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"

	"github.com/docker/docker/client"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/config"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/connect"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/runtime"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/worker"
)

func openCore(ctx context.Context, cfg *config.Config) (*core.Engine, *identity.Store, *connect.Store, *worker.Store, *runtime.Store, func(), error) {
	path := os.Getenv("KAMUI_CORE_DATA_FILE")
	if path == "" {
		path = filepath.Join(filepath.Dir(cfg.WorkspaceDataFile), "kamui-core.sqlite")
	}
	for _, candidate := range []string{path, path + ".lock", path + "-wal", path + "-shm"} {
		if samePath(candidate, cfg.WorkspaceDataFile) {
			return nil, nil, nil, nil, nil, nil, errors.New("core and workspace data must use separate files")
		}
	}

	var executor core.Executor
	var probe core.Probe
	closeExecutor := func() {}
	switch mode := os.Getenv("KAMUI_CORE_EXECUTOR"); mode {
	case "", "local":
		cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return nil, nil, nil, nil, nil, nil, err
		}
		closeExecutor = func() { _ = cli.Close() }
		local, err := core.NewDockerExecutor(cli, "native")
		if err != nil {
			closeExecutor()
			return nil, nil, nil, nil, nil, nil, err
		}
		executor = local
		probe = hostProbe(path)
	case "manager":
		bridge, err := core.NewBridge(cfg.WorkspaceManagerURL, cfg.Token)
		if err != nil {
			return nil, nil, nil, nil, nil, nil, err
		}
		executor = bridge
	default:
		return nil, nil, nil, nil, nil, nil, errors.New("KAMUI_CORE_EXECUTOR must be local or manager")
	}
	store, err := core.Open(ctx, path)
	if err != nil {
		closeExecutor()
		return nil, nil, nil, nil, nil, nil, err
	}
	cleanup := func() {
		_ = store.Close()
		closeExecutor()
	}
	idStore := identity.NewStore(store.DB(), cfg.Token, cfg.AdminPassword)
	if err := idStore.EnsureAdminBootstrap(ctx); err != nil {
		cleanup()
		return nil, nil, nil, nil, nil, nil, err
	}
	connStore := connect.NewStore(store.DB(), idStore, cfg.CAPath)
	engine := core.NewEngine(store, executor, probe)
	workers := worker.NewStore(store.DB())
	workers.UseLocalStore(store)
	// Cluster scheduling extends the engine; a nil store would keep the
	// single-host behavior, which is never what a Kamui deployment wants here.
	engine.Cluster = workers
	storageRoot := os.Getenv("KAMUI_STORAGE_ROOT")
	if storageRoot == "" {
		storageRoot = filepath.Join(filepath.Dir(path), "kamui-storage")
	}
	runtimeStore := runtime.NewStore(store.DB(), cfg.Token, storageRoot)
	engine.Volumes = runtimeStore
	if err := engine.Register(ctx); err != nil {
		cleanup()
		return nil, nil, nil, nil, nil, nil, err
	}
	return engine, idStore, connStore, workers, runtimeStore, cleanup, nil
}

func samePath(a, b string) bool {
	first, _ := filepath.Abs(a)
	second, _ := filepath.Abs(b)
	if first == second {
		return true
	}
	firstInfo, e1 := os.Stat(first)
	secondInfo, e2 := os.Stat(second)
	return e1 == nil && e2 == nil && os.SameFile(firstInfo, secondInfo)
}

func hostProbe(dataFile string) core.Probe {
	agent := os.Getenv("KAMUI_AGENT_PATH")
	if agent == "" {
		agent, _ = exec.LookPath("kamui-agent")
	}
	return func(ctx context.Context) (*core.Device, error) {
		device := &core.Device{ID: core.LocalWorker}
		cpus := goruntime.NumCPU()
		hostname, _ := os.Hostname()
		device.Capabilities = core.Capabilities{Hostname: hostname, OS: goruntime.GOOS, Arch: goruntime.GOARCH, LogicalCPUs: &cpus}
		if agent == "" {
			return device, nil
		}
		cmd := exec.CommandContext(ctx, agent, "capabilities", filepath.Dir(dataFile))
		raw, err := cmd.Output()
		if err != nil {
			return device, fmt.Errorf("run kamui-agent: %w", err)
		}
		if err := json.Unmarshal(raw, &device.Capabilities); err != nil {
			return nil, errors.New("decode kamui-agent capabilities")
		}
		return device, nil
	}
}
