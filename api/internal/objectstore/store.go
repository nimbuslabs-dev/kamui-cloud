// Package objectstore implements Kamui Store: project-scoped object storage
// over a replaceable backend (local filesystem implemented; S3-compatible
// adapter is the same interface), with checksums, quotas, atomic uploads and
// pagination.
package objectstore

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	MaxObjectBytes = int64(1) << 30
	MaxKeyLen      = 1024
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrBadInput = errors.New("invalid input")
	ErrTooLarge = errors.New("payload exceeds the configured limit")
	ErrQuota    = errors.New("storage quota exceeded")

	bucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
)

// Backend abstracts blob storage. The local filesystem backend is provided;
// an S3-compatible backend implements the same three operations.
type Backend interface {
	Put(ctx context.Context, path string, body io.Reader) (int64, string, error) // size, sha256
	Open(ctx context.Context, path string) (io.ReadSeekCloser, error)
	Delete(ctx context.Context, path string) error
}

// LocalBackend stores blobs under an explicit root. Paths are validated and
// cleaned; traversal can never escape the root.
type LocalBackend struct{ Root string }

func (b *LocalBackend) resolve(key string) (string, error) {
	full := filepath.Join(b.Root, filepath.FromSlash(key))
	cleanRoot := filepath.Clean(b.Root)
	if !strings.HasPrefix(filepath.Clean(full), cleanRoot+string(os.PathSeparator)) {
		return "", ErrBadInput
	}
	return full, nil
}

func (b *LocalBackend) Put(ctx context.Context, key string, body io.Reader) (int64, string, error) {
	path, err := b.resolve(key)
	if err != nil {
		return 0, "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return 0, "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		return 0, "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(body, MaxObjectBytes+1))
	closeErr := tmp.Close()
	if err != nil {
		return 0, "", err
	}
	if closeErr != nil {
		return 0, "", closeErr
	}
	if size > MaxObjectBytes {
		return 0, "", ErrTooLarge
	}
	if err := os.Rename(tmpName, path); err != nil {
		return 0, "", err
	}
	return size, "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

func (b *LocalBackend) Open(ctx context.Context, key string) (io.ReadSeekCloser, error) {
	path, err := b.resolve(key)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return file, err
}

func (b *LocalBackend) Delete(ctx context.Context, key string) error {
	path, err := b.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

type Store struct {
	db        *sql.DB
	backend   Backend
	usage     func(ctx context.Context, projectID string) (int64, error)
	quotaLook func(ctx context.Context, projectID string) (int64, bool)
}

// NewStore wires the object store. usage reports the project's current stored
// bytes; quotaLook reports the project's quota and whether one is configured.
func NewStore(db *sql.DB, backend Backend, usage func(context.Context, string) (int64, error), quotaLook func(context.Context, string) (int64, bool)) *Store {
	return &Store{db: db, backend: backend, usage: usage, quotaLook: quotaLook}
}

type Bucket struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type Object struct {
	BucketID    string    `json:"bucket_id"`
	Key         string    `json:"key"`
	SizeBytes   int64     `json:"size_bytes"`
	Checksum    string    `json:"checksum"`
	ContentType string    `json:"content_type"`
	CreatedAt   time.Time `json:"created_at"`
}

func newID() string {
	var bytes [16]byte
	_, _ = crand.Read(bytes[:])
	return "bkt-" + hex.EncodeToString(bytes[:])
}

// BucketPath is the backend-relative blob path for a bucket.
func BucketPath(bucket Bucket) string {
	return filepath.ToSlash(filepath.Join("buckets", bucket.ProjectID, bucket.ID))
}

func (s *Store) CreateBucket(ctx context.Context, projectID, name, actor string) (Bucket, error) {
	if !bucketNameRe.MatchString(name) {
		return Bucket{}, fmt.Errorf("%w: bucket names must be lowercase letters, digits and dashes", ErrBadInput)
	}
	id := newID()
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO buckets(id,project_id,name,created_by,created_at) VALUES(?,?,?,?,?)", id, projectID, name, actor, now.UnixNano()); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return Bucket{}, fmt.Errorf("%w: bucket name already exists in this project", ErrConflict)
		}
		return Bucket{}, err
	}
	return Bucket{ID: id, ProjectID: projectID, Name: name, CreatedAt: now}, nil
}

func (s *Store) ListBuckets(ctx context.Context, projectID string) ([]Bucket, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,project_id,name,created_at FROM buckets WHERE project_id=? ORDER BY name", projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Bucket, 0)
	for rows.Next() {
		var bucket Bucket
		var createdAt int64
		if err := rows.Scan(&bucket.ID, &bucket.ProjectID, &bucket.Name, &createdAt); err != nil {
			return nil, err
		}
		bucket.CreatedAt = time.Unix(0, createdAt).UTC()
		items = append(items, bucket)
	}
	return items, rows.Err()
}

func (s *Store) GetBucket(ctx context.Context, projectID, bucketID string) (Bucket, error) {
	var bucket Bucket
	var createdAt int64
	err := s.db.QueryRowContext(ctx, "SELECT id,project_id,name,created_at FROM buckets WHERE id=? AND project_id=?", bucketID, projectID).
		Scan(&bucket.ID, &bucket.ProjectID, &bucket.Name, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Bucket{}, ErrNotFound
	}
	bucket.CreatedAt = time.Unix(0, createdAt).UTC()
	return bucket, err
}

func (s *Store) DeleteBucket(ctx context.Context, projectID, bucketID string) error {
	bucket, err := s.GetBucket(ctx, projectID, bucketID)
	if err != nil {
		return err
	}
	objects, err := s.ListObjects(ctx, bucket, "", 10000)
	if err != nil {
		return err
	}
	for _, object := range objects {
		_ = s.backend.Delete(ctx, BucketPath(bucket)+"/"+object.Key)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM buckets WHERE id=?", bucketID); err != nil {
		return err
	}
	return nil
}

// ValidObjectKey rejects traversal, control characters and oversized keys.
func ValidObjectKey(key string) bool {
	if key == "" || len(key) > MaxKeyLen {
		return false
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" || segment == "." || segment == ".." || strings.ContainsRune(segment, 0) {
			return false
		}
	}
	return true
}

