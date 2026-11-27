package core

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

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
