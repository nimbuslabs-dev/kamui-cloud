# Core backend foundation

## Repository reconciliation

The Phase 00 baseline is `9b648a8b3686fddc1fe4f2406667aa7e51353ac7` on `feat/mobile-workspace-mvp`. The active deployment is the Mobile Workspace Compose stack. Nginx terminates TLS, Console owns session and CSRF checks, the Go API owns workspace metadata, Workspace Manager owns Docker and Git operations, and IDE Tunnel handles session-bound upgrades. The original node registry and job queue are in-memory compatibility APIs. The Rust agent retains its legacy node registration and heartbeat mode. The standalone Run, Store, Train, Infer, Drop, and CLI directories are separate prototypes or stubs with varying coverage. The core service labels reserve a common execution contract for those integrations.

## Service boundaries

| Boundary | Responsibility |
| --- | --- |
| `api/internal/core.Repository` / `EventReader` | SQLite snapshots, monotonic events, idempotency keys, worker heartbeat, and pending execution cleanup. |
| `core.Placement` | Select the local worker automatically or validate the explicit `worker_id`. Compare OS, architecture, CPU, RAM, and GPU requirements to the execution runtime. |
| `core.Engine` | Persist transitions and cancellation intent, reserve CPU/RAM, fence attempts, and reconcile execution with Docker. |
| `core.Executor` | Report runtime capacity, create and start attempts, inspect status, stop, remove, and fetch logs. A future remote worker can implement the same interface. |
| Rust `kamui-agent capabilities` | Probe native OS/architecture, logical CPUs, RAM, storage, GPU inventory and available VRAM, and local Docker availability. |

A bearer token grants local administrator access to the seeded default project. Batch jobs progress through `PENDING`, `QUEUED`, `ASSIGNED`, `STARTING`, and `RUNNING` before reaching a terminal state. Services can become `UNHEALTHY` based on the image health check and can return to `RUNNING`. A service stop goes through `STOPPING` and ends in `STOPPED`. Failed runs can retry up to three times. Job timeouts measure process execution after a 45-second startup budget. Service timeouts apply to startup, with no fixed running deadline. Fatal image setup errors terminate the attempt. Capacity mismatches produce a durable queue reason.

The single coordinator holds an OS-released SQLite lock and writes state and events in the same transaction. Resource reservations are embedded in persisted assignments and apply to core workloads. The legacy Workspace Manager enforces its own limits. An assignment records the Docker daemon identity and a renewable lease. Expiry or loss of contact cannot release its resources while execution is unconfirmed. Restart inspects the same named attempt. Completed attempts retain a bounded log tail and result in SQLite before their managed container enters the cleanup queue. `after` on the event API is a durable cursor. Workers and workloads use the [core OpenAPI contract](../shared/core-openapi.yaml).

## Rollout and data

Compose uses the existing private Workspace Manager connection and stores `kamui-core.sqlite` alongside `workspaces.json` on the existing API data volume. The additive migration initializes only the core database. Existing JSON metadata, repository volumes, Git configuration, credentials, and TLS material retain their existing owners and paths. The API container can be rolled back to its previous image with the volume retained. Keep the SQLite database and its WAL together for an offline backup, or use SQLite's backup API while running. Keep a running workload bound to its original Docker daemon, Compose project, and database across coordinator restarts.

The Compose worker reports Docker Engine capacity, which can describe a Linux VM on Docker Desktop. Run the coordinator natively with the Rust agent for physical host metrics. The allocator uses Docker Engine totals and the core reservation ledger. Detected GPUs are informational and requests for GPU execution remain queued.

The execution API is private to the Compose control network and loopback in native mode. It accepts locally provisioned Linux images with no declared volumes. Docker receives an argument vector and the original image entrypoint. Workloads run as a nonroot UID with a read-only root filesystem, a bounded temporary directory, no networking, no host mounts, and CPU, RAM, PID and log limits. Treat arguments and logs as operator-visible persisted data. Hosted services currently provide lifecycle supervision within the local runtime. Network publication, remote workers, GPU passthrough, collaborative identity management, and an artifact store are later integrations.
