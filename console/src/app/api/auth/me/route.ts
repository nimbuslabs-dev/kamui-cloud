import { NextRequest } from 'next/server';
import { requireSession, sessionResponse } from '@/lib/auth/session';

export function GET(request: NextRequest) {
  const auth = requireSession(request);
  return auth.error ?? sessionResponse({ authenticated: true, userId: 'kamui-user', csrfToken: auth.session.csrfToken });
}
