package core

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"
)

const (
	volumeProjectLabel      = "dev.nimbuslabs.kamui.volume-project"
	volumeNameLabel         = "dev.nimbuslabs.kamui.volume-name"
	volumeHelperLabel       = "dev.nimbuslabs.kamui.volume-helper"
	volumeRestoreLabel      = "dev.nimbuslabs.kamui.restore"
	managedVolumeMountLabel = "dev.nimbuslabs.kamui.managed-volume-mounts"
	volumeMountPath         = "/kamui-volume"
	maxVolumeArchive        = int64(8 << 30)
)

var (
	ErrBackupVolumeConflict = errors.New("volume is in use or conflicts with an existing Docker volume")
	ErrBackupVolumeLocality = errors.New("volume operation requires its original Docker engine")
	ErrBackupVolumeNotFound = errors.New("managed volume not found")
	ErrBackupVolumeArchive  = errors.New("invalid or unverifiable volume archive")
	errVolumeTransferBusy   = fmt.Errorf("%w: another volume operation is active", ErrBackupVolumeConflict)
	backupProjectRE         = regexp.MustCompile(`^(default|prj-[a-f0-9]{24})$`)
	restoreVolumeRE         = regexp.MustCompile(`^rst-[a-f0-9]{24}$`)
)

type BackupVolumeRef struct {
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
}

func (d *DockerExecutor) SetBackupHelperImage(image string) { d.backupHelperImage = image }

func (d *DockerExecutor) physicalVolume(ref BackupVolumeRef) (string, error) {
	if !backupProjectRE.MatchString(ref.ProjectID) || !volumeNameRe.MatchString(ref.Name) {
		return "", fmt.Errorf("%w: invalid project or volume name", ErrBackupVolumeNotFound)
	}
	return fmt.Sprintf("kamui-vol-%x", sha256.Sum256([]byte(d.scope+"\x00"+ref.ProjectID+"\x00"+ref.Name))), nil
}

func (d *DockerExecutor) volumeLabels(ref BackupVolumeRef) map[string]string {
	return map[string]string{scopeLabel: d.scope, volumeProjectLabel: ref.ProjectID, volumeNameLabel: ref.Name}
}

func (d *DockerExecutor) ownedVolume(v volume.Volume, name string, ref BackupVolumeRef) bool {
	if v.Name != name || v.Driver != "local" || v.Scope != "local" || len(v.Options) != 0 {
		return false
	}
	for key, value := range d.volumeLabels(ref) {
		if v.Labels[key] != value {
			return false
		}
	}
	return true
}

// prepareWorkloadVolume shares the canonical volume identity with backup and restore.
// Unlabelled legacy data needs an explicit migration before first use.
func (d *DockerExecutor) prepareWorkloadVolume(ctx context.Context, ref BackupVolumeRef, requireExisting bool) (string, error) {
	name, err := d.physicalVolume(ref)
	if err != nil {
		return "", err
	}
	if d.volumeInTransfer == name {
		return "", errVolumeTransferBusy
	}
	v, err := d.client.VolumeInspect(ctx, name)
	if errdefs.IsNotFound(err) {
		if requireExisting || restoreVolumeRE.MatchString(ref.Name) {
			return "", ErrBackupVolumeNotFound
		}
		if _, err = d.client.VolumeInspect(ctx, ref.Name); !errdefs.IsNotFound(err) {
			if err != nil {
				return "", err
			}
			return "", fmt.Errorf("%w: legacy volume name exists and requires migration", ErrBackupVolumeConflict)
		}
		v, err = d.client.VolumeCreate(ctx, volume.CreateOptions{Name: name, Driver: "local", Labels: d.volumeLabels(ref)})
	}
	if err != nil {
		return "", err
	}
	if !d.ownedVolume(v, name, ref) {
		return "", ErrBackupVolumeConflict
	}
	return name, nil
}

func (d *DockerExecutor) backupEngine(ctx context.Context, engineID string) error {
	info, err := d.client.Info(ctx)
	if err != nil {
		return err
	}
	if engineID == "" || info.ID != engineID || info.OSType != "linux" {
		return ErrBackupVolumeLocality
	}
	return nil
}

// Reserve one transfer while allowing unrelated workload mutations to proceed.
// Workload start and create check the reservation under the same mutex.
func (d *DockerExecutor) claimVolumeTransfer(name string) (func(), error) {
	d.mutations.Lock()
	defer d.mutations.Unlock()
	if d.volumeInTransfer != "" {
		return nil, errVolumeTransferBusy
	}
	d.volumeInTransfer = name
	return func() {
		d.mutations.Lock()
		d.volumeInTransfer = ""
		d.mutations.Unlock()
	}, nil
}

