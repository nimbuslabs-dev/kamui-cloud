'use client';

import { Github, Play, Square, Terminal, RotateCw, Trash2, Clock, FolderGit2 } from 'lucide-react';
import { formatRelativeTime, getStatusBadge } from '@/lib/utils';
import { Button } from '@/components/ui/Button';
import { Badge } from '@/components/ui/Badge';
import { Card, CardContent } from '@/components/ui/Card';
import type { Workspace } from '@/types';

interface WorkspaceCardProps {
  workspace: Workspace;
  onStart: (id: string) => void;
  onStop: (id: string) => void;
  onRestart: (id: string) => void;
  onGit: (id: string) => void;
  onDelete: (id: string) => void;
  busy: boolean;
}

export function WorkspaceCard({ workspace, onStart, onStop, onRestart, onGit, onDelete, busy }: WorkspaceCardProps) {
  const { id, status } = workspace;
  const idePath = `/workspace/${id}/`;
  const canStart = status === 'stopped' || status === 'error';

  return (
    <Card className="overflow-hidden">
      <CardContent className="space-y-3 p-4">
        <div className="flex items-center gap-2">
          <FolderGit2 className="h-5 w-5 shrink-0 text-kamui-indigo-600 dark:text-kamui-indigo-400" />
          <h3 className="min-w-0 break-words font-medium">{workspace.name}</h3>
          <Badge variant={getStatusBadge(status)}>{status}</Badge>
        </div>
        <p className="break-all text-sm text-neutral-500 dark:text-neutral-400">{workspace.repository_url}</p>
        <div className="flex flex-wrap items-center gap-3 text-xs text-neutral-500 dark:text-neutral-400">
          <span className="flex items-center gap-1">
            <Clock className="h-3 w-3" />
            Last active {workspace.last_active_at ? formatRelativeTime(workspace.last_active_at) : 'never'}
          </span>
          {workspace.branch && <span className="break-all">{workspace.branch}</span>}
        </div>
        {busy && <p role="status" className="text-sm">Updating workspace…</p>}
        <div className="flex flex-wrap items-center gap-2">
          {status === 'running' && (
            <>
              {!busy && workspace.code_server_url === idePath && (
                <a href={idePath} target="_blank" rel="noopener noreferrer"
                   className="inline-flex h-9 items-center gap-1 rounded-md bg-neutral-100 px-3 text-sm font-medium dark:bg-neutral-800">
                  <Terminal className="h-4 w-4" /> Open IDE
                </a>
              )}
              <Button size="sm" variant="secondary" onClick={() => onGit(id)} disabled={busy}>
                <Github className="h-4 w-4" /> Git
              </Button>
              <Button size="sm" variant="secondary" onClick={() => onStop(id)} disabled={busy}>
                <Square className="h-4 w-4" /> Stop
              </Button>
              <Button size="sm" variant="secondary" onClick={() => onRestart(id)} disabled={busy}>
                <RotateCw className="h-4 w-4" /> Restart
              </Button>
            </>
          )}
          {canStart && (
            <Button size="sm" onClick={() => onStart(id)} disabled={busy}>
              <Play className="h-4 w-4" /> Start
            </Button>
          )}
          <Button size="sm" variant="ghost" onClick={() => {
            if (confirm(`Remove workspace "${workspace.name}"?`)) onDelete(id);
          }} disabled={busy} className="text-red-600" aria-label="Remove workspace">
            <Trash2 className="h-4 w-4" />
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}
