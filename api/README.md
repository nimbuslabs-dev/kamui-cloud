# Kamui coordinator

The Mobile Workspace deployment uses the [Compose stack](../ops/README.md).
The core workload API is available inside its private control network at
`http://api:8080`. The [core architecture](../docs/CORE_BACKEND.md) describes
data paths and migration behavior.

## Native coordinator

Build the Rust agent with `cargo build --locked --manifest-path agent/Cargo.toml`.
Set `KAMUI_AGENT_PATH` to the absolute path to the binary or put `kamui-agent`
on PATH. Set `KAMUI_TOKEN` to a random token of at least 32 bytes and configure
`DOCKER_HOST` for the local Docker Engine if it uses a nondefault socket or
named pipe. Start with `go run ./cmd/api` from `api/`. Native mode listens on
`127.0.0.1:8080`. The optional `KAMUI_CORE_DATA_FILE` overrides the SQLite path.
By default it is `kamui-core.sqlite` next to the Workspace JSON file.

Provision a suitable Linux Docker image locally before submitting a workload.
This example assumes BusyBox has already been provisioned.

```sh
curl http://127.0.0.1:8080/v1/workloads \
  -H "Authorization: Bearer $KAMUI_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: local-smoke-1' \
  -d '{"image":"busybox:latest","args":["echo","hello"]}'
```

Use the returned ID to retrieve `GET /v1/workloads/{id}` and
`GET /v1/workloads/{id}/logs`. Use `POST /v1/workloads/{id}/cancel` to request
cancellation and `GET /v1/workers/local` to inspect the worker. See the
[OpenAPI definition](../shared/core-openapi.yaml) for the full contract.

## Verify

From `api/`, run `go vet ./...` and `go test -race ./...`.
