package ml

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/core"
	"github.com/nimbuslabs-dev/kamui-cloud/api/internal/runtime"
)

func TestTrainingReadsPersistedCheckpointFields(t *testing.T) {
	ctx := context.Background()
	store, err := core.Open(ctx, filepath.Join(t.TempDir(), "core.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.DB().Exec("INSERT INTO training_jobs(id, project_id, name, created_by, created_at, updated_at, checkpoint_artifact, checkpoint_step, resume_from, checkpoint_path) VALUES('trn-fixture','default','fixture','local-admin',0,0,'art-fixture',3,'chk-fixture','/kamui/checkpoint')"); err != nil {
		t.Fatal(err)
	}
	s := &TrainStore{db: store.DB(), core: store}
	result, err := s.GetTraining(ctx, "default", "trn-fixture")
	if err != nil || result.CheckpointStep != 3 || result.ResumeFrom != "chk-fixture" || result.CheckpointPath != "/kamui/checkpoint" {
		t.Fatalf("checkpoint fields = %+v, %v", result, err)
	}
}

func TestCheckpointAndModelOwnershipAndIntegrity(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := core.Open(ctx, filepath.Join(root, "core.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sql := store.DB()
	if _, err := sql.Exec("INSERT INTO projects(id,owner_id) VALUES('other','local-admin')"); err != nil {
		t.Fatal(err)
	}
	for _, job := range []struct{ id, project string }{
		{"trn-first", "default"}, {"trn-second", "default"}, {"trn-other", "other"},
	} {
		if _, err := sql.Exec("INSERT INTO training_jobs(id,project_id,name,created_by,created_at,updated_at) VALUES(?,?,'fixture','local-admin',0,0)", job.id, job.project); err != nil {
			t.Fatal(err)
		}
	}
	artifacts := runtime.NewStore(sql, "test-master", root)
	put := func(project, name, kind string) runtime.Artifact {
		t.Helper()
		a, err := artifacts.CreateArtifact(ctx, project, name, kind, "application/octet-stream", "local-admin", strings.NewReader("fixture"))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	s := NewTrainStore(sql, store, artifacts)
	checkpointBlob := put("default", "checkpoint", "checkpoint")
	foreignCheckpoint := put("other", "other-checkpoint", "checkpoint")
	modelBlob := put("default", "model", "model")
	foreignModel := put("other", "other-model", "model")
	jobID := "trn-first"
	chk, err := s.CreateCheckpoint(ctx, "default", jobID, checkpointBlob.ID, 1, nil, "local-admin")
	if err != nil || string(chk.Metrics) != "{}" {
		t.Fatalf("checkpoint metrics = %s, %v", chk.Metrics, err)
	}
	model, err := s.RegisterModel(ctx, "default", "fixture-model", modelBlob.ID, "opaque", jobID, "", 0, "local-admin")
	if err != nil || model.Checksum != modelBlob.Checksum || model.SizeBytes != modelBlob.SizeBytes {
		t.Fatalf("trusted model metadata = %+v, %v", model, err)
	}
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"foreign training listing", func() error { _, e := s.ListCheckpoints(ctx, "default", "trn-other"); return e }(), ErrNotFound},
		{"foreign training creation", func() error {
			_, e := s.CreateCheckpoint(ctx, "default", "trn-other", checkpointBlob.ID, 2, nil, "local-admin")
			return e
		}(), ErrNotFound},
		{"foreign checkpoint artifact", func() error {
			_, e := s.CreateCheckpoint(ctx, "default", jobID, foreignCheckpoint.ID, 2, nil, "local-admin")
			return e
		}(), ErrNotFound},
		{"wrong kind", func() error {
			_, e := s.CreateCheckpoint(ctx, "default", jobID, modelBlob.ID, 2, nil, "local-admin")
			return e
		}(), ErrBadInput},
		{"resume mismatch", func() error {
			_, e := s.ResumeFromCheckpoint(ctx, "default", "trn-second", "trial", chk.ID, "local-admin")
			return e
		}(), ErrNotFound},
		{"unsupported resume", func() error {
			_, e := s.ResumeFromCheckpoint(ctx, "default", jobID, "trial", chk.ID, "local-admin")
			return e
		}(), ErrResumeUnsupported},
		{"foreign model artifact", func() error {
			_, e := s.RegisterModel(ctx, "default", "invalid", foreignModel.ID, "opaque", "", "", 0, "local-admin")
			return e
		}(), ErrNotFound},
		{"fabricated model artifact", func() error {
			_, e := s.RegisterModel(ctx, "default", "invalid", "art-missing", "opaque", "", "", 0, "local-admin")
			return e
		}(), ErrNotFound},
		{"foreign source training", func() error {
			_, e := s.RegisterModel(ctx, "default", "invalid", modelBlob.ID, "opaque", "trn-other", "", 0, "local-admin")
			return e
		}(), ErrNotFound},
		{"untrusted model digest", func() error {
			_, e := s.RegisterModel(ctx, "default", "invalid", modelBlob.ID, "opaque", "", "sha256:provided", 0, "local-admin")
			return e
		}(), ErrBadInput},
	} {
		if !errors.Is(tc.err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, tc.err, tc.want)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "artifacts", "default", modelBlob.ID, "blob"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterModel(ctx, "default", "corrupt", modelBlob.ID, "opaque", "", "", 0, "local-admin"); !errors.Is(err, ErrBadInput) {
		t.Fatalf("corrupt model blob: %v", err)
	}
}
