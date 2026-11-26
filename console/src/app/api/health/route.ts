import { apiError, authConfiguration, sessionResponse } from '@/lib/auth/session';

export const dynamic = 'force-dynamic';

export async function GET() {
  const config = authConfiguration();
  if (config) {
    try {
      const upstream = await fetch(config.api + '/v1/nodes', {
        headers: { Authorization: 'Bearer ' + config.token },
        cache: 'no-store',
        redirect: 'error',
        signal: AbortSignal.timeout(3000),
      });
      await upstream.body?.cancel();
      if (upstream.ok) return sessionResponse({ status: 'ok' });
    } catch {
      // Keep internal network details out of the public response.
    }
  }
  return apiError(503, 'unavailable', 'Kamui services are not ready');
}
