'use client';

import { 
  Github, 
  Play, 
  Square, 
  Terminal, 
  ExternalLink,
  Trash2,
  MoreVertical,
  Clock,
  FolderGit2
} from 'lucide-react';
import { formatRelativeTime, getStatusBadge, cn } from '@/lib/utils';
import { Button } from '@/components/ui/Button';
import { Badge } from '@/components/ui/Badge';
import { Card, CardContent, CardFooter } from '@/components/ui/Card';
import type { Workspace } from '@/types';

interface WorkspaceCardProps {
  workspace: Workspace;
  onStart: (id: string) => void;
  onStop: (id: string) => void;
  onOpen: (id: string) => void;
  onDelete: (id: string) => void;
  isStarting?: boolean;
  isStopping?: boolean;
}

export function WorkspaceCard({
  workspace,
  onStart,
  onStop,
  onOpen,
  onDelete,
  isStarting,
  isStopping,
}: WorkspaceCardProps) {
  const isRunning = workspace.status === 'running';
  const isStopped = workspace.status === 'stopped';
  const isError = workspace.status === 'error';
  const isCreating = workspace.status === 'creating';

  const handleStart = () => {
    if (isStopped || isError) onStart(workspace.id);
  };

  const handleStop = () => {
    if (isRunning) onStop(workspace.id);
  };

  const handleOpen = () => {
    if (isRunning && workspace.code_server_url) {
      window.open(workspace.code_server_url, '_blank');
    }
  };

  const handleDelete = (e: React.MouseEvent) => {
    e.stopPropagation();
    if (confirm(`Delete workspace "${workspace.name}"? This cannot be undone.`)) {
      onDelete(workspace.id);
    }
  };

  return (
    <Card className="overflow-hidden transition-shadow hover:shadow-md">
      <CardContent className="p-4">
        <div className="flex items-start justify-between gap-4">
          <div className="flex-1 min-w-0">
            <div className="flex items-center gap-2 flex-wrap">
              <FolderGit2 className="h-5 w-5 text-kamui-indigo-600 dark:text-kamui-indigo-400 flex-shrink-0" />
              <h3 className="font-medium text-neutral-900 dark:text-neutral-100 truncate">
                {workspace.name}
              </h3>
              <Badge variant={getStatusBadge(workspace.status) as any}>
                {workspace.status}
              </Badge>
            </div>
            
            <p className="mt-1 text-sm text-neutral-500 dark:text-neutral-400 truncate">
              {workspace.repository_url}
            </p>
            
            <div className="mt-2 flex items-center gap-4 text-xs text-neutral-500 dark:text-neutral-400">
              <span className="flex items-center gap-1">
                <Clock className="h-3 w-3" />
                Last active: {workspace.last_active_at 
                  ? formatRelativeTime(workspace.last_active_at)
                  : 'never'}
              </span>
              <span className="flex items-center gap-1">
                <Github className="h-3 w-3" />
                {workspace.branch}
              </span>
            </div>
            
            {workspace.container_id && (
              <p className="mt-1 text-xs text-neutral-400 dark:text-neutral-500 font-mono">
                Container: {workspace.container_id.slice(0, 12)}
              </p>
            )}
          </div>

          {/* Actions */}
          <div className="flex items-center gap-2 flex-shrink-0">
            {isRunning && (
              <>
                <Button
                  variant="secondary"
                  size="sm"
                  onClick={handleOpen}
                  disabled={isStarting || isStopping}
                  className="hidden sm:inline-flex"
                >
                  <Terminal className="h-3.5 w-3.5" />
                  Open IDE
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={handleOpen}
                  disabled={isStarting || isStopping}
                  className="sm:hidden"
                  aria-label="Open IDE"
                >
                  <Terminal className="h-4 w-4" />
                </Button>
                <Button
                  variant="secondary"
                  size="sm"
                  onClick={handleStop}
                  disabled={isStarting || isStopping}
                  className="hidden sm:inline-flex"
                >
                  <Square className="h-3.5 w-3.5" />
                  Stop
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={handleStop}
                  disabled={isStarting || isStopping}
                  className="sm:hidden"
                  aria-label="Stop workspace"
                >
                  <Square className="h-4 w-4" />
                </Button>
              </>
            )}
            
            {(isStopped || isError) && (
              <>
                <Button
                  variant="primary"
                  size="sm"
                  onClick={handleStart}
                  disabled={isStarting || isStopping}
                  className="hidden sm:inline-flex"
                >
                  <Play className="h-3.5 w-3.5" />
                  Start
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={handleStart}
                  disabled={isStarting || isStopping}
                  className="sm:hidden"
                  aria-label="Start workspace"
                >
                  <Play className="h-4 w-4" />
                </Button>
              </>
            )}
            
            <Button
              variant="ghost"
              size="sm"
              onClick={handleDelete}
              className="text-red-600 hover:bg-red-50 dark:hover:bg-red-900/20"
              aria-label="Delete workspace"
            >
              <Trash2 className="h-4 w-4" />
            </Button>
          </div>
        </div>
      </CardContent>
      
      {isCreating && (
        <CardFooter className="px-4 py-2 bg-yellow-50 dark:bg-yellow-900/20 border-yellow-200 dark:border-yellow-800">
          <div className="flex items-center gap-2 text-sm text-yellow-800 dark:text-yellow-200">
            <div className="animate-spin rounded-full h-4 w-4 border-2 border-yellow-500 border-t-transparent" />
            Creating workspace...
          </div>
        </CardFooter>
      )}
    </Card>
  );
}