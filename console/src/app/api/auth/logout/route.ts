import { NextRequest } from 'next/server';
import { requireSession, signOut } from '@/lib/auth/session';

export function POST(request: NextRequest) {
  const auth = requireSession(request);
  return auth.error ?? signOut(request);
}
