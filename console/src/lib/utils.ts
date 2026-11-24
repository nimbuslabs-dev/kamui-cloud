import { clsx, type ClassValue } from 'clsx';
import { twMerge } from 'tailwind-merge';

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

export function formatRelativeTime(dateString: string): string {
  const date = new Date(dateString);
  const now = new Date();
  const diffMs = now.getTime() - date.getTime();
  const diffSecs = Math.floor(diffMs / 1000);
  const diffMins = Math.floor(diffSecs / 60);
  const diffHours = Math.floor(diffMins / 60);
  const diffDays = Math.floor(diffHours / 24);

  if (diffSecs < 60) return 'just now';
  if (diffMins < 60) return `${diffMins}m ago`;
  if (diffHours < 24) return `${diffHours}h ago`;
  if (diffDays < 7) return `${diffDays}d ago`;
  return date.toLocaleDateString();
}

export function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  const secs = Math.floor(ms / 1000);
  if (secs < 60) return `${secs}s`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins}m ${secs % 60}s`;
  const hours = Math.floor(mins / 60);
  return `${hours}h ${mins % 60}m`;
}

export function truncate(str: string, length: number): string {
  if (str.length <= length) return str;
  return str.slice(0, length - 3) + '...';
}

export function getStatusColor(status: string): string {
  switch (status) {
    case 'online':
    case 'running':
    case 'succeeded':
      return 'text-green-600 dark:text-green-400';
    case 'offline':
    case 'stopped':
    case 'failed':
      return 'text-red-600 dark:text-red-400';
    case 'creating':
    case 'starting':
    case 'queued':
      return 'text-yellow-600 dark:text-yellow-400';
    case 'stopping':
      return 'text-orange-600 dark:text-orange-400';
    default:
      return 'text-neutral-600 dark:text-neutral-400';
  }
}

export function getStatusBadge(status: string): string {
  switch (status) {
    case 'online':
    case 'running':
    case 'succeeded':
      return 'badge-success';
    case 'offline':
    case 'stopped':
    case 'failed':
      return 'badge-error';
    case 'creating':
    case 'starting':
    case 'queued':
      return 'badge-warning';
    case 'stopping':
      return 'badge-warning';
    default:
      return 'badge-neutral';
  }
}

export function generateId(): string {
  return Math.random().toString(36).substring(2, 15) +
    Math.random().toString(36).substring(2, 15);
}

export function debounce<T extends (...args: unknown[]) => unknown>(
  fn: T,
  delay: number
): (...args: Parameters<T>) => void {
  let timeoutId: ReturnType<typeof setTimeout>;
  return (...args: Parameters<T>) => {
    clearTimeout(timeoutId);
    timeoutId = setTimeout(() => fn(...args), delay);
  };
}