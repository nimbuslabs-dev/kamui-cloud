// CSRF token endpoint - returns current CSRF token for client-side use

import { NextRequest, NextResponse } from 'next/server';
import { getSessionFromRequest } from '@/lib/auth/session';

export async function GET(request: NextRequest) {
  try {
    const session = getSessionFromRequest(request);
    
    if (!session) {
      return NextResponse.json(
        { error: { code: 'unauthorized', message: 'Authentication required' } },
        { status: 401 }
      );
    }
    
    return NextResponse.json({ csrfToken: session.csrfToken });
  } catch (error) {
    return NextResponse.json(
      { error: { code: 'internal', message: 'Failed to get CSRF token' } },
      { status: 500 }
    );
  }
}