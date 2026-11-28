ALTER TABLE backups ADD COLUMN storage_engine_id TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN cleanup_due_at INTEGER;
ALTER TABLE volume_placements ADD COLUMN engine_id TEXT NOT NULL DEFAULT '';

CREATE TABLE backup_restores (
    id TEXT PRIMARY KEY,
    backup_id TEXT NOT NULL REFERENCES backups(id),
    project_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK(kind IN ('objects', 'volume')),
    status TEXT NOT NULL CHECK(status IN ('in_progress', 'succeeded', 'failed')),
    engine_id TEXT NOT NULL DEFAULT '',
    result TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    finished_at INTEGER,
    cleaned_at INTEGER,
    cleanup_due_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX backup_restores_cleanup ON backup_restores (status, cleaned_at, created_at);

CREATE TABLE backup_restore_blobs (
    restore_id TEXT NOT NULL REFERENCES backup_restores(id),
    blob_key TEXT NOT NULL,
    PRIMARY KEY (restore_id, blob_key)
);

-- Older executors could mount raw Docker names without recording their owner.
-- Preserve those logical names as unverified, even when the original daemon is
-- no longer visible. Existing assignments and recorded placements stay intact.
INSERT OR IGNORE INTO volume_placements(project_id,name,worker_id,size_bytes,created_at)
SELECT w.project_id, json_extract(v.value,'$.name'), 'legacy-unverified',
       MAX(COALESCE(json_extract(v.value,'$.size_bytes'),0)), MIN(w.created_at)
FROM workloads w, json_each(w.snapshot,'$.submission.volumes') v
WHERE json_extract(w.snapshot,'$.attempt') > 0
  AND json_type(v.value,'$.name') = 'text'
GROUP BY w.project_id, json_extract(v.value,'$.name');
