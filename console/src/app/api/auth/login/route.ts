import { NextRequest } from 'next/server';
import {
  apiError, authConfiguration, compareSecret, isSameOrigin,
  loginRateLimit, recordLoginFailure, signIn,
} from '@/lib/auth/session';

export async function POST(request: NextRequest) {
  if (!isSameOrigin(request)) return apiError(403, 'origin_invalid', 'Invalid request origin');
  const config = authConfiguration();
  if (!config) return apiError(503, 'unavailable', 'Authentication is not configured');
  const limited = loginRateLimit();
  if (limited) return limited;
  const body = await request.json().catch(() => null);
  if (!body || typeof body.password !== 'string' || body.password.length > 256) {
    return apiError(400, 'bad_request', 'A password is required');
  }
  const retryLimit = loginRateLimit();
  if (retryLimit) return retryLimit;
  if (!compareSecret(body.password, config.password)) {
    recordLoginFailure();
    return apiError(401, 'invalid_credentials', 'Invalid password');
  }
  return signIn(request);
}
