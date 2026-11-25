// API Client for Kamui Cloud - Server-side only via Next.js API routes
// This module provides type-safe API calls that go through Next.js API routes
// to keep the KAMUI_TOKEN server-side and never expose it to the browser.

import type {
  Node,
  Workspace,
  WorkspaceCreate,
  Job,
  JobCreate,
  GitStatus,
  HealthResponse,
  ApiError,
  RegistrationResponse,
} from '@/types';

// Base URL for Next.js API routes (relative, works in browser and server)
const API_BASE = '/api';

function getHeaders(): HeadersInit {
  return {
    'Content-Type': 'application/json',
  };
}

async function handleResponse<T>(response: Response): Promise<T> {
  if (!response.ok) {
    const error: ApiError = await response.json().catch(() => ({
      error: { code: 'unknown', message: response.statusText },
    }));
    throw new Error(error.error.message || `HTTP ${response.status}`);
  }
  if (response.status === 204) {
    return undefined as T;
  }
  return response.json();
}

// Health
export async function checkHealth(): Promise<HealthResponse> {
  const response = await fetch(`${API_BASE}/healthz`, {
    method: 'GET',
    headers: { 'Content-Type': 'application/json' },
  });
  return handleResponse<HealthResponse>(response);
}

// Workspaces
export async function listWorkspaces(): Promise<Workspace[]> {
  const response = await fetch(`${API_BASE}/workspaces`, {
    method: 'GET',
    headers: getHeaders(),
  });
  return handleResponse<Workspace[]>(response);
}

export async function createWorkspace(workspace: WorkspaceCreate): Promise<Workspace> {
  const response = await fetch(`${API_BASE}/workspaces`, {
    method: 'POST',
    headers: getHeaders(),
    body: JSON.stringify(workspace),
  });
  return handleResponse<Workspace>(response);
}

export async function getWorkspace(workspaceId: string): Promise<Workspace> {
  const response = await fetch(`${API_BASE}/workspaces/${workspaceId}`, {
    method: 'GET',
    headers: getHeaders(),
  });
  return handleResponse<Workspace>(response);
}

export async function startWorkspace(workspaceId: string): Promise<Workspace> {
  const response = await fetch(`${API_BASE}/workspaces/${workspaceId}/start`, {
    method: 'POST',
    headers: getHeaders(),
  });
  return handleResponse<Workspace>(response);
}

export async function stopWorkspace(workspaceId: string): Promise<Workspace> {
  const response = await fetch(`${API_BASE}/workspaces/${workspaceId}/stop`, {
    method: 'POST',
    headers: getHeaders(),
  });
  return handleResponse<Workspace>(response);
}

export async function deleteWorkspace(workspaceId: string): Promise<void> {
  const response = await fetch(`${API_BASE}/workspaces/${workspaceId}`, {
    method: 'DELETE',
    headers: getHeaders(),
  });
  await handleResponse<void>(response);
}

// Git operations
export async function getGitStatus(workspaceId: string): Promise<GitStatus> {
  const response = await fetch(`${API_BASE}/workspaces/${workspaceId}/git/status`, {
    method: 'GET',
    headers: getHeaders(),
  });
  return handleResponse<GitStatus>(response);
}

export async function gitAdd(workspaceId: string, files: string[]): Promise<void> {
  const response = await fetch(`${API_BASE}/workspaces/${workspaceId}/git/add`, {
    method: 'POST',
    headers: getHeaders(),
    body: JSON.stringify({ files }),
  });
  await handleResponse<void>(response);
}

export async function gitCommit(workspaceId: string, message: string): Promise<{ hash: string }> {
  const response = await fetch(`${API_BASE}/workspaces/${workspaceId}/git/commit`, {
    method: 'POST',
    headers: getHeaders(),
    body: JSON.stringify({ message }),
  });
  return handleResponse<{ hash: string }>(response);
}

export async function gitPush(workspaceId: string, remote?: string, branch?: string): Promise<void> {
  const response = await fetch(`${API_BASE}/workspaces/${workspaceId}/git/push`, {
    method: 'POST',
    headers: getHeaders(),
    body: JSON.stringify({ remote, branch }),
  });
  await handleResponse<void>(response);
}

export async function gitPull(workspaceId: string, remote?: string, branch?: string): Promise<void> {
  const response = await fetch(`${API_BASE}/workspaces/${workspaceId}/git/pull`, {
    method: 'POST',
    headers: getHeaders(),
    body: JSON.stringify({ remote, branch }),
  });
  await handleResponse<void>(response);
}

// Nodes (for agent registration)
export async function registerNode(nodeInfo: Omit<Node, 'id' | 'status' | 'last_seen'>): Promise<RegistrationResponse> {
  const response = await fetch(`${API_BASE}/nodes/register`, {
    method: 'POST',
    headers: getHeaders(),
    body: JSON.stringify(nodeInfo),
  });
  return handleResponse<RegistrationResponse>(response);
}

export async function listNodes(): Promise<Node[]> {
  const response = await fetch(`${API_BASE}/nodes`, {
    method: 'GET',
    headers: getHeaders(),
  });
  return handleResponse<Node[]>(response);
}

export async function heartbeatNode(nodeId: string, load?: { cpu_pct?: number; mem_pct?: number }): Promise<void> {
  const response = await fetch(`${API_BASE}/nodes/${nodeId}/heartbeat`, {
    method: 'POST',
    headers: getHeaders(),
    body: JSON.stringify(load || {}),
  });
  await handleResponse<void>(response);
}

export async function deleteNode(nodeId: string): Promise<void> {
  const response = await fetch(`${API_BASE}/nodes/${nodeId}`, {
    method: 'DELETE',
    headers: getHeaders(),
  });
  await handleResponse<void>(response);
}

// Jobs
export async function listJobs(): Promise<Job[]> {
  const response = await fetch(`${API_BASE}/jobs`, {
    method: 'GET',
    headers: getHeaders(),
  });
  return handleResponse<Job[]>(response);
}

export async function createJob(job: JobCreate): Promise<Job> {
  const response = await fetch(`${API_BASE}/jobs`, {
    method: 'POST',
    headers: getHeaders(),
    body: JSON.stringify(job),
  });
  return handleResponse<Job>(response);
}

export async function claimJob(nodeId: string): Promise<Job | null> {
  const response = await fetch(`${API_BASE}/jobs/claim`, {
    method: 'POST',
    headers: getHeaders(),
    body: JSON.stringify({ node_id: nodeId }),
  });
  if (response.status === 204) return null;
  return handleResponse<Job>(response);
}

export async function getJob(jobId: string): Promise<Job> {
  const response = await fetch(`${API_BASE}/jobs/${jobId}`, {
    method: 'GET',
    headers: getHeaders(),
  });
  return handleResponse<Job>(response);
}

export async function finishJob(jobId: string, exitCode: number, logs?: string): Promise<Job> {
  const response = await fetch(`${API_BASE}/jobs/${jobId}/finish`, {
    method: 'POST',
    headers: getHeaders(),
    body: JSON.stringify({ exit_code: exitCode, logs }),
  });
  return handleResponse<Job>(response);
}

// Utility to get local IP for display
export function getLocalIP(): Promise<string> {
  return fetch('https://api.ipify.org?format=json')
    .then(r => r.json())
    .then(d => d.ip)
    .catch(() => 'unknown');
}