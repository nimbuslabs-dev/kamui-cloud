package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/identity"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/objectstore"
)

func setupBackupStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	db, err := core.Open(context.Background(), filepath.Join(root, "core.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewStore(db.DB(), nil, root, nil)
}

func TestBackupRestoresPublishIsolatedCopies(t *testing.T) {
	s := setupBackupStore(t)
	ctx := context.Background()
	content := "fixture"
	backup, err := s.CreateObjectBackup(ctx, "default", "local-admin", []ObjectRef{{Bucket: "bucket", Key: "dir/note.txt", Size: int64(len(content))}},
		func(context.Context, ObjectRef) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(content)), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	admin := identity.Principal{User: identity.User{IsSystemAdmin: true}}
	snapshot, err := s.CreateMetadataBackup(ctx, "local-admin")
	if err != nil || snapshot.ID == backup.ID {
		t.Fatalf("metadata backup %q: %v", snapshot.ID, err)
	}
	if _, err := s.RestoreMetadata(ctx, identity.Principal{}, snapshot.ID); err != ErrForbidden {
		t.Fatalf("non-admin restore: %v", err)
	}
	metadata, err := s.RestoreMetadata(ctx, admin, snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	copy, err := os.ReadFile(filepath.Join(s.root, filepath.FromSlash(metadata)))
	if err != nil || fmt.Sprintf("sha256:%x", sha256.Sum256(copy)) != snapshot.Checksum {
		t.Fatalf("restored snapshot integrity: %v", err)
	}

	first, err := s.restore(ctx, "default", backup.ID, "objects", newID("restore"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.restore(ctx, "default", backup.ID, "objects", newID("restore"))
	if err != nil || first == second {
		t.Fatalf("restore locations: %q and %q, %v", first, second, err)
	}
	for _, location := range []string{first, second} {
		got, err := os.ReadFile(filepath.Join(s.root, filepath.FromSlash(location), "bucket", "dir", "note.txt"))
		if err != nil || string(got) != content {
			t.Fatalf("restore content: %q, %v", got, err)
		}
	}
}

func TestRestoreRejectsLinksAndCorruptionAndCleansUp(t *testing.T) {
	s := setupBackupStore(t)
	ctx := context.Background()
	objects := objectstore.NewStore(s.db, &objectstore.LocalBackend{Root: s.root}, nil, nil)
	s.UseObjectStore(objects)
	for i, tc := range []struct {
		kind    byte
		corrupt bool
	}{
		{tar.TypeSymlink, false}, {tar.TypeLink, false}, {tar.TypeReg, true},
	} {
		var data bytes.Buffer
		w := tar.NewWriter(&data)
		if err := w.WriteHeader(&tar.Header{Name: "bucket/fixture", Typeflag: tc.kind, Mode: 0600, Linkname: "inside"}); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("bck-fixture-%d", i)
		path, err := s.backupPath(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		checksum := fmt.Sprintf("sha256:%x", sha256.Sum256(data.Bytes()))
		if tc.corrupt {
			checksum = "sha256:invalid"
		}
		if _, err := s.db.Exec("INSERT INTO backups(id,project_id,kind,target,status,size_bytes,checksum,created_by,created_at) VALUES(?,'default','objects','objects:default','succeeded',?,?,'local-admin',0)", id, data.Len(), checksum); err != nil {
			t.Fatal(err)
		}
		if _, err := s.restore(ctx, "default", id, "objects", newID("restore")); err == nil {
			t.Fatalf("invalid archive case %d succeeded", i)
		}
		if op, err := s.RestoreProjectBackup(ctx, "default", id, "local-admin"); err == nil || op.Status != "failed" {
			t.Fatalf("import accepted invalid archive case %d: %+v, %v", i, op, err)
		}
		if buckets, err := objects.ListBuckets(ctx, "default"); err != nil || len(buckets) != 0 {
			t.Fatalf("import published invalid archive case %d: %+v, %v", i, buckets, err)
		}
		entries, err := os.ReadDir(filepath.Join(s.root, "restores"))
		if err != nil || len(entries) != 0 {
			t.Fatalf("partial restore left files: %v, %v", entries, err)
		}
	}
}
