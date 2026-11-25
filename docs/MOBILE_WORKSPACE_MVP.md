# Kamui Cloud — Mobile Workspace MVP

A working prototype for coding from your phone using a real browser-based IDE (code-server) with GitHub integration.

## Quick Start

### Prerequisites

- Windows 10/11 with Docker Desktop 29+ (WSL2 backend)
- GitHub account (for private repos)
- Android phone on same Wi-Fi (for mobile testing)
- Tailscale account (optional, for remote access)

### 1. One-Command Startup (Windows PowerShell)

```powershell
# From repository root (C:\Users\lifeg\Downloads\kamui-cloud)
.\start-kamui.ps1
```

This single command:
- Validates Docker environment
- Creates `.env` with secure `KAMUI_TOKEN` if missing
- Builds all Docker images
- Starts all services (API, Console, nginx Proxy, Workspace Manager)
- Waits for health checks
- Reports the LAN-accessible URL for your phone

### Manual Startup (if needed)

```bash
# From ops directory
cd ops
cp .env.example .env
# Edit .env if needed (token auto-generated on first run)
docker compose up -d --build
```

### 3. Access from Phone

1. Find your server's LAN IP (shown by `start-kamui.ps1` or run `.\status-kamui.ps1`)
2. On your Android phone, open browser to `http://<SERVER_IP>:8081`
3. You should see the Kamui Cloud mobile workspace UI

### 4. Create a Workspace

1. Tap "New Workspace"
2. Enter:
   - **Name**: `my-project`
   - **Repository URL**: `https://github.com/youruser/yourrepo.git`
   - **Branch**: `main` (default)
3. Tap "Create Workspace"
4. Tap "Start" on the workspace card
4. Tap "Open IDE" → code-server opens in new tab

### 5. Code and Push

1. In code-server: edit files, use terminal (`Ctrl+\``)
2. Back in Kamui UI: tap the workspace to see Git panel
3. Stage files → enter commit message → "Commit"
4. Tap "Push" → changes go to GitHub

### 6. Remote Access (Tailscale)

1. Install Tailscale on server and phone
2. Add `TAILSCALE_AUTHKEY` to `.env`
3. Restart: `docker compose up -d`
4. On phone: use Tailscale IP (e.g., `http://100.x.x.x:8081`)

## Architecture

```
Phone Browser (Android)
    │
    ▼
┌─────────────────────────────────────────────────────────────┐
│  Workspace Proxy (nginx:8081)                               │
│  - Routes /                    → Console (Next.js)          │
│  - Routes /api/               → API (Go)                    │
│  - Routes /workspace/{id}/*   → code-server container       │
│  - WebSocket proxy for code-server terminal/editor          │
└─────────────────────────────────────────────────────────────┘
    │                          │                          │
    ▼                          ▼                          ▼
┌──────────────┐         ┌──────────────┐         ┌──────────────────┐
│  Console     │         │    API       │         │ code-server      │
│  (Next.js)   │         │   (Go)       │         │ containers       │
│  Port 3000   │         │  Port 8080   │         │  Port 8080       │
│              │         │              │         │  (per workspace) │
└──────────────┘         └──────────────┘         └──────────────────┘
                                                      │
                                                      ▼
                                               ┌──────────────────┐
                                               │  Workspace       │
                                               │  Manager         │
                                               │  (Go + Docker    │
                                               │   API)           │
                                               │  Port 8082       │
                                               └──────────────────┘
                                                      │
                                                      ▼
                                               ┌──────────────────┐
                                               │  Docker Volumes  │
                                               │  kamui-workspaces│
                                               │  kamui-api-data  │
                                               │  tailscale-state │
                                               └──────────────────┘
```

All services communicate on the `kamui-network` (attachable bridge).
Workspace Manager connects to Docker Engine via read-only socket mount.

## Services

| Service | Port | Description |
|---------|------|-------------|
| API | 8080 | Go coordinator (nodes, jobs, workspaces) |
| Console | 3000 | Next.js mobile UI |
| Workspace Proxy | 8081 | Nginx reverse proxy + WebSocket |
| code-server | 8081+ | VS Code in browser (per workspace) |
| Workspace Manager | 8082 | Docker container lifecycle manager |
| Tailscale | - | VPN for remote access |

## API Endpoints (Workspace)

| Method | Path | Description |
|--------|------|-------------|
| POST | `/v1/workspaces` | Create workspace |
| GET | `/v1/workspaces` | List workspaces |
| GET | `/v1/workspaces/{id}` | Get workspace |
| POST | `/v1/workspaces/{id}/start` | Start code-server |
| POST | `/v1/workspaces/{id}/stop` | Stop code-server |
| DELETE | `/v1/workspaces/{id}` | Delete workspace |
| GET | `/v1/workspaces/{id}/git/status` | Git status |
| POST | `/v1/workspaces/{id}/git/add` | Stage files |
| POST | `/v1/workspaces/{id}/git/commit` | Commit changes |
| POST | `/v1/workspaces/{id}/git/push` | Push to remote |
| POST | `/v1/workspaces/{id}/git/pull` | Pull from remote |

