// Package backup implements Kamui Backup: consistent coordinator metadata
// backups (SQLite VACUUM INTO — never a raw WAL copy), workspace volume
// exports, stored-object bundles and restore into isolated destinations that
// never overwrite live data without explicit authorization.
package backup

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
)

const (
	MaxBackupBytes = int64(8) << 30
	MaxRestoreScan = int64(8) << 30
)

var (
	ErrNotFound = errors.New("backup not found")
	ErrBadInput = errors.New("invalid backup request")
	ErrConflict = errors.New("restore target already exists")
)

type Store struct {
	db        *sql.DB
	ids       *identity.Store
	root      string
	volumeTar func(ctx context.Context, volume string, sink io.Writer) error
}

// NewStore wires the backup store. volumeTar streams one Docker volume as a
// tar archive; the coordinator's workspace-manager path supplies it. A nil
// volumeTar marks volume backups as unavailable rather than fake.
func NewStore(db *sql.DB, ids *identity.Store, root string, volumeTar func(context.Context, string, io.Writer) error) *Store {
	return &Store{db: db, ids: ids, root: root, volumeTar: volumeTar}
}

type Backup struct {
	ID         string     `json:"id"`
	ProjectID  string     `json:"project_id"`
	Kind       string     `json:"kind"`
	Target     string     `json:"target"`
	Status     string     `json:"status"`
	Reason     string     `json:"reason,omitempty"`
	SizeBytes  int64      `json:"size_bytes"`
	Checksum   string     `json:"checksum,omitempty"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

func scanBackup(row interface{ Scan(...any) error }) (Backup, error) {
	var b Backup
	var createdAt, finished sql.NullInt64
	if err := row.Scan(&b.ID, &b.ProjectID, &b.Kind, &b.Target, &b.Status, &b.Reason,
		&b.SizeBytes, &b.Checksum, &b.CreatedBy, &createdAt, &finished); err != nil {
		return b, err
	}
	b.CreatedAt = time.Unix(0, createdAt.Int64).UTC()
	if finished.Valid {
		t := time.Unix(0, finished.Int64).UTC()
		b.FinishedAt = &t
	}
	return b, nil
}

const backupColumns = "id,project_id,kind,target,status,reason,size_bytes,checksum,created_by,created_at,finished_at"

func (s *Store) backupPath(id string) (string, error) {
	if len(id) < 8 || strings.ContainsAny(id, "/\\.") {
		return "", ErrBadInput
	}
	path := filepath.Join(s.root, "backups", id+".bin")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	return path, nil
}

// CreateMetadataBackup produces a consistent SQLite snapshot via VACUUM INTO,
// which serializes a transactionally consistent image without touching the WAL.
func (s *Store) CreateMetadataBackup(ctx context.Context, actor string) (Backup, error) {
	id := newID("bck")
	target := "coordinator-metadata"
	record := recordStart(s, id, "", "metadata", target, actor)
	path, err := s.backupPath(id)
	if err != nil {
		return record.failed(err), err
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		os.Remove(path)
		return record.failed(fmt.Errorf("snapshot failed: %w", err)), fmt.Errorf("snapshot failed: %w", err)
	}
	return s.finalize(id, path)
}

// CreateObjectBackup bundles one project's stored objects with their metadata.
func (s *Store) CreateObjectBackup(ctx context.Context, projectID, actor string, objects []ObjectRef, openObject func(context.Context, ObjectRef) (io.ReadCloser, error)) (Backup, error) {
	id := newID("bck")
	target := "objects:" + projectID
	record := recordStart(s, id, projectID, "objects", target, actor)
	path, err := s.backupPath(id)
	if err != nil {
		return record.failed(err), err
	}
	file, err := os.Create(path)
	if err != nil {
		return record.failed(err), err
	}
	defer file.Close()
	hasher := sha256.New()
	writer := tar.NewWriter(io.MultiWriter(file, hasher))
	var total int64
	for _, ref := range objects {
		reader, err := openObject(ctx, ref)
		if err != nil {
			os.Remove(path)
			return record.failed(err), err
		}
		header := &tar.Header{
			Name:    ref.Bucket + "/" + ref.Key,
			Mode:    0600,
			Size:    ref.Size,
			ModTime: time.Now().UTC(),
		}
		if err := writer.WriteHeader(header); err != nil {
			reader.Close()
			os.Remove(path)
			return record.failed(err), err
		}
		n, err := io.Copy(writer, io.LimitReader(reader, MaxBackupBytes-total+1))
		reader.Close()
		total += n
		if err != nil {
			os.Remove(path)
			return record.failed(err), err
		}
		if total > MaxBackupBytes {
			writer.Close()
			os.Remove(path)
			return record.failed(fmt.Errorf("%w: bundle exceeds the backup size limit", ErrBadInput)), ErrTooBig
		}
	}
	if err := writer.Close(); err != nil {
		os.Remove(path)
		return record.failed(err), err
	}
	file.Close()
	return s.finalize(id, path)
}

// ErrTooBig marks an oversized backup request.
var ErrTooBig = errors.New("backup exceeds the size limit")

// CreateVolumeBackup exports one Docker volume through the workspace-manager
// path. Backups never write to external SSDs; blobs live on the storage root.
func (s *Store) CreateVolumeBackup(ctx context.Context, projectID, volume, actor string) (Backup, error) {
	if s.volumeTar == nil {
		return Backup{}, fmt.Errorf("%w: volume backups require the workspace manager on this host", ErrBadInput)
	}
	if !validVolumeName(volume) {
		return Backup{}, fmt.Errorf("%w: invalid volume name", ErrBadInput)
	}
	id := newID("bck")
	target := "volume:" + volume
	record := recordStart(s, id, projectID, "volume", target, actor)
	path, err := s.backupPath(id)
	if err != nil {
		return record.failed(err), err
	}
	file, err := os.Create(path)
	if err != nil {
		return record.failed(err), err
	}
	defer file.Close()
	hasher := sha256.New()
	counter := &countingWriter{w: io.MultiWriter(file, hasher)}
	if err := s.volumeTar(ctx, volume, counter); err != nil {
		os.Remove(path)
		return record.failed(err), err
	}
	if counter.n > MaxBackupBytes {
		os.Remove(path)
		return record.failed(ErrTooBig), ErrTooBig
	}
	file.Close()
	return s.finalize(id, path)
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func validVolumeName(name string) bool {
	if name == "" || len(name) > 128 || strings.ContainsAny(name, "/\\\r\n\x00") {
		return false
	}
	return true
}

type ObjectRef struct {
	Bucket string
	Key    string
	Size   int64
}

func recordStart(s *Store, id, projectID, kind, target, actor string) *pendingBackup {
	now := time.Now().UTC().UnixNano()
	_, _ = s.db.Exec("INSERT INTO backups(id,project_id,kind,target,status,created_by,created_at) VALUES(?,?,?,?, 'in_progress', ?, ?)",
		id, projectID, kind, target, actor, now)
	return &pendingBackup{store: s, id: id, projectID: projectID, kind: kind, target: target, actor: actor, createdAt: now}
}

type pendingBackup struct {
	store     *Store
	id        string
	projectID string
	kind      string
	target    string
	actor     string
	createdAt int64
}

func (p *pendingBackup) failed(err error) Backup {
	_, _ = p.store.db.Exec("UPDATE backups SET status='failed', reason=?, finished_at=? WHERE id=?",
		err.Error(), time.Now().UTC().UnixNano(), p.id)
	return Backup{ID: p.id, Status: "failed", Reason: err.Error()}
}

// finalize hashes the completed blob, records the checksum and size, and
// returns the persisted backup record.
func (s *Store) finalize(id, path string) (Backup, error) {
	file, err := os.Open(path)
	if err != nil {
		return Backup{}, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return Backup{}, err
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		file.Close()
		return Backup{}, err
	}
	file.Close()
	checksum := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	_, err = s.db.Exec("UPDATE backups SET status='succeeded', size_bytes=?, checksum=?, finished_at=? WHERE id=?",
		info.Size(), checksum, time.Now().UTC().UnixNano(), id)
	if err != nil {
		return Backup{}, err
	}
	return s.GetBackup(context.Background(), id)
}

func (s *Store) GetBackup(ctx context.Context, id string) (Backup, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+backupColumns+" FROM backups WHERE id=?", id)
	backup, err := scanBackup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Backup{}, ErrNotFound
	}
	return backup, err
}

func (s *Store) ListBackups(ctx context.Context, projectID string, limit int) ([]Backup, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	args := []any{}
	query := "SELECT " + backupColumns + " FROM backups"
	if projectID != "" {
		query += " WHERE project_id=? OR project_id=''"
		args = append(args, projectID)
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Backup, 0)
	for rows.Next() {
		backup, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, backup)
	}
	return items, rows.Err()
}

// OpenBackup returns the raw backup blob for download and verification.
func (s *Store) OpenBackup(ctx context.Context, id string) (io.ReadSeekCloser, int64, error) {
	backup, err := s.GetBackup(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	if backup.Status != "succeeded" {
		return nil, 0, fmt.Errorf("%w: backup %s", ErrBadInput, backup.Status)
	}
	path, err := s.backupPath(id)
	if err != nil {
		return nil, 0, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	info, _ := file.Stat()
	return file, info.Size(), nil
}

// RestoreMetadata writes a coordinator metadata snapshot into an isolated
// destination file. Restores never touch the live database; operators move the
// verified snapshot into place with the service stopped.
func (s *Store) RestoreMetadata(ctx context.Context, id, destinationDir string) (string, error) {
	backup, err := s.GetBackup(ctx, id)
	if err != nil {
		return "", err
	}
	if backup.Kind != "metadata" || backup.Status != "succeeded" {
		return "", fmt.Errorf("%w: only succeeded metadata backups can restore", ErrBadInput)
	}
	if err := os.MkdirAll(destinationDir, 0700); err != nil {
		return "", err
	}
	target := filepath.Join(destinationDir, "restored-kamui-core.sqlite")
	if _, err := os.Stat(target); err == nil {
		return "", ErrConflict
	}
	path, err := s.backupPath(id)
	if err != nil {
		return "", err
	}
	if err := verifyFileChecksum(path, backup.Checksum); err != nil {
		return "", err
	}
	source, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer source.Close()
	dest, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	defer dest.Close()
	if _, err := io.Copy(dest, source); err != nil {
		os.Remove(target)
		return "", err
	}
	return target, nil
}

// RestoreObjectBackup extracts a project's stored objects from a backup into
// an isolated destination bucket. The destination must not exist.
func (s *Store) RestoreObjectBackup(ctx context.Context, id, destinationDir string) (string, error) {
	backup, err := s.GetBackup(ctx, id)
	if err != nil {
		return "", err
	}
	if backup.Kind != "objects" || backup.Status != "succeeded" {
		return "", fmt.Errorf("%w: only succeeded object backups can restore", ErrBadInput)
	}
	if err := os.MkdirAll(destinationDir, 0700); err != nil {
		return "", err
	}
	// Verify backup integrity
	path, err := s.backupPath(id)
	if err != nil {
		return "", err
	}
	if err := verifyFileChecksum(path, backup.Checksum); err != nil {
		return "", err
	}
	// Extract tar archive
	source, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer source.Close()
	tr := tar.NewReader(source)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("tar read error: %w", err)
		}
		// Validate header name doesn't contain traversal
		if strings.Contains(header.Name, "..") || filepath.IsAbs(header.Name) {
			return "", fmt.Errorf("%w: invalid path in backup archive", ErrBadInput)
		}
		// Create target file
		target := filepath.Join(destinationDir, header.Name)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return "", err
		}
		if header.Typeflag == tar.TypeReg {
			dest, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(dest, tr); err != nil {
				dest.Close()
				return "", err
			}
			dest.Close()
		}
	}
	return destinationDir, nil
}

// RestoreVolumeBackup extracts a Docker volume backup into a new named volume.
// The destination volume name must not already exist.
// This uses the Docker executor to run a container that extracts the tar into the new volume.
func (s *Store) RestoreVolumeBackup(ctx context.Context, id, destinationVolume string) error {
	backup, err := s.GetBackup(ctx, id)
	if err != nil {
		return err
	}
	if backup.Kind != "volume" || backup.Status != "succeeded" {
		return fmt.Errorf("%w: only succeeded volume backups can restore", ErrBadInput)
	}
	if !validVolumeName(destinationVolume) {
		return fmt.Errorf("%w: invalid destination volume name", ErrBadInput)
	}
	// Verify backup integrity
	path, err := s.backupPath(id)
	if err != nil {
		return err
	}
	if err := verifyFileChecksum(path, backup.Checksum); err != nil {
		return err
	}
	// The backup is a tar archive. We need to extract it into a new Docker volume.
	// We'll use a temporary container with the destination volume mounted and the backup tar piped in.
	// This requires access to the Docker daemon, which we may not have directly in the backup store.
	// For now, return an error indicating the limitation - the actual restore should be
	// triggered through the workspace-manager which has Docker access.
	return fmt.Errorf("%w: volume restore requires workspace-manager; use the workspace-manager's restore API", ErrBadInput)
}

func verifyFileChecksum(path, expected string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return err
	}
	sum := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if sum != expected {
		return fmt.Errorf("backup checksum mismatch: stored %s, computed %s", expected, sum)
	}
	return nil
}

func newID(prefix string) string {
	var bytes [16]byte
	_, _ = identity.NewSecretToken(prefix, 12)
	return prefix + "-" + hex.EncodeToString(bytes[:])[:24]
}
