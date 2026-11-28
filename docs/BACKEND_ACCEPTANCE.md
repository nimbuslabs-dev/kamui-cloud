# Backend acceptance

Reviewed baseline `f15b8a81c447521fd6e9a967d1ac2f070e34e24e`. These checks use disposable SQLite databases and files. Earlier deployment evidence remains historical.

| Finding | Scope | Result |
| --- | --- | --- |
| BAK-01 | Backup project and administrator authorization | PASS. Two independent users exercise inspection, content download, restore and listing. The baseline failed the isolation regression. Project-scoped queries now require an exact match. |
| BAK-02 | Restore destination, archive and integrity safety | PASS. Fixture tests restore two isolated exports, compare SQLite snapshot bytes by SHA-256, reject link entries and a corrupt checksum, and verify cleanup. The export location is selected by the coordinator. |
| BAK-03 | Backup identity and creation | PASS. Tests create independent object and metadata backup records. IDs now use random entropy and storage writes are exclusive. Object enumeration paginates and propagates read errors. |
| BAK-04 | Managed Docker-volume restore and project object import | PARTIAL. Object restore produces an isolated export. Managed volume restore reports HTTP 501, and the standard coordinator has no volume export adapter. Live Docker volume and object import acceptance were not run. |
| ML-01 | Training, checkpoint and model artifact ownership | PASS. Store tests reject foreign training jobs, mismatched checkpoints, wrong-kind and cross-project artifacts, fabricated IDs, a conflicting client digest and a corrupt blob. Successful model registration derives bytes and digest from the artifact record. |
| ML-02 | Model parent foreign key | PASS. A v6 migration preserves rows from v4 and v5, normalizes absent parents to SQL NULL, and verifies that FK enforcement is active after upgrade. |
| ML-03 | Functional resume and model serving | PARTIAL. Resume validates ownership and integrity and returns HTTP 501. The deterministic resume and real model-output acceptance scenarios were not run. |

## Remaining priority order

1. Implement managed volume restore and project object import. Check volume ownership, locality and operation recovery before enabling volume export or restore.
2. Verify real deterministic checkpoint resume and remote artifact registration. Verify model-derived output from a supported CPU inference adapter.
3. Verify Deploy Git builds and staged traffic activation, rollback and failure preservation across restart.
4. Verify database-native backup and isolated restore, including project secret resolution, placement and same-name isolation.
5. Verify S3 cancellation and multipart behavior against an isolated service. Audit Workspace idle and routing lifecycle, then the remaining backend modules and transaction boundaries.

## Executed checks

API and CLI package tests, race tests and vet passed. Rust agent tests reported 10 unit and 1 integration test passing. Rust formatting and strict Clippy failed on existing Rust code. The change set contains no Rust source edits. Compose configuration and isolated Docker Nginx syntax validation passed.

The backup and ML tests use temporary local SQLite and filesystem fixtures. The two-user backup test exercises handler authorization directly with independent principals. Physical distributed tests, isolated Docker volume restore, S3 live acceptance, deterministic training resume, real model inference, Deploy A/B/rollback, and native PostgreSQL record comparison are NOT RUN.

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
