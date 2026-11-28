package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/objectstore"
)

func TestObjectBackupRestoresThroughProjectObjectStore(t *testing.T) {
	s := setupBackupStore(t)
	ctx := context.Background()
	objects := objectstore.NewStore(s.db, &objectstore.LocalBackend{Root: s.root}, nil, nil)
	s.UseObjectStore(objects)
	bucket, err := objects.CreateBucket(ctx, "default", "source-bucket", "local-admin")
	if err != nil {
		t.Fatal(err)
	}
	const data = "deterministic BAK-04 object fixture\n"
	source, err := objects.PutObject(ctx, bucket, strings.Repeat("d/", 70)+"payload: 01", "text/plain", strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	empty, err := objects.CreateBucket(ctx, "default", "empty-bucket", "local-admin")
	if err != nil {
		t.Fatal(err)
	}
	backup, err := s.CreateProjectObjectBackup(ctx, "default", "local-admin")
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.RestoreProjectBackup(ctx, "default", backup.ID, "local-admin")
	if err != nil || op.Status != "succeeded" {
		t.Fatalf("restore %s: %v", op.Status, err)
	}
	var result []objectstore.RestoredBucket
	if err := json.Unmarshal(op.Result, &result); err != nil || len(result) != 2 {
		t.Fatalf("restore result %s: %v", op.Result, err)
	}
	if result[0].SourceBucket != empty.Name || result[0].ObjectCount != 0 || result[0].ID == empty.ID {
		t.Fatalf("empty bucket restore: %+v", result[0])
	}
	restored, err := objects.GetBucket(ctx, "default", result[1].ID)
	if err != nil || restored.ID == bucket.ID || restored.Name == bucket.Name {
		t.Fatalf("restored bucket %+v: %v", restored, err)
	}
	for _, b := range []objectstore.Bucket{bucket, restored} {
		r, metadata, err := objects.OpenObject(ctx, b, source.Key)
		if err != nil {
			t.Fatal(err)
		}
		if metadata.Checksum != source.Checksum {
			t.Fatal("object digest differs")
		}
		if metadata.ContentType != source.ContentType {
			t.Fatalf("object content type = %q, want %q", metadata.ContentType, source.ContentType)
		}
		err = objectstore.VerifyChecksum(r, metadata)
		content, readErr := io.ReadAll(r)
		if err = errors.Join(err, readErr, r.Close()); err != nil || string(content) != data {
			t.Fatalf("object content does not match: %v", err)
		}
	}
	if _, err := s.GetRestore(ctx, "other", op.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project operation lookup: %v", err)
	}
	t.Logf("original and restored object SHA-256: %s", source.Checksum)
}

func TestRecoverDiscardsInterruptedBackupOnly(t *testing.T) {
	s := setupBackupStore(t)
	ctx := t.Context()
	complete, err := s.CreateObjectBackup(ctx, "default", "local-admin", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	interrupted := newID("bck")
	if _, err := s.recordStart(ctx, interrupted, "default", "objects", "objects:default", "local-admin"); err != nil {
		t.Fatal(err)
	}
	path, err := s.backupPath(interrupted)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("partial archive"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	s.cleanupPending(ctx)
	if record, err := s.GetBackup(ctx, "default", interrupted); err != nil || record.Status != "failed" {
		t.Fatalf("interrupted record: %+v, %v", record, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial backup remains: %v", err)
	}
	r, _, err := s.OpenBackup(ctx, "default", complete.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
}

type fixtureVolumes struct {
	restored bool
	cleaned  bool
}

func (*fixtureVolumes) Capacity(context.Context) (core.DockerCapacity, error) {
	return core.DockerCapacity{ID: "original"}, nil
}
func (*fixtureVolumes) BackupVolume(ctx context.Context, ref core.BackupVolumeRef, engine string, out io.Writer) error {
	w := tar.NewWriter(out)
	if err := w.WriteHeader(&tar.Header{Name: "test.txt", Size: 2, Mode: 0600}); err != nil {
		return err
	}
	if _, err := w.Write([]byte("ok")); err != nil {
		return err
	}
	return w.Close()
}
func (v *fixtureVolumes) RestoreVolume(_ context.Context, ref core.BackupVolumeRef, engine, op string, archive io.Reader) error {
	tr := tar.NewReader(archive)
	if _, err := tr.Next(); err != nil {
		return err
	}
	payload, err := io.ReadAll(tr)
	if err != nil {
		return err
	}
	if !bytes.Equal(payload, []byte("ok")) || ref.Name != op || engine != "original" {
		return ErrBadInput
	}
	v.restored = true
	return nil
}
func (v *fixtureVolumes) CleanupRestoreVolume(context.Context, core.BackupVolumeRef, string, string) error {
	v.cleaned = true
	return nil
}

func TestVolumeRestorePublishesPlacementAndRecoversInterruptedOperations(t *testing.T) {
	s := setupBackupStore(t)
	v := &fixtureVolumes{}
	s.volumes = v
	ctx := context.Background()
	if _, err := s.db.Exec("INSERT INTO volume_placements(project_id,name,worker_id,size_bytes,created_at,engine_id) VALUES('default','source','local',2,0,'original'),('default','remote','wk-a',2,0,'remote')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateVolumeBackup(ctx, "default", "remote", "local-admin"); !errors.Is(err, ErrConflict) {
		t.Fatalf("remote backup: %v", err)
	}
	record, err := s.CreateVolumeBackup(ctx, "default", "source", "local-admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO project_quotas(project_id,max_storage_bytes,updated_by,updated_at) VALUES('default',4,'local-admin',0)"); err != nil {
		t.Fatal(err)
	}
	failed, err := s.RestoreProjectBackup(ctx, "default", record.ID, "local-admin")
	if !errors.Is(err, objectstore.ErrQuota) || failed.Status != "failed" || v.restored {
		t.Fatalf("quota restore %s: %v", failed.Status, err)
	}
	if _, err := s.db.Exec("UPDATE project_quotas SET max_storage_bytes=0 WHERE project_id='default'"); err != nil {
		t.Fatal(err)
	}
	done, err := s.RestoreProjectBackup(ctx, "default", record.ID, "local-admin")
	if err != nil || done.Status != "succeeded" || !v.restored {
		t.Fatalf("volume publication %s: %v", done.Status, err)
	}
	var engine string
	if err := s.db.QueryRow("SELECT engine_id FROM volume_placements WHERE project_id='default' AND name=?", done.ID).Scan(&engine); err != nil || engine != "original" {
		t.Fatalf("volume placement %q: %v", engine, err)
	}
	pending, err := s.startRestore(ctx, record)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	v.cleaned = false
	s.cleanupPending(ctx)
	interrupted, err := s.GetRestore(ctx, "default", pending.ID)
	if err != nil || interrupted.Status != "failed" || !v.cleaned {
		t.Fatalf("recovery %+v: %v", interrupted, err)
	}

	// Failed/cancelled restores must leave the durable source archive intact.
	r, _, err := s.OpenBackup(ctx, "default", record.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, r); err != nil || fmt.Sprintf("sha256:%x", hash.Sum(nil)) != record.Checksum {
		t.Fatalf("source archive changed: %v", err)
	}
}
