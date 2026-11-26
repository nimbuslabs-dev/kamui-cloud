import { NextRequest } from 'next/server';
import { apiError, authConfiguration, requireSession } from '@/lib/auth/session';

type Context = { params: Promise<{ segments?: string[] }> };

function allowed(parts: string[], method: string) {
  const [id, action, gitCommand] = parts;
  if (parts.length === 0) return method === 'GET' || method === 'POST';
  if (!/^ws-[a-f0-9]{8,32}$/.test(id)) return false;
  if (parts.length === 1) return ['GET', 'DELETE'].includes(method);
  if (parts.length === 2) return method === 'POST' && ['start', 'stop', 'restart'].includes(action);
  return parts.length === 3 && action === 'git' &&
    (method === 'GET' && gitCommand === 'status' ||
     method === 'POST' && ['add', 'branch', 'commit', 'push', 'pull'].includes(gitCommand));
}

async function proxy(request: NextRequest, { params }: Context) {
  const auth = requireSession(request);
  if (auth.error) return auth.error;
  const config = authConfiguration();
  if (!config) return apiError(503, 'unavailable', 'Kamui services are not ready');

  const { segments = [] } = await params;
  if (!allowed(segments, request.method)) return apiError(404, 'not_found', 'Workspace route not found');
  const path = ['/v1/workspaces', ...segments].join('/');
  const headers = new Headers({ Authorization: 'Bearer ' + config.token });
  if (request.body) headers.set('Content-Type', 'application/json');

  try {
    const upstream = await fetch(config.api + path, {
      method: request.method,
      headers,
      body: request.body,
      ...(request.body ? { duplex: 'half' } : {}),
      cache: 'no-store',
      redirect: 'error',
      signal: AbortSignal.any([request.signal, AbortSignal.timeout(180_000)]),
    });
    const responseHeaders = new Headers({ 'Cache-Control': 'no-store' });
    if (upstream.status !== 204) {
      if (!upstream.headers.get('content-type')?.startsWith('application/json')) {
        await upstream.body?.cancel();
        return apiError(502, 'upstream_error', 'The workspace service returned an invalid response');
      }
      responseHeaders.set('Content-Type', 'application/json');
    }
    return new Response(upstream.body, { status: upstream.status, headers: responseHeaders });
  } catch {
    return apiError(502, 'upstream_error', 'Could not reach the workspace service');
  }
}

export { proxy as GET, proxy as POST, proxy as DELETE };