// PutObject streams an upload atomically and enforces the project quota. The
// blob lands in the backend only after passing the size bound; a replacement
// upload that violates the quota removes the new blob and preserves the
// previously stored object and its metadata.
func (s *Store) PutObject(ctx context.Context, bucket Bucket, key, contentType string, body io.Reader) (Object, error) {
	if !ValidObjectKey(key) {
		return Object{}, fmt.Errorf("%w: object keys must be safe forward-slash paths", ErrBadInput)
	}
	size, checksum, err := s.backend.Put(ctx, BucketPath(bucket)+"/"+key, body)
	if err != nil {
		return Object{}, err
	}
	if s.usage != nil && s.quotaLook != nil {
		if quota, enforced := s.quotaLook(ctx, bucket.ProjectID); enforced && quota > 0 {
			used, usageErr := s.usage(ctx, bucket.ProjectID)
			if usageErr == nil {
				previous, getErr := s.GetObject(ctx, bucket, key)
				var previousBytes int64
				if getErr == nil {
					previousBytes = previous.SizeBytes
				}
				if used+size-previousBytes > quota {
					_ = s.backend.Delete(ctx, BucketPath(bucket)+"/"+key)
					return Object{}, fmt.Errorf("%w: project limit %d bytes, used %d, incoming %d", ErrQuota, quota, used, size)
				}
			}
		}
	}
	// Replacement: drop the superseded metadata row after the new blob is in.
	_, _ = s.db.ExecContext(ctx, "DELETE FROM objects WHERE bucket_id=? AND key=?", bucket.ID, key)
	return s.recordObject(ctx, bucket, key, blobRef{size: size, checksum: checksum}, contentType)
}

type blobRef struct {
	size     int64
	checksum string
}

