ALTER TABLE workloads ADD COLUMN worker_id TEXT NOT NULL DEFAULT '';
ALTER TABLE workloads ADD COLUMN priority INTEGER NOT NULL DEFAULT 50;
ALTER TABLE workloads ADD COLUMN placement TEXT NOT NULL DEFAULT '';
CREATE INDEX workload_worker ON workloads (worker_id, state);
CREATE INDEX workload_queue ON workloads (state, priority DESC, created_at);

CREATE TABLE worker_enrollments (
    id TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL DEFAULT '',
    auto_approve INTEGER NOT NULL DEFAULT 0,
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    used_at INTEGER,
    used_by TEXT,
    revoked_at INTEGER
);

CREATE TABLE remote_workers (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    hostname TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    enrolled_at INTEGER NOT NULL,
    enrolled_by TEXT NOT NULL DEFAULT '',
    approved_at INTEGER,
    revoked_at INTEGER,
    last_heartbeat INTEGER,
    capabilities TEXT,
    telemetry TEXT,
    latency_ms REAL NOT NULL DEFAULT 0,
    reliability REAL NOT NULL DEFAULT 0.5,
    policy TEXT
);

CREATE TABLE worker_tokens (
    token_hash TEXT PRIMARY KEY,
    worker_id TEXT NOT NULL REFERENCES remote_workers(id),
    created_at INTEGER NOT NULL,
    last_used INTEGER,
    revoked_at INTEGER
);
CREATE INDEX worker_tokens_worker ON worker_tokens (worker_id);

CREATE TABLE worker_reports (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    worker_id TEXT NOT NULL,
    workload_id TEXT NOT NULL,
    fence TEXT NOT NULL,
    kind TEXT NOT NULL,
    payload TEXT NOT NULL,
    at INTEGER NOT NULL
);
CREATE INDEX worker_reports_worker ON worker_reports (worker_id, id);

CREATE TABLE placement_decisions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    workload_id TEXT NOT NULL REFERENCES workloads(id),
    attempt INTEGER NOT NULL,
    mode TEXT NOT NULL,
    chosen_worker TEXT,
    explanation TEXT NOT NULL,
    candidates TEXT NOT NULL,
    at INTEGER NOT NULL
);
CREATE INDEX placement_decisions_workload ON placement_decisions (workload_id, id);

CREATE TABLE project_scheduling_prefs (
    project_id TEXT PRIMARY KEY REFERENCES projects(id),
    mode TEXT NOT NULL DEFAULT 'auto',
    preferred_workers TEXT NOT NULL DEFAULT '[]',
    fallback INTEGER NOT NULL DEFAULT 1,
    updated_at INTEGER NOT NULL,
    updated_by TEXT NOT NULL
);
