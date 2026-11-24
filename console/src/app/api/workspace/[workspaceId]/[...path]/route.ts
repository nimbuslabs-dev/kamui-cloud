// Workspace Proxy - Proxies requests to code-server containers

import { NextRequest, NextResponse } from 'next/server';

const API_BASE = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080';
const TOKEN = process.env.KAMUI_TOKEN || '';

// In a real implementation, this would map workspace IDs to code-server container ports
// For MVP, we simulate with a placeholder
const WORKSPACE_PORTS: Record<string, number> = {};

function getAuthHeaders() {
  return {
    'Authorization': `Bearer ${TOKEN}`,
  };
}

async function fetchWorkspace(workspaceId: string): Promise<{ code_server_port: number; code_server_url: string } | null> {
  try {
    const response = await fetch(`${API_BASE}/v1/workspaces/${workspaceId}`, {
      headers: getAuthHeaders(),
    });
    if (!response.ok) return null;
    const data = await response.json();
    return {
      code_server_port: data.code_server_port,
      code_server_url: data.code_server_url,
    };
  } catch {
    return null;
  }
}

export async function GET(
  request: NextRequest,
  { params }: { params: Promise<{ workspaceId: string; path: string[] }> }
) {
  const { workspaceId, path } = await params;
  const pathString = path.join('/');
  const url = new URL(request.url);
  
  // For MVP: return a simulated response
  // In production, this would proxy to the actual code-server container
  const workspace = await fetchWorkspace(workspaceId);
  
  if (!workspace) {
    return NextResponse.json(
      { error: { code: 'not_found', message: 'Workspace not found' } },
      { status: 404 }
    );
  }
  
  if (workspace.code_server_port === 0) {
    return NextResponse.json(
      { error: { code: 'conflict', message: 'Workspace not running' } },
      { status: 409 }
    );
  }
  
  // For MVP: return info about the workspace
  // In production: proxy to http://workspace-{id}:{port}/{path}
  return NextResponse.json({
    message: 'Workspace proxy endpoint',
    workspaceId,
    path: pathString,
    codeServerUrl: workspace.code_server_url,
    note: 'In production, this proxies to the code-server container',
  });
}

export async function POST(
  request: NextRequest,
  { params }: { params: Promise<{ workspaceId: string; path: string[] }> }
) {
  const { workspaceId, path } = await params;
  const pathString = path.join('/');
  
  const workspace = await fetchWorkspace(workspaceId);
  
  if (!workspace) {
    return NextResponse.json(
      { error: { code: 'not_found', message: 'Workspace not found' } },
      { status: 404 }
    );
  }
  
  if (workspace.code_server_port === 0) {
    return NextResponse.json(
      { error: { code: 'conflict', message: 'Workspace not running' } },
      { status: 409 }
    );
  }
  
  // Proxy POST requests to code-server
  const body = await request.text();
  const targetUrl = `http://workspace-${workspaceId}:${workspace.code_server_port}/${pathString}`;
  
  try {
    const response = await fetch(targetUrl, {
      method: 'POST',
      headers: {
        'Content-Type': request.headers.get('Content-Type') || 'application/json',
        'Authorization': request.headers.get('Authorization') || '',
      },
      body,
    });
    
    const data = await response.text();
    return new NextResponse(data, {
      status: response.status,
      headers: response.headers,
    });
  } catch (error) {
    return NextResponse.json(
      { error: { code: 'internal', message: 'Failed to proxy to code-server' } },
      { status: 500 }
    );
  }
}

// Note: Next.js App Router doesn't directly support WebSocket upgrades
// In production, use a custom server or separate WebSocket proxy