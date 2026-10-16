# Ports and config

Single source for ports, environment variables and timings. Only the head edits this file.

## Services

| Service | Port | Language | Folder | Health check |
|---|---|---|---|---|
| Agent (mDNS) | 7070 | Rust | `agent/` | `/healthz` |
| API (coordinator) | 8080 | Go | `api/` | `/healthz` |
| Store | 9000 | Go | `store/` | `/healthz` |
| Run | 8082 | Go | `run/` | `/healthz` |
| Infer | 8090 | Python | `infer/` | `/healthz` |
| Console | 3000 | TypeScript | `console/` | `/healthz` |
| Drop | 3001 | TypeScript | `drop/` | `/healthz` |

Train has no port. It is a script that runs once and saves the model.

## Environment variables

| Variable | Used by | Default | Meaning |
|---|---|---|---|
| `KAMUI_API` | agent, cli, run, console | `http://localhost:8080` | API address |
| `KAMUI_STORE` | cli, train, infer, console, drop | `http://localhost:9000` | Store address |
| `KAMUI_TOKEN` | all | none, required | Join token sent as `Authorization: Bearer <token>` |
| `KAMUI_STORE_DIR` | store | `./data` | Folder where files are saved |
| `KAMUI_MAX_MB` | store | `512` | Maximum upload size in MB |

Never commit a real `KAMUI_TOKEN`. Keep it in your shell or a local `.env` file that git ignores.

## Discovery and timings

| Item | Value |
|---|---|
| mDNS service type | `_kamui._tcp.local.` |
| mDNS port | UDP 5353 |
| Heartbeat every | 5 seconds |
| Node is offline after | 15 seconds without a heartbeat |
| Agent retry backoff | 1, 2, 4 seconds, then stay at 4 |
| Timeout on every HTTP call | 10 seconds |

## Firewall

Open these on every laptop in Network mode: TCP 8080, 9000, 8082, 8090, 3000, 3001, 7070 and UDP 5353. Never forward them to the internet.
