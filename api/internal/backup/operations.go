package backup

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/objectstore"
)

const transferTimeout = 65 * time.Second

var restoreIDPattern = regexp.MustCompile(`^rst-[a-f0-9]{24}$`)
var backupIDPattern = regexp.MustCompile(`^bck-[a-f0-9]{24}$`)

type VolumeBackend interface {
	Capacity(context.Context) (core.DockerCapacity, error)
	BackupVolume(context.Context, core.BackupVolumeRef, string, io.Writer) error
	RestoreVolume(context.Context, core.BackupVolumeRef, string, string, io.Reader) error
	CleanupRestoreVolume(context.Context, core.BackupVolumeRef, string, string) error
}

type Restore struct {
	ID        string          `json:"id"`
	BackupID  string          `json:"backup_id"`
	ProjectID string          `json:"project_id"`
	Kind      string          `json:"kind"`
	Status    string          `json:"status"`
	Result    json.RawMessage `json:"result,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	engineID  string
}

func (s *Store) acquire() (func(), error) {
	select {
	case s.active <- struct{}{}:
		return func() { <-s.active }, nil
	default:
		return nil, ErrBusy
	}
}

func (s *Store) CreateVolumeBackup(ctx context.Context, projectID, name, actor string) (Backup, error) {
	if s.volumes == nil {
		return Backup{}, ErrNotImplemented
	}
	ctx, cancel := context.WithTimeout(ctx, transferTimeout)
	defer cancel()
	release, err := s.acquire()
	if err != nil {
		return Backup{}, err
	}
	defer release()

	var owner, engineID string
	err = s.db.QueryRowContext(ctx, "SELECT worker_id,engine_id FROM volume_placements WHERE project_id=? AND name=?", projectID, name).Scan(&owner, &engineID)
	if errors.Is(err, sql.ErrNoRows) {
		return Backup{}, ErrNotFound
	}
	if err != nil {
		return Backup{}, err
	}
	if owner != core.LocalWorker {
		return Backup{}, ErrConflict
	}
	runtime, err := s.volumes.Capacity(ctx)
	if err != nil {
		return Backup{}, err
	}
	if runtime.ID == "" || engineID == "" || runtime.ID != engineID {
		return Backup{}, ErrConflict
	}
	limit, err := s.snapshotLimit(ctx, projectID)
	if err != nil {
		return Backup{}, err
	}

	id := newID("bck")
	record, err := s.recordStart(ctx, id, projectID, "volume", "volume:"+name, actor)
	if err != nil {
		return Backup{}, err
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE backups SET storage_engine_id=? WHERE id=?", runtime.ID, id); err != nil {
		return record.failed(err), err
	}
	path, err := s.backupPath(id)
	if err != nil {
		return record.failed(err), err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return record.failed(err), err
	}
	writer := &countingWriter{w: file, limit: limit}
	err = s.volumes.BackupVolume(ctx, core.BackupVolumeRef{ProjectID: projectID, Name: name}, runtime.ID, writer)
	if err := errors.Join(err, file.Close()); err != nil {
		if cleanupErr := os.Remove(path); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
		return record.failed(err), err
	}
	return record.finalize(ctx, path)
}

func (s *Store) snapshotLimit(ctx context.Context, projectID string) (int64, error) {
	var quota, used int64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE((SELECT max_storage_bytes FROM project_quotas WHERE project_id=?),0),
		       COALESCE((SELECT SUM(size_bytes) FROM backups WHERE project_id=? AND status='succeeded'),0)`,
		projectID, projectID).Scan(&quota, &used); err != nil {
		return 0, err
	}
	if quota == 0 {
		return MaxBackupBytes, nil
	}
	if used >= quota {
		return 0, objectstore.ErrQuota
	}
	return min(quota-used, MaxBackupBytes), nil
}

