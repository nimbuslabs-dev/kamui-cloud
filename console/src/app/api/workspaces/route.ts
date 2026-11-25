// Workspaces API Routes - Proxy to Kamui Go API (server-side only, with auth)

import { NextRequest, NextResponse } from 'next/server';
import { requireAuth, requireCsrf, getSessionFromRequest } from '@/lib/auth/session';

const API_BASE = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080';
const TOKEN = process.env.KAMUI_TOKEN || '';

function getAuthHeaders() {
  return {
    'Authorization': `Bearer ${TOKEN}`,
    'Content-Type': 'application/json',
  };
}

async function proxyRequest(
  path: string,
  options: RequestInit = {}
): Promise<Response> {
  const url = `${API_BASE}${path}`;
  return fetch(url, {
    ...options,
    headers: {
      ...getAuthHeaders(),
      ...options.headers,
    },
  });
}

// GET /api/workspaces - List all workspaces
export async function GET(request: NextRequest) {
  try {
    // Require authentication
    const auth = requireAuth(request);
    if (auth.response) return auth.response;
    
    const response = await proxyRequest('/v1/workspaces', { method: 'GET' });
    const data = await response.json();
    return NextResponse.json(data, { status: response.status });
  } catch (error) {
    return NextResponse.json(
      { error: { code: 'internal', message: 'Failed to fetch workspaces' } },
      { status: 500 }
    );
  }
}

// POST /api/workspaces - Create a new workspace
export async function POST(request: NextRequest) {
  try {
    // Require authentication
    const auth = requireAuth(request);
    if (auth.response) return auth.response;
    
    // Require CSRF for state-changing operation
    const csrfError = requireCsrf(request, auth.session);
    if (csrfError) return csrfError;
    
    const body = await request.json();
    const response = await proxyRequest('/v1/workspaces', {
      method: 'POST',
      body: JSON.stringify(body),
    });
    const data = await response.json();
    return NextResponse.json(data, { status: response.status });
  } catch (error) {
    return NextResponse.json(
      { error: { code: 'internal', message: 'Failed to create workspace' } },
      { status: 500 }
    );
  }
}