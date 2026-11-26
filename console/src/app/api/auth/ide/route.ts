import { NextRequest } from 'next/server';
import { apiError, isSameOrigin, sessionFromRequest } from '@/lib/auth/session';

export function GET(request: NextRequest) {
  const session = sessionFromRequest(request);
  if (!session) return apiError(401, 'unauthorized', 'Authentication required');
  const websocket = request.headers.get('x-kamui-upgrade')?.toLowerCase() === 'websocket';
  const watch = request.nextUrl.searchParams.get('watch') === '1';
  if (request.headers.get('x-forwarded-proto') !== 'https' ||
      ((request.headers.has('origin') || websocket || watch) && !isSameOrigin(request)) ||
      (watch && !websocket)) {
    return apiError(403, 'origin_invalid', 'Invalid request origin');
  }
  if (watch) {
    const invalid = AbortSignal.any([request.signal, session.revoked.signal]);
    const heartbeat = Uint8Array.of(49); // The tunnel expects the byte '1'.
    let cleanup = () => {};
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        if (invalid.aborted) {
          controller.close();
          return;
        }
        const close = () => {
          cleanup();
          controller.close();
        };
        const interval = setInterval(() => {
          if (!sessionFromRequest(request)) {
            if (!invalid.aborted) close();
          }
          else if ((controller.desiredSize ?? 0) > 0) controller.enqueue(heartbeat);
        }, 5000);
        cleanup = () => {
          clearInterval(interval);
          invalid.removeEventListener('abort', close);
        };
        invalid.addEventListener('abort', close, { once: true });
        controller.enqueue(heartbeat);
      },
      cancel() { cleanup(); },
    });
    return new Response(body, {
      headers: { 'Cache-Control': 'no-store', 'Content-Type': 'application/octet-stream' },
    });
  }
  return new Response(null, { status: 204, headers: { 'Cache-Control': 'no-store' } });
}
