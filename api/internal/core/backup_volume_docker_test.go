package core

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/auth"
)

func volumeFixture(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	contents := []byte("Kamui managed-volume deterministic fixture\n")
	for _, h := range []*tar.Header{
		{Name: ".", Typeflag: tar.TypeDir, Mode: 0750, Uid: 900, Gid: 901},
		{Name: "data", Typeflag: tar.TypeDir, Mode: 0751, Uid: 900, Gid: 901},
		{Name: "data/payload.txt", Typeflag: tar.TypeReg, Mode: 0640, Uid: 900, Gid: 901, Size: int64(len(contents))},
	} {
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestVolumeArchivePolicy(t *testing.T) {
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	if err := w.WriteHeader(&tar.Header{Name: "symlink", Typeflag: tar.TypeSymlink, Linkname: "untrusted"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := scanVolumeArchive(t.Context(), &b, nil); !errors.Is(err, ErrBackupVolumeArchive) {
		t.Fatalf("symlink archive err = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := scanVolumeArchive(ctx, bytes.NewReader(volumeFixture(t)), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled archive err = %v", err)
	}
}

func TestDockerManagedVolumeBackupRestore(t *testing.T) {
	if os.Getenv("KAMUI_TEST_DOCKER_VOLUMES") != "1" {
		t.Skip("requires an isolated Docker Engine and local busybox:1.37 image")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	capacity, err := cli.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var random [12]byte
	rand.Read(random[:])
	d, err := NewDockerExecutor(cli, fmt.Sprintf("kamui-backup-test-%x", random))
	if err != nil {
		t.Fatal(err)
	}
	source := BackupVolumeRef{ProjectID: "default", Name: "source"}
	restored := BackupVolumeRef{ProjectID: "default", Name: fmt.Sprintf("rst-%x", random)}
	// The executor scope and logical source are randomly generated for this invocation.
	source.Name = fmt.Sprintf("source-%x", random)
	name, err := d.prepareWorkloadVolume(ctx, source, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := cli.VolumeRemove(cleanup, name, false); err != nil {
			t.Error(err)
		}
	}()
	raw := volumeFixture(t)
	if err := d.withVolumeHelper(ctx, name, "", false, func(id string) error {
		return cli.CopyToContainer(ctx, id, volumeMountPath, bytes.NewReader(raw), container.CopyToContainerOptions{})
	}); err != nil {
		t.Fatal(err)
	}
	running, err := cli.ContainerCreate(ctx, &container.Config{Image: "busybox:1.37", Cmd: []string{"sleep", "60"}},
		&container.HostConfig{NetworkMode: "none", Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: name, Target: volumeMountPath}}},
		nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := cli.ContainerRemove(cleanup, running.ID, container.RemoveOptions{Force: true}); err != nil {
			t.Error(err)
		}
	}()
	if err := cli.ContainerStart(ctx, running.ID, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := d.BackupVolume(ctx, source, capacity.ID, &bytes.Buffer{}); !errors.Is(err, ErrBackupVolumeConflict) {
		t.Fatalf("busy volume err = %v", err)
	}
	if err := cli.ContainerStop(ctx, running.ID, container.StopOptions{Timeout: new(0)}); err != nil {
		t.Fatal(err)
	}
	t.Run("a missing pinned volume stays missing", func(t *testing.T) {
		ref := ExecutionRef{WorkloadID: "missing-pinned", Attempt: 1}
		missing := BackupVolumeRef{ProjectID: "default", Name: "missing"}
		spec := ContainerSpec{ProjectID: missing.ProjectID, Image: "busybox:1.37", CPUMillis: 100, MemoryBytes: 8 << 20,
			Volumes: []VolumeBinding{{Name: missing.Name, Target: volumeMountPath, RequireExisting: true}}}
		if err := d.Create(ctx, ref, spec); !errors.Is(err, ErrBackupVolumeNotFound) {
			t.Fatalf("missing pinned volume create err = %v", err)
		}
		physical, err := d.physicalVolume(missing)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cli.VolumeInspect(ctx, physical); !errdefs.IsNotFound(err) {
			t.Fatalf("pinned volume was recreated: %v", err)
		}
	})
	t.Run("independent workloads can start during an export", func(t *testing.T) {
		unrelated := ExecutionRef{WorkloadID: "unrelated", Attempt: 1}
		sameVolume := ExecutionRef{WorkloadID: "same-volume", Attempt: 1}
		spec := ContainerSpec{ProjectID: "default", Image: "busybox:1.37", Args: []string{"sleep", "15"},
			CPUMillis: 100, MemoryBytes: 8 << 20}
		for _, ref := range []ExecutionRef{unrelated, sameVolume} {
			if ref == sameVolume {
				spec.Volumes = []VolumeBinding{{Name: source.Name, Target: volumeMountPath}}
			}
			if err := d.Create(ctx, ref, spec); err != nil {
				t.Fatal(err)
			}
			defer func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := errors.Join(d.Stop(cleanup, ref), d.Remove(cleanup, ref)); err != nil {
					t.Error(err)
				}
			}()
		}
		sink := &pausedVolumeWriter{entered: make(chan struct{}), release: make(chan struct{})}
		backupDone := make(chan error, 1)
		go func() { backupDone <- d.BackupVolume(ctx, source, capacity.ID, sink) }()
		defer func() {
			close(sink.release)
			if err := <-backupDone; err != nil {
				t.Error(err)
			}
		}()
		select {
		case <-sink.entered:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		start := func(ref ExecutionRef) <-chan error {
			result := make(chan error, 1)
			go func() { result <- d.Start(ctx, ref) }()
			return result
		}
		select {
		case err := <-start(unrelated):
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("export blocked an unrelated workload start")
		}
		select {
		case err := <-start(sameVolume):
			if !errors.Is(err, ErrBackupVolumeConflict) {
				t.Fatalf("same-volume start err = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("export blocked the same-volume start instead of returning a retryable error")
		}
	})
	token := strings.Repeat("x", 32)
	server := httptest.NewServer(auth.Middleware(token)(BridgeHandler(d)))
	defer server.Close()
	bridge, err := NewBridge(server.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	var before bytes.Buffer
	if err := bridge.BackupVolume(ctx, source, capacity.ID, &before); err != nil {
		t.Fatal(err)
	}
	if err := d.BackupVolume(ctx, source, "wrong-engine", &bytes.Buffer{}); !errors.Is(err, ErrBackupVolumeLocality) {
		t.Fatalf("wrong engine matched: %v", err)
	}
	crossProject := BackupVolumeRef{ProjectID: "prj-000000000000000000000001", Name: source.Name}
	if err := d.BackupVolume(ctx, crossProject, capacity.ID, &bytes.Buffer{}); !errors.Is(err, ErrBackupVolumeNotFound) {
		t.Fatalf("cross-project volume err = %v", err)
	}
	if err := bridge.RestoreVolume(ctx, restored, capacity.ID, restored.Name, bytes.NewReader(before.Bytes())); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := bridge.CleanupRestoreVolume(cleanup, restored, capacity.ID, restored.Name); err != nil {
			t.Error(err)
		}
	}()
	if err := d.RestoreVolume(ctx, restored, capacity.ID, restored.Name, bytes.NewReader(before.Bytes())); !errors.Is(err, ErrBackupVolumeConflict) {
		t.Fatalf("repeat restore err = %v", err)
	}
	digest := func(archive []byte) volumeArchiveEntry {
		t.Helper()
		entries, err := scanVolumeArchive(ctx, bytes.NewReader(archive), nil)
		if err != nil {
			t.Fatal(err)
		}
		entry := entries["data/payload.txt"]
		if entry.digest == ([sha256.Size]byte{}) {
			t.Fatal("fixture content is missing")
		}
		return entry
	}
	expected := digest(before.Bytes())
	if expected.digest != sha256.Sum256([]byte("Kamui managed-volume deterministic fixture\n")) {
		t.Fatal("source payload digest differs from the original fixture")
	}
	for _, ref := range []BackupVolumeRef{source, restored} {
		var after bytes.Buffer
		if err := bridge.BackupVolume(ctx, ref, capacity.ID, &after); err != nil {
			t.Fatal(err)
		}
		got := digest(after.Bytes())
		if got != expected {
			t.Fatalf("volume %s file digest or permissions changed: %#v vs %#v", ref.Name, got, expected)
		}
		t.Logf("%s SHA-256 %x", ref.Name, got.digest)
	}
}

type pausedVolumeWriter struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (w *pausedVolumeWriter) Write(data []byte) (int, error) {
	w.once.Do(func() {
		close(w.entered)
		<-w.release
	})
	return io.Discard.Write(data)
}
