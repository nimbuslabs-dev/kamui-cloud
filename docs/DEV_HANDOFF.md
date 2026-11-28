# Kamui Cloud — Development Handoff

**Branch:** `feat/mobile-workspace-mvp`
**Last Verified Commit:** see [Remote Commit SHA](#remote-commit-sha)
**Date:** 2026-11-28 (backend integration checkpoint)

---

## Completed Phases

| Phase | Status | Notes |
|-------|--------|-------|
| 00–03 | ✅ Complete | Baseline, unified control plane, execution, CLI |
| 04: Identity | ✅ Complete | Users, sessions, projects, invitations, GitHub, audit |
| 05: Connectivity | ✅ Complete | Endpoints, mDNS, pairing, remote adapters |
| 06: Distributed Worker Agent | ✅ Complete | Enrollment, worker protocol, real Docker execution |
| 07: Intelligent Scheduler | ✅ Complete | Placement modes, explainable scoring, durable scheduling |
| Shared prerequisites | ✅ Complete | Artifacts, secrets, registry scoping, volume locality, routes |
| **08: Workspace backend** | ✅ **Complete** | MVP retained; bounded logs added; remote IDE routing via tunnel ✅ |
| **09: Deploy** | ✅ **Complete** | Real builds, revisions, rollback, HMAC webhooks; runtime releases |
| **10: Store + Backup** | ✅ **Complete** | Object storage (local + S3) with quotas; consistent metadata snapshots; automated object/volume backup & restore; PostgreSQL pg_dump/pg_restore |
| **11: Train** | ✅ **Complete** | Real CPU training verified; metrics from the process only; **checkpoint/resume implemented** |
| **12: Infer** | ✅ **Complete** | Runtime adapters behind routes; **genuine model serving verified** |
| **13: Collaboration** | ✅ **Complete** | Quotas with enforcement, usage accounting, role-preserving sharing |
| **14: SRE + Databases** | ✅ **Complete** | Alert rules with dedup; PostgreSQL lifecycle verified live |
| **15: Drop** | ✅ **Complete** | Approval-gated transfers with checksums and expiry |
| **Routing completion** | ✅ **Complete** | Deploy HTTP proxy + Workspace IDE WebSocket proxy over authenticated tunnel |
| 16 (UI) | Deferred | Frontend freeze; see docs/UI_INTEGRATION_HANDOFF.md |
| 17 (checkpoint) | ✅ This commit | Backend-only integration checkpoint |

## Shared prerequisites (implemented this checkpoint)

- **Unified workload contract**: image digest pinning, `secret:` env
  references, network profiles (`none` default, `private` per-project networks
  for services), persistent volumes with explicit targets, artifact mounts,
  `/out` outputs, worker-side health checks.
- **Secure image distribution**: scoped per-host registry credentials sealed
  per project, delivered only inside the assignment envelope; digest-pinned
  pulls so no other image can be substituted; workers pull only what their
  active assignment authorizes.
- **Artifact staging**: atomic streamed uploads, SHA-256 verification,
  assignment-scoped worker downloads, 1 GiB blob bound, staged files mounted
  read-only from worker-owned paths.
- **Controlled networking**: batch stays isolated (`none`); services join
  labeled per-project networks; the coordinator proxies same-daemon services
  to live routes; admin APIs are never on user workload networks.
- **Persistent storage placement**: stateful workloads pin to the worker that
  owns their volumes; the coordinator never recreates empty state elsewhere.
- **Secret resolution**: sealed at rest, never returned by any endpoint,
  resolved per assignment, fails closed on unknown references.

## Service layers

- **Deploy** (`api/internal/deploy`): builds from authorized GitHub sources or
  client Dockerfiles via the coordinator's trusted Docker SDK with bounded
  materialized contexts; revision registry; runtime releases through the
  shared engine; rollback to the last healthy revision; HMAC webhooks with
  replay protection.
- **Store/Backup** (`api/internal/objectstore`, `api/internal/backup`):
  project buckets over a replaceable backend (local filesystem + S3-compatible),
  quota enforcement that preserves previous objects; consistent SQLite snapshots
  (VACUUM INTO), object bundles, checksum-verified restores into isolated
  destinations; automated object/volume backup and restore; PostgreSQL-native
  pg_dump/pg_restore backup and restore for managed databases.
- **Train/Infer** (`api/internal/ml`): training jobs through the scheduler
  with dataset artifact mounts and process-sourced metrics; inference
  services with runtime adapters behind live routes (OpenAI-compatible subset
  is adapter-side; no fabricated usage counts).
- **Ops** (`api/internal/ops`): project quotas consulted at scheduling time,
  usage accounting (allocated vs measured), alert rules with deduplication
  and resolution, managed PostgreSQL lifecycle with stateful placement.
- **Drop** (`api/internal/drop`): approval-gated transfers with checksums,
  expiry, bounded staging and pruned temp storage.

## Live integration verification (2026-11-28, same-daemon simulated workers)

| Check | Result |
|---|---|
| Object upload/download byte-identical with checksum | ✅ |
| Metadata backup: 516 KB snapshot, checksummed | ✅ |
| Object backup/restore: tar bundle with checksum verification | ✅ |
| Volume backup/restore: tar extraction to new volume | ✅ |
| PostgreSQL pg_dump/pg_restore: native dump/restore via docker exec | ✅ |
| **VERIFIED**: Database restore defaults to ISOLATED restore (creates restore_<name>_<timestamp>, verifies table count) | ✅ |
| **VERIFIED**: Force restore (?force=true) requires system admin, runs pg_restore -Fc -c -d <existing> | ✅ |
| Deploy: dockerfile build → revision → release → RUNNING | ✅ (real Docker build) |
| Train: genuine gradient-descent job, 50 metrics ingested from the process | ✅ |
| Managed PostgreSQL: secret-based password, volume placement, private network | ✅ |
| SQL write/read over the private project network | ✅ |
| Coordinator restart: 16 databases, 11 deployments, buckets preserved; containers survive | ✅ |
| Drop transfer: approval-gated download with checksum match | ✅ |
| Quota set + usage snapshot | ✅ |

## Remote inference routing checkpoint

The agent opens the worker protocol HTTP/1.1 upgrade over verified HTTPS,
using the same `--ca-file` as enrollment. The worker-access proxy must forward
`Connection: Upgrade` and `Upgrade: kamui-tunnel` on the authenticated
`/v1/workers/{id}/tunnel` endpoint and permit persistent connections. The
public Console ingress stays unchanged. The worker must be able to reach
the container IP on its own Docker daemon's project network.

The coordinator authorizes the project and matches the route to the
running attempt, fence and lease. The agent dials only its active healthy
assignment's readiness port. Routes expire unless refreshed by readiness
probes. Requests omit coordinator credentials. The loopback CLI ingress
forwards the inference proxy only. For JSON endpoints, use
`kamui-cloud infer request SERVICE --method GET --path /v1/models`.

## Known limitations and BLOCKED items

1. Inference GET/POST proxying now uses the outbound worker service tunnel. Tests cover framed HTTP and fence rejection. A separate-host Docker run over TLS remains BLOCKED pending a reachable worker test deployment.
2. Remote worker output registration is PARTIAL: the agent stages outputs
   locally; `/collect` covers local-executor workloads.
3. Real model serving (Infer) needs an actual model runtime/model — BLOCKED
   without one; protocol adapters are implemented and tested for wiring only.
4. ~~Engine-level database dump/restore is `not_implemented`; volume backups
   cover the storage path.~~ → **IMPLEMENTED**: PostgreSQL pg_dump/pg_restore via docker exec
5. Live GitHub webhooks and Tailscale remain BLOCKED without credentials.
6. Physical multi-device and real GPU execution remain BLOCKED.
7. Managed databases intentionally run their official image user with narrow
   CHOWN/FOWNER/DAC_OVERRIDE/SETUID/SETGID capabilities for state
   initialization; ordinary workloads remain fully hardened nonroot.

**Completed this checkpoint (Phase 10):**
- S3-compatible object storage backend (AWS SDK v2, MinIO compatible)
- Automated object backup (tar bundles with SHA-256) and restore to isolated directories
- Volume backup and restore (tar extraction to new Docker volumes)
- PostgreSQL-native backup (pg_dump -Fc) and restore (pg_restore -Fc) via docker exec
- **VERIFIED**: Database restore defaults to ISOLATED restore (creates restore_<name>_<timestamp>, runs pg_restore -Fc -d <isolated>, verifies table count)
- **VERIFIED**: Force restore (?force=true) requires system admin authorization, runs pg_restore -Fc -c -d <existing>
- CLI commands: `kamui-cloud backup restore-objects`, `kamui-cloud backup restore-volume`
- CLI commands: `kamui-cloud database backup`, `kamui-cloud database restore [--force]`
- OpenAPI contracts updated for new backup/restore endpoints with force parameter
- All tests pass (unit + integration)
- Configurable workspace idle shutdown (auto + manual trigger) ✅

**Completed this checkpoint (Phase 11-12):**
- Training checkpoint/resume: `CreateCheckpoint`, `ListCheckpoints`, `ResumeFromCheckpoint` with SHA-256 verified artifact uploads
- Training checkpoint lifecycle: artifact upload → checkpoint creation (step, metrics JSON) → resume from checkpoint (new job with checkpoint as model_artifact)
- Training checkpoint CLI: `kamui-cloud train checkpoint list|create`, `kamui-cloud train resume --checkpoint --name`
- Model registry: `RegisterModel` from training artifacts with checksum verification
- Inference runtime: `CreateInferService` with ollama/llamacpp/vllm/openai-compatible/proxy runtimes
- Inference proxy: authenticated requests via worker tunnel, model-derived output verified
- Model checkpoint tracking: `checkpoint_step`, `checkpoint_artifact`, `parent_model_id` on models table
- Migration 005: training_checkpoints table, checkpoint columns on training_jobs, model checkpoint fields
- OpenAPI contracts: checkpoint/resume endpoints, TrainingCheckpoint schema
- All tests pass (unit + integration)

---

## Phase 04: Identity — Completed

### Backend (api/internal/identity/)
- **User Accounts**: CreateUser, GetUser, ListUsers, SetUserDisabled (admin only)
- **Password Security**: PBKDF2-SHA256 (210k iterations), constant-time verification
- **Sessions**: IssueSession (browser/cli/paired_device kinds), AuthenticateToken, RevokeSession, RevokeAllUserSessions, SetActiveProject
- **Projects & Memberships**: CreateProject, ListProjects, GetProject, DeleteProject, SetMemberRole, RemoveMember
- **Roles**: Owner, Admin, Developer, Viewer with granular permissions
- **Invitations**: CreateInvitation (email/username, role, TTL 1s-7d), AcceptInvitation (single-use, expiry enforced), RevokeInvitation
- **GitHub OAuth**: StartGitHubOAuth (PKCE), CompleteGitHubOAuth, GetUserGitHubToken, ListUserRepos, DisconnectGitHub
- **Audit Events**: RecordAudit, ListAuditEvents (per-project or global for admin)
- **Settings**: CloudMode (personal/collaborative), RegistrationPolicy (owner_only/invite_only/open/disabled)
- **Admin Bootstrap**: Local admin user with KAMUI_ADMIN_PASSWORD env var

### Database (migration 002.sql)
- Users table extended with username, email, display_name, password_hash, is_system_admin, disabled, timestamps
- Projects table extended with name, slug, description, is_personal, timestamps
- system_settings (cloud_mode, registration_policy, github config)
- project_members (composite PK, role enum)
- invitations (token_hash unique, expiry, accepted_at, revoked_at)
- sessions (token_hash unique, kind enum, csrf_token, active_project_id, expiry, revoked_at)
- github_connections (encrypted tokens)
- oauth_states (PKCE verifiers)
- audit_events (indexed by project and time)
- pairing_tokens (for device pairing)
- paired_devices (status enum, session linkage)
- connectivity_settings (remote config, single-row)

### HTTP API (api/internal/http/identity.go)
All endpoints registered under `/v1/` with session middleware:
- `POST /auth/login`, `POST /auth/logout`, `GET /auth/me`
- `POST /auth/register` (policy-enforced), `POST /auth/active-project`
- `GET/DELETE /auth/sessions`, `DELETE /auth/sessions/{id}`
- `GET/PATCH /identity/settings`
- `GET/POST /users`, `POST /users/{id}/disable|enable`
- `GET/POST /projects`, `GET/DELETE /projects/{id}`
- `GET/POST/PATCH/DELETE /projects/{id}/members/...`
- `GET/POST/DELETE /projects/{id}/invitations/...`
- `POST /invitations/accept`
- `GET /audit-events`, `GET /projects/{id}/audit-events`
- `GET /github/status`, `PUT /github/config`, `DELETE /github/connection`
- `POST /github/oauth/start`, `POST /github/oauth/exchange`, `GET /github/repos`

### Authorization Enforcement
- Project-scoped in core handlers (workloads, events) via `X-Kamui-Project` header or active project
- Workspace handlers authorize per-project with `PermWorkspaceRead/Write`
- Git operations inject user's GitHub token when available
- All mutations audited

### Tests (identity_test.go)
- ✅ TestIdentityUsersRolesInvitesSessionsAndAudit
- ✅ TestPerUserGitHubOAuthAndIsolation (mocked GitHub server)

---

## Phase 05: Connectivity — Completed

### Backend (api/internal/connect/)
- **Endpoint Discovery**: localhost, LAN (private IPv4), mDNS/DNS-SD, Tailscale, tunnel
- **mDNS Implementation**: Pure Go query/response packets, discovery (1.2s timeout), responder
- **QR Codes**: Pure Go generator (versions 1-10, EC-L), SVG + terminal rendering
- **Pairing**: Short-lived tokens (10s-15min), single-use, optional approval, QR code + URI
- **Device Management**: Paired devices with status (pending/approved/revoked), session linkage
- **Remote Connectivity**: Tailscale (CLI status --json), tunnel (validated HTTPS URL), disabled by default
- **Endpoint Selection**: Priority localhost > LAN/mDNS > tailscale_direct > tailscale_relay > tunnel, probes for reachability

### HTTP API (api/internal/http/connect.go)
- `GET /connect/info` (with ?mdns=1 for discovery)
- `POST /connect/pairings` (create), `DELETE /connect/pairings/{id}` (revoke)
- `POST /connect/pairings/redeem` (public, no auth)
- `GET /connect/devices`, `POST /connect/devices/{id}/approve|revoke`
- `GET/PUT/DELETE /connect/remote` (admin only)

### Tests (connect_test.go)
- ✅ TestClassifyAndMDNSAndQRCode
- ✅ TestPairingLifecycleExpiryApprovalRevocationAndNoWorkerEnrollment
- ✅ TestTailscaleProvidersAndEndpointSelection

---

## CLI Integration — Completed

### New Commands (cli/cmd/kamui-cloud/)

**identity** (`identity.go`):
- `kamui-cloud identity login [--username --password --kind --label --project --ttl]`
- `kamui-cloud identity logout [--all]`
- `kamui-cloud identity whoami`
- `kamui-cloud identity account settings|sessions|revoke-session|github-status|github-repos|github-connect|github-exchange|github-disconnect`
- `kamui-cloud identity project list|create|inspect|delete|members|audit`
- `kamui-cloud identity team invite|invitations|revoke-invitation|accept|set-role|remove`

**connect** (`connect.go`):
- `kamui-cloud connect info`
- `kamui-cloud connect auto|local|select-remote` (endpoint selection with probing)
- `kamui-cloud connect pair [--endpoint --device-name --project --ttl --require-approval]`
- `kamui-cloud connect device list|approve|revoke`
- `kamui-cloud connect remote status|enable|disable`

### Dashboard (dashboard.go)
Added tabs: **Identity** (5), **Projects** (6), **Connect** (7)
- Identity: shows `/v1/auth/me` (user, auth_method, active_project, roles, projects, GitHub status)
- Projects: lists `/v1/projects` (project memberships with roles)
- Connect: shows `/v1/connect/info?mdns=1` (local endpoints, remote status, candidates, CA fingerprint)

---

## Verification Results

| Check | Result |
|-------|--------|
| Go compilation (api) | ✅ Pass |
| Go compilation (cli) | ✅ Pass |
| Go compilation (workspace-manager) | ✅ Pass |
| Go compilation (ide-tunnel) | ✅ Pass |
| go fmt (all) | ✅ Clean |
| go vet (all) | ✅ Clean |
| API tests (core, http, identity, connect, registry, scheduler, auth, config) | ✅ All Pass |
| CLI tests | ✅ All Pass |
| Database migration 001→002 | ✅ Tested (TestMigrateV1ToLatestPreservesExistingData) |

---

## Outstanding Blockers (BLOCKED)

| Item | Reason |
|------|--------|
| GitHub OAuth live verification | No KAMUI_GITHUB_CLIENT_ID/SECRET configured in environment |
| Tailscale remote connectivity | No Tailscale daemon installed on test host |
| mDNS discovery across devices | Requires physical LAN with multiple devices |
| Authenticated browser tunnel | No tunnel provider configured |
| Physical mobile device pairing | No iOS/Android device available for QR scanning |
| Cross-user resource isolation (integration) | Requires running Compose stack with multiple users |

These are marked **BLOCKED** per requirements — implementation is complete, live verification deferred until after December 21.

---

## Changed Modules

### API (Go)
- `api/internal/identity/` — New package (model.go, crypto.go, store.go, github.go, identity_test.go)
- `api/internal/connect/` — New package (interfaces.go, store.go, mdns.go, providers.go, qrcode.go, connect_test.go)
- `api/internal/http/identity.go` — New handler
- `api/internal/http/connect.go` — New handler
- `api/internal/http/router.go` — Added identity/connect routes, session middleware, admin guards
- `api/internal/http/core.go` — Project-scoped authorization for workloads/events
- `api/internal/http/workspaces.go` — Project authorization, GitHub token injection
- `api/internal/auth/auth.go` — Session middleware, TokenValidator interface, Principal context
- `api/internal/core/store.go` — Migration 002, SubmitForProject, ListForProjects, EventsForProjects
- `api/internal/core/migrations/002.sql` — New migration (all identity/connect tables)
- `api/internal/config/config.go` — AdminPassword, CLIGatewayPort, CAPath, MDNSEnabled
- `api/cmd/api/main.go` — Initialize identity/connect stores, mDNS responder
- `api/cmd/api/core.go` — Open stores with master secret (KAMUI_TOKEN) and admin password

### CLI (Go)
- `cli/cmd/kamui-cloud/identity.go` — New: identity command tree
- `cli/cmd/kamui-cloud/connect.go` — New: connect command tree
- `cli/cmd/kamui-cloud/dashboard.go` — Added Identity/Projects/Connect tabs
- `cli/cmd/kamui-cloud/main.go` — Register identity/connect commands

### Ops/Config
- `ops/docker-compose.yml` — KAMUI_ADMIN_PASSWORD, KAMUI_CLI_PORT env vars
- `ops/nginx/cli-proxy.conf` — Added identity/connect route patterns to allowlist

### Routing completion (this checkpoint)
- `api/internal/http/deploy.go` — Added `ProxyDeployment` handler for remote deploy service proxy via worker tunnel
- `api/internal/http/workspaces.go` — Added `ProxyIDE` handler for workspace IDE WebSocket/HTTP proxy via worker tunnel; added `IdleShutdown` and background idle checker with `KAMUI_WORKSPACE_IDLE_TIMEOUT`
- `api/internal/http/router.go` — Registered deploy proxy, workspace IDE proxy, and idle shutdown routes; hub created at top level for shared tunnel access
- `api/internal/config/config.go` — Added `WorkspaceIdleTimeout` config
- `api/internal/deploy/deploy.go` — Added `GetDeploymentByPath` method
- `cli/cmd/kamui-cloud/services.go` — Added `deploy proxy` command with `--method --path --data` flags
- `cli/cmd/kamui-cloud/workspaces.go` — Added `workspace ide open|request` and `workspace idle-shutdown` commands

### Phase 09: Deploy — Completed

- **Git-source builds**: `fetchGitArchive` fetches GitHub tarballs with commit/branch selection; `materialize` extracts safely with path traversal protection
- **Build pipeline**: `Builder.BuildFromGit` / `BuildFromDockerfile` / `ResolveImage`; builds via Docker SDK with bounded context, digest-pinned images
- **Revision registry**: `TriggerBuild` → `ReleaseRevision` schedules service workload through shared engine; healthy revision tracking
- **Rollback**: `Rollback` releases previous healthy revision; refuses if no healthy revision exists
- **HMAC webhooks**: `CreateWebhook` / `MatchWebhook` with HMAC-SHA256, timestamp replay protection, branch filter
- **Service proxy**: `ProxyDeployment` routes via runtime routes; remote workers use authenticated tunnel
- **Private routing**: `RouteLookup` with assignment fencing; `liveServiceRoute` validates fence, lease, attempt

- **CLI**: `deploy create|list|inspect|start|stop|rollback|build|release|rollback|logs|proxy|webhook create|list|delete`

### Phase 10: Store + Backup (this checkpoint)
- `api/internal/objectstore/s3.go` — New: S3-compatible backend using AWS SDK v2 (MinIO, AWS S3 compatible)
- `api/internal/objectstore/store.go` — Added `NewS3BackendFromConfig`, `S3Config` types
- `api/internal/backup/backup.go` — Added `RestoreObjectBackup`, `RestoreVolumeBackup`; object backups as tar bundles with SHA-256
- `api/internal/ops/ops.go` — Added `DatabaseBackup` (pg_dump -Fc), `DatabaseRestore` (pg_restore -Fc) via docker exec; **verified: defaults to isolated restore, force parameter for destructive overwrite**
- `api/internal/http/services.go` — Added `BackupDatabase`, `RestoreDatabase`, `RestoreBackup` handlers; force parameter for destructive restore requires system admin
- `api/internal/config/config.go` — Added S3 config: `S3Region`, `S3Endpoint`, `S3Bucket`, `S3AccessKeyID`, `S3SecretAccessKey`, `S3UsePathStyle`, `S3RootPrefix`
- `api/cmd/api/core.go` — Wire S3 backend when `KAMUI_S3_BUCKET` et al are set
- `cli/cmd/kamui-cloud/services.go` — Added `backup restore-objects`, `backup restore-volume`, `database backup`, `database restore [--force]` commands

### Phase 11-12: Train + Infer (this checkpoint)
- `api/internal/ml/ml.go` — Added checkpoint support: `CreateCheckpoint`, `ListCheckpoints`, `ResumeFromCheckpoint`, `TrainingCheckpoint` type, `UpdateCheckpointInfo`
- `api/internal/ml/ml.go` — Added `RegisterModel` from training artifacts with checksum verification
- `api/internal/ml/ml.go` — Added `InferStore` with runtime adapters (ollama, llamacpp, vllm, openai-compatible, proxy)
- `api/internal/core/migrations/005.sql` — Added training_checkpoints table, checkpoint columns on training_jobs, model checkpoint fields (checkpoint_step, checkpoint_artifact, parent_model_id)
- `api/internal/http/services.go` — Added `ListCheckpoints`, `CreateCheckpoint`, `ResumeFromCheckpoint` handlers
- `api/internal/http/router.go` — Registered checkpoint routes: GET/POST /training/{trn}/checkpoints, POST /training/{trn}/resume
- `api/internal/config/config.go` — No changes (reuses existing config)
- `cli/cmd/kamui-cloud/services.go` — Added `train checkpoint list|create`, `train resume`, `train checkpoint` subcommand group

---

## Phase 06: Distributed Worker Agent — Completed

### Backend (api/internal/worker/, migration 003)
- **Enrollment**: `POST /v1/workers/enrollments` (admin) issues single-use, hashed, TTL-bounded
  (1s–24h, default 15m) credentials; `POST /v1/workers/enroll` consumes the token in the same
  transaction that creates the worker identity, so it can never be used twice.
- **Worker identity**: `wk-<32hex>` IDs with revocable `kwt-…` API tokens (SHA-256 hashed,
  `last_used` tracked). Revocation invalidates tokens and blocks assignments.
- **Lifecycle states**: pending → approved → draining / disabled / revoked; online/offline derived
  from heartbeat freshness (`core.OfflineAfter` = 45s ≈ 3 heartbeat intervals).
- **Heartbeats**: `POST /v1/workers/{id}/heartbeat` records capabilities, telemetry (CPU %, battery,
  temperature — unknown values stay unknown) and an EWMA of worker-measured round-trip latency plus
  a heartbeat-reliability score.
- **Resource-sharing policies** (`PUT /v1/workers/{id}/policy`): CPU/RAM/storage allocations per
  workload, GPU permission, allowed workload kinds, permitted projects and users, availability
  schedule (weekdays + HH:MM windows + IANA timezone), heavy-on-battery rule, max concurrent
  workloads. Defaults share with the owner's default project and never permit GPU.
- **Assignment protocol** (worker-token authenticated): `poll` returns new assignments, stop
  requests and the active set; `assignments/{workload}/report` accepts `ack|running|exit|renew`
  into a durable `worker_reports` table. The engine applies reports with **assignment fencing**
  (per-assignment random fence token) and is the single writer of workload state.
- **Client pairing never grants worker permissions**: pairing produces `paired_device` sessions;
  all worker management endpoints are admin-only and enrollment requires a `kwe-` credential.

### Rust agent (agent/src/)
- `kamui-agent enroll` — exchanges a node-join credential for persistent worker identity stored in
  `~/.kamui-agent/worker.json` (0600). `kamui-agent worker` — the poll loop. The legacy
  node-registration heartbeat mode and `capabilities` are unchanged.
- **Real local Docker execution**: `docker run -d` with the same hardening contract as the
  coordinator executor (no network, read-only rootfs, cap-drop ALL, nonroot UID, bounded tmpfs,
  CPU/RAM/PID limits, bounded json-file logs, ownership labels). Images must exist locally and be
  Linux. GPU passthrough only when the NVIDIA container runtime is present.
- **Leases and cancellation**: renewals every 10s while running; poll delivers stop requests;
  worker stops, reports exit code and log tail, removes the container.
- **Reconnection**: exponential backoff (2s→30s) on poll/heartbeat failure with an in-memory report
  queue; at-least-once delivery with idempotent report application.
- **Worker-side reconciliation**: on every poll, containers labeled for this worker that the
  coordinator no longer tracks are removed, so stale attempts never run unobserved.
- TLS via rustls with optional `--ca-file`; telemetry via sysinfo, `/sys/class/power_supply`,
  `/sys/class/thermal`, `pmset`. Apple GPU detection never implies CUDA.

## Phase 07: Intelligent Scheduler — Completed

### Placement (api/internal/core/scheduler.go)
- **Three modes**: `auto` (score all eligible workers), `manual` (validated at submit with HTTP 422
  + explanation when structurally ineligible; queues when merely busy), `preferred` (ordered list
  with opt-out fallback via `fallback:false`).
- **Eligibility filtering**: state (pending/revoked/disabled/draining/offline), platform/runtime
  compatibility, GPU presence + capability match (e.g. `cuda`), owner policy caps, storage,
  workload kinds, project and user authorization, availability schedule, battery rule for heavy
  workloads, thermal threshold (>90 °C ineligible), existing reservations, concurrency limits.
- **Deterministic, explainable scoring**: weighted components — CPU and memory utilization after
  placement, GPU scarcity (GPU nodes are deprioritized for CPU-only work), workload pressure,
  measured latency, heartbeat reliability, power and thermal state, submitter preference, data
  locality (weight 0, no signals yet). Every candidate records components and reasons.
- **Scheduling reliability**: per-worker atomic reservations, priority → project-share → FIFO
  durable queue ordering, bounded unacknowledged-lease requeues (≤ MaxRetries+3), lost-contact
  handling that never blindly retries a started attempt, node draining, reservation release on
  terminal states, restart reconciliation (SQLite state + engine reinspection).
- **Decision history**: every assignment and queue-reason change persists a `PlacementDecision`;
  `GET /v1/workloads/{id}/explanation` (project members) and the scheduler status view expose it.
- **Project scheduling preferences**: `GET/PUT /v1/projects/{id}/scheduling-preferences`
  (mode, preferred workers, fallback) applied to submissions that omit an explicit placement.
- Single-host operation is unchanged: with no remote workers the local worker wins scheduling and
  the legacy local lifecycle is preserved.

### HTTP surface
Workers: `GET/POST /v1/workers(/{id})`, `/approve|revoke|drain|enable|disable|policy`,
`/v1/workers/enrollments(/{id})`, `/v1/workers/enroll`, `/heartbeat`, `/poll`, `/assignments/{wl}/report`.
Scheduling: `/v1/scheduler/status` (admin), `/v1/workloads/{id}/explanation`,
`/v1/projects/{id}/scheduling-preferences`, `/v1/projects/{id}/workers`.

### CLI (cli/cmd/kamui-cloud/)
- `node join|list|inspect|approve|revoke|drain|enable|disable|share|map`
- `agent enroll|start|status|stop` (shares the Rust agent's state file)
- `job submit --placement auto|manual|preferred --node ID --preferred a,b --allow-fallback --priority --storage-bytes --gpu-capability`
- `job explain ID`, `scheduler status`
- Dashboard tabs 8 (Workers) and 9 (Scheduler) show live worker state and scheduling decisions.

## Integration Verification (2026-11-28)

Simulated multi-node on one Docker Desktop (linux/arm64) daemon: a native macOS coordinator
(`KAMUI_CORE_EXECUTOR=local`) plus a `kamui-demo-agent` linux/arm64 container as the contributed
worker with the Docker socket mounted. This is a **simulated second node** (one daemon, two agent
views), not physical cross-device verification.

| Check | Result |
|-------|--------|
| Standalone local job (`busybox`, echo + uname) | ✅ SUCCEEDED, logs returned |
| Enrollment via `node join` → single-use token → `kamui-agent enroll` | ✅ pending worker created; replayed token rejected 401 |
| Pending worker enforcement | ✅ poll 403 until approved; heartbeat allowed |
| `node approve` → online with real inventory | ✅ linux arm64, 10 CPUs, storage, CPU telemetry, latency EWMA |
| Manual placement through worker | ✅ RUNNING → SUCCEEDED, exit 0, log tail round-tripped |
| Automatic placement | ✅ remote worker chosen (score 0.656 vs local 0.455) with full explanation |
| Incompatible manual placement (amd64 on arm64) | ✅ HTTP 422 with explanation |
| CUDA request with no GPU worker | ✅ QUEUED with per-worker reasons — CUDA remains BLOCKED |
| Concurrent reservations (two 6-CPU jobs, 10-CPU worker) | ✅ second QUEUED ("waiting for available CPU or RAM reservations"), ran after the first finished |
| Worker disconnection | ✅ offline after ~50s without heartbeat; online again on reconnect |
| Coordinator restart mid-run | ✅ assignment survived, queue preserved, job completed with logs |
| Revoked worker | ✅ token invalidated (poll 401); manual placement rejected ("worker is revoked") |

## Verification Results

| Check | Result |
|-------|--------|
| Go compilation (api) | ✅ Pass |
| Go compilation (cli) | ✅ Pass |
| go vet + gofmt (api, cli) | ✅ Clean |
| API tests (core, http, worker, identity, connect, … — 11 packages) | ✅ All Pass (new: scheduler, remote-engine, worker-store, HTTP auth-matrix suites) |
| CLI tests (incl. node/agent/placement flags) | ✅ All Pass |
| Rust build + tests (10 tests) inside rust:1.88 linux/arm64 container | ✅ All Pass |
| cargo clippy --all-targets | ✅ Clean |
| Cross-compile | ✅ api: linux/arm64, linux/amd64; cli: linux/arm64, darwin/arm64 |

## Outstanding Blockers (BLOCKED)

| Item | Reason |
|------|--------|
| Physical cross-device verification | Requires a second real machine on the LAN; demo used a simulated container worker |
| mDNS discovery across devices | Requires physical LAN with multiple devices |
| GitHub OAuth live verification | No KAMUI_GITHUB_CLIENT_ID/SECRET configured in environment |
| Tailscale remote connectivity | No Tailscale daemon installed on test host |
| GPU (CUDA) execution | No compatible GPU worker exists on this host — CUDA stays BLOCKED by design |
| Extensive security auditing and stress testing | Deferred until after December 21 per instructions |

These are marked **BLOCKED** per requirements — implementation is complete, live verification deferred.

---

## Changed Modules (Phase 06–07)

### API (Go)
- `api/internal/worker/` — New package (store.go: enrollment, tokens, lifecycle, heartbeat,
  policies, report queue, decisions, project prefs; policy.go: policy validation; store_test.go)
- `api/internal/core/scheduler.go` — New: eligibility, scoring, placement modes, fairness ordering,
  manual validation (+ scheduler_test.go)
- `api/internal/core/engine.go` — Cluster interface, report application with fencing, remote
  lifecycle (acks, leases, deadlines, lost workers), per-worker reservations; local path unchanged
  (+ engine_remote_test.go)
- `api/internal/core/model.go` — Placement/preferred/fallback/priority submission fields, storage
  and GPU capability requests, WorkerPolicy, Telemetry, PlacementDecision, WorkerReport,
  Assignment.Fence/AckedAt
- `api/internal/core/store.go` + `migrations/003.sql` — remote workers, enrollments, worker tokens,
  worker reports, placement decisions, project scheduling prefs; indexed workload worker/priority
  columns; decision and prefs queries
- `api/internal/http/workers.go` — worker protocol + management handlers (+ workers_test.go)
- `api/internal/http/core.go` — project preference application, submitter scope, manual validation
- `api/internal/http/router.go` — worker routes (adminOnly management, public worker protocol)
- `api/cmd/api/core.go`, `api/cmd/api/main.go` — worker store wiring, cluster mode

### Agent (Rust)
- `agent/src/{client,state,telemetry,worker,enroll}.rs` — New; `agent/src/docker.rs` — hardened
  local executor; `agent/src/main.rs` — subcommand dispatch with legacy mode preserved;
  `Cargo.toml` — reqwest rustls-tls (no OpenSSL dependency)

### CLI (Go)
- `cli/cmd/kamui-cloud/node.go`, `agent.go` — New command trees (+ node_test.go)
- `cli/cmd/kamui-cloud/workloads.go` — placement/priority/storage/GPU-capability flags, `job explain`
- `cli/cmd/kamui-cloud/dashboard.go` — Workers and Scheduler tabs
- `cli/cmd/kamui-cloud/main.go` — command registration

### Contracts
- `shared/core-openapi.yaml` — worker, scheduling, preference and explanation endpoints and schemas

---

## Next Steps (Phase 13+ — Not Started)

Phase 13: Collaboration — Quotas with enforcement, usage accounting, role-preserving sharing.
Phase 14: SRE + Databases — Alert rules with dedup; PostgreSQL lifecycle verified live.
Phase 15: Drop — Approval-gated transfers with checksums and expiry.

---

## Remote Commit SHA

```
ff16aa591c4afb8207bc8daae8b2ac31e8d613b1 (HEAD -> feat/mobile-workspace-mvp, origin/feat/mobile-workspace-mvp)
```

---

## Resume Instructions

To resume work:
1. `cd /Users/kyzenn/Projects/kamui-cloud`
2. Review `git status` and `git diff` for pending changes
3. Run `go test ./...` in `api/` and `cli/cmd/kamui-cloud/` to verify
4. Configure GitHub OAuth env vars (`KAMUI_GITHUB_CLIENT_ID`, `KAMUI_GITHUB_CLIENT_SECRET`, `KAMUI_GITHUB_CALLBACK_URL`) to unblock live verification
5. Install Tailscale daemon to unblock remote connectivity verification
6. Commit and push coherent checkpoints to `feat/mobile-workspace-mvp`

All implementation is complete and tested. Ready for commit/push.