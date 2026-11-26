"""Exercise HTTPS, Docker lifecycle, and Git on an isolated Kamui Compose project."""
import http.client
import json
import os
from pathlib import Path
import re
import ssl
import subprocess
import time
from urllib.parse import urlsplit


project = os.environ["KAMUI_QA_PROJECT"]
if not project.startswith("kamui-") or not project.endswith("-qa"):
    raise SystemExit("Set KAMUI_QA_PROJECT to an isolated kamui-*-qa project.")
root = Path(__file__).resolve().parents[2]
env_file = Path(os.environ.get("KAMUI_QA_ENV_FILE", root / "ops/.env"))
url = urlsplit(os.environ.get("KAMUI_QA_URL", "https://127.0.0.1:8081"))
ca = Path(os.environ.get("KAMUI_QA_CA", root / "ops/tls-local/ca.pem"))
tls = ssl.create_default_context(cafile=ca)
password = os.environ.get("KAMUI_ADMIN_PASSWORD")
if not password:
    password = next(
        line.partition("=")[2].strip() for line in env_file.read_text().splitlines()
        if line.startswith("KAMUI_ADMIN_PASSWORD=")
    )

compose_prefix = [
    "docker", "compose", "-p", project, "--env-file", str(env_file),
    "-f", str(root / "ops/docker-compose.yml"),
]


def command(*args):
    return subprocess.check_output(args, text=True, timeout=180).strip()


def compose(*args):
    return command(*compose_prefix, *args)


class BrowserSession:
    cookie = None
    csrf = None

    def request(self, path, expected, method="GET", payload=None, headers=None):
        request_headers = {}
        if payload is not None:
            payload = json.dumps(payload)
            request_headers["Content-Type"] = "application/json"
        if method not in {"GET", "HEAD"}:
            request_headers["Origin"] = url.geturl().rstrip("/")
        if self.csrf:
            request_headers["X-CSRF-Token"] = self.csrf
        if self.cookie:
            request_headers["Cookie"] = self.cookie
        request_headers.update(headers or {})
        conn = http.client.HTTPSConnection(url.hostname, url.port, context=tls, timeout=165)
        try:
            conn.request(method, path, payload, headers=request_headers)
            response = conn.getresponse()
            data = response.read()
            is_json = "application/json" in (response.getheader("Content-Type") or "")
            payload = json.loads(data) if is_json else data
            error_code = payload.get("error", {}).get("code") if isinstance(payload, dict) else None
            assert response.status in expected, (method, path, "expected", expected, "got", response.status, error_code)
            if path == "/api/auth/login" and response.status == 200:
                cookie = response.getheader("Set-Cookie")
                for flag in ("Secure", "HttpOnly", "SameSite=strict", "Path=/"):
                    assert flag.lower() in cookie.lower(), flag
                self.cookie = cookie.split(";", 1)[0]
                token = json.loads(data).get("csrfToken")
                if not token:
                    token = self.request("/api/auth/csrf", {200})["csrfToken"]
                self.csrf = token
            return payload
        finally:
            conn.close()

    def login(self):
        self.request("/api/auth/login", {200}, "POST", {"password": password})


