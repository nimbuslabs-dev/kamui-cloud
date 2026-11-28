package core

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestUpgradeV6RemembersUnverifiedVolumeNames(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "core.sqlite")
	old, err := openDB(path, "_pragma=foreign_keys(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	for _, schema := range []string{schema001, schema002, schema003, schema004, schema005, schema006, "PRAGMA user_version=6"} {
		if _, err := old.Exec(schema); err != nil {
			t.Fatal(err)
		}
	}
	_, err = old.Exec(`INSERT INTO workloads(id,project_id,idempotency_key,fingerprint,state,created_at,revision,snapshot)
		VALUES ('old-running','default','r','fp','RUNNING',1,1,'{"id":"old-running","state":"RUNNING","attempt":1,"assignment":{"worker_id":"local","runtime_id":"historic-engine"},"submission":{"volumes":[{"name":"established","size_bytes":64},{"name":"pinned","size_bytes":128}]}}'),
		       ('old-pending','default','p','fp','PENDING',1,1,'{"id":"old-pending","state":"PENDING","attempt":0,"submission":{"volumes":[{"name":"new-volume"}]}}');
		INSERT INTO volume_placements(project_id,name,worker_id,size_bytes,created_at)
		VALUES ('default','pinned','old-worker',32,1)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	current, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	for _, expected := range []struct {
		name, owner string
		size        int64
	}{{"established", "legacy-unverified", 64}, {"pinned", "old-worker", 32}} {
		var owner, engine string
		var size int64
		err := current.db.QueryRow(`SELECT worker_id,engine_id,size_bytes FROM volume_placements WHERE project_id='default' AND name=?`, expected.name).Scan(&owner, &engine, &size)
		if err != nil || owner != expected.owner || engine != "" || size != expected.size {
			t.Errorf("placement for %s = %s/%s (%d bytes), err = %v", expected.name, owner, engine, size, err)
		}
	}
	err = current.db.QueryRow(`SELECT worker_id FROM volume_placements WHERE name='new-volume'`).Scan(new(string))
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("unattempted volume acquired a placement, err = %v", err)
	}
	w, err := current.Get(ctx, "old-running")
	if err != nil || w.State != Running || w.Assignment == nil || w.Assignment.RuntimeID != "historic-engine" {
		t.Fatalf("legacy workload changed during upgrade: %+v, err = %v", w, err)
	}
}

func TestStoreReopenPreservesIdempotencyAndEvents(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "core.sqlite")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	other, err := Open(ctx, path)
	if err == nil {
		other.Close()
		t.Fatal("two coordinators acquired the same database")
	}
	var version int
	var mode string
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != LatestSchemaVersion {
		t.Fatalf("schema version = %d, err = %v", version, err)
	}
	if err := store.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal mode = %q, err = %v", mode, err)
	}
	spec := Submission{Image: "example:local"}
	if err := spec.Normalize(); err != nil {
		t.Fatal(err)
	}
	job, created, err := store.Submit(ctx, spec, "original")
	if err != nil || !created || job.State != Pending {
		t.Fatalf("submit = %+v, %v, %v", job, created, err)
	}
	job.State = Queued
	if err := store.Save(ctx, job, false); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, job, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	replay, created, err := store.Submit(ctx, spec, "original")
	if err != nil || created || replay.ID != job.ID || replay.State != Queued || replay.Revision != 2 {
		t.Fatalf("replay = %+v, %v, %v", replay, created, err)
	}
	spec.Args = []string{"different"}
	if _, _, err := store.Submit(ctx, spec, "original"); !errors.Is(err, ErrConflict) {
		t.Fatalf("idempotency conflict = %v", err)
	}
	events, err := store.Events(ctx, 1)
	if err != nil || len(events) != 1 || events[0].State != Queued || events[0].WorkloadID != job.ID {
		t.Fatalf("events = %+v, %v", events, err)
	}
}

func TestMigrateV1ToLatestPreservesExistingData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	db, err := openDB(path, "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, schema001); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO workloads(id,project_id,idempotency_key,fingerprint,state,created_at,revision,snapshot)
		 VALUES('wl-00000000000000000000000000000001','default','legacy-k','fp','SUCCEEDED',1,1,'{"id":"wl-00000000000000000000000000000001","project_id":"default","state":"SUCCEEDED"}')`,
	); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var version int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != LatestSchemaVersion {
		t.Fatalf("upgraded version = %d, want %d (%v)", version, LatestSchemaVersion, err)
	}
	var username string
	var isAdmin int
	if err := store.db.QueryRow("SELECT username, is_system_admin FROM users WHERE id='local-admin'").Scan(&username, &isAdmin); err != nil || username != "admin" || isAdmin != 1 {
		t.Fatalf("local-admin after upgrade = %q, %d (%v)", username, isAdmin, err)
	}
	var role string
	if err := store.db.QueryRow("SELECT role FROM project_members WHERE project_id='default' AND user_id='local-admin'").Scan(&role); err != nil || role != "owner" {
		t.Fatalf("default owner membership = %q (%v)", role, err)
	}
	w, err := store.Get(ctx, "wl-00000000000000000000000000000001")
	if err != nil || w.ProjectID != "default" {
		t.Fatalf("preserved workload = %+v (%v)", w, err)
	}
}

func TestUpgradeModelsFromV4AndV5(t *testing.T) {
	for _, version := range []string{"4", "5"} {
		t.Run(version, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "core.sqlite")
			old, err := openDB(path, "_pragma=foreign_keys(1)")
			if err != nil {
				t.Fatal(err)
			}
			for _, schema := range []string{schema001, schema002, schema003, schema004} {
				if _, err := old.Exec(schema); err != nil {
					old.Close()
					t.Fatal(err)
				}
			}
			if version == "5" {
				if _, err := old.Exec(schema005); err != nil {
					old.Close()
					t.Fatal(err)
				}
				// Preserve a row written by an older installation without FK enforcement.
				if _, err := old.Exec("PRAGMA foreign_keys=OFF"); err != nil {
					old.Close()
					t.Fatal(err)
				}
			}
			if _, err := old.Exec("INSERT INTO models(id, project_id, name, artifact_id, created_by, created_at) VALUES('mdl-before','default','before','art-fixture','local-admin',0)"); err != nil {
				old.Close()
				t.Fatal(err)
			}
			if _, err := old.Exec("PRAGMA user_version=" + version); err != nil {
				old.Close()
				t.Fatal(err)
			}
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}
			current, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer current.Close()
			var parentIsNull, fk int
			if err := current.db.QueryRow("SELECT parent_model_id IS NULL FROM models WHERE id='mdl-before'").Scan(&parentIsNull); err != nil || parentIsNull != 1 {
				t.Fatalf("legacy row parent was not normalized: %d, %v", parentIsNull, err)
			}
			if err := current.db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
				t.Fatalf("foreign_keys = %d, %v", fk, err)
			}
			if _, err := current.db.Exec("UPDATE models SET parent_model_id='missing-parent' WHERE id='mdl-before'"); err == nil {
				t.Fatal("foreign key enforcement was not restored")
			}
		})
	}
}
