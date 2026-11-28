// Package backup implements Kamui Backup: consistent coordinator metadata
// backups (SQLite VACUUM INTO — never a raw WAL copy), workspace volume
// exports, stored-object bundles and restore into isolated destinations that
// never overwrite live data without explicit authorization.
package backup

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
)

const (
	MaxBackupBytes    = int64(8) << 30
	MaxRestoreScan    = int64(8) << 30
	MaxRestoreEntries = 10000
)

var (
	ErrNotFound       = errors.New("backup not found")
	ErrBadInput       = errors.New("invalid backup request")
	ErrNotImplemented = errors.New("managed volume restore is unavailable")
	ErrForbidden      = errors.New("coordinator metadata requires a system administrator")
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
	record, err := s.recordStart(ctx, id, "", "metadata", target, actor)
	if err != nil {
		return Backup{}, err
	}
	path, err := s.backupPath(id)
	if err != nil {
		return record.failed(err), err
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		os.Remove(path)
		return record.failed(fmt.Errorf("snapshot failed: %w", err)), fmt.Errorf("snapshot failed: %w", err)
	}
	return record.finalize(ctx, path)
}

// CreateObjectBackup bundles one project's stored objects with their metadata.
func (s *Store) CreateObjectBackup(ctx context.Context, projectID, actor string, objects []ObjectRef, openObject func(context.Context, ObjectRef) (io.ReadCloser, error)) (Backup, error) {
	id := newID("bck")
	target := "objects:" + projectID
	if projectID == "" || len(objects) > MaxRestoreEntries {
		return Backup{}, ErrBadInput
	}
	record, err := s.recordStart(ctx, id, projectID, "objects", target, actor)
	if err != nil {
		return Backup{}, err
	}
	path, err := s.backupPath(id)
	if err != nil {
		return record.failed(err), err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return record.failed(err), err
	}
	defer file.Close()
	writer := tar.NewWriter(file)
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
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return record.failed(err), err
	}
	return record.finalize(ctx, path)
}

// ErrTooBig marks an oversized backup request.
var ErrTooBig = errors.New("backup exceeds the size limit")

// CreateVolumeBackup exports one Docker volume through the workspace-manager
// path. Backups never write to external SSDs; blobs live on the storage root.
func (s *Store) CreateVolumeBackup(ctx context.Context, projectID, volume, actor string) (Backup, error) {
	if s.volumeTar == nil {
		return Backup{}, fmt.Errorf("%w: volume backups require the workspace manager on this host", ErrBadInput)
	}
	if projectID == "" || !validVolumeName(volume) {
		return Backup{}, fmt.Errorf("%w: invalid volume name", ErrBadInput)
	}
	id := newID("bck")
	target := "volume:" + volume
	record, err := s.recordStart(ctx, id, projectID, "volume", target, actor)
	if err != nil {
		return Backup{}, err
	}
	path, err := s.backupPath(id)
	if err != nil {
		return record.failed(err), err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return record.failed(err), err
	}
	defer file.Close()
	counter := &countingWriter{w: file}
	if err := s.volumeTar(ctx, volume, counter); err != nil {
		os.Remove(path)
		return record.failed(err), err
	}
	if counter.n > MaxBackupBytes {
		os.Remove(path)
		return record.failed(ErrTooBig), ErrTooBig
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return record.failed(err), err
	}
	return record.finalize(ctx, path)
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > MaxBackupBytes-c.n {
		return 0, ErrTooBig
	}
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

func (s *Store) recordStart(ctx context.Context, id, projectID, kind, target, actor string) (*pendingBackup, error) {
	now := time.Now().UTC().UnixNano()
	_, err := s.db.ExecContext(ctx, "INSERT INTO backups(id,project_id,kind,target,status,created_by,created_at) VALUES(?,?,?,?, 'in_progress', ?, ?)",
		id, projectID, kind, target, actor, now)
	if err != nil {
		return nil, err
	}
	return &pendingBackup{store: s, id: id, projectID: projectID}, nil
}

type pendingBackup struct {
	store         *Store
	id, projectID string
}

func (p *pendingBackup) failed(cause error) Backup {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const reason = "backup creation failed"
	if _, err := p.store.db.ExecContext(ctx, "UPDATE backups SET status='failed', reason=?, finished_at=? WHERE id=?",
		reason, time.Now().UTC().UnixNano(), p.id); err != nil {
		slog.Warn("record backup failure", "backup_id", p.id, "error", err, "cause", cause)
	}
	return Backup{ID: p.id, Status: "failed", Reason: reason}
}

func (p *pendingBackup) finalize(ctx context.Context, path string) (Backup, error) {
	file, err := os.Open(path)
	if err == nil {
		hash := sha256.New()
		size, readErr := io.Copy(hash, io.LimitReader(contextReader{ctx, file}, MaxBackupBytes+1))
		err = errors.Join(readErr, file.Close())
		if err == nil && size > MaxBackupBytes {
			err = ErrTooBig
		}
		if err == nil {
			sum := "sha256:" + hex.EncodeToString(hash.Sum(nil))
			_, err = p.store.db.ExecContext(ctx, "UPDATE backups SET status='succeeded', size_bytes=?, checksum=?, finished_at=? WHERE id=?",
				size, sum, time.Now().UTC().UnixNano(), p.id)
		}
	}
	if err != nil {
		_ = os.Remove(path)
		return p.failed(err), err
	}
	return p.store.GetBackup(ctx, p.projectID, p.id)
}

// GetBackup requires the exact project. An empty project selects coordinator metadata.
func (s *Store) GetBackup(ctx context.Context, projectID, id string) (Backup, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+backupColumns+" FROM backups WHERE id=? AND project_id=?", id, projectID)
	result, err := scanBackup(row)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (result.Kind == "metadata") != (projectID == "")) {
		return Backup{}, ErrNotFound
	}
	return result, err
}

