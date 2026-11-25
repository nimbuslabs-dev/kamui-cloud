// Session-based authentication for Kamui Console
// Single-user MVP with secure HttpOnly cookie sessions

import { cookies } from 'next/headers';
import { NextRequest, NextResponse } from 'next/server';

// Session configuration
const SESSION_COOKIE_NAME = 'kamui_session';
const SESSION_TTL_MS = 24 * 60 * 60 * 1000; // 24 hours
const CSRF_COOKIE_NAME = 'kamui_csrf';
const CSRF_HEADER_NAME = 'x-csrf-token';

// In-memory session store (single-user MVP)
interface SessionData {
  userId: string;
  createdAt: number;
  expiresAt: number;
  csrfToken: string;
}

const sessions = new Map<string, SessionData>();

// Generate secure random token
function generateToken(length: number = 32): string {
  const bytes = new Uint8Array(length);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, b => b.toString(16).padStart(2, '0')).join('');
}

// Generate CSRF token
function generateCsrfToken(): string {
  return generateToken(32);
}

// Create a new session
export function createSession(userId: string = 'kamui-user'): { sessionId: string; csrfToken: string } {
  const sessionId = generateToken(32);
  const csrfToken = generateCsrfToken();
  const now = Date.now();
  
  sessions.set(sessionId, {
    userId,
    createdAt: now,
    expiresAt: now + SESSION_TTL_MS,
    csrfToken,
  });
  
  return { sessionId, csrfToken };
}

// Validate session and return session data if valid
export function validateSession(sessionId: string): SessionData | null {
  const session = sessions.get(sessionId);
  if (!session) return null;
  
  if (Date.now() > session.expiresAt) {
    sessions.delete(sessionId);
    return null;
  }
  
  return session;
}

// Delete session (logout)
export function deleteSession(sessionId: string): void {
  sessions.delete(sessionId);
}

// Get session from request cookies
export function getSessionFromRequest(request: NextRequest): SessionData | null {
  const cookieStore = request.cookies;
  const sessionCookie = cookieStore.get(SESSION_COOKIE_NAME);
  
  if (!sessionCookie?.value) return null;
  
  return validateSession(sessionCookie.value);
}

// Set session cookie in response
export function setSessionCookie(response: NextResponse, sessionId: string, csrfToken: string): void {
  response.cookies.set(SESSION_COOKIE_NAME, sessionId, {
    httpOnly: true,
    secure: process.env.NODE_ENV === 'production',
    sameSite: 'lax',
    maxAge: SESSION_TTL_MS / 1000,
    path: '/',
  });
  
  response.cookies.set(CSRF_COOKIE_NAME, csrfToken, {
    httpOnly: true,
    secure: process.env.NODE_ENV === 'production',
    sameSite: 'lax',
    maxAge: SESSION_TTL_MS / 1000,
    path: '/',
  });
}

// Clear session cookies
export function clearSessionCookies(response: NextResponse): void {
  response.cookies.set(SESSION_COOKIE_NAME, '', {
    httpOnly: true,
    secure: process.env.NODE_ENV === 'production',
    sameSite: 'lax',
    maxAge: 0,
    path: '/',
  });
  response.cookies.set(CSRF_COOKIE_NAME, '', {
    httpOnly: true,
    secure: process.env.NODE_ENV === 'production',
    sameSite: 'lax',
    maxAge: 0,
    path: '/',
  });
}

// Verify CSRF token from request
export function verifyCsrfToken(request: NextRequest, session: SessionData): boolean {
  // Check header first (for API calls)
  const headerToken = request.headers.get(CSRF_HEADER_NAME);
  if (headerToken && headerToken === session.csrfToken) {
    return true;
  }
  
  // Check cookie (for form submissions)
  const cookieToken = request.cookies.get(CSRF_COOKIE_NAME)?.value;
  if (cookieToken && cookieToken === session.csrfToken) {
    return true;
  }
  
  return false;
}

// Require authentication middleware
export function requireAuth(request: NextRequest): { session: SessionData; response?: NextResponse } {
  const session = getSessionFromRequest(request);
  
  if (!session) {
    const response = NextResponse.json(
      { error: { code: 'unauthorized', message: 'Authentication required' } },
      { status: 401 }
    );
    return { session: null as any, response };
  }
  
  return { session };
}

// Require CSRF protection for state-changing methods
export function requireCsrf(request: NextRequest, session: SessionData): NextResponse | null {
  const method = request.method.toUpperCase();
  if (['GET', 'HEAD', 'OPTIONS'].includes(method)) {
    return null; // No CSRF needed for safe methods
  }
  
  if (!verifyCsrfToken(request, session)) {
    return NextResponse.json(
      { error: { code: 'csrf_invalid', message: 'Invalid or missing CSRF token' } },
      { status: 403 }
    );
  }
  
  return null;
}

// Get CSRF token for client-side use
export async function getCsrfToken(): Promise<string | null> {
  const cookieStore = await cookies();
  const sessionCookie = cookieStore.get(SESSION_COOKIE_NAME);
  
  if (!sessionCookie?.value) return null;
  
  const session = validateSession(sessionCookie.value);
  return session?.csrfToken || null;
}

// Clean up expired sessions (call periodically)
export function cleanupExpiredSessions(): void {
  const now = Date.now();
  const keysToDelete: string[] = [];
  sessions.forEach((session, sessionId) => {
    if (now > session.expiresAt) {
      keysToDelete.push(sessionId);
    }
  });
  keysToDelete.forEach(key => sessions.delete(key));
}