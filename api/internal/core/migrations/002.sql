ALTER TABLE users ADD COLUMN username TEXT;
ALTER TABLE users ADD COLUMN email TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN display_name TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN password_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN is_system_admin INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0;

CREATE UNIQUE INDEX IF NOT EXISTS users_username_uq ON users (lower(username)) WHERE username IS NOT NULL AND username != '';
CREATE UNIQUE INDEX IF NOT EXISTS users_email_uq ON users (lower(email)) WHERE email != '';

UPDATE users
SET username = 'admin',
    display_name = 'Local Administrator',
    is_system_admin = 1
WHERE id = 'local-admin' AND (username IS NULL OR username = '');

ALTER TABLE projects ADD COLUMN name TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN slug TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN description TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN is_personal INTEGER NOT NULL DEFAULT 0;
ALTER TABLE projects ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE projects ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0;

UPDATE projects
SET name = 'Default',
    slug = 'default',
    description = 'Default Kamui Cloud project',
    is_personal = 1
WHERE id = 'default' AND name = '';

CREATE TABLE IF NOT EXISTS system_settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

INSERT OR IGNORE INTO system_settings (key, value, updated_at)
VALUES ('cloud_mode', 'personal', 0),
       ('registration_policy', 'owner_only', 0);

CREATE TABLE IF NOT EXISTS project_members (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'developer', 'viewer')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (project_id, user_id)
);

INSERT OR IGNORE INTO project_members (project_id, user_id, role, created_at, updated_at)
VALUES ('default', 'local-admin', 'owner', 0, 0);

CREATE TABLE IF NOT EXISTS invitations (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    email TEXT NOT NULL DEFAULT '',
    username TEXT NOT NULL DEFAULT '',
    role TEXT NOT NULL CHECK (role IN ('admin', 'developer', 'viewer')),
    token_hash TEXT NOT NULL UNIQUE,
    invited_by TEXT NOT NULL REFERENCES users(id),
    expires_at INTEGER NOT NULL,
    accepted_at INTEGER,
    accepted_by TEXT REFERENCES users(id),
    revoked_at INTEGER,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS invitations_project ON invitations (project_id, created_at DESC);

CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    csrf_token TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL CHECK (kind IN ('browser', 'cli', 'paired_device')),
    device_id TEXT NOT NULL DEFAULT '',
    label TEXT NOT NULL DEFAULT '',
    active_project_id TEXT REFERENCES projects(id) ON DELETE SET NULL,
    expires_at INTEGER NOT NULL,
    revoked_at INTEGER,
    last_seen_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_user ON sessions (user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS github_connections (
    user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    github_user_id INTEGER NOT NULL,
    github_login TEXT NOT NULL,
    token_encrypted TEXT NOT NULL,
    refresh_token_encrypted TEXT NOT NULL DEFAULT '',
    scopes TEXT NOT NULL DEFAULT '',
    expires_at INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS oauth_states (
    state_hash TEXT PRIMARY KEY,
    user_id TEXT NOT NULL DEFAULT '',
    code_verifier TEXT NOT NULL,
    redirect_uri TEXT NOT NULL,
    mode TEXT NOT NULL CHECK (mode IN ('login', 'connect')),
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    at INTEGER NOT NULL,
    actor_user_id TEXT NOT NULL DEFAULT '',
    project_id TEXT NOT NULL DEFAULT '',
    action TEXT NOT NULL,
    target_type TEXT NOT NULL DEFAULT '',
    target_id TEXT NOT NULL DEFAULT '',
    outcome TEXT NOT NULL CHECK (outcome IN ('success', 'denied', 'failure')),
    detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS audit_events_at ON audit_events (at DESC, id DESC);
CREATE INDEX IF NOT EXISTS audit_events_project ON audit_events (project_id, id DESC);

CREATE TABLE IF NOT EXISTS pairing_tokens (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    code_hash TEXT NOT NULL UNIQUE,
    endpoint_url TEXT NOT NULL DEFAULT '',
    device_name TEXT NOT NULL DEFAULT '',
    require_approval INTEGER NOT NULL DEFAULT 0,
    expires_at INTEGER NOT NULL,
    used_at INTEGER,
    revoked_at INTEGER,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS paired_devices (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    platform TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('pending', 'approved', 'revoked')),
    session_id TEXT REFERENCES sessions(id) ON DELETE SET NULL,
    last_endpoint TEXT NOT NULL DEFAULT '',
    last_transport TEXT NOT NULL DEFAULT '',
    approved_by TEXT NOT NULL DEFAULT '',
    approved_at INTEGER,
    revoked_at INTEGER,
    last_seen_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS paired_devices_user ON paired_devices (user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS connectivity_settings (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    remote_enabled INTEGER NOT NULL DEFAULT 0,
    provider TEXT NOT NULL DEFAULT 'none' CHECK (provider IN ('none', 'tailscale', 'tunnel')),
    tailscale_hostname TEXT NOT NULL DEFAULT '',
    tunnel_url TEXT NOT NULL DEFAULT '',
    tunnel_provider TEXT NOT NULL DEFAULT '',
    authorized_by TEXT NOT NULL DEFAULT '',
    authorized_at INTEGER,
    updated_at INTEGER NOT NULL DEFAULT 0
);

INSERT OR IGNORE INTO connectivity_settings (id, remote_enabled, provider, updated_at)
VALUES (1, 0, 'none', 0);
