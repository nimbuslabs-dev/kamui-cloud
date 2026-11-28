# Backend acceptance

Reviewed baseline `f15b8a81c447521fd6e9a967d1ac2f070e34e24e`. These checks use disposable SQLite databases and files. Earlier deployment evidence remains historical.

| Finding | Scope | Result |
| --- | --- | --- |
| BAK-01 | Backup project and administrator authorization | PASS. Two independent users exercise inspection, content download, restore and listing. The baseline failed the isolation regression. Project-scoped queries now require an exact match. |
| BAK-02 | Restore destination, archive and integrity safety | PASS. Fixture tests restore two isolated exports, compare SQLite snapshot bytes by SHA-256, reject link entries and a corrupt checksum, and verify cleanup. The export location is selected by the coordinator. |
| BAK-03 | Backup identity and creation | PASS. Tests create independent object and metadata backup records. IDs now use random entropy and storage writes are exclusive. Object enumeration paginates and propagates read errors. |
| BAK-04 | Managed Docker-volume restore and project object import | PARTIAL. Object restore produces an isolated export. Managed volume restore reports HTTP 501, and the standard coordinator has no volume export adapter. Live Docker volume and object import acceptance were not run. |

## Remaining priority order

1. Finish the training checkpoint and model ownership audit and negative regression tests.
2. Implement managed volume restore and project object import. Check volume ownership, locality and operation recovery before enabling volume export or restore.
3. Verify real deterministic checkpoint resume and remote artifact registration. Verify model-derived output from a supported CPU inference adapter.
4. Verify Deploy Git builds and staged traffic activation, rollback and failure preservation across restart.
5. Verify database-native backup and isolated restore, including project secret resolution, placement and same-name isolation.
6. Verify S3 cancellation and multipart behavior against an isolated service. Audit Workspace idle and routing lifecycle, then the remaining backend modules and transaction boundaries.
