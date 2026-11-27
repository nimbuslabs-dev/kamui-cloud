# Kamui worker agent

Run the worker on a trusted host. The default command registers with the existing
node API using `KAMUI_API_URL` (default `http://localhost:8080`) and `KAMUI_TOKEN`,
then sends periodic utilization heartbeats.

The single-host coordinator can invoke the same executable for a capability snapshot.

```sh
cargo build --locked
./target/debug/kamui-agent capabilities
./target/debug/kamui-agent capabilities /path/to/coordinator/data
```

The command writes one JSON object to stdout and does not require a token. It
reports the host operating system, CPU architecture, logical CPU count, memory
and available bytes on the volume holding the supplied path (default current
directory). Unavailable measurements are null. GPU inventory comes from the
operating system or installed driver utilities. Available VRAM is reported when
NVIDIA's driver or Linux's AMDGPU driver provides usable counters. GPU inventory
describes the host and does not imply GPU passthrough to containers.

Docker availability requires a responding local Unix socket or Windows named
pipe. A Docker Desktop VM's CPU, memory, and storage limits can differ from the
host snapshot and should be checked by the execution adapter.
