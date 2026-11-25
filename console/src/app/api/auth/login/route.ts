// Login endpoint - creates session and sets HttpOnly cookies

import { NextRequest, NextResponse } from 'next/server';
import { createSession, setSessionCookie } from '@/lib/auth/session';

const ADMIN_PASSWORD = process.env.KAMUI_ADMIN_PASSWORD || 'changeme';

// Simple password check (in production, use proper password hashing)
function verifyPassword(password: string): boolean {
  return password === ADMIN_PASSWORD;
}

export async function POST(request: NextRequest) {
  try {
    const body = await request.json();
    const { password } = body;
    
    if (!password || !verifyPassword(password)) {
      return NextResponse.json(
        { error: { code: 'invalid_credentials', message: 'Invalid password' } },
        { status: 401 }
      );
    }
    
    // Create new session
    const { sessionId, csrfToken } = createSession();
    
    const response = NextResponse.json({
      success: true,
      csrfToken, // Return CSRF token for client-side use
    });
    
    // Set HttpOnly cookies
    setSessionCookie(response, sessionId, csrfToken);
    
    return response;
  } catch (error) {
    return NextResponse.json(
      { error: { code: 'internal', message: 'Login failed' } },
      { status: 500 }
    );
  }
}