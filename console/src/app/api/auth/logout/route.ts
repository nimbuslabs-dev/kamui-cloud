// Logout endpoint - clears session and cookies

import { NextRequest, NextResponse } from 'next/server';
import { getSessionFromRequest, deleteSession, clearSessionCookies } from '@/lib/auth/session';

export async function POST(request: NextRequest) {
  try {
    const session = getSessionFromRequest(request);
    
    if (session) {
      // Delete session from store
      const cookieStore = request.cookies;
      const sessionCookie = cookieStore.get('kamui_session');
      if (sessionCookie?.value) {
        deleteSession(sessionCookie.value);
      }
    }
    
    const response = NextResponse.json({ success: true });
    clearSessionCookies(response);
    
    return response;
  } catch (error) {
    return NextResponse.json(
      { error: { code: 'internal', message: 'Logout failed' } },
      { status: 500 }
    );
  }
}