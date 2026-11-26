// Kamui Cloud Type Definitions

export interface NodeInfo {
  hostname: string;
  os: 'macos' | 'windows' | 'linux';
  arch: 'arm64' | 'amd64';
  cpu_cores: number;
  ram_mb: number;
  gpu?: string;
  ip: string;
  roles: string[];
}

export interface Node extends NodeInfo {
  id: string;
  status: 'online' | 'offline';
  last_seen: string;
}

export interface RegistrationResponse {
  id: string;
  heartbeat_interval_s: number;
  offline_after_s: number;
}

export interface Load {
  cpu_pct?: number;
  mem_pct?: number;
}

export interface JobCreate {
  image: string;
  command?: string[];
}

export interface Job {
  id: string;
  image: string;
  command: string[];
  status: 'queued' | 'running' | 'succeeded' | 'failed';
  node_id?: string;
  exit_code?: number;
  logs?: string;
  created_at: string;
  finished_at?: string;
}

export interface Workspace {
  id: string;
  name: string;
  repository_url: string;
  branch: string;
  status: 'creating' | 'running' | 'stopped' | 'error' | 'unavailable';
  created_at: string;
  updated_at: string;
  last_active_at?: string;
  container_id?: string;
  code_server_port?: number;
  code_server_url?: string;
}

export interface WorkspaceCreate {
  name: string;
  repository_url: string;
  branch?: string;
}

export interface GitStatus {
  branch: string;
  ahead: number;
  behind: number;
  staged: string[];
  unstaged: string[];
  untracked: string[];
}

export interface GitCommit {
  hash: string;
  message: string;
  author: string;
  date: string;
}

export interface ApiError {
  error: {
    code: string;
    message: string;
  };
}

export interface HealthResponse {
  status: 'ok';
  service?: string;
  version?: string;
}

// Connection states
export type ConnectionState = 'connecting' | 'connected' | 'disconnected' | 'error';

// Workspace status for UI
export type WorkspaceStatus = 'creating' | 'starting' | 'running' | 'stopping' | 'stopped' | 'error';

// Mobile breakpoints
export const MOBILE_BREAKPOINT = 640;
export const TABLET_BREAKPOINT = 1024;