## GitHub Authentication

For private repositories, use HTTPS URLs with a Personal Access Token:

```
https://<TOKEN>@github.com/user/private-repo.git
```

Create token at: GitHub Settings → Developer settings → Personal access tokens → Tokens (classic)

Required scopes: `repo` (full control of private repositories)

## Persistence

Workspaces use Docker volumes:
- `kamui-workspace-{id}` - Workspace source code and Git history (bind-mounted to `/workspaces/{id}`)
- `kamui-api-data` - API database
- `tailscale-state` - Tailscale identity
- All survive container restarts, server reboots, and `docker compose down` (not `down -v`)

## Mobile UI Features

- Responsive design (360px+ width)
- Touch-friendly controls (44px+ targets)
- Offline-capable (cached via service worker)
- PWA installable on Android
- Dark mode support

## Security

- All API endpoints require `Bearer KAMUI_TOKEN`
- code-server runs as non-root user (UID 1000)
- No Docker socket mounted in workspace containers
- Workspace containers have resource limits (2 CPU, 4GB RAM)
- Workspace Manager has read-only Docker socket access (privileged component)
- Tailscale provides encrypted mesh network (optional)
- All inter-service communication on isolated Docker network
- GitHub tokens only in repository URLs (never in logs or browser)

## Testing Checklist

### LAN Access
- [ ] Phone connects to `http://<LAN_IP>:8081`
- [ ] Console loads, shows server info
- [ ] Create workspace → starts → opens code-server
- [ ] Edit file in code-server → save
- [ ] Git panel shows changes → commit → push
- [ ] Verify commit on GitHub

### Remote Access (Tailscale)
- [ ] Tailscale connected on server and phone
- [ ] Access via `http://<TAILSCALE_IP>:8081`
- [ ] Full workflow works over VPN

### Persistence
- [ ] Stop workspace → start again → files intact
- [ ] Restart Docker Compose → workspaces survive
- [ ] Git history preserved

### Error Handling
- [ ] Invalid repo URL → clear error
- [ ] Private repo without token → auth error
- [ ] Network offline → graceful degradation
- [ ] Push rejected → shows Git error

## Known Limitations (MVP)

1. **Single-user only** - No multi-user isolation yet (single shared KAMUI_TOKEN)
2. **No resource quotas** - Can start unlimited workspaces (recommend max 2-3 on 16GB)
3. **No HTTPS on LAN** - Self-signed certs needed for production (use mkcert)
4. **GitHub tokens in URLs** - Tokens embedded in repository URLs (use GitHub App for production)
5. **Tailscale optional** - Remote access requires manual Tailscale setup
6. **code-server auth disabled** - `--auth none` for simplicity; enable password for production

## Next Steps

1. Implement real code-server container lifecycle (Docker API)
2. Add workspace container exec for Git operations
3. Add resource limits and quotas
4. Implement HTTPS with mkcert for LAN
5. Add multi-user support with separate tokens
6. Add workspace templates (Node, Python, Go, Rust)
7. Add file browser in mobile UI
8. Add terminal access in mobile UI

## Troubleshooting

### "Cannot connect to API"
- Check `KAMUI_TOKEN` matches in `.env` and API
- Verify API container is healthy: `docker compose logs api`
- Check firewall allows port 8080
- Run `.\status-kamui.ps1` to diagnose

### "code-server won't load"
- Check workspace proxy logs: `docker compose logs workspace-proxy`
- Verify WebSocket upgrade works (nginx config)
- Try direct access: `http://<SERVER_IP>:3000`
- Ensure workspace is in "running" state (not "creating" or "stopped")

### "Git push fails"
- Verify GitHub token has `repo` scope
- Check repository URL format: `https://TOKEN@github.com/...`
- Check network access to github.com
- Check workspace is running: `.\status-kamui.ps1`

### Phone can't reach server
- Verify both on same Wi-Fi (not guest network)
- Check Windows firewall: `Allow 8081` in Windows Defender Firewall
- Try server's hostname: `http://kamui-server:8081`
- Disable AP isolation on router
- Run `.\status-kamui.ps1` to get correct LAN IP

### Workspace stuck in "creating" or "error"
- Check workspace-manager logs: `docker compose logs workspace-manager`
- Verify Docker socket access: workspace-manager needs `/var/run/docker.sock`
- Check disk space: `docker system df`
- Restart workspace-manager: `docker compose restart workspace-manager`

## Development

### Automated Scripts

```powershell
# One-command startup (Windows)
.\start-kamui.ps1

# Check status
.\status-kamui.ps1

# Run smoke tests
.\test-kamui.ps1

# Stop services (preserves data)
.\stop-kamui.ps1
```

### Local Development

```bash
# Run console locally (hot reload)
cd console
npm install
npm run dev

# Run API locally
cd api
KAMUI_TOKEN=dev go run ./cmd/api

# Run tests
cd api && go test -race ./...
cd console && npm run type-check && npm run lint
```

## License

MIT - Part of Kamui Cloud project.