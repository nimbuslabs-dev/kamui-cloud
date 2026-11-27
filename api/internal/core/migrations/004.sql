-- Shared distributed execution prerequisites (image distribution, artifacts,
-- secrets, persistent volumes, service routes) and service-layer metadata for
-- Deploy, Store/Backup, Train, Infer, collaboration quotas, SRE, managed
-- databases and Drop. Additive; no existing tables are altered.

-- Project-scoped registry credentials. Values are sealed; never returned.
CREATE TABLE registry_credentials (
    host TEXT NOT NULL,
    project_id TEXT NOT NULL REFERENCES projects(id),
    sealed_username TEXT NOT NULL,
    sealed_password TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (host, project_id)
);

-- Project-scoped artifact staging metadata. Blobs live on the storage root.
CREATE TABLE artifacts (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id),
    name TEXT NOT NULL,
    kind TEXT NOT NULL DEFAULT 'generic',
    size_bytes INTEGER NOT NULL,
    checksum TEXT NOT NULL,
    content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    last_download_at INTEGER,
    UNIQUE (project_id, name)
);
CREATE INDEX artifacts_project ON artifacts (project_id, created_at);

-- Project-scoped secrets. Values are sealed; there is no read endpoint.
CREATE TABLE secrets (
    project_id TEXT NOT NULL REFERENCES projects(id),
    name TEXT NOT NULL,
    sealed_value TEXT NOT NULL,
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    rotated_at INTEGER,
    PRIMARY KEY (project_id, name)
);

-- Persistent volume placement. Stateful workloads pin to their first worker;
-- the coordinator never recreates an empty volume elsewhere after a failure.
CREATE TABLE volume_placements (
    project_id TEXT NOT NULL,
    name TEXT NOT NULL,
    worker_id TEXT NOT NULL,
    size_bytes INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    last_used_at INTEGER,
    PRIMARY KEY (project_id, name)
);

-- Service routes registered by a live, fenced assignment.
CREATE TABLE service_routes (
    workload_id TEXT PRIMARY KEY REFERENCES workloads(id),
    project_id TEXT NOT NULL,
    worker_id TEXT NOT NULL,
    attempt INTEGER NOT NULL,
    fence TEXT NOT NULL,
    port INTEGER NOT NULL,
    kind TEXT NOT NULL DEFAULT 'http',
    registered_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX service_routes_project ON service_routes (project_id);

-- Deployments: declared application services with revisions and builds.
CREATE TABLE deployments (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id),
    name TEXT NOT NULL,
    source_kind TEXT NOT NULL,
    source_ref TEXT NOT NULL DEFAULT '',
    build_image TEXT NOT NULL DEFAULT '',
    current_revision INTEGER NOT NULL DEFAULT 0,
    healthy_revision INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL DEFAULT 'created',
    placement TEXT NOT NULL DEFAULT 'auto',
    worker_id TEXT NOT NULL DEFAULT '',
    resources TEXT NOT NULL DEFAULT '{}',
    env TEXT NOT NULL DEFAULT '{}',
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (project_id, name)
);

CREATE TABLE deployment_revisions (
    id TEXT PRIMARY KEY,
    deployment_id TEXT NOT NULL REFERENCES deployments(id),
    revision INTEGER NOT NULL,
    image TEXT NOT NULL,
    image_digest TEXT NOT NULL DEFAULT '',
    source_commit TEXT NOT NULL DEFAULT '',
    workload_id TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'pending',
    note TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE (deployment_id, revision)
);
CREATE INDEX deployment_revisions_deploy ON deployment_revisions (deployment_id, revision);

CREATE TABLE builds (
    id TEXT PRIMARY KEY,
    deployment_id TEXT NOT NULL REFERENCES deployments(id),
    revision INTEGER NOT NULL,
    status TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    commit_sha TEXT NOT NULL DEFAULT '',
    image TEXT NOT NULL DEFAULT '',
    image_digest TEXT NOT NULL DEFAULT '',
    log TEXT NOT NULL DEFAULT '',
    started_at INTEGER NOT NULL,
    finished_at INTEGER,
    created_by TEXT NOT NULL
);
CREATE INDEX builds_deploy ON builds (deployment_id, started_at);

-- Provider-neutral deployment triggers with HMAC validation.
CREATE TABLE deploy_webhooks (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id),
    deployment_id TEXT NOT NULL REFERENCES deployments(id),
    secret TEXT NOT NULL,
    branch_filter TEXT NOT NULL DEFAULT '',
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    deliveries INTEGER NOT NULL DEFAULT 0
);

-- Object storage metadata. Blob bytes live on the storage root, never in SQLite.
CREATE TABLE buckets (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id),
    name TEXT NOT NULL,
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE (project_id, name)
);

