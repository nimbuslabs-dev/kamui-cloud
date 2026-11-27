# Kamui Cloud — Development Handoff

**Branch:** `feat/mobile-workspace-mvp`
**Last Verified Commit:** `d172c1616c45b24be9dd2c9a05e2c6306ca06756` (HEAD)
**Date:** 2026-11-27

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

## Next Steps (Phase 06-07 — Not Started)

Per instructions, **stop after Phase 05**. Do not independently begin Phase 06-07.

Phase 06 would cover: Mobile workspace UX, Rust agent integration, file sync, terminal UX.
Phase 07 would cover: Multi-host execution, artifact store, GPU passthrough, production hardening.

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