def main():
    assert password, "Password missing"
    public = BrowserSession()
    public.request("/healthz", {200})
    public.request("/api/workspaces", {401})
    public.request("/api/auth/ide?watch=1", {404})
    public.request("/api/auth/login", {401}, "POST", {"password": "wrong-password"})
    test_path = "/workspace/ws-00000000/"
    public.request(test_path, {401})
    ws_headers = {
        "Connection": "Upgrade",
        "Upgrade": "websocket",
        "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ==",
        "Sec-WebSocket-Version": "13",
        "Origin": url.geturl().rstrip("/"),
    }
    public.request(test_path, {401}, headers=ws_headers)
    public.login()
    public.request(test_path, {403}, headers={**ws_headers, "Origin": "https://untrusted.invalid"})

    base = "/api/workspaces"
    new = {"name": "Automated Git smoke", "repository_url": "https://github.com/octocat/Hello-World.git"}
    public.request(base, {403}, "POST", new, {"X-CSRF-Token": ""})
    public.request(base, {403}, "POST", new, {"Origin": "https://untrusted.invalid"})
    public.request(base, {400}, "POST", {**new, "repository_url": "https://example.invalid/demo.git"})
    assert public.request(base, {200}) == [], "Test project must start with no workspaces."
    ws = public.request(base, {201}, "POST", new)
    path = f"{base}/{ws['id']}"
    ide = f"/workspace/{ws['id']}/"
    started = time.monotonic()
    ws = public.request(path + "/start", {200}, "POST")
    elapsed = time.monotonic() - started
    assert ws["status"] == "running"
    assert ws["code_server_url"] == ide
    print(f"Workspace created and code-server healthy in {elapsed:.2f}s", flush=True)
    container = ws["container_id"]

    inspected = json.loads(command("docker", "inspect", container))[0]
    limits = inspected["HostConfig"]
    assert not limits["PortBindings"] and limits["NanoCpus"] == 1_000_000_000
    assert limits["Memory"] == 2 * 1024**3 and limits["PidsLimit"] == 256
    assert not limits["Privileged"] and inspected["Config"]["User"] == "1000:1000"
    assert not any(env.startswith(("KAMUI_TOKEN=", "KAMUI_ADMIN_PASSWORD=", "DOCKER_HOST="))
                   for env in inspected["Config"]["Env"])
    assert set(inspected["NetworkSettings"]["Networks"]) == {project + "-ide"}
    assert len(inspected["Mounts"]) == 1 and inspected["Mounts"][0]["Type"] == "volume"
    for service in ("api", "workspace-manager", "console", "ide-tunnel"):
        assert not json.loads(command("docker", "inspect", compose("ps", "-q", service)))[0]["HostConfig"]["PortBindings"]
    public.request(ide, {200, 302})
    BrowserSession().request(ide, {401})

    def run(*args):
        return command("docker", "exec", "-u", "1000", "-w", "/home/coder/project", container, *args)

    assert run("git", "rev-parse", "--is-inside-work-tree") == "true"
    status = public.request(path + "/git/status", {200})
    assert status["branch"]
    other = public.request(base, {201}, "POST", {**new, "name": "Concurrency check"})
    other_path = f"{base}/{other['id']}"
    public.request(other_path + "/start", {409}, "POST")
    public.request(other_path, {204}, "DELETE")
    public.request(path + "/git/pull", {204}, "POST")
    public.request(path + "/git/branch", {204}, "POST", {"branch": "kamui-smoke"})
    public.request(path + "/git/push", {400, 409, 422, 502}, "POST", {"remote": "invalid-remote"})
    run("git", "config", "user.name", "Kamui smoke test")
    run("git", "config", "user.email", "kamui-smoke@example.invalid")
    run("sh", "-c", "printf 'workspace persistence\n' > acceptance.txt")
    assert "acceptance.txt" in public.request(path + "/git/status", {200})["untracked"]
    public.request(path + "/git/add", {204}, "POST", {"files": ["acceptance.txt"]})
    result = public.request(path + "/git/commit", {200}, "POST", {"message": "Verify Kamui workspace"})
    sha = result["hash"]
    assert re.fullmatch("[a-f0-9]{40,64}", sha) and run("git", "rev-parse", "HEAD") == sha
    run("git", "init", "--bare", "/tmp/kamui-remote.git")
    # Dashboard transfers are restricted to GitHub. Use a disposable local
    # remote for the CI transfer and verify the ref from the receiving repo.
    run("git", "push", "/tmp/kamui-remote.git", "HEAD:refs/heads/kamui-smoke")
    remote_sha = run("git", "--git-dir=/tmp/kamui-remote.git", "rev-parse", "refs/heads/kamui-smoke")
    assert remote_sha == sha
    print(f"Verified disposable bare remote commit {remote_sha}", flush=True)
    github = os.environ.get("KAMUI_QA_GITHUB_REPO")
    if github:
        assert re.fullmatch(r"[A-Za-z0-9-]+/kamui-workspace-qa-[0-9]+", github)
        run("git", "remote", "set-url", "--push", "origin", f"git@github.com:{github}.git")
        github_branch = "kamui-smoke-" + ws["id"][3:11]
        denied = public.request(path + "/git/push", {422}, "POST", {"branch": github_branch})
        assert denied["error"]["code"] == "git_auth", denied["error"]["code"]
        key = Path(os.environ["KAMUI_QA_GITHUB_KEY"]).read_bytes()
        run("ssh-agent", "-a", "/tmp/kamui-git-agent")
        subprocess.run(
            ["docker", "exec", "-i", "-u", "1000", container, "ssh-add", "-t", "900", "-"],
            input=key, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True, timeout=15,
        )
        public.request(path + "/git/push", {204}, "POST", {"branch": github_branch})
        confirmed = command("gh", "api", f"repos/{github}/git/ref/heads/{github_branch}", "--jq", ".object.sha")
        assert confirmed == sha, (confirmed, sha)
        print(f"Independently verified GitHub ref {github}:refs/heads/{github_branch} at {confirmed}", flush=True)

    stats = ("docker", "stats", "--no-stream", "--format", "{{.Name}} {{.CPUPerc}} {{.MemUsage}}")
    print("Idle:", command(*stats, container), flush=True)
    with subprocess.Popen(["docker", "exec", "-u", "1000", container, "timeout", "6", "dd",
                           "if=/dev/zero", "of=/dev/null", "bs=1M", "status=none"]) as load:
        print("Active:", command(*stats, container), flush=True)
        load.wait(timeout=10)

    public.request(path + "/stop", {200}, "POST")
    assert public.request(path, {200})["status"] == "stopped"
    public.request(path + "/start", {200}, "POST")
    assert run("cat", "acceptance.txt") == "workspace persistence"
    assert run("git", "rev-parse", "HEAD") == sha
    public.request(path + "/restart", {200}, "POST")
    assert run("git", "rev-parse", "HEAD") == sha
    compose("restart", "api", "workspace-manager", "console", "ide-tunnel", "workspace-proxy")
    compose("up", "-d", "--no-build", "--wait", "--wait-timeout", "90")
    public.request(path, {401})
    public.login()
    assert public.request(path, {200})["container_id"] == container
    assert run("cat", "acceptance.txt") == "workspace persistence"

    public.request(path + "/stop", {200}, "POST")
    command("docker", "rm", container)
    replacement = public.request(path + "/start", {200}, "POST")["container_id"]
    assert replacement != container
    container = replacement
    assert run("cat", "acceptance.txt") == "workspace persistence"
    assert run("git", "rev-parse", "HEAD") == sha

    public.request(path, {204}, "DELETE")
    assert public.request(base, {200}) == []
    command("docker", "volume", "inspect", inspected["Mounts"][0]["Name"])
    public.request("/api/auth/logout", {200, 204}, "POST")
    public.request(base, {401})
    print("PASS: HTTPS, auth, CSRF, workspace isolation, Git, restart, persistence and retained volume.")


if __name__ == "__main__":
    main()
