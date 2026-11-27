# Kamui Cloud agent guide

- The Mobile Workspace feature branch owns the current Compose deployment. Keep Console login, CSRF, trusted HTTPS, IDE authorization, and Git flows working.
- Workspace Manager is the trusted Docker boundary in Compose. Keep Docker access off the API and Console containers, and keep the manager on the private control network. Use loopback for a native coordinator.
- Keep workspace metadata and named volumes on their existing paths. Apply core schema changes as new ordered SQLite migrations. Preserve user data and credentials during tests, upgrades, and rollbacks.
- Start with [core architecture](docs/CORE_BACKEND.md) and [core API](shared/core-openapi.yaml). Implement new execution behavior through the core repository, placement policy, and executor boundaries.
- Run Go API tests and vet from `api/`, Rust tests and Clippy from `agent/`, and the existing Workspace stack checks from `ops/`. Verify Docker behavior against an isolated Compose project or development daemon. Report runtime checks as blocked if infrastructure prevents them.
- Review the branch diff and run focused checks before committing. Push only to the requested feature branch using a fast-forward update.
