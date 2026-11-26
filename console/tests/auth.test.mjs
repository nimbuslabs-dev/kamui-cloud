import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { once } from 'node:events';
import { createServer } from 'node:http';
import { setTimeout as wait } from 'node:timers/promises';
import { test } from 'node:test';

async function listen(server) {
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  return server.address().port;
}

test('production Console enforces sessions, CSRF, IDE origins and login limits', async t => {
  const password = randomBytes(32).toString('hex');
  const token = randomBytes(32).toString('hex');
  let workspaceCalls = 0;
  const upstream = createServer(async (request, response) => {
    response.setHeader('Content-Type', 'application/json');
    if (request.headers.authorization !== 'Bearer ' + token || request.headers.cookie) {
      response.writeHead(401).end('{"error":{"code":"unauthorized","message":"Unauthorized"}}');
      return;
    }
    if (request.url === '/v1/nodes') {
      response.end('[]');
      return;
    }
    workspaceCalls++;
    if (request.method === 'POST' && request.url === '/v1/workspaces') {
      const chunks = [];
      for await (const chunk of request) chunks.push(chunk);
      response.writeHead(201).end(Buffer.concat(chunks));
      return;
    }
    response.writeHead(409).end('{"error":{"code":"conflict","message":"Fixture conflict"}}');
  });
  const apiPort = await listen(upstream);
  t.after(() => upstream.close());

  const reserved = createServer();
  const port = await listen(reserved);
  await new Promise(resolve => reserved.close(resolve));
  // Verify the timestamp check independently of the automatic expiry timer.
  const clock = `const now = Date.now; process.on('message', offset => {
    Date.now = () => now() + offset;
    process.send('clock advanced');
  });`;
  const child = spawn(process.execPath, ['--import', 'data:text/javascript,' + encodeURIComponent(clock), '.next/standalone/server.js'], {
    env: {
      ...process.env,
      PORT: String(port),
      HOSTNAME: '127.0.0.1',
      KAMUI_ADMIN_PASSWORD: password,
      KAMUI_TOKEN: token,
      KAMUI_API_URL: `http://127.0.0.1:${apiPort}`,
      KAMUI_SESSION_TTL_SECONDS: '5',
      NEXT_TELEMETRY_DISABLED: '1',
    },
    stdio: ['ignore', 'pipe', 'pipe', 'ipc'],
  });
  t.after(() => child.kill());
  let logs = '';
  child.stdout.on('data', data => { logs += data; });
  child.stderr.on('data', data => { logs += data; });

  const address = `127.0.0.1:${port}`;
  const origin = `https://${address}`;
  const request = (path, { method = 'GET', body, timeout = 1500, ...headers } = {}) =>
    fetch(`http://${address}${path}`, {
      method,
      headers: {
        'X-Forwarded-Proto': 'https',
        ...(body && { 'Content-Type': 'application/json' }),
        ...headers,
      },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(timeout),
    });

  let healthy = false;
  for (let tries = 0; tries < 50 && child.exitCode === null; tries++) {
    const response = await request('/api/health').catch(() => null);
    if (response?.ok) {
      await response.body?.cancel();
      healthy = true;
      break;
    }
    await wait(100);
  }
  assert.ok(healthy, logs);

  for (const endpoint of ['/api/workspaces', '/api/auth/me', '/api/auth/csrf', '/api/auth/ide']) {
    assert.equal((await request(endpoint)).status, 401);
  }
  assert.equal(workspaceCalls, 0);
  const login = (value, loginOrigin = origin) => request('/api/auth/login', {
    method: 'POST', body: { password: value }, Origin: loginOrigin,
  });
  assert.equal((await login(password, 'https://other.invalid')).status, 403);
  assert.equal((await login('invalid')).status, 401);
  const accepted = await login(password);
  assert.equal(accepted.status, 200);
  const setCookie = accepted.headers.get('set-cookie');
  assert.match(setCookie, /^__Host-kamui_session=[a-f0-9]+/);
  for (const flag of ['Secure', 'HttpOnly', 'SameSite=strict', 'Path=/']) {
    assert.ok(setCookie.toLowerCase().includes(flag.toLowerCase()));
  }
  const Cookie = setCookie.split(';', 1)[0];
  const { csrfToken } = await accepted.json();
  assert.equal((await request('/api/auth/me', { Cookie, 'X-Forwarded-Proto': 'http' })).status, 401);
  const validMutation = { Cookie, Origin: origin, 'X-CSRF-Token': csrfToken, method: 'POST' };
  const csrf = await request('/api/auth/csrf', { Cookie });
  assert.equal(csrf.status, 200);
  assert.equal((await csrf.json()).csrfToken, csrfToken);
  assert.equal((await request('/api/auth/ide', { Cookie })).status, 204);
  assert.equal((await request('/api/auth/ide', { Cookie, 'X-Kamui-Upgrade': 'websocket' })).status, 403);
  assert.equal((await request('/api/auth/ide', { Cookie, Origin: 'https://other.invalid', 'X-Kamui-Upgrade': 'websocket' })).status, 403);
  assert.equal((await request('/api/auth/ide', { Cookie, Origin: origin, 'X-Kamui-Upgrade': 'websocket' })).status, 204);
  assert.equal((await request('/api/auth/ide?watch=1', { Cookie, Origin: origin })).status, 403);
  const watch = async Cookie => {
    const lease = await request('/api/auth/ide?watch=1', {
      Cookie, Origin: origin, 'X-Kamui-Upgrade': 'websocket', timeout: 8000,
    });
    assert.equal(lease.status, 200);
    const reader = lease.body.getReader();
    const first = await reader.read();
    assert.equal(new TextDecoder().decode(first.value), '1');
    return reader;
  };
  assert.equal((await request('/api/workspaces', { ...validMutation, 'X-CSRF-Token': 'bad', body: {} })).status, 403);
  assert.equal((await request('/api/workspaces', { Cookie: Cookie + '; kamui_csrf=' + csrfToken, method: 'POST', Origin: origin, body: {} })).status, 403);
  assert.equal((await request('/api/workspaces', { ...validMutation, Origin: 'https://other.invalid', body: {} })).status, 403);
  assert.equal((await request('/api/workspaces/ws-12345678/unknown', validMutation)).status, 404);
  assert.equal(workspaceCalls, 0);
  const body = { name: 'Fixture workspace', repository_url: 'https://github.com/example/example' };
  const created = await request('/api/workspaces', { ...validMutation, body });
  assert.equal(created.status, 201);
  assert.deepEqual(await created.json(), body);
  const error = await request('/api/workspaces/ws-12345678', { Cookie, method: 'DELETE', Origin: origin, 'X-CSRF-Token': csrfToken });
  assert.equal(error.status, 409);
  assert.equal((await error.json()).error.message, 'Fixture conflict');

  assert.equal((await request('/api/auth/logout', { Cookie, Origin: origin, method: 'POST' })).status, 403);
  const [firstLease, secondLease] = await Promise.all([watch(Cookie), watch(Cookie)]);
  assert.equal((await request('/api/auth/logout', validMutation)).status, 200);
  assert.equal((await firstLease.read()).done, true);
  assert.equal((await secondLease.read()).done, true);
  assert.equal((await request('/api/auth/me', { Cookie })).status, 401);
  assert.equal((await request('/api/auth/ide', { Cookie })).status, 401);
  const expiring = await login(password);
  assert.equal(expiring.status, 200);
  const expiringCookie = expiring.headers.get('set-cookie').split(';', 1)[0];
  assert.equal((await request('/api/auth/me', { Cookie: expiringCookie })).status, 200);
  const expiringLease = await watch(expiringCookie);
  assert.equal((await expiringLease.read()).done, true);
  const clockSession = await login(password);
  const clockCookie = clockSession.headers.get('set-cookie').split(';', 1)[0];
  const advanced = once(child, 'message');
  child.send(9 * 60 * 60 * 1000);
  assert.equal((await advanced)[0], 'clock advanced');
  assert.equal((await request('/api/auth/me', { Cookie: clockCookie })).status, 401);
  assert.equal((await request('/api/auth/ide', { Cookie: clockCookie })).status, 401);

  for (let attempt = 0; attempt < 8; attempt++) {
    assert.equal((await login('invalid')).status, 401);
  }
  assert.equal((await login(password)).status, 429);
  assert.ok(!logs.includes(token) && !logs.includes(password));
});
