// Stop Workspace

import { NextRequest, NextResponse } from 'next/server';
import { requireAuth, requireCsrf } from '@/lib/auth/session';

const API_BASE = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080';
const TOKEN = process.env.KAMUI_TOKEN || '';

function getAuthHeaders() {
  return {
    'Authorization': `Bearer ${TOKEN}`,
    'Content-Type': 'application/json',
  };
}

async function proxyRequest(path: string, options: RequestInit = {}): Promise<Response> {
  const url = `${API_BASE}${path}`;
  return fetch(url, {
    ...options,
    headers: { ...getAuthHeaders(), ...options.headers },
  });
}

export async function POST(
  request: NextRequest,
  { params }: { params: Promise<{ id: string }> }
) {
  try {
    const auth = requireAuth(request);
    if (auth.response) return auth.response;
    
    const csrfError = requireCsrf(request, auth.session);
    if (csrfError) return csrfError;
    
    const { id } = await params;
    const response = await proxyRequest(`/v1/workspaces/${id}/stop`, { method: 'POST' });
    const data = await response.json();
    return NextResponse.json(data, { status: response.status });
  } catch (error) {
    return NextResponse.json(
      { error: { code: 'internal', message: 'Failed to stop workspace' } },
      { status: 500 }
    );
  }
}