CREATE TABLE objects (
    bucket_id TEXT NOT NULL REFERENCES buckets(id),
    key TEXT NOT NULL,
    size_bytes INTEGER NOT NULL,
    checksum TEXT NOT NULL,
    content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
    created_at INTEGER NOT NULL,
    PRIMARY KEY (bucket_id, key)
);

-- Backup metadata for coordinator metadata, volumes and stored objects.
CREATE TABLE backups (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL,
    target TEXT NOT NULL,
    status TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    size_bytes INTEGER NOT NULL DEFAULT 0,
    checksum TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    finished_at INTEGER
);

-- Training jobs and observed metrics. Metrics come only from the process.
CREATE TABLE training_jobs (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id),
    name TEXT NOT NULL,
    workload_id TEXT NOT NULL DEFAULT '',
    dataset_artifact TEXT NOT NULL DEFAULT '',
    model_artifact TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'created',
    reason TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE training_metrics (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    training_id TEXT NOT NULL REFERENCES training_jobs(id),
    step INTEGER NOT NULL,
    metric TEXT NOT NULL,
    value REAL NOT NULL,
    at INTEGER NOT NULL
);
CREATE INDEX training_metrics_job ON training_metrics (training_id, step);

-- Registered models promoted from training or external sources.
CREATE TABLE models (
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
    UNIQUE (project_id, name)
);

-- Inference services and runtime configuration.
CREATE TABLE infer_services (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id),
    name TEXT NOT NULL,
    model_id TEXT NOT NULL,
    runtime TEXT NOT NULL,
    upstream_url TEXT NOT NULL DEFAULT '',
    workload_id TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'created',
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (project_id, name)
);

-- Project resource quotas and durable usage accounting.
CREATE TABLE project_quotas (
    project_id TEXT PRIMARY KEY REFERENCES projects(id),
    max_active_workloads INTEGER NOT NULL DEFAULT 0,
    max_cpu_millis INTEGER NOT NULL DEFAULT 0,
    max_memory_bytes INTEGER NOT NULL DEFAULT 0,
    max_storage_bytes INTEGER NOT NULL DEFAULT 0,
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL
);

CREATE TABLE usage_records (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    workload_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    worker_id TEXT NOT NULL DEFAULT '',
    service TEXT NOT NULL DEFAULT 'run',
    allocated_cpu_millis INTEGER NOT NULL DEFAULT 0,
    allocated_memory_bytes INTEGER NOT NULL DEFAULT 0,
    measured_cpu_pct REAL,
    duration_seconds INTEGER NOT NULL DEFAULT 0,
    started_at INTEGER NOT NULL,
    finished_at INTEGER
);
CREATE INDEX usage_records_project ON usage_records (project_id, started_at);

-- Durable alert rules and their lifecycle.
CREATE TABLE alert_rules (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL,
    target TEXT NOT NULL DEFAULT '',
    threshold INTEGER NOT NULL DEFAULT 0,
    webhook_url TEXT NOT NULL DEFAULT '',
    enabled INTEGER NOT NULL DEFAULT 1,
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE alert_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    rule_id TEXT NOT NULL REFERENCES alert_rules(id),
    state TEXT NOT NULL,
    target TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    at INTEGER NOT NULL,
    resolved_at INTEGER
);
CREATE INDEX alert_events_rule ON alert_events (rule_id, at);

-- Managed database instances.
CREATE TABLE databases (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id),
    name TEXT NOT NULL,
    engine TEXT NOT NULL,
    version TEXT NOT NULL DEFAULT '',
    image TEXT NOT NULL DEFAULT '',
    workload_id TEXT NOT NULL DEFAULT '',
    volume TEXT NOT NULL DEFAULT '',
    sealed_password TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'created',
    reason TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (project_id, name)
);

-- Drop file transfers between authorized principals.
CREATE TABLE transfers (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL DEFAULT '',
    sender_id TEXT NOT NULL,
    recipient_id TEXT NOT NULL,
    filename TEXT NOT NULL,
    size_bytes INTEGER NOT NULL DEFAULT 0,
    checksum TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending',
    reason TEXT NOT NULL DEFAULT '',
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    completed_at INTEGER
);
CREATE INDEX transfers_recipient ON transfers (recipient_id, status);

-- Durable scheduled task definitions and their run history.
CREATE TABLE schedules (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id),
    name TEXT NOT NULL,
    spec TEXT NOT NULL,
    timezone TEXT NOT NULL DEFAULT 'UTC',
    workload TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    missed_policy TEXT NOT NULL DEFAULT 'skip',
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    last_run_at INTEGER,
    next_run_at INTEGER NOT NULL,
    UNIQUE (project_id, name)
);

CREATE TABLE schedule_runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    schedule_id TEXT NOT NULL REFERENCES schedules(id),
    workload_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT '',
    at INTEGER NOT NULL
);