func (s *Store) startRestore(ctx context.Context, record Backup) (Restore, error) {
	op := Restore{ID: newID("rst"), BackupID: record.ID, ProjectID: record.ProjectID, Kind: record.Kind, Status: "in_progress", engineID: record.storageEngine}
	_, err := s.db.ExecContext(ctx, "INSERT INTO backup_restores(id, backup_id, project_id, kind, status, engine_id, created_at) VALUES(?,?,?,?,?,?,?)",
		op.ID, op.BackupID, op.ProjectID, op.Kind, op.Status, op.engineID, time.Now().UnixNano())
	return op, err
}

func scanRestore(row interface{ Scan(...any) error }) (Restore, error) {
	var op Restore
	var result string
	err := row.Scan(&op.ID, &op.BackupID, &op.ProjectID, &op.Kind, &op.Status, &op.engineID, &result, &op.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return op, ErrNotFound
	}
	if result != "" {
		op.Result = json.RawMessage(result)
	}
	return op, err
}

const restoreColumns = "id,backup_id,project_id,kind,status,engine_id,result,reason"

func (s *Store) GetRestore(ctx context.Context, project, id string) (Restore, error) {
	if project == "" || !restoreIDPattern.MatchString(id) {
		return Restore{}, ErrNotFound
	}
	return scanRestore(s.db.QueryRowContext(ctx, "SELECT "+restoreColumns+" FROM backup_restores WHERE project_id=? AND id=?", project, id))
}

func (s *Store) ListRestores(ctx context.Context, projectID, backupID string) ([]Restore, error) {
	if projectID == "" {
		return nil, ErrNotFound
	}
	if _, err := s.GetBackup(ctx, projectID, backupID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+restoreColumns+" FROM backup_restores WHERE project_id=? AND backup_id=? ORDER BY created_at DESC LIMIT 50", projectID, backupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := make([]Restore, 0)
	for rows.Next() {
		op, err := scanRestore(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, op)
	}
	return results, rows.Err()
}

func (s *Store) RestoreProjectBackup(ctx context.Context, project, id, actor string) (Restore, error) {
	if project == "" {
		return Restore{}, ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, transferTimeout)
	defer cancel()
	release, err := s.acquire()
	if err != nil {
		return Restore{}, err
	}
	defer release()
	record, err := s.GetBackup(ctx, project, id)
	if err != nil {
		return Restore{}, err
	}
	if record.Status != "succeeded" {
		return Restore{}, ErrBadInput
	}
	switch record.Kind {
	case "volume":
		if s.volumes == nil {
			return Restore{}, ErrNotImplemented
		}
		if record.storageEngine == "" {
			return Restore{}, ErrConflict
		}
	case "objects":
		if s.objects == nil {
			return Restore{}, ErrNotImplemented
		}
	default:
		return Restore{}, ErrBadInput
	}
	op, err := s.startRestore(ctx, record)
	if err != nil {
		return Restore{}, err
	}

	if record.Kind == "volume" {
		err = s.restoreVolume(ctx, record, op)
	} else {
		err = s.restoreObjects(ctx, record, op, actor)
	}
	// Read the durable result even if the client disconnected after publication.
	finishCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
	defer stop()
	if err != nil {
		reason := "restore failed"
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			reason = "restore cancelled or timed out"
		}
		if _, updateErr := s.db.ExecContext(finishCtx,
			"UPDATE backup_restores SET status='failed', reason=?, finished_at=? WHERE id=? AND status='in_progress'",
			reason, time.Now().UnixNano(), op.ID); updateErr != nil {
			return op, errors.Join(err, updateErr)
		}
	}
	current, readErr := s.GetRestore(finishCtx, project, op.ID)
	if readErr != nil {
		return op, errors.Join(err, readErr)
	}
	if cleanupErr := s.cleanupRestore(finishCtx, current); cleanupErr != nil {
		slog.Warn("restore cleanup deferred", "restore_id", current.ID, "error", cleanupErr)
	}
	if current.Status == "succeeded" {
		return current, nil
	}
	return current, err
}

