import { createHash, randomBytes, timingSafeEqual } from 'node:crypto';
import { NextRequest, NextResponse } from 'next/server';

const cookieName = '__Host-kamui_session';
const lifetimeSeconds = 8 * 60 * 60;
const failureLimit = 8;
const failureWindowMs = 60_000;

interface Session {
  csrfToken: string;
  expiresAt: number;
  revoked: AbortController;
  timer: ReturnType<typeof setTimeout>;
}

// Next.js can bundle route handlers separately. Share the revocable sessions
// across handlers in the one Console process. A restart signs everyone out.
const globals = globalThis as typeof globalThis & {
  kamuiAuth?: {
    sessions: Map<string, Session>;
    loginLimit: { failures: number; resetAt: number };
  };
};
const state = globals.kamuiAuth ??= {
  sessions: new Map(),
  loginLimit: { failures: 0, resetAt: 0 },
};

export function authConfiguration() {
  const password = process.env.KAMUI_ADMIN_PASSWORD;
  const token = process.env.KAMUI_TOKEN;
  const address = process.env.KAMUI_API_URL;
  if (!password || password.length < 24 || password.length > 256 ||
      !token || token.length < 32 || password === token || !address) return null;

  try {
    const api = new URL(address);
    if (!['http:', 'https:'].includes(api.protocol) || api.username || api.password ||
        api.pathname !== '/' || api.search || api.hash) return null;
    return { password, token, api: api.origin };
  } catch {
    return null;
  }
}

function digest(value: string) {
  return createHash('sha256').update(value).digest();
}

export function compareSecret(a: string, b: string): boolean {
  return timingSafeEqual(digest(a), digest(b));
}

export function apiError(status: number, code: string, message: string) {
  return NextResponse.json({ error: { code, message } }, {
    status,
    headers: { 'Cache-Control': 'no-store' },
  });
}

export function sessionResponse(data: object) {
  return NextResponse.json(data, { headers: { 'Cache-Control': 'no-store' } });
}

export function isSameOrigin(request: NextRequest): boolean {
  // The unpublished Console trusts only nginx to overwrite these headers.
  const host = request.headers.get('host');
  if (!host || request.headers.get('x-forwarded-proto') !== 'https') return false;
  try {
    return request.headers.get('origin') === new URL('https://' + host).origin;
  } catch {
    return false;
  }
}

export function loginRateLimit() {
  const now = Date.now();
  if (now >= state.loginLimit.resetAt) {
    state.loginLimit.failures = 0;
    state.loginLimit.resetAt = now + failureWindowMs;
  }
  if (state.loginLimit.failures < failureLimit) return null;
  const response = apiError(429, 'rate_limited', 'Too many login attempts. Try again in a minute.');
  response.headers.set('Retry-After', String(Math.ceil((state.loginLimit.resetAt - now) / 1000)));
  return response;
}

export function recordLoginFailure() {
  state.loginLimit.failures++;
}

export function sessionFromRequest(request: NextRequest): Session | null {
  const id = request.cookies.get(cookieName)?.value;
  if (request.headers.get('x-forwarded-proto') !== 'https' ||
      !id || !/^[a-f0-9]{64}$/.test(id) || !authConfiguration()) return null;
  const session = state.sessions.get(id);
  if (session && session.expiresAt <= Date.now()) {
    invalidateSession(id);
    return null;
  }
  return session ?? null;
}

function setSessionCookie(response: NextResponse, value: string, maxAge: number) {
  response.cookies.set(cookieName, value, {
    httpOnly: true,
    secure: true,
    sameSite: 'strict',
    maxAge,
    path: '/',
  });
}

export function signIn(request: NextRequest) {
  const now = Date.now();
  for (const [id, session] of state.sessions) {
    if (session.expiresAt <= now) invalidateSession(id);
  }
  revokeSession(request);
  if (state.sessions.size >= 32) {
    const oldest = state.sessions.keys().next().value;
    if (oldest) invalidateSession(oldest);
  }
  const id = randomBytes(32).toString('hex');
  const csrfToken = randomBytes(32).toString('hex');
  const configuredLifetime = Number(process.env.KAMUI_SESSION_TTL_SECONDS);
  const maxAge = Number.isInteger(configuredLifetime) && configuredLifetime >= 1 && configuredLifetime <= lifetimeSeconds
    ? configuredLifetime : lifetimeSeconds;
  state.sessions.set(id, {
    csrfToken, expiresAt: now + maxAge * 1000, revoked: new AbortController(),
    timer: setTimeout(() => invalidateSession(id), maxAge * 1000),
  });
  state.loginLimit.failures = 0;
  const response = sessionResponse({ success: true, csrfToken });
  setSessionCookie(response, id, maxAge);
  return response;
}

function revokeSession(request: NextRequest) {
  const id = request.cookies.get(cookieName)?.value;
  if (id) invalidateSession(id);
}

function invalidateSession(id: string) {
  const session = state.sessions.get(id);
  if (!session) return;
  state.sessions.delete(id);
  clearTimeout(session.timer);
  session.revoked.abort();
}

export function signOut(request: NextRequest) {
  revokeSession(request);
  const response = sessionResponse({ success: true });
  setSessionCookie(response, '', 0);
  return response;
}

export function requireSession(request: NextRequest) {
  const session = sessionFromRequest(request);
  if (!session) {
    return { error: apiError(401, 'unauthorized', 'Authentication required') };
  }
  if (request.method !== 'GET' && request.method !== 'HEAD') {
    const csrf = request.headers.get('x-csrf-token');
    if (!csrf || !isSameOrigin(request) || !compareSecret(csrf, session.csrfToken)) {
      return { error: apiError(403, 'csrf_invalid', 'Invalid request origin or CSRF token') };
    }
  }
  return { session };
}
