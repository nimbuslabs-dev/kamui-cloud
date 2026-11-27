package core

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

func TestDockerExecutorRejectsForeignContainer(t *testing.T) {
	executor := dockerTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("foreign container was targeted by a mutation")
		}
		_ = json.NewEncoder(w).Encode(container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{ID: "foreign", State: &container.State{Running: true}},
			Config:            &container.Config{Labels: map[string]string{}},
		})
	})
	if err := executor.Stop(context.Background(), ExecutionRef{WorkloadID: "a", Attempt: 1}); err == nil {
		t.Fatal("expected container ownership rejection")
	}
}

type dockerTestTransport func(*http.Request) (*http.Response, error)

func (fn dockerTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func dockerTestExecutor(t *testing.T, handler http.HandlerFunc) *DockerExecutor {
	t.Helper()
	transport := dockerTestTransport(func(r *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler(recorder, r)
		return recorder.Result(), nil
	})
	cli, err := client.NewClientWithOpts(
		client.WithHost("http://docker.invalid"), client.WithVersion("1.47"),
		client.WithHTTPClient(&http.Client{Transport: transport}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	executor, err := NewDockerExecutor(cli, "tester")
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func TestDockerExecutorDoesNotRestartExitedAttempt(t *testing.T) {
	started := false
	executor := dockerTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/container-id/start") {
			if started {
				t.Error("already exited attempt was started again")
			}
			started = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/json") {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		state := &container.State{Status: "created"}
		if started {
			state = &container.State{Status: "exited", ExitCode: 7, StartedAt: "2026-11-27T12:00:00Z"}
		}
		_ = json.NewEncoder(w).Encode(container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{ID: "container-id", State: state},
			Config: &container.Config{Labels: map[string]string{
				scopeLabel: "tester", workloadLabel: "a", attemptLabel: "2",
			}},
		})
	})
	ctx := context.Background()
	ref := ExecutionRef{WorkloadID: "a", Attempt: 2}
	for range 2 {
		if err := executor.Start(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	status, err := executor.Inspect(ctx, ref)
	if err != nil || !status.Exists || !status.Started || status.Running || status.ExitCode != 7 || status.StartedAt.IsZero() {
		t.Fatalf("unexpected exit state: %+v, %v", status, err)
	}
}

func TestDockerExecutorRemovesCreatedAttemptOnStop(t *testing.T) {
	deleted := false
	executor := dockerTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/container-id") {
			deleted = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if deleted {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{ID: "container-id", State: &container.State{Status: "created"}},
			Config: &container.Config{Labels: map[string]string{
				scopeLabel: "tester", workloadLabel: "a", attemptLabel: "1",
			}},
		})
	})
	ctx := context.Background()
	ref := ExecutionRef{WorkloadID: "a", Attempt: 1}
	if err := executor.Stop(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("unstarted attempt was left startable")
	}
	if err := executor.Stop(ctx, ref); err != nil {
		t.Fatal(err)
	}
}

func TestDockerExecutorRequiresPreinstalledVolumeFreeImages(t *testing.T) {
	for _, test := range []struct {
		name       string
		statusCode int
	}{
		{name: "missing", statusCode: http.StatusNotFound},
		{name: "volume", statusCode: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor := dockerTestExecutor(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected mutation: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if strings.Contains(r.URL.Path, "/containers/") {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.WriteHeader(test.statusCode)
				_, _ = w.Write([]byte(`{"Id":"sha256:someid","Os":"linux","Config":{"Volumes":{"/data":{}}}}`))
			})
			err := executor.Create(context.Background(), ExecutionRef{WorkloadID: "a", Attempt: 1}, ContainerSpec{
				Image: "alpine", CPUMillis: 500, MemoryBytes: 32 << 20,
			})
			if !errors.Is(err, ErrInvalidContainer) {
				t.Fatalf("want permanent invalid container error, got %v", err)
			}
		})
	}
}

func TestDockerWorkloadIsolation(t *testing.T) {
	d := &DockerExecutor{scope: "test"}
	args := []string{"arg1", "literal;arg2"}
	cfg, host := d.containerConfig(ExecutionRef{WorkloadID: "a", Attempt: 2}, ContainerSpec{
		Image: "alpine", Args: args, CPUMillis: 500, MemoryBytes: 32 << 20,
	}, "sha256:someid")
	if cfg.Image != "sha256:someid" || !slices.Equal(cfg.Cmd, args) || cfg.User != "65534:65534" || cfg.Labels[attemptLabel] != "2" {
		t.Fatalf("unexpected process configuration: %+v", cfg)
	}
	if host.NetworkMode != "none" || !host.ReadonlyRootfs || host.Privileged ||
		len(host.Binds)+len(host.Mounts)+len(host.Devices) != 0 ||
		!slices.Equal(host.CapDrop, []string{"ALL"}) ||
		!slices.Equal(host.SecurityOpt, []string{"no-new-privileges:true"}) {
		t.Fatalf("workload isolation settings are incomplete: %+v", host)
	}
	if host.NanoCPUs != 500_000_000 || host.Memory != 32<<20 || host.MemorySwap != host.Memory ||
		host.PidsLimit == nil || *host.PidsLimit == 0 {
		t.Fatalf("workload limits are incomplete: %+v", host.Resources)
	}
}

func TestReadDockerLogsBoundsMultiplexedFrames(t *testing.T) {
	var stream bytes.Buffer
	for fd, data := range []string{"stdout\n", "stderr\n"} {
		var header [8]byte
		header[0] = byte(fd) + 1
		binary.BigEndian.PutUint32(header[4:], uint32(len(data)))
		stream.Write(header[:])
		stream.WriteString(data)
	}
	logs, err := readDockerLogs(&stream)
	if err != nil || logs != "stdout\nstderr\n" {
		t.Fatalf("unexpected logs: %q, %v", logs, err)
	}
	stream.Reset()
	var header [8]byte
	header[0] = 1
	binary.BigEndian.PutUint32(header[4:], ^uint32(0))
	stream.Write(header[:])
	stream.WriteString(strings.Repeat("x", maxLogBytes))
	logs, err = readDockerLogs(&stream)
	if err != nil || len(logs) > maxLogBytes+25 || !strings.Contains(logs, "truncated") {
		t.Fatalf("unexpected truncated log length %d: %v", len(logs), err)
	}
}
