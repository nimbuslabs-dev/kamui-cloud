# Mobile Workspace MVP

Kamui runs a single-owner coding environment. The Console creates and manages a workspace backed by Docker. code-server provides the editor, explorer and terminal. Files and Git history live on a named volume, and the coordinator stores workspace records on a separate named volume.

## Start on Windows

Install Git and [Docker Desktop](https://docs.docker.com/desktop/setup/install/windows-install/). Select the WSL2 backend and Linux containers, and start Docker Desktop. Use Windows PowerShell 5.1 or newer, an up-to-date Compose plugin and `curl.exe` (7.70 or newer when using Schannel). Allocate at least 3 GiB of RAM and two CPUs to Docker and allow several GB of free storage. The first build and GitHub operations use the internet. Keep Docker Desktop or Engine on a current patched release.

In PowerShell, run these commands in the parent folder where you want the checkout:

```powershell
git clone --branch feat/mobile-workspace-mvp --single-branch https://github.com/nimbuslabs-dev/kamui-cloud.git
Set-Location .\kamui-cloud
powershell.exe -NoProfile -ExecutionPolicy RemoteSigned -File .\start-kamui.ps1
powershell.exe -NoProfile -ExecutionPolicy RemoteSigned -File .\status-kamui.ps1
powershell.exe -NoProfile -ExecutionPolicy RemoteSigned -File .\test-kamui.ps1
```

To stop the project, run `powershell.exe -NoProfile -ExecutionPolicy RemoteSigned -File .\stop-kamui.ps1`. Each execution policy argument applies to that PowerShell process. Follow managed-device policy on borrowed computers.

`start-kamui.ps1 -CleanBuild` pulls base images and rebuilds from scratch. `-SkipBuild` reuses built images. Startup waits for all five services to become healthy and probes the HTTPS gateway and anonymous API access. The test script checks nginx, HTTPS trust and rejection of unauthenticated API, IDE and WebSocket upgrade requests.

The script creates distinct secrets in `ops/.env` and preserves saved credentials. If a previous install has a short or example credential, replace it with 32 random bytes encoded as hex before starting. Read `KAMUI_ADMIN_PASSWORD` from the file in a private editor to sign in. The coordinator token is an internal service credential. Keep the file private.

The local URL defaults to `https://127.0.0.1:8081`. Trust the public certificate `ops/tls-local/ca.pem` before opening the browser. On Windows, with permission to trust the CA for your current user, run:

```powershell
certutil.exe -user -addstore Root .\ops\tls-local\ca.pem
Start-Process 'https://127.0.0.1:8081'
```

This is an explicit one-time device action. Keep `ca.key` on the host. A startup that reports healthy has verified local access through its CA.

## Phone on a private LAN

1. Connect Windows and the phone to a private network or personal hotspot.
2. Run `Get-NetIPConfiguration` on Windows and find the IPv4 address of the active physical Wi-Fi or Ethernet adapter.
3. Set `KAMUI_BIND_IP` in `ops/.env` to that exact private IPv4 address and rerun startup. The generated HTTPS certificate covers the IP and reuses the same CA. Set `KAMUI_PORT` if the default port is occupied.
4. Transfer only `ca.pem` through a trusted channel. With the owner’s consent, search Android Settings for **Install a certificate** and choose **CA certificate**. If Android's file picker hides the PEM, export the same public certificate with `certutil.exe -decode .\ops\tls-local\ca.pem (Join-Path $env:TEMP 'kamui-local-ca.cer')` and transfer that file. Install it, then open the printed HTTPS URL in the browser and confirm it displays no certificate warning.
5. Sign in, start a test workspace, and open the IDE. Check the explorer, save an edit, and verify it using the terminal. Sign out with the IDE still open and confirm terminal access closes. Sign in again, restart the workspace, and confirm the edit persists. Remove the user-installed CA from the phone after borrowed-device testing.
6. If the phone cannot connect, check that both devices are on the same private network and that client isolation or the host firewall has not blocked the connection. Use an authorized network and firewall configuration.

The gateway binds one address. When a LAN IP is selected, use that IP from Windows too. Port 8081 is the default host port. The API, coordinator token endpoints, workspace manager and each code-server instance have no published host port. Keep the deployment on a trusted network without router forwarding.

## Edit and push

1. Sign in. Create a workspace with a public GitHub HTTPS URL and an optional branch. Start it and open the IDE. One workspace can run at a time.
2. Use the file explorer, editor and terminal. In the project terminal, set a workspace-local commit identity with `git config user.name "Your Name"` and `git config user.email "your-git-email"`.
3. Use the Console Git controls to select or create a branch, stage changes and commit. Check the returned full commit SHA.
4. For a writable test repository that permits deploy keys, generate a dedicated temporary key in the workspace terminal. Add the public key to the repository with write access, then load the private key into the temporary workspace agent.

```sh
ssh-keygen -t ed25519 -f /tmp/kamui-deploy-key
eval "$(ssh-agent -a /tmp/kamui-git-agent)"
ssh-add -t 15m /tmp/kamui-deploy-key
rm /tmp/kamui-deploy-key
git remote set-url --push origin git@github.com:OWNER/REPO.git
```

Push from the Console or terminal, then independently compare `refs/heads/BRANCH` on GitHub with the local SHA. Revoke the test deploy key. Stopping the workspace clears the SSH agent and temporary files. The Git dashboard accepts GitHub remotes and has no credential input. Initial cloning through the dashboard supports public GitHub repositories. If your organization restricts deploy keys, use an authorized disposable repository that permits a dedicated test key.

Stop and start the workspace, then confirm the file and Git history. Use the scripts to stop or start the deployment. Named volumes survive application restarts. Deleting a workspace removes its container and dashboard record while retaining its project volume for recovery. Back up the coordinator and workspace volumes before replacing the host. Preserve the project name when restarting an existing stack and keep older Docker data until you have checked it for files.

## Reproduce the Docker checks

For Linux/macOS using a Docker engine whose daemon can see the checkout directory:

```sh
umask 077
if [ ! -e ops/.env ]; then
  printf 'KAMUI_TOKEN=%s\nKAMUI_ADMIN_PASSWORD=%s\n' "$(openssl rand -hex 32)" "$(openssl rand -hex 32)" > ops/.env
fi
docker compose --env-file ops/.env -f ops/docker-compose.yml --profile tools config --quiet
docker compose --env-file ops/.env -f ops/docker-compose.yml build --no-cache --pull workspace-image api workspace-manager console ide-tunnel workspace-proxy
docker compose --env-file ops/.env -f ops/docker-compose.yml run --rm --no-deps tls-init
docker compose --env-file ops/.env -f ops/docker-compose.yml up -d --no-build --wait --wait-timeout 120
docker compose --env-file ops/.env -f ops/docker-compose.yml exec -T workspace-proxy nginx -t
curl --fail --cacert ops/tls-local/ca.pem https://127.0.0.1:8081/healthz
```

For CI-style lifecycle testing, start the commands above with `COMPOSE_PROJECT_NAME=kamui-ci-qa` exported, then run `KAMUI_QA_PROJECT=kamui-ci-qa python3 ops/tests/stack.py`. The test requires an empty project and exercises the public GitHub clone, Git operations, a disposable local Git remote, workspace limits and persistence. For a real GitHub push, set `KAMUI_QA_GITHUB_REPO` to a private disposable `OWNER/kamui-workspace-qa-DATE` repository and `KAMUI_QA_GITHUB_KEY` to its temporary write deploy key file. An authenticated `gh` CLI independently reads the pushed ref. Revoke the key after the test. Enable editor, terminal and WebSocket checks with the browser test. Run it against the same isolated project with Playwright and Chromium available locally.

```sh
KAMUI_TEST_ENV="$PWD/ops/.env" KAMUI_TEST_URL=https://127.0.0.1:8081 node ops/tests/browser.cjs
```

The browser test permits the local test certificate in its isolated Chromium context. The earlier `curl` probe verifies the CA and hostname separately.

## Operational boundaries

- This is a single-owner deployment. Only open repositories and extensions you trust. Dashboard and IDE share the HTTPS origin, and a signed-in browser can exercise the owner's permissions.
- A workspace gets 1 CPU, 2 GiB of memory and 256 processes by default. File storage uses the Docker disk, with no per-volume hard disk quota. The manager has daemon access and therefore must be treated as a privileged infrastructure service.
- Logout and session expiration close established IDE WebSockets and reject new connections. The private tunnel also closes sessions when it loses contact with Console authorization. Coordinator and workspace data survive restarts, and restarting the Console requires signing in again.
- Windows PowerShell 5.1, the target Windows Docker Desktop hosts and physical Android LAN access require testing on those devices. Desktop and mobile viewport browser tests can be run independently.


## Clean up an isolated QA instance

Run these commands from the QA checkout. Substitute the exact project name you used before running them.

```powershell
$env:COMPOSE_PROJECT_NAME = 'kamui-local-qa'
docker compose --project-name $env:COMPOSE_PROJECT_NAME --env-file .\ops\.env -f .\ops\docker-compose.yml ps -a
docker ps -a --filter "label=dev.nimbuslabs.kamui.project=$env:COMPOSE_PROJECT_NAME" --filter 'label=dev.nimbuslabs.kamui.workspace'
docker volume ls --filter "label=dev.nimbuslabs.kamui.project=$env:COMPOSE_PROJECT_NAME"
docker volume ls --filter "label=com.docker.compose.project=$env:COMPOSE_PROJECT_NAME"
powershell.exe -NoProfile -ExecutionPolicy RemoteSigned -File .\stop-kamui.ps1
Remove-Item Env:COMPOSE_PROJECT_NAME
```

Review the listed resources first. The stop command preserves project data. For disposable resources, remove only individually verified QA container IDs or volume names after checking their labels and contents. Revoke the QA repository deploy key and remove its dedicated key files and browser trust entry. To remove a Windows user trust entry, run `certutil.exe -user -store Root` and then `certutil.exe -user -delstore Root '<exact QA CA thumbprint>'`. Remove the QA checkout’s generated `.env` and `ops/tls-local` only after confirming their paths belong to the disposable checkout. Keep the production checkout and its persistent volumes separate.
