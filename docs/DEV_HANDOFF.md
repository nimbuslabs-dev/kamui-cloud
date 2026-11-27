# Kamui Cloud — Development Handoff

**Branch:** `feat/mobile-workspace-mvp`
**Last Verified Commit:** see [Remote Commit SHA](#remote-commit-sha)
**Date:** 2026-11-28

---

## Completed Phases

| Phase | Status | Notes |
|-------|--------|-------|
| 00: Repository Reconciliation | ✅ Complete | Baseline established at `d172c16` |
| 01: Unified Go Backend + SQLite | ✅ Complete | Core store, migrations, executor boundaries |
| 02: Single-Host Execution + Rust Discovery | ✅ Complete | Docker executor, kamui-agent capabilities |
| 03: Universal CLI | ✅ Complete | Bubble Tea dashboard, workspace/job/service commands |
| **04: Identity** | ✅ **Complete** | Full backend + CLI integration |
| **05: Connectivity** | ✅ **Complete** | Full backend + CLI integration |
| **06: Distributed Worker Agent** | ✅ **Complete** | Enrollment, worker protocol, real Docker execution |
| **07: Intelligent Scheduler** | ✅ **Complete** | Placement modes, explainable scoring, durable scheduling |

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

## Next Steps (Phase 08–10 — Not Started)

Per instructions, **stop after Phase 07**.

Phase 08 would cover: deployment service on the shared scheduling contracts, artifact handoff.
Phase 09 would cover: object storage integration, GPU passthrough hardening on real NVIDIA hosts.
Phase 10 would cover: production hardening, security audit (after December 21), stress testing.

---

## Remote Commit SHA

```
d172c1616c45b24be9dd2c9a05e2c6306ca06756 (HEAD -> feat/mobile-workspace-mvp, origin/feat/mobile-workspace-mvp)
```

No new commits pushed yet — all changes are staged/unstaged in working tree.

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