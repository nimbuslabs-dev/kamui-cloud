# kamui-cloud

Manage a local Kamui deployment from macOS, Linux, or Windows. Running
`kamui-cloud` in a terminal opens a live dashboard for the local worker, jobs,
hosted services, and Workspaces. Use **1–4** or **Tab** to switch views,
**↑/↓** and **Enter** to inspect, **l** for workload logs, **r** to refresh,
and **q** to exit.

## Install or build

Use Go 1.26.11, the toolchain version in `go.mod`, and check out the commit
you want to build. From `cli/`:

```sh
go install -mod=readonly -trimpath -buildvcs=false ./cmd/kamui-cloud
```

Put Go's bin directory on PATH. Go uses `GOBIN` when set and the `bin`
subdirectory of `GOPATH` otherwise. To produce reproducible binaries, use
the same commit and Go version, disable CGO, and run from `cli/`:

```sh
for os in darwin linux windows; do
  for arch in amd64 arm64; do
    suffix=
    [ "$os" != windows ] || suffix=.exe
    GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 \
      go build -mod=readonly -trimpath -buildvcs=false \
      -o "dist/kamui-cloud-${os}-${arch}${suffix}" ./cmd/kamui-cloud
  done
done
```

For a Windows PowerShell build, set `$env:CGO_ENABLED = '0'`,
`$env:GOOS = 'windows'`, and `$env:GOARCH = 'amd64'` or `'arm64'`.
Run `go build -mod=readonly -trimpath -buildvcs=false -o kamui-cloud.exe ./cmd/kamui-cloud`.

## Start a local cloud

From the Kamui checkout or a subdirectory, with local Docker Engine or Docker
Desktop and its Compose plugin installed:

```sh
kamui-cloud docker context
kamui-cloud cloud init
kamui-cloud cloud start
kamui-cloud cloud status --json
kamui-cloud cloud health --json
```

`cloud init` generates separate credentials in `ops/.env` when the file is
absent. Read `KAMUI_ADMIN_PASSWORD` there privately to sign in to the Console.
`cloud start` initializes TLS, starts the Compose stack and its opt-in CLI
gateway, then checks the gateway with the generated CA and the coordinator token.
`cloud stop` stops the Compose services and retains their volumes. To end a
specific running job, service, or Workspace, use its own cancel or stop command.

Use `--root` or `KAMUI_ROOT` to select the checkout from another directory.
The CLI respects the selected Docker context and Docker environment.
`--context` selects an endpoint for the current commands. The CLI never updates
Docker's default context. Local lifecycle commands require a Unix socket or a
local Windows named pipe.

### Endpoint and credentials

The default Compose CLI endpoint is `https://127.0.0.1:8443`.
`KAMUI_CLI_PORT` can override the port. The dedicated gateway binds loopback
and uses the public CA from `ops/tls-local/ca.pem`, or the directory selected
by `KAMUI_TLS_DIR`. The browser Console and its HTTPS URL keep their own port.

For a separately configured backend, set `--url` and optionally `--ca-file`.
The CLI verifies the hostname and certificate using the supplied CA or the OS
trust store. Explicit HTTP is supported for literal loopback IPs in native mode.
A credential comes from `--token-file`, then `KAMUI_TOKEN`, then the local
`ops/.env` for automatic Compose endpoint discovery. Keep token files private.
Use a literal value in `ops/.env` or supply the resolved token separately.

`config set` saves supplied flags such as `--root`, `--url`, `--ca-file`,
`--token-file`, and `--context` without storing token contents. `config show`
shows the resolved settings. A flag overrides its environment variable
(`KAMUI_ROOT`, `KAMUI_URL`, `KAMUI_CA_FILE`, `KAMUI_TOKEN_FILE`, or
`KAMUI_DOCKER_CONTEXT`), then the saved setting. Use `KAMUI_CONFIG` to select
a different configuration file.

## Automate

Provision the Linux image on the selected Docker daemon before submitting a
workload. Pass container arguments after `--`. The CLI sends an argument vector.

```sh
kamui-cloud device inspect --json
kamui-cloud job submit --json --idempotency-key example-001 busybox:1.37 -- echo hello
kamui-cloud job list --json
kamui-cloud job inspect JOB_ID --json
kamui-cloud job logs JOB_ID
kamui-cloud job cancel JOB_ID --json
kamui-cloud service start --service deploy busybox:1.37 -- sleep 3600
kamui-cloud service list --json
kamui-cloud service stop SERVICE_ID --json
kamui-cloud workspace create example https://github.com/octocat/Hello-World
kamui-cloud workspace start WORKSPACE_ID
kamui-cloud workspace git status WORKSPACE_ID
kamui-cloud workspace stop WORKSPACE_ID
```

Workload, service, and Workspace subcommands show their supported commands via
`--help`. Reuse an explicit idempotency key for a submission retry. The
coordinator supplies up to 100 recent workloads, which the CLI filters by kind.
Services use the same durable engine and currently have local lifecycle
management. Network publication and GPU execution remain future integrations.

In JSON mode, successful commands produce JSON on stdout and errors produce
an `error` object on stderr. Exit codes include 0 for success, 1 for other
failures, 2 for invalid input or configuration, 3 for authorization failures,
4 for missing resources, 5 for conflicts or limits, 6 for connectivity, TLS
or unhealthy backend failures, and 130 for interruption.
