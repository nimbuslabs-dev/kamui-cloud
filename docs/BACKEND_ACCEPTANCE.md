# Backend acceptance

Reviewed baseline `f15b8a81c447521fd6e9a967d1ac2f070e34e24e`. These checks use disposable SQLite databases and files. Earlier deployment evidence remains historical.

| Finding | Scope | Result |
| --- | --- | --- |
| BAK-01 | Backup project and administrator authorization | PASS. Two independent users exercise inspection, content download, restore and listing. The baseline failed the isolation regression. Project-scoped queries now require an exact match. |
| BAK-02 | Restore destination, archive and integrity safety | PASS. Fixture tests restore two isolated exports, compare SQLite snapshot bytes by SHA-256, reject link entries and a corrupt checksum, and verify cleanup. The export location is selected by the coordinator. |
| BAK-03 | Backup identity and creation | PASS. Tests create independent object and metadata backup records. IDs now use random entropy and storage writes are exclusive. Object enumeration paginates and propagates read errors. |
| BAK-04 | Managed Docker-volume restore and project object import | PASS for coordinator-managed local Docker volumes and local project object storage. The authenticated manager bridge exported an offline Docker volume, restored it to a fresh volume, and reread its bytes, UID, GID and mode. Project object import into new buckets preserved the checksum, key, content type and empty buckets. |
| ML-01 | Training, checkpoint and model artifact ownership | PASS. Store tests reject foreign training jobs, mismatched checkpoints, wrong-kind and cross-project artifacts, fabricated IDs, a conflicting client digest and a corrupt blob. Successful model registration derives bytes and digest from the artifact record. |
| ML-02 | Model parent foreign key | PASS. A v6 migration preserves rows from v4 and v5, normalizes absent parents to SQL NULL, and verifies that FK enforcement is active after upgrade. |
| ML-03 | Functional resume and model serving | PARTIAL. Resume validates ownership and integrity and returns HTTP 501. The deterministic resume and real model-output acceptance scenarios were not run. |

## Remaining priority order

1. ML-03. Stage a verified checkpoint on the assigned executor, register remote outputs, and prove deterministic resume and model-derived CPU inference.
2. Verify Deploy Git builds and staged traffic activation, rollback and failure preservation across restart.
3. Verify database-native backup and isolated restore, including project secret resolution, placement and same-name isolation.
4. Verify S3 cancellation and multipart behavior against an isolated service. Audit Workspace idle and routing lifecycle, then the remaining backend modules and transaction boundaries.

## Executed checks

The BAK-04 Docker Engine round trip produced the same SHA-256 on both volumes, `b4691d93d4b95b2e5dc39fafb62bd5b84c78458256bb1f25a934a34a788ac2d8`. The local object-store round trip produced `4ab2143d5c4592799dc26553eac9ba7252b5081d5235d3a0d546eeedfead6cc1` in both buckets. Regression tests cover project isolation, corrupt archives, links, cancellation, quota rechecks and cleanup after restart. The Docker test rejects an in-use source, a foreign project, a mismatched engine and a missing pinned volume. Unrelated workloads can start while a transfer is active. The Docker Nginx test forwarded the escaped key verbatim and rejected writes on the read-only object route.

API and CLI package tests, race checks and `go vet` passed. The read-only Linux Workspace Manager restored and reread the fixture through its authenticated bridge using the dedicated scratch volume. The isolated Compose workspace suite passed HTTPS, auth, CSRF, workspace isolation, Git with a disposable bare remote, restart and retention using freshly built API and Workspace Manager images.

Volume backup requires a stopped source workload and a managed volume pinned to the current coordinator Docker engine. Existing unverified Docker names require operator migration before new mounts or backup. Volume archives accept regular files and directories. A manager interruption can leave a stopped export helper, reclaimed on the next export of its source. Project object import requires the local object backend. S3-backed installations can request a verified filesystem export with `mode=export`. Remote-worker volume backup and restore are outside this checkpoint.

The earlier Rust checkpoint reported 10 unit and 1 integration test passing, with formatting and strict Clippy failures. Physical distributed tests, S3 live acceptance, deterministic training resume, real model inference, Deploy A/B rollback and native PostgreSQL record comparison remain NOT RUN at this checkpoint.

## Confirmed by current source and still open

- Deploy publishes `current_revision` during release and discards important update errors in `api/internal/deploy/deploy.go`. Safe candidate traffic preservation remains unverified.
- PostgreSQL backup and restore still read the workload's stored password reference in `api/internal/ops/ops.go`. Secret resolution and assigned-worker execution require validation before relying on this path. Destructive restore still needs confirmation and recovery protection.
- S3 uses a pipe into `PutObject` in `api/internal/objectstore/s3.go`. Multipart support and cancellation-safe producer shutdown remain open.
- Workspace idle checks skip absent activity timestamps in `api/internal/http/workspaces.go`. Active-editor tracking, shutdown reconciliation and tunnel resource behavior require review.
- Training and inference submit workloads before writing service metadata. An atomic create path and reservation/restart reconciliation remain open.

## Local binaries

The tracked `api/workspace-manager` and `cli/kamui-cloud` files were compiled Mach-O executables. Build replacements from their respective module directories with the repository Go toolchain:

```sh
# From api/
go build -mod=readonly -trimpath -o workspace-manager ./cmd/workspace-manager

# From cli/
go build -mod=readonly -trimpath -o kamui-cloud ./cmd/kamui-cloud
```
