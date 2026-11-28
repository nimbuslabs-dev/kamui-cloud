package objectstore

import (
	"context"
	"database/sql"
	"encoding/json"
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
	maxImportObjects = 10000
	maxImportBytes   = 8 << 30
	importClaim      = "@import"
)

var (
	ErrBackupImportUnavailable = errors.New("backup import requires the local object backend")
	restoreBucketID            = regexp.MustCompile(`^bkt-[0-9a-f]{32}$`)
)

func ValidBackupContentType(value string) bool {
	return len(value) <= 4096 && !strings.ContainsAny(value, "\r\n\x00")
}

// ImportObject describes one object from the verified archive. Open must return
// the extracted bytes of this entry.
type ImportObject struct {
	SourceBucket string
	Key          string // Empty only for a bucket without objects.
	Size         int64
	ContentType  string
	Checksum     string
	Open         func() (io.ReadCloser, error)
}

type RestoredBucket struct {
	SourceBucket string `json:"source_bucket"`
	Name         string `json:"name"`
	ID           string `json:"id"`
	ObjectCount  int    `json:"object_count"`
}

// ImportBackup stages data under fresh unpublished bucket IDs. Every bucket
// and object becomes visible in the same transaction as the operation result.
// A failed operation retains its journal until CleanupBackupImport completes.
func (s *Store) ImportBackup(ctx context.Context, projectID, actor, restoreID string, inputs []ImportObject) ([]RestoredBucket, error) {
	backend, ok := s.backend.(*LocalBackend)
	if !ok {
		return nil, ErrBackupImportUnavailable
	}
	if !validImportProject(projectID) || actor == "" {
		return nil, ErrBadInput
	}
	var active bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM backup_restores
		WHERE id=? AND project_id=? AND kind='objects' AND status='in_progress')`, restoreID, projectID).Scan(&active); err != nil {
		return nil, err
	}
	if !active {
		return nil, ErrNotFound
	}
	total, err := validateImport(inputs)
	if err != nil {
		return nil, err
	}
	if err := CheckStorageQuota(ctx, s.db, projectID, total); err != nil {
		return nil, err
	}

	// Reserve each new bucket path before staging any bytes. Pending blobs are
	// accessible only to the backend because the bucket metadata does not exist.
	if err := os.MkdirAll(backend.Root, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(backend.Root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := s.journalImportPath(ctx, projectID, restoreID, importClaim); err != nil {
		return nil, err
	}
	buckets := make([]RestoredBucket, 0)
	sources := make(map[string]int)
	objects := make([]Object, 0, len(inputs))
	for _, input := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		index, exists := sources[input.SourceBucket]
		if !exists {
			id, err := s.reserveImportBucket(ctx, root, projectID, restoreID)
			if err != nil {
				return nil, err
			}
			index = len(buckets)
			sources[input.SourceBucket] = index
			buckets = append(buckets, RestoredBucket{
				SourceBucket: input.SourceBucket, ID: id, Name: "restore-" + strings.TrimPrefix(id, "bkt-"),
			})
		}
		if input.Key == "" {
			continue
		}
		id := buckets[index].ID
		path := BucketPath(Bucket{ProjectID: projectID, ID: id}) + "/" + input.Key
		reader, err := input.Open()
		if err != nil {
			return nil, err
		}
		if reader == nil {
			return nil, ErrBadInput
		}
		size, checksum, putErr := backend.Put(ctx, path, io.LimitReader(importReader{ctx: ctx, reader: reader}, input.Size+1))
		if err := errors.Join(putErr, reader.Close()); err != nil {
			return nil, err
		}
		if size != input.Size || (input.Checksum != "" && checksum != input.Checksum) {
			return nil, fmt.Errorf("%w: extracted object failed integrity verification", ErrBadInput)
		}
		contentType := input.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		objects = append(objects, Object{
			BucketID: id, Key: input.Key, SizeBytes: size, Checksum: checksum, ContentType: contentType,
		})
		buckets[index].ObjectCount++
	}
	if err := s.publishImport(ctx, projectID, actor, restoreID, total, buckets, objects); err != nil {
		return nil, err
	}
	return buckets, nil
}

func validImportProject(projectID string) bool {
	return ValidObjectKey(projectID) && !strings.ContainsAny(projectID, "/\\:")
}

func validateImport(inputs []ImportObject) (int64, error) {
	if len(inputs) > maxImportObjects {
		return 0, ErrTooLarge
	}
	var total int64
	keys := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		if !bucketNameRe.MatchString(input.SourceBucket) {
			return 0, ErrBadInput
		}
		if input.Key == "" && input.Open == nil && input.Size == 0 {
			continue
		}
		if !ValidObjectKey(input.Key) || input.Open == nil || input.Size < 0 || !ValidBackupContentType(input.ContentType) {
			return 0, ErrBadInput
		}
		if _, err := filepath.Localize(input.Key); err != nil {
			return 0, ErrBadInput
		}
		if input.Size > MaxObjectBytes || input.Size > maxImportBytes-total {
			return 0, ErrTooLarge
		}
		total += input.Size
		path := input.SourceBucket + "/" + input.Key
		if _, exists := keys[path]; exists {
			return 0, fmt.Errorf("%w: duplicate archive object", ErrConflict)
		}
		keys[path] = struct{}{}
	}
	return total, nil
}

func (s *Store) reserveImportBucket(ctx context.Context, root *os.Root, projectID, restoreID string) (string, error) {
	id := newID()
	key := BucketPath(Bucket{ProjectID: projectID, ID: id})
	local, err := filepath.Localize(key)
	if err != nil {
		return "", err
	}
	if err := root.MkdirAll(filepath.Dir(local), 0700); err != nil {
		return "", err
	}
	// Reserve exclusively before journaling so recovery cannot remove a
	// pre-existing directory. An interruption before the journal can leave
	// an empty directory only. No blob write precedes the durable journal.
	if err := root.Mkdir(local, 0700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", ErrConflict
		}
		return "", err
	}
	if err := s.journalImportPath(ctx, projectID, restoreID, key); err != nil {
		return "", errors.Join(err, root.Remove(local))
	}
	return id, nil
}

func (s *Store) journalImportPath(ctx context.Context, projectID, restoreID, key string) error {
	result, err := s.db.ExecContext(ctx, `INSERT INTO backup_restore_blobs(restore_id,blob_key)
		SELECT id,? FROM backup_restores WHERE id=? AND project_id=? AND kind='objects' AND status='in_progress'
		ON CONFLICT(restore_id,blob_key) DO NOTHING`, key, restoreID, projectID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errors.Join(ErrConflict, err)
	}
	return nil
}

type quotaQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// CheckStorageQuota compares additional project bytes with the current object and volume usage.
// Call it inside the publication transaction to avoid concurrent restore overcommit.
func CheckStorageQuota(ctx context.Context, db quotaQuery, projectID string, size int64) error {
	var quota int64
	err := db.QueryRowContext(ctx, "SELECT max_storage_bytes FROM project_quotas WHERE project_id=?", projectID).Scan(&quota)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && quota == 0) {
		return nil
	}
	if err != nil {
		return err
	}
	var used int64
	if err := db.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT SUM(o.size_bytes) FROM objects o JOIN buckets b ON b.id=o.bucket_id WHERE b.project_id=?),0)
		+ COALESCE((SELECT SUM(v.size_bytes) FROM volume_placements v WHERE v.project_id=?),0)`, projectID, projectID).Scan(&used); err != nil {
		return err
	}
	if size > quota-used {
		return fmt.Errorf("%w: project limit %d bytes, used %d, restoring %d", ErrQuota, quota, used, size)
	}
	return nil
}

