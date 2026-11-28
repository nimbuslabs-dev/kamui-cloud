# Kamui Cloud — Frontend Integration Handoff (Phase 16)

**Audience:** the UI agent. Backend-only checkpoint `df82f4d` on
`feat/mobile-workspace-mvp`. This document maps every implemented service to
its endpoints, permissions and lifecycle states. Contracts live in
`shared/core-openapi.yaml` and `shared/services-openapi.yaml`; use those as
the schema source of truth — this file covers discovery, states and gaps.

## Discovery: what is available

- `GET /healthz` — coordinator reachability.
- `GET /v1/auth/me` — session, user, projects, active project.
- `GET /v1/workers` (admin) or `GET /v1/projects/{id}/workers` (member) —
  real workers with `state` (pending/online/offline/draining/disabled/revoked),
  capabilities and telemetry. Never fabricate GPU fields; `capabilities.gpus`
  carries exactly what the worker reported.
- `GET /v1/scheduler/status` (admin) — queue depth, per-project queue, worker
  states, live assignments and the last 20 placement decisions.

## Services and endpoints

| Service | Endpoints | Permissions | Notes |
|---|---|---|---|
| Workloads (Run) | `/v1/workloads`, `/v1/workloads/{id}` (+cancel, logs, explanation) | workload:read / workload:write per project | Submission supports placement, priority, volumes, artifacts, network profile, health checks |
| Workers | `node join/list/inspect/approve/revoke/drain/enable/share` CLI; REST under `/v1/workers` | admin | Enrollment token shown once |
| Deploy | `/v1/projects/{id}/deployments` + builds, revisions, release, rollback, logs, webhooks | workload:read/write | `GET {dep}` returns `{deployment, revisions, builds, runtime}` — runtime is the live workload snapshot |
| Store | `/v1/projects/{id}/buckets`, `.../objects/{key}` (PUT raw bytes, GET raw bytes, `?metadata=true`) | workload:read/write | Downloads are raw bytes with `X-Checksum-Sha256`; use streaming fetch, not JSON |
| Backup | `/v1/backups*`, project object backups, restore | Admin for metadata; project read or write permissions otherwise | Use `project_id` for project records. Restore returns a server-generated export path relative to storage. Object import and managed volume restore remain unavailable. |
| Train | `/v1/projects/{id}/training`, `/collect`, `/metrics`; `/models` | workload:read/write | Metrics come only from the training process; empty until `POST /collect` |
| Infer | `/v1/projects/{id}/inference`, `/stop`, `/proxy/*` | workload:read/write | Authenticated GET/POST via the current worker tunnel; separate-host acceptance pending |
| Quotas/SRE | `/v1/projects/{id}/quotas` (PUT needs project:update), `/usage`, `/alerts`, `/v1/alerts/{alr}/events` | project:update for writes | Usage distinguishes allocated vs measured; measured may be null |
| Databases | `/v1/projects/{id}/databases` (create/list/inspect/stop/delete) | workload:read/write | Password secret `db-{name}-password` must exist before create; credentials are never returned by the API |
| Drop | `/v1/drop/transfers` (+ approve/reject/cancel/content) | any authenticated user | Uploads/downloads are raw bytes; approval gates download |

## Lifecycle states

- Workload: PENDING → QUEUED → ASSIGNED → STARTING → RUNNING → (STOPPING) →
  SUCCEEDED / FAILED / CANCELLED / STOPPED. `queue_reason` explains waiting.
- Deployment: created → queued → releasing → running / failed / stopped /
  unhealthy / build_failed; `healthy_revision` only advances with a live route.
- Training: created → queued → running → succeeded / failed / cancelled.
- Database: starting → running / unhealthy / failed / stopped.
- Transfer: pending → approved → completed / rejected / cancelled / expired.

## Error handling

All errors are `{"error": {"code", "message"}}`. Codes used across services:
`invalid_request`, `invalid_workload`, `invalid_placement`, `invalid_policy`,
`conflict`, `quota_exceeded` (HTTP 507), `too_large` (413), `forbidden`,
`not_found`, `unauthorized`, `worker_unauthorized`, `worker_forbidden`,
`enrollment_rejected`, `stale_assignment`, `service_unavailable`,
`scheduler_unavailable`, `proxy_unavailable`, `report_unavailable`,
`artifacts_unavailable`, `deploy_unavailable`, `collect_unavailable`,
`runtime_unavailable`, `routes_unavailable`, `not_implemented` (exit-code 4 in
the CLI).

## Known gaps (do not build UI for these yet)

1. Inference worker transport is implemented and covered by protocol tests. Separate-host TLS/Docker acceptance, Workspace IDE routing, and Deploy HTTP routing remain open.
2. Remote worker output registration is PARTIAL: the agent stages outputs
   locally; `/collect` works for local-executor workloads.
3. Engine-level database backup/restore is `not_implemented` (volume backups
   and metadata backups are available).
4. Inference `restart` is stop + fresh deploy; no in-place restart.
5. Live GitHub webhooks and Tailscale remain BLOCKED without credentials.
6. Training checkpoint resume is CLI-advised as a resubmission, not automated.

## Examples (illustrative values, not live data)

Deploy inspect: `{"deployment":{"id":"dep-…","state":"running","current_revision":2,"healthy_revision":1},"builds":[…],"revisions":[…],"runtime":{"state":"RUNNING","attempt":1}}`

Scheduler status: `{"queue":{"depth":0,"by_project":{}},"workers":[{"id":"local","state":"online","active_assignments":1}]}`

Quota view: `{"quota":{"max_active_workloads":3},"usage":{"active_workloads":1,"reserved_cpu_millis":500,"reserved_memory_bytes":536870912}}`
