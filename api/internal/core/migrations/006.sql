-- A model without a parent must store NULL for the optional foreign key.
CREATE TABLE models_v6 (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id),
    name TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    format TEXT NOT NULL DEFAULT '',
    source_training TEXT NOT NULL DEFAULT '',
    checksum TEXT NOT NULL DEFAULT '',
    size_bytes INTEGER NOT NULL DEFAULT 0,
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    checkpoint_step INTEGER NOT NULL DEFAULT 0,
    checkpoint_artifact TEXT NOT NULL DEFAULT '',
    parent_model_id TEXT REFERENCES models_v6(id),
    UNIQUE (project_id, name)
);

INSERT INTO models_v6
SELECT id, project_id, name, artifact_id, format, source_training, checksum, size_bytes,
       created_by, created_at, checkpoint_step, checkpoint_artifact, NULLIF(parent_model_id, '')
FROM models;
DROP TABLE models;
ALTER TABLE models_v6 RENAME TO models;