func (s *Store) ListBackups(ctx context.Context, projectID string, limit int) ([]Backup, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	query := "SELECT " + backupColumns + " FROM backups WHERE project_id=? AND kind != 'metadata' ORDER BY created_at DESC LIMIT ?"
	if projectID == "" {
		query = "SELECT " + backupColumns + " FROM backups WHERE project_id=? AND kind = 'metadata' ORDER BY created_at DESC LIMIT ?"
	}
	rows, err := s.db.QueryContext(ctx, query, projectID, limit)
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

// OpenBackup returns the blob only when the scoped record is complete.
func (s *Store) OpenBackup(ctx context.Context, projectID, id string) (io.ReadSeekCloser, int64, error) {
	record, err := s.GetBackup(ctx, projectID, id)
	if err != nil {
		return nil, 0, err
	}
	return s.openBackup(record)
}

func (s *Store) openBackup(record Backup) (*os.File, int64, error) {
	if record.Status != "succeeded" {
		return nil, 0, fmt.Errorf("%w: backup is not complete", ErrBadInput)
	}
	path, err := s.backupPath(record.ID)
	if err != nil {
		return nil, 0, err
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, 0, err
	}
	defer root.Close()
	info, err := root.Lstat(filepath.Base(path))
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() || info.Size() != record.SizeBytes || info.Size() > MaxRestoreScan {
		return nil, 0, fmt.Errorf("%w: backup blob has an invalid type or size", ErrBadInput)
	}
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return nil, 0, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Size() != info.Size() {
		file.Close()
		return nil, 0, fmt.Errorf("%w: backup blob changed", ErrBadInput)
	}
	return file, info.Size(), nil
}

type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func (s *Store) RestoreMetadata(ctx context.Context, principal identity.Principal, id string) (string, error) {
	if !principal.User.IsSystemAdmin {
		return "", ErrForbidden
	}
	return s.restore(ctx, "", id, "metadata")
}

// RestoreObjectBackup produces an isolated export for this project.
func (s *Store) RestoreObjectBackup(ctx context.Context, projectID, id string) (string, error) {
	if projectID == "" {
		return "", ErrNotFound
	}
	return s.restore(ctx, projectID, id, "objects")
}

func (s *Store) RestoreVolumeBackup(ctx context.Context, projectID, id string) error {
	record, err := s.GetBackup(ctx, projectID, id)
	if err != nil {
		return err
	}
	if record.Kind != "volume" {
		return ErrBadInput
	}
	return ErrNotImplemented
}

func (s *Store) restore(ctx context.Context, projectID, id, kind string) (location string, resultErr error) {
	record, err := s.GetBackup(ctx, projectID, id)
	if err != nil {
		return "", err
	}
	if record.Kind != kind {
		return "", ErrBadInput
	}
	file, size, err := s.openBackup(record)
	if err != nil {
		return "", err
	}
	defer file.Close()

	if err := os.MkdirAll(filepath.Join(s.root, "restores"), 0700); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return "", err
	}
	defer root.Close()
	dir := filepath.Join("restores", newID("restore"))
	if err := root.Mkdir(dir, 0700); err != nil {
		return "", err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, root.RemoveAll(dir))
		}
	}()
	stage := filepath.Join(dir, "pending")
	if err := root.Mkdir(stage, 0700); err != nil {
		return "", err
	}
	hash := sha256.New()
	content := io.TeeReader(io.LimitReader(contextReader{ctx, file}, size), hash)
	name := "data"
	if kind == "metadata" {
		dest, err := root.OpenFile(filepath.Join(stage, "kamui-core.sqlite"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(dest, content)
		if err := errors.Join(err, dest.Close()); err != nil {
			return "", err
		}
		name = "kamui-core.sqlite"
	} else {
		out, err := root.OpenRoot(stage)
		if err != nil {
			return "", err
		}
		err = extractTar(out, content)
		if err := errors.Join(err, out.Close()); err != nil {
			return "", err
		}
	}
	if _, err := io.Copy(io.Discard, content); err != nil {
		return "", err
	}
	if "sha256:"+hex.EncodeToString(hash.Sum(nil)) != record.Checksum {
		return "", fmt.Errorf("%w: backup checksum mismatch", ErrBadInput)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	published := filepath.Join(dir, "data")
	if err := root.Rename(stage, published); err != nil {
		return "", err
	}
	if kind == "metadata" {
		published = filepath.Join(published, name)
	}
	return filepath.ToSlash(published), nil
}

func extractTar(root *os.Root, content io.Reader) error {
	tr := tar.NewReader(content)
	var total int64
	entries := 0
	dirs := map[string]bool{".": true}
	mkdir := func(dir string) error {
		for parent := dir; !dirs[parent]; parent = filepath.Dir(parent) {
			if entries >= MaxRestoreEntries {
				return ErrTooBig
			}
			dirs[parent] = true
			entries++
		}
		return root.MkdirAll(dir, 0700)
	}
	for records := 0; ; records++ {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: malformed backup archive", ErrBadInput)
		}
		if records >= MaxRestoreEntries || entries >= MaxRestoreEntries || h.Size < 0 || h.Size > MaxRestoreScan-total {
			return ErrTooBig
		}
		total += h.Size

		name := h.Name
		if h.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		if !fs.ValidPath(name) || name == "." || len(name) > 4096 || strings.Count(name, "/") > 64 || strings.ContainsAny(name, "\\:") {
			return fmt.Errorf("%w: invalid archive entry name", ErrBadInput)
		}
		local, err := filepath.Localize(name)
		if err != nil {
			return fmt.Errorf("%w: invalid archive entry name", ErrBadInput)
		}

		switch h.Typeflag {
		case tar.TypeDir:
			if err := mkdir(local); err != nil {
				return err
			}
		case tar.TypeReg:
			entries++
			if err := mkdir(filepath.Dir(local)); err != nil {
				return err
			}
			dest, err := root.OpenFile(local, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
			if err != nil {
				return err
			}
			_, err = io.Copy(dest, tr)
			if err := errors.Join(err, dest.Close()); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: unsupported archive entry type", ErrBadInput)
		}
	}
}

func newID(prefix string) string {
	var bytes [12]byte
	rand.Read(bytes[:])
	return prefix + "-" + hex.EncodeToString(bytes[:])
}