func (s *Store) restoreObjects(ctx context.Context, record Backup, op Restore, actor string) error {
	file, size, err := s.openBackup(record)
	if err != nil {
		return err
	}
	defer file.Close()
	archive := &io.LimitedReader{R: contextReader{ctx, file}, N: size}
	archiveHash := sha256.New()
	content := io.TeeReader(archive, archiveHash)
	tarReader := tar.NewReader(content)
	var inputs []objectstore.ImportObject
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.Join(ErrBadInput, err)
		}
		if len(inputs) >= MaxRestoreEntries {
			return ErrTooBig
		}
		if header.Typeflag == tar.TypeDir && header.Size == 0 {
			inputs = append(inputs, objectstore.ImportObject{SourceBucket: strings.TrimSuffix(header.Name, "/")})
			continue
		}
		bucket, key, ok := strings.Cut(header.Name, "/")
		if !ok || !objectstore.ValidObjectKey(key) || header.Typeflag != tar.TypeReg {
			return ErrBadInput
		}
		// Section offsets are meaningful only for contiguous regular-file entries.
		for name := range header.PAXRecords {
			if strings.HasPrefix(name, "GNU.sparse.") {
				return ErrBadInput
			}
		}
		if header.Size > objectstore.MaxObjectBytes || header.Size > archive.N {
			return ErrTooBig
		}
		offset, length := size-archive.N, header.Size
		objectHash := sha256.New()
		if _, err := io.Copy(objectHash, tarReader); err != nil {
			return err
		}
		checksum := "sha256:" + hex.EncodeToString(objectHash.Sum(nil))
		if expected := header.PAXRecords[objectChecksum]; expected != "" && checksum != expected {
			return fmt.Errorf("%w: object checksum mismatch", ErrBadInput)
		}
		inputs = append(inputs, objectstore.ImportObject{SourceBucket: bucket, Key: key, Size: length,
			Checksum: checksum, ContentType: header.PAXRecords[objectContentType], Open: func() (io.ReadCloser, error) {
				return io.NopCloser(io.NewSectionReader(file, offset, length)), nil
			}})
	}
	if _, err := io.Copy(io.Discard, content); err != nil {
		return err
	}
	if archive.N != 0 || "sha256:"+hex.EncodeToString(archiveHash.Sum(nil)) != record.Checksum {
		return fmt.Errorf("%w: backup checksum mismatch", ErrBadInput)
	}
	_, err = s.objects.ImportBackup(ctx, op.ProjectID, actor, op.ID, inputs)
	return err
}

func (s *Store) restoreVolume(ctx context.Context, record Backup, op Restore) error {
	file, size, err := s.openBackup(record)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(contextReader{ctx, file}, size+1))
	if err != nil {
		return err
	}
	if n != size || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != record.Checksum {
		return fmt.Errorf("%w: backup checksum mismatch", ErrBadInput)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := objectstore.CheckStorageQuota(ctx, s.db, op.ProjectID, size); err != nil {
		return err
	}
	ref := core.BackupVolumeRef{ProjectID: op.ProjectID, Name: op.ID}
	if err := s.volumes.RestoreVolume(ctx, ref, op.engineID, op.ID, io.LimitReader(contextReader{ctx, file}, size)); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := objectstore.CheckStorageQuota(ctx, tx, op.ProjectID, size); err != nil {
		return err
	}
	now := time.Now().UnixNano()
	if _, err := tx.ExecContext(ctx, `INSERT INTO volume_placements(project_id,name,worker_id,size_bytes,created_at,last_used_at,engine_id)
		VALUES(?,?,?,?,?,?,?)`, op.ProjectID, op.ID, core.LocalWorker, size, now, now, op.engineID); err != nil {
		return err
	}
	result, err := json.Marshal(map[string]string{"volume": op.ID, "worker_id": core.LocalWorker})
	if err != nil {
		return err
	}
	update, err := tx.ExecContext(ctx, "UPDATE backup_restores SET status='succeeded',result=?,finished_at=? WHERE id=? AND status='in_progress'",
		string(result), now, op.ID)
	if err != nil {
		return err
	}
	count, err := update.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrConflict
	}
	return tx.Commit()
}

