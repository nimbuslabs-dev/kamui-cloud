// Git Pull

import { NextRequest, NextResponse } from 'next/server';

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
    const { id } = await params;
    const body = await request.json();
    const response = await proxyRequest(`/v1/workspaces/${id}/git/pull`, {
      method: 'POST',
      body: JSON.stringify(body),
    });
    return new NextResponse(null, { status: response.status });
  } catch (error) {
    return NextResponse.json(
      { error: { code: 'internal', message: 'Failed to pull' } },
      { status: 500 }
    );
  }
}