func (d *DockerExecutor) BackupVolume(ctx context.Context, ref BackupVolumeRef, engineID string, sink io.Writer) error {
	name, err := d.physicalVolume(ref)
	if err != nil {
		return err
	}
	release, err := d.claimVolumeTransfer(name)
	if err != nil {
		return err
	}
	defer release()
	if err := d.backupEngine(ctx, engineID); err != nil {
		return err
	}
	v, err := d.client.VolumeInspect(ctx, name)
	if errdefs.IsNotFound(err) {
		return ErrBackupVolumeNotFound
	}
	if err != nil {
		return err
	}
	if !d.ownedVolume(v, name, ref) {
		return ErrBackupVolumeConflict
	}
	active, err := d.client.ContainerList(ctx, container.ListOptions{Filters: filters.NewArgs(filters.Arg("volume", name))})
	if err != nil {
		return err
	}
	if len(active) != 0 {
		return fmt.Errorf("%w: stop the workloads using the volume before backup", ErrBackupVolumeConflict)
	}
	return d.withVolumeHelper(ctx, name, "", true, func(id string) error {
		reader, _, err := d.client.CopyFromContainer(ctx, id, volumeMountPath+"/.")
		if err != nil {
			return err
		}
		defer reader.Close()
		writer := tar.NewWriter(sink)
		_, err = scanVolumeArchive(ctx, reader, writer)
		if err != nil {
			return err
		}
		return writer.Close()
	})
}

func (d *DockerExecutor) RestoreVolume(ctx context.Context, ref BackupVolumeRef, engineID, operationID string, archive io.Reader) error {
	if !restoreVolumeRE.MatchString(operationID) || operationID != ref.Name {
		return ErrBackupVolumeConflict
	}
	name, err := d.physicalVolume(ref)
	if err != nil {
		return err
	}
	release, err := d.claimVolumeTransfer(name)
	if err != nil {
		return err
	}
	defer release()
	if err := d.backupEngine(ctx, engineID); err != nil {
		return err
	}
	clean, err := os.CreateTemp("", "kamui-volume-restore-*")
	if err != nil {
		return err
	}
	defer clean.Close()
	// The scratch file stays open for the transfer and is reclaimed if the manager exits.
	if err := os.Remove(clean.Name()); err != nil {
		return err
	}
	writer := tar.NewWriter(&volumeArchiveWriter{Writer: clean, remaining: maxVolumeArchive})
	expected, err := scanVolumeArchive(ctx, archive, writer)
	if err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	if _, err := clean.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err = d.client.VolumeInspect(ctx, name); !errdefs.IsNotFound(err) {
		if err != nil {
			return err
		}
		return ErrBackupVolumeConflict
	}
	labels := d.volumeLabels(ref)
	labels[volumeRestoreLabel] = operationID
	v, err := d.client.VolumeCreate(ctx, volume.CreateOptions{Name: name, Driver: "local", Labels: labels})
	if err == nil && (!d.ownedVolume(v, name, ref) || v.Labels[volumeRestoreLabel] != operationID) {
		return ErrBackupVolumeConflict
	}
	if err == nil {
		err = d.withVolumeHelper(ctx, name, operationID, false, func(id string) error {
			if err := d.client.CopyToContainer(ctx, id, volumeMountPath, clean, container.CopyToContainerOptions{}); err != nil {
				return err
			}
			output, _, err := d.client.CopyFromContainer(ctx, id, volumeMountPath+"/.")
			if err != nil {
				return err
			}
			defer output.Close()
			got, err := scanVolumeArchive(ctx, output, nil)
			if err != nil {
				return err
			}
			if _, ok := expected["."]; !ok {
				delete(got, ".")
			}
			if !maps.Equal(expected, got) {
				return fmt.Errorf("%w: restored volume contents or metadata differ", ErrBackupVolumeArchive)
			}
			return nil
		})
	}
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		err = errors.Join(err, d.cleanupRestoreVolume(cleanup, ref, name, engineID, operationID))
	}
	return err
}

func (d *DockerExecutor) CleanupRestoreVolume(ctx context.Context, ref BackupVolumeRef, engineID, operationID string) error {
	if !restoreVolumeRE.MatchString(operationID) || operationID != ref.Name {
		return ErrBackupVolumeConflict
	}
	name, err := d.physicalVolume(ref)
	if err != nil {
		return err
	}
	release, err := d.claimVolumeTransfer(name)
	if err != nil {
		return err
	}
	defer release()
	return d.cleanupRestoreVolume(ctx, ref, name, engineID, operationID)
}

