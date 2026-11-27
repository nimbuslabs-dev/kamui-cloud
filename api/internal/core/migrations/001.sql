CREATE TABLE users (id TEXT PRIMARY KEY);
CREATE TABLE projects (id TEXT PRIMARY KEY, owner_id TEXT NOT NULL REFERENCES users(id));
INSERT INTO users VALUES ('local-admin');
INSERT INTO projects VALUES ('default', 'local-admin');

CREATE TABLE workers (id TEXT PRIMARY KEY, snapshot TEXT NOT NULL);
CREATE TABLE workloads (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id),
    idempotency_key TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    state TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    revision INTEGER NOT NULL,
    snapshot TEXT NOT NULL,
    log_tail TEXT NOT NULL DEFAULT '',
    UNIQUE (project_id, idempotency_key)
);
CREATE INDEX workload_state ON workloads (state, created_at);
CREATE TABLE events (
    id INTEGER PRIMARY KEY,
    workload_id TEXT NOT NULL REFERENCES workloads(id),
    state TEXT NOT NULL,
    attempt INTEGER NOT NULL,
    at INTEGER NOT NULL
);
CREATE TABLE execution_cleanup (
    workload_id TEXT NOT NULL REFERENCES workloads(id),
    attempt INTEGER NOT NULL,
    PRIMARY KEY (workload_id, attempt)
);