func (s *Store) publishImport(ctx context.Context, projectID, actor, restoreID string, total int64, buckets []RestoredBucket, objects []Object) error {
	result, err := json.Marshal(buckets)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().UnixNano()
	// Take SQLite's write lock before checking quota and publishing metadata.
	update, err := tx.ExecContext(ctx, `UPDATE backup_restores SET status='succeeded',result=?,finished_at=?
		WHERE id=? AND project_id=? AND kind='objects' AND status='in_progress'`,
		string(result), now, restoreID, projectID)
	if err != nil {
		return err
	}
	if n, err := update.RowsAffected(); err != nil || n != 1 {
		return errors.Join(ErrConflict, err)
	}
	if err := CheckStorageQuota(ctx, tx, projectID, total); err != nil {
		return err
	}
	for _, bucket := range buckets {
		if _, err := tx.ExecContext(ctx, "INSERT INTO buckets(id,project_id,name,created_by,created_at) VALUES(?,?,?,?,?)",
			bucket.ID, projectID, bucket.Name, actor, now); err != nil {
			return err
		}
	}
	for _, object := range objects {
		if _, err := tx.ExecContext(ctx, "INSERT INTO objects(bucket_id,key,size_bytes,checksum,content_type,created_at) VALUES(?,?,?,?,?,?)",
			object.BucketID, object.Key, object.SizeBytes, object.Checksum, object.ContentType, now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM backup_restore_blobs WHERE restore_id=?", restoreID); err != nil {
		return err
	}
	return tx.Commit()
}

// CleanupBackupImport removes the unpublished bucket namespaces listed in a
// failed operation's journal. Published bucket IDs and paths are protected.
func (s *Store) CleanupBackupImport(ctx context.Context, restoreID string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT j.blob_key,r.project_id FROM backup_restore_blobs j
		JOIN backup_restores r ON r.id=j.restore_id WHERE r.id=? AND r.kind='objects' AND r.status='failed'`, restoreID)
	if err != nil {
		return err
	}
	type pending struct{ key, project string }
	var keys []pending
	for rows.Next() {
		var item pending
		if err := rows.Scan(&item.key, &item.project); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, item)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil || len(keys) == 0 {
		return err
	}
	backend, ok := s.backend.(*LocalBackend)
	if !ok {
		return ErrBackupImportUnavailable
	}
	root, err := os.OpenRoot(backend.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, item := range keys {
		if item.key == importClaim {
			if _, err := s.db.ExecContext(ctx, "DELETE FROM backup_restore_blobs WHERE restore_id=? AND blob_key=?", restoreID, item.key); err != nil {
				return err
			}
			continue
		}
		prefix := "buckets/" + item.project + "/"
		id, matched := strings.CutPrefix(item.key, prefix)
		if !validImportProject(item.project) || !matched || !restoreBucketID.MatchString(id) {
			return fmt.Errorf("%w: malformed backup import journal key", ErrBadInput)
		}
		var published bool
		if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM buckets WHERE id=?)", id).Scan(&published); err != nil {
			return err
		}
		if published {
			return fmt.Errorf("%w: cannot clean a published bucket", ErrConflict)
		}
		local, err := filepath.Localize(item.key)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := root.RemoveAll(local); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, "DELETE FROM backup_restore_blobs WHERE restore_id=? AND blob_key=?", restoreID, item.key); err != nil {
			return err
		}
	}
	return nil
}

type importReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r importReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
