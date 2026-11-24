# Kamui Cloud — Mobile Workspace MVP

A working prototype for coding from your phone using a real browser-based IDE (code-server) with GitHub integration.

## Quick Start

### Prerequisites

- Docker and Docker Compose
- GitHub account (for private repos)
- Android phone on same Wi-Fi (for mobile testing)
- Tailscale account (optional, for remote access)

### 1. Configure Environment

```bash
cd /Users/kyzenn/Projects/kamui-cloud/ops

# Copy and edit environment
cp .env.example .env
# Edit .env with your values:
# KAMUI_TOKEN=your-secure-token
# TAILSCALE_AUTHKEY=tskey-xxx (optional)
```

### 2. Start Services

```bash
# From ops directory
docker compose up -d --build
```

This starts:
- **API** (port 8080) - Kamui coordinator with workspace management
- **Console** (port 3000) - Next.js mobile UI
- **Workspace Proxy** (port 8081) - Nginx reverse proxy for code-server instances
- **Tailscale** (optional) - For remote access

### 3. Access from Phone

1. Find your server's LAN IP: `ip addr show` (Linux) or `ipconfig` (Windows)
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
5. Tap "Open IDE" → code-server opens in new tab

### 5. Code and Push

1. In code-server: edit files, use terminal (`Ctrl+``)
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
Phone Browser
    │
    ▼
┌─────────────────────────────────────┐
│  Workspace Proxy (nginx:8081)       │
│  - Routes /workspace/{id}/*         │
│  - WebSocket proxy for code-server  │
└─────────────────────────────────────┘
    │
    ├──────────────────┬──────────────────┐
    ▼                  ▼                  ▼
┌─────────┐      ┌─────────────┐   ┌───────────┐
│ Console │      │    API      │   │code-server│
│(Next.js)│      │   (Go)      │   │containers │
│ Port 3000│      │  Port 8080  │   │  Port 8081+│
└─────────┘      └─────────────┘   └───────────┘
                      │
                      ▼
               ┌─────────────┐
               │  Docker     │
               │  Volumes    │
               │ (workspace  │
               │  data)      │
               └─────────────┘
```

## Services

| Service | Port | Description |
|---------|------|-------------|
| API | 8080 | Go coordinator (nodes, jobs, workspaces) |
| Console | 3000 | Next.js mobile UI |
| Workspace Proxy | 8081 | Nginx reverse proxy + WebSocket |
| code-server | 8081+ | VS Code in browser (per workspace) |
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
- `kamui-workspace-{id}` - Workspace source code and Git history
- Survives container restarts, server reboots

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
- Tailscale provides encrypted mesh network

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

1. **code-server containers not fully implemented** - Currently simulated in Go API
2. **No real Docker container orchestration** - Workspace start/stop is in-memory
3. **Git operations simulated** - Need to exec into workspace containers
4. **Single-user only** - No multi-user isolation yet
5. **No resource quotas** - Can start unlimited workspaces
6. **No HTTPS on LAN** - Self-signed certs needed for production

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

### "code-server won't load"
- Check workspace proxy logs: `docker compose logs workspace-proxy`
- Verify WebSocket upgrade works (nginx config)
- Try direct access: `http://<SERVER_IP>:3000`

### "Git push fails"
- Verify GitHub token has `repo` scope
- Check repository URL format: `https://TOKEN@github.com/...`
- Check network access to github.com

### Phone can't reach server
- Verify both on same Wi-Fi (not guest network)
- Check server firewall: `sudo ufw allow 8081`
- Try server's hostname: `http://kamui-server:8081`
- Disable AP isolation on router

## Development

```bash
# Run console locally (hot reload)
cd ../console
npm install
npm run dev

# Run API locally
cd ../api
KAMUI_TOKEN=dev go run ./cmd/api

# Run tests
cd ../api && go test -race ./...
cd ../console && npm run type-check && npm run lint
```

## License

MIT - Part of Kamui Cloud project.