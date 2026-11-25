// Session validation endpoint - returns current user info

import { NextRequest, NextResponse } from 'next/server';
import { getSessionFromRequest } from '@/lib/auth/session';

export async function GET(request: NextRequest) {
  try {
    const session = getSessionFromRequest(request);
    
    if (!session) {
      return NextResponse.json(
        { error: { code: 'unauthorized', message: 'Not authenticated' } },
        { status: 401 }
      );
    }
    
    return NextResponse.json({
      authenticated: true,
      userId: session.userId,
      csrfToken: session.csrfToken,
    });
  } catch (error) {
    return NextResponse.json(
      { error: { code: 'internal', message: 'Session check failed' } },
      { status: 500 }
    );
  }
}