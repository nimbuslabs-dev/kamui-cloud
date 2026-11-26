import { NextRequest } from 'next/server';
import { apiError, isSameOrigin, sessionFromRequest } from '@/lib/auth/session';

export function GET(request: NextRequest) {
  if (!sessionFromRequest(request)) return apiError(401, 'unauthorized', 'Authentication required');
  if (request.headers.get('x-forwarded-proto') !== 'https' ||
      ((request.headers.has('origin') || request.headers.get('x-kamui-upgrade')?.toLowerCase() === 'websocket') &&
        !isSameOrigin(request))) {
    return apiError(403, 'origin_invalid', 'Invalid request origin');
  }
  return new Response(null, { status: 204, headers: { 'Cache-Control': 'no-store' } });
}