// RecoverInterrupted runs before requests can start work on the single coordinator.
func (s *Store) RecoverInterrupted(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixNano()
	if _, err := tx.ExecContext(ctx, "UPDATE backup_restores SET status='failed', reason='coordinator restarted', finished_at=? WHERE status='in_progress'", now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE backups SET status='failed', reason='coordinator restarted', finished_at=?, cleanup_due_at=0 WHERE status='in_progress'", now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) cleanupRestore(ctx context.Context, op Restore) error {
	if !restoreIDPattern.MatchString(op.ID) || op.Status == "in_progress" {
		return ErrBadInput
	}
	if op.Kind == "objects" {
		if op.Status == "failed" {
			if s.objects == nil {
				return ErrNotImplemented
			}
			if err := s.objects.CleanupBackupImport(ctx, op.ID); err != nil {
				return err
			}
		}
		root, err := os.OpenRoot(s.root)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if root != nil {
			err = errors.Join(root.RemoveAll(filepath.Join("restores", op.ID)), root.Close())
			if err != nil {
				return err
			}
		}
	}
	if op.Kind == "volume" && op.Status == "failed" {
		if s.volumes == nil {
			return ErrNotImplemented
		}
		if err := s.volumes.CleanupRestoreVolume(ctx, core.BackupVolumeRef{ProjectID: op.ProjectID, Name: op.ID}, op.engineID, op.ID); err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, "UPDATE backup_restores SET cleaned_at=? WHERE id=?", time.Now().UnixNano(), op.ID)
	return err
}

func (s *Store) RunCleanup(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for ctx.Err() == nil {
		s.cleanupPending(ctx)
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

func (s *Store) cleanupPending(ctx context.Context) {
	s.cleanupInterruptedBackups(ctx)
	rows, err := s.db.QueryContext(ctx, "SELECT "+restoreColumns+" FROM backup_restores WHERE cleaned_at IS NULL AND status IN ('succeeded','failed') AND cleanup_due_at<=? ORDER BY cleanup_due_at, created_at LIMIT 16", time.Now().UnixNano())
	if err != nil {
		return
	}
	var ops []Restore
	for rows.Next() {
		op, err := scanRestore(rows)
		if err != nil {
			break
		}
		ops = append(ops, op)
	}
	rows.Close()
	for _, op := range ops {
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := s.db.ExecContext(attempt, "UPDATE backup_restores SET cleanup_due_at=? WHERE id=? AND cleaned_at IS NULL", time.Now().Add(time.Minute).UnixNano(), op.ID)
		if err == nil {
			err = s.cleanupRestore(attempt, op)
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Warn("retry restore cleanup", "restore_id", op.ID, "error", err)
		}
	}
}

func (s *Store) cleanupInterruptedBackups(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM backups WHERE status='failed' AND cleanup_due_at<=? ORDER BY cleanup_due_at, created_at LIMIT 16`, time.Now().UnixNano())
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if !backupIDPattern.MatchString(id) {
			continue
		}
		path, err := s.backupPath(id)
		if err == nil {
			root, openErr := os.OpenRoot(filepath.Dir(path))
			err = openErr
			if root != nil {
				err = errors.Join(root.Remove(filepath.Base(path)), root.Close())
			}
		}
		var retry any
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			retry = time.Now().Add(time.Minute).UnixNano()
			slog.Warn("retry interrupted backup cleanup", "backup_id", id, "error", err)
		}
		if _, err := s.db.ExecContext(ctx, "UPDATE backups SET cleanup_due_at=? WHERE id=? AND status='failed'", retry, id); err != nil {
			slog.Warn("record backup cleanup", "backup_id", id, "error", err)
		}
	}
}
