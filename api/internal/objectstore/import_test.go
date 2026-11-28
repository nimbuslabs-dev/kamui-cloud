package objectstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
)

func TestImportBackupPublishesIndependentProjectBuckets(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := core.Open(ctx, filepath.Join(root, "core.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := NewStore(db.DB(), &LocalBackend{Root: root}, nil, nil)
	source, err := s.CreateBucket(ctx, "default", "fixtures", "local-admin")
	if err != nil {
		t.Fatal(err)
	}
	const payload = "immutable isolated fixture"
	expected := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(payload)))
	original, err := s.PutObject(ctx, source, "nested/key", "text/plain", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	startRestoreFixture(t, s, "restore-a")
	entries := []ImportObject{{
		SourceBucket: source.Name, Key: original.Key, Size: original.SizeBytes, ContentType: original.ContentType, Checksum: original.Checksum,
		Open: func() (io.ReadCloser, error) {
			if _, err := s.ImportBackup(ctx, "default", "local-admin", "restore-a", nil); !errors.Is(err, ErrConflict) {
				return nil, fmt.Errorf("concurrent restore claim = %v", err)
			}
			reader, _, err := s.OpenObject(ctx, source, original.Key)
			return reader, err
		},
	}}
	restored, err := s.ImportBackup(ctx, "default", "local-admin", "restore-a", entries)
	if err != nil || len(restored) != 1 || restored[0].Name == source.Name || restored[0].ObjectCount != 1 {
		t.Fatalf("restore = %+v, %v", restored, err)
	}
	destination, err := s.GetBucket(ctx, "default", restored[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetBucket(ctx, "other-project", destination.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unexpected cross-project result: %v", err)
	}
	for _, bucket := range []Bucket{source, destination} {
		reader, info, err := s.OpenObject(ctx, bucket, original.Key)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err = errors.Join(err, reader.Close()); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s digest = %s", bucket.Name, info.Checksum)
		if string(data) != payload || info.Checksum != expected || info.ContentType != "text/plain" ||
			fmt.Sprintf("sha256:%x", sha256.Sum256(data)) != expected {
			t.Fatalf("%s objects differ: %+v", bucket.Name, info)
		}
	}
	var status string
	if err := s.db.QueryRow("SELECT status FROM backup_restores WHERE id='restore-a'").Scan(&status); err != nil || status != "succeeded" {
		t.Fatalf("restore status = %s, %v", status, err)
	}
}

func startRestoreFixture(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.db.Exec("INSERT INTO backups(id,project_id,kind,target,status,created_by,created_at) VALUES('bck-fixture','default','objects','fixture','succeeded','local-admin',1) ON CONFLICT(id) DO NOTHING"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO backup_restores(id,backup_id,project_id,kind,status,created_at) VALUES(?,'bck-fixture','default','objects','in_progress',1)", id); err != nil {
		t.Fatal(err)
	}
}

func TestImportBackupQuotaRecheckAndCancellationCleanPendingBuckets(t *testing.T) {
	for _, reason := range []struct {
		name string
		want error
	}{
		{"quota", ErrQuota}, {"cancellation", context.Canceled},
	} {
		t.Run(reason.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			root := t.TempDir()
			db, err := core.Open(ctx, filepath.Join(root, "core.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			s := NewStore(db.DB(), &LocalBackend{Root: root}, nil, nil)
			startRestoreFixture(t, s, "restore-reject")
			entries := []ImportObject{
				{SourceBucket: "original", Key: "first", Size: 1, Open: func() (io.ReadCloser, error) {
					return io.NopCloser(strings.NewReader("a")), nil
				}},
				{SourceBucket: "original", Key: "last", Size: 1, Open: func() (io.ReadCloser, error) {
					if reason.name == "quota" {
						if _, err := s.db.Exec("INSERT INTO project_quotas(project_id,max_storage_bytes,updated_at) VALUES('default',1,1)"); err != nil {
							return nil, err
						}
					} else {
						cancel()
					}
					return io.NopCloser(strings.NewReader("b")), nil
				}},
			}
			if _, err := s.ImportBackup(ctx, "default", "local-admin", "restore-reject", entries); !errors.Is(err, reason.want) {
				t.Fatalf("import = %v, want %v", err, reason.want)
			}
			if buckets, err := s.ListBuckets(context.Background(), "default"); err != nil || len(buckets) != 0 {
				t.Fatalf("partial publication: %+v, %v", buckets, err)
			}
			var stagingPath string
			if err := s.db.QueryRow("SELECT blob_key FROM backup_restore_blobs WHERE restore_id='restore-reject' AND blob_key LIKE 'buckets/%'").Scan(&stagingPath); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, filepath.FromSlash(stagingPath))
			if got, err := os.ReadFile(filepath.Join(path, "first")); err != nil || string(got) != "a" {
				t.Fatalf("fixture did not stage any bytes: %q, %v", got, err)
			}
			if err := s.CleanupBackupImport(context.Background(), "restore-reject"); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("active restore staging removed: %v", err)
			}
			if _, err := s.db.Exec("UPDATE backup_restores SET status='failed' WHERE id='restore-reject'"); err != nil {
				t.Fatal(err)
			}
			if err := s.CleanupBackupImport(context.Background(), "restore-reject"); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("failed restore staging retained: %v", err)
			}
			var pending int
			if err := s.db.QueryRow("SELECT COUNT(*) FROM backup_restore_blobs WHERE restore_id='restore-reject'").Scan(&pending); err != nil || pending != 0 {
				t.Fatalf("journal entries = %d, %v", pending, err)
			}
		})
	}
}
