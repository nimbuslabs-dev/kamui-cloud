// Workspace Proxy - Proxies requests to code-server containers
// Note: Actual proxying is handled by nginx in production.
// This endpoint provides workspace metadata only.

import { NextRequest, NextResponse } from 'next/server';

const API_BASE = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080';
const TOKEN = process.env.KAMUI_TOKEN || '';

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
  const { workspaceId } = await params;
  
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
  
  // Return workspace info - actual proxying is handled by nginx
  return NextResponse.json({
    workspaceId,
    codeServerUrl: workspace.code_server_url,
    codeServerPort: workspace.code_server_port,
  });
}

// Note: All proxying to code-server (including WebSocket) is handled by nginx
// This endpoint only provides workspace metadata