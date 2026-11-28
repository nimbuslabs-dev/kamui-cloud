-- Training checkpoint support and artifact checkpoint tracking
-- Add checkpoint columns to training_jobs
ALTER TABLE training_jobs ADD COLUMN checkpoint_artifact TEXT NOT NULL DEFAULT '';
ALTER TABLE training_jobs ADD COLUMN checkpoint_step INTEGER NOT NULL DEFAULT 0;
ALTER TABLE training_jobs ADD COLUMN resume_from TEXT NOT NULL DEFAULT '';
ALTER TABLE training_jobs ADD COLUMN checkpoint_path TEXT NOT NULL DEFAULT '';

-- Training checkpoint artifacts tracked separately for durability
CREATE TABLE training_checkpoints (
    id TEXT PRIMARY KEY,
    training_id TEXT NOT NULL REFERENCES training_jobs(id),
    artifact_id TEXT NOT NULL REFERENCES artifacts(id),
    step INTEGER NOT NULL,
    metrics TEXT NOT NULL DEFAULT '{}', -- JSON of metrics at checkpoint
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE (training_id, step)
);
CREATE INDEX training_checkpoints_training ON training_checkpoints (training_id, created_at DESC);

-- Model checkpoints for inference
ALTER TABLE models ADD COLUMN checkpoint_step INTEGER NOT NULL DEFAULT 0;
ALTER TABLE models ADD COLUMN checkpoint_artifact TEXT NOT NULL DEFAULT '';
ALTER TABLE models ADD COLUMN parent_model_id TEXT NOT NULL DEFAULT '' REFERENCES models(id);