func (d *DockerExecutor) cleanupRestoreVolume(ctx context.Context, ref BackupVolumeRef, name, engineID, operationID string) error {
	if err := d.backupEngine(ctx, engineID); err != nil {
		return err
	}
	v, err := d.client.VolumeInspect(ctx, name)
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !d.ownedVolume(v, name, ref) || v.Labels[volumeRestoreLabel] != operationID {
		return ErrBackupVolumeConflict
	}
	if err := d.removeVolumeHelper(ctx, helperName(name, operationID), name, operationID); err != nil {
		return err
	}

	err = d.client.VolumeRemove(ctx, name, false)
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func helperName(volumeName, operation string) string {
	if operation != "" {
		return volumeName + "-restore"
	}
	return volumeName + "-export"
}

func (d *DockerExecutor) removeVolumeHelper(ctx context.Context, helperName, name, operation string) error {
	helper, err := d.client.ContainerInspect(ctx, helperName)
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if helper.Config == nil || helper.Config.Labels[scopeLabel] != d.scope ||
		helper.Config.Labels[volumeHelperLabel] != name || helper.Config.Labels[volumeRestoreLabel] != operation ||
		helper.State == nil || helper.State.Running {
		return ErrBackupVolumeConflict
	}
	err = d.client.ContainerRemove(ctx, helper.ID, container.RemoveOptions{})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (d *DockerExecutor) withVolumeHelper(ctx context.Context, name, operation string, readOnly bool, fn func(string) error) (result error) {
	image := d.backupHelperImage
	if image == "" {
		image = "busybox:1.37"
	}
	local, err := d.client.ImageInspect(ctx, image)
	if err != nil {
		return fmt.Errorf("inspect local volume helper image: %w", err)
	}
	if local.Os != "linux" || local.ID == "" || local.Config == nil || len(local.Config.Volumes) != 0 {
		return fmt.Errorf("%w: backup helper must be a Linux image without declared volumes", ErrBackupVolumeConflict)
	}
	cfg := &container.Config{
		Image: local.ID, Entrypoint: []string{"/bin/false"},
		Labels: map[string]string{scopeLabel: d.scope, volumeHelperLabel: name, volumeRestoreLabel: operation},
	}
	host := &container.HostConfig{
		NetworkMode: "none", ReadonlyRootfs: true,
		CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"},
		Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: name, Target: volumeMountPath, ReadOnly: readOnly,
			VolumeOptions: &mount.VolumeOptions{NoCopy: true}}},
	}
	containerName := helperName(name, operation)
	if err := d.removeVolumeHelper(ctx, containerName, name, operation); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		result = errors.Join(result, d.removeVolumeHelper(cleanup, containerName, name, operation))
	}()
	helper, err := d.client.ContainerCreate(ctx, cfg, host, nil, nil, containerName)
	if err != nil {
		return err
	}
	return fn(helper.ID)
}

type volumeArchiveEntry struct {
	kind   byte
	mode   int64
	uid    int
	gid    int
	size   int64
	digest [sha256.Size]byte
}

type volumeContextReader struct {
	ctx context.Context
	io.Reader
}

type volumeArchiveWriter struct {
	io.Writer
	remaining int64
}

func (w *volumeArchiveWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, ErrBackupVolumeArchive
	}
	n, err := w.Writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}

func (r volumeContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func scanVolumeArchive(ctx context.Context, source io.Reader, out *tar.Writer) (map[string]volumeArchiveEntry, error) {
	limit := &io.LimitedReader{R: volumeContextReader{ctx, source}, N: maxVolumeArchive + 1}
	reader := tar.NewReader(limit)
	entries := make(map[string]volumeArchiveEntry)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrBackupVolumeArchive, err)
		}
		name := strings.TrimPrefix(header.Name, "./")
		if header.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
			if header.Name == "./" {
				name = "."
			}
		}
		if !fs.ValidPath(name) || (name == "." && header.Typeflag != tar.TypeDir) ||
			len(name) > 4096 || strings.Count(name, "/") > 64 || strings.ContainsAny(name, "\\\\:") ||
			header.Size < 0 || header.Size >= limit.N || len(entries) >= 10000 ||
			(header.Typeflag != tar.TypeDir && header.Typeflag != tar.TypeReg) ||
			(header.Typeflag == tar.TypeDir && header.Size != 0) || header.Uid < 0 || header.Gid < 0 {
			return nil, ErrBackupVolumeArchive
		}
		if _, exists := entries[name]; exists {
			return nil, ErrBackupVolumeArchive
		}
		entry := volumeArchiveEntry{kind: header.Typeflag, mode: header.Mode & 0777, uid: header.Uid, gid: header.Gid, size: header.Size}
		var sink io.Writer = io.Discard
		if out != nil {
			// Construct a fresh header rather than relaying extended attributes or links.
			if err := out.WriteHeader(&tar.Header{Name: name, Typeflag: entry.kind, Mode: entry.mode,
				Uid: entry.uid, Gid: entry.gid, Size: entry.size, ModTime: header.ModTime}); err != nil {
				return nil, err
			}
			sink = out
		}
		hash := sha256.New()
		n, err := io.Copy(io.MultiWriter(sink, hash), reader)
		if err != nil || n != entry.size {
			return nil, errors.Join(ErrBackupVolumeArchive, err)
		}
		copy(entry.digest[:], hash.Sum(nil))
		entries[name] = entry
	}
	// Drain the bounded input so an archive's end marker cannot conceal extra bytes.
	if _, err := io.Copy(io.Discard, limit); err != nil {
		return nil, err
	}
	if limit.N == 0 {
		return nil, ErrBackupVolumeArchive
	}
	return entries, nil
}