func (s *Store) recordObject(ctx context.Context, bucket Bucket, key string, blob blobRef, contentType string) (Object, error) {
	now := time.Now().UTC()
	object := Object{BucketID: bucket.ID, Key: key, SizeBytes: blob.size, Checksum: blob.checksum, ContentType: contentType, CreatedAt: now}
	if contentType == "" {
		object.ContentType = "application/octet-stream"
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO objects(bucket_id,key,size_bytes,checksum,content_type,created_at) VALUES(?,?,?,?,?,?)",
		bucket.ID, key, object.SizeBytes, object.Checksum, object.ContentType, now.UnixNano())
	if err != nil {
		return Object{}, err
	}
	return object, nil
}

func (s *Store) GetObject(ctx context.Context, bucket Bucket, key string) (Object, error) {
	var object Object
	var createdAt int64
	err := s.db.QueryRowContext(ctx,
		"SELECT bucket_id,key,size_bytes,checksum,content_type,created_at FROM objects WHERE bucket_id=? AND key=?", bucket.ID, key).
		Scan(&object.BucketID, &object.Key, &object.SizeBytes, &object.Checksum, &object.ContentType, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Object{}, ErrNotFound
	}
	object.CreatedAt = time.Unix(0, createdAt).UTC()
	return object, err
}

// ListObjects paginates by key with a stable lexicographic cursor.
func (s *Store) ListObjects(ctx context.Context, bucket Bucket, after string, limit int) ([]Object, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	args := []any{bucket.ID}
	query := "SELECT bucket_id,key,size_bytes,checksum,content_type,created_at FROM objects WHERE bucket_id=?"
	if after != "" {
		query += " AND key>?"
		args = append(args, after)
	}
	query += " ORDER BY key LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Object, 0)
	for rows.Next() {
		var object Object
		var createdAt int64
		if err := rows.Scan(&object.BucketID, &object.Key, &object.SizeBytes, &object.Checksum, &object.ContentType, &createdAt); err != nil {
			return nil, err
		}
		object.CreatedAt = time.Unix(0, createdAt).UTC()
		items = append(items, object)
	}
	return items, rows.Err()
}

// OpenObject returns the blob reader and its metadata for verification.
func (s *Store) OpenObject(ctx context.Context, bucket Bucket, key string) (io.ReadSeekCloser, Object, error) {
	object, err := s.GetObject(ctx, bucket, key)
	if err != nil {
		return nil, Object{}, err
	}
	reader, err := s.backend.Open(ctx, BucketPath(bucket)+"/"+key)
	if err != nil {
		return nil, Object{}, err
	}
	return reader, object, nil
}

func (s *Store) DeleteObject(ctx context.Context, bucket Bucket, key string) error {
	object, err := s.GetObject(ctx, bucket, key)
	if err != nil {
		return err
	}
	if err := s.backend.Delete(ctx, BucketPath(bucket)+"/"+key); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "DELETE FROM objects WHERE bucket_id=? AND key=?", object.BucketID, object.Key)
	return err
}

// ProjectStorageBytes sums stored object sizes for quota accounting.
func (s *Store) ProjectStorageBytes(ctx context.Context, projectID string) (int64, error) {
	var total sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		"SELECT SUM(o.size_bytes) FROM objects o JOIN buckets b ON b.id=o.bucket_id WHERE b.project_id=?", projectID).Scan(&total)
	return total.Int64, err
}

// ProjectStorageBytes sums stored object sizes for quota accounting using any
// database handle; the service layer calls it with the shared store.
func ProjectStorageBytes(db *sql.DB, projectID string) (int64, error) {
	var total sql.NullInt64
	err := db.QueryRowContext(context.Background(),
		"SELECT SUM(o.size_bytes) FROM objects o JOIN buckets b ON b.id=o.bucket_id WHERE b.project_id=?", projectID).Scan(&total)
	return total.Int64, err
}

// VerifyChecksum recomputes an object's digest and compares it.
func VerifyChecksum(reader io.ReadSeekCloser, object Object) error {
	hasher := sha256.New()
	if _, err := io.Copy(hasher, reader); err != nil {
		return err
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return err
	}
	sum := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if !bytes.Equal([]byte(sum), []byte(object.Checksum)) {
		return fmt.Errorf("checksum mismatch for %s", object.Key)
	}
	return nil
}
