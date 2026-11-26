// API Client for Kamui Cloud - Uses Next.js API routes with cookie-based auth
// The KAMUI_TOKEN is never exposed to the browser - all calls go through Next.js API routes

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

async function mutationHeaders(): Promise<HeadersInit> {
  return { ...getHeaders(), 'X-CSRF-Token': await getCsrfToken() };
}

export class ApiRequestError extends Error {
  constructor(public status: number, message: string) {
    super(message);
  }
}

async function handleResponse<T>(response: Response): Promise<T> {
  if (!response.ok) {
    const error: ApiError = await response.json().catch(() => ({
      error: { code: 'unknown', message: response.statusText },
    }));
    throw new ApiRequestError(response.status, error?.error?.message || `HTTP ${response.status}`);
  }
  if (response.status === 204) {
    return undefined as T;
  }
  return response.json();
}

// Auth endpoints
export async function login(password: string): Promise<{ csrfToken: string }> {
  const response = await fetch(`${API_BASE}/auth/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password }),
    credentials: 'include', // Include cookies
  });
  return handleResponse<{ csrfToken: string }>(response);
}

export async function logout(): Promise<void> {
  const response = await fetch(`${API_BASE}/auth/logout`, {
    method: 'POST',
    headers: await mutationHeaders(),
    credentials: 'include',
  });
  await handleResponse<void>(response);
}

export async function getCsrfToken(): Promise<string> {
  const response = await fetch(`${API_BASE}/auth/csrf`, {
    credentials: 'include',
  });
  const data = await handleResponse<{ csrfToken: string }>(response);
  return data.csrfToken;
}

export async function checkAuth(): Promise<{ authenticated: boolean; userId: string; csrfToken: string }> {
  const response = await fetch(`${API_BASE}/auth/me`, {
    credentials: 'include',
  });
  return handleResponse<{ authenticated: boolean; userId: string; csrfToken: string }>(response);
}

// Health
export async function checkHealth(): Promise<HealthResponse> {
  const response = await fetch(`${API_BASE}/health`, {
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
    credentials: 'include',
  });
  return handleResponse<Workspace[]>(response);
}

export async function createWorkspace(workspace: WorkspaceCreate): Promise<Workspace> {
  const response = await fetch(`${API_BASE}/workspaces`, {
    method: 'POST',
    headers: await mutationHeaders(),
    credentials: 'include',
    body: JSON.stringify(workspace),
  });
  return handleResponse<Workspace>(response);
}

export async function getWorkspace(workspaceId: string): Promise<Workspace> {
  const response = await fetch(`${API_BASE}/workspaces/${workspaceId}`, {
    method: 'GET',
    headers: getHeaders(),
    credentials: 'include',
  });
  return handleResponse<Workspace>(response);
}

export async function startWorkspace(workspaceId: string): Promise<Workspace> {
  const response = await fetch(`${API_BASE}/workspaces/${workspaceId}/start`, {
    method: 'POST',
    headers: await mutationHeaders(),
    credentials: 'include',
  });
  return handleResponse<Workspace>(response);
}

export async function stopWorkspace(workspaceId: string): Promise<Workspace> {
  const response = await fetch(`${API_BASE}/workspaces/${workspaceId}/stop`, {
    method: 'POST',
    headers: await mutationHeaders(),
    credentials: 'include',
  });
  return handleResponse<Workspace>(response);
}

export async function restartWorkspace(workspaceId: string): Promise<Workspace> {
  const response = await fetch(`${API_BASE}/workspaces/${workspaceId}/restart`, {
    method: 'POST',
    headers: await mutationHeaders(),
  });
  return handleResponse<Workspace>(response);
}

export async function gitBranch(workspaceId: string, branch: string): Promise<void> {
  const response = await fetch(`${API_BASE}/workspaces/${workspaceId}/git/branch`, {
    method: 'POST',
    headers: await mutationHeaders(),
    body: JSON.stringify({ branch }),
  });
  await handleResponse<void>(response);
}

export async function deleteWorkspace(workspaceId: string): Promise<void> {
  const response = await fetch(`${API_BASE}/workspaces/${workspaceId}`, {
    method: 'DELETE',
    headers: await mutationHeaders(),
    credentials: 'include',
  });
  await handleResponse<void>(response);
}

// Git operations
export async function getGitStatus(workspaceId: string): Promise<GitStatus> {
  const response = await fetch(`${API_BASE}/workspaces/${workspaceId}/git/status`, {
    method: 'GET',
    headers: getHeaders(),
    credentials: 'include',
  });
  return handleResponse<GitStatus>(response);
}

export async function gitAdd(workspaceId: string, files: string[]): Promise<void> {
  const response = await fetch(`${API_BASE}/workspaces/${workspaceId}/git/add`, {
    method: 'POST',
    headers: await mutationHeaders(),
    credentials: 'include',
    body: JSON.stringify({ files }),
  });
  await handleResponse<void>(response);
}

export async function gitCommit(workspaceId: string, message: string): Promise<{ hash: string }> {
  const response = await fetch(`${API_BASE}/workspaces/${workspaceId}/git/commit`, {
    method: 'POST',
    headers: await mutationHeaders(),
    credentials: 'include',
    body: JSON.stringify({ message }),
  });
  return handleResponse<{ hash: string }>(response);
}

export async function gitPush(workspaceId: string, remote?: string, branch?: string): Promise<void> {
  const response = await fetch(`${API_BASE}/workspaces/${workspaceId}/git/push`, {
    method: 'POST',
    headers: await mutationHeaders(),
    credentials: 'include',
    body: JSON.stringify({ remote, branch }),
  });
  await handleResponse<void>(response);
}

export async function gitPull(workspaceId: string, remote?: string, branch?: string): Promise<void> {
  const response = await fetch(`${API_BASE}/workspaces/${workspaceId}/git/pull`, {
    method: 'POST',
    headers: await mutationHeaders(),
    credentials: 'include',
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
