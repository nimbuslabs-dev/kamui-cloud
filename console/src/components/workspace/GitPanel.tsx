'use client';

import { useCallback, useEffect, useState } from 'react';
import { ArrowDown, ArrowUp, GitBranch, RefreshCw } from 'lucide-react';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/Card';
import { Badge } from '@/components/ui/Badge';
import type { GitStatus } from '@/types';
import { getGitStatus, gitAdd, gitBranch, gitCommit, gitPush, gitPull } from '@/lib/api';

interface GitPanelProps {
  workspaceId: string;
  isWorkspaceRunning: boolean;
}

export function GitPanel({ workspaceId, isWorkspaceRunning }: GitPanelProps) {
  const [status, setStatus] = useState<GitStatus | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [commitMessage, setCommitMessage] = useState('');
  const [branch, setBranch] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    setStatus(await getGitStatus(workspaceId));
  }, [workspaceId]);

  const run = useCallback(async (label: string, action?: () => Promise<string>) => {
    setBusy(label);
    setError(null);
    setSuccess(null);
    try {
      if (action) setSuccess(await action());
      await refresh();
    } catch (error) {
      setError(error instanceof Error ? error.message : 'Git operation failed');
    } finally {
      setBusy(null);
    }
  }, [refresh]);

  useEffect(() => {
    if (!isWorkspaceRunning) return;
    const initial = setTimeout(() => void run('refresh'), 0);
    return () => clearTimeout(initial);
  }, [isWorkspaceRunning, run]);

  return (
    <Card>
      <CardHeader>
        <div className="flex items-center justify-between">
          <CardTitle className="flex items-center gap-2"><GitBranch className="h-5 w-5" /> Git</CardTitle>
          {isWorkspaceRunning && (
            <Button variant="ghost" size="sm" aria-label="Refresh Git status"
              onClick={() => run('refresh')} disabled={!!busy} loading={busy === 'refresh'}>
              <RefreshCw className="h-4 w-4" />
            </Button>
          )}
        </div>
      </CardHeader>
      <CardContent className="space-y-4">
        {!isWorkspaceRunning && <p>Start the workspace to access Git controls.</p>}
        {error && <p className="break-words text-sm text-red-700 dark:text-red-300" role="alert">{error}</p>}
        {success && <p className="break-words text-sm text-green-700 dark:text-green-300" role="status">{success}</p>}
        {isWorkspaceRunning && status && (
          <>
            <div className="flex flex-wrap items-center gap-2 text-sm">
              <span className="break-all font-medium">{status.branch}</span>
              {status.ahead > 0 && <Badge variant="info">{status.ahead} ahead</Badge>}
              {status.behind > 0 && <Badge variant="warning">{status.behind} behind</Badge>}
            </div>
            {([
              ['Staged', status.staged],
              ['Unstaged', status.unstaged],
              ['Untracked', status.untracked],
            ] as const).map(([label, files]) => files.length > 0 && (
              <div key={label}>
                <h3 className="text-sm font-medium">{label} ({files.length})</h3>
                <ul className="mt-2 max-h-32 space-y-1 overflow-y-auto text-xs">
                  {files.map(file => <li key={file} className="break-all font-mono">{file}</li>)}
                </ul>
              </div>
            ))}
            {!(status.staged.length || status.unstaged.length || status.untracked.length) &&
              <p className="text-sm text-neutral-500 dark:text-neutral-400">Working tree clean</p>}
            <Button size="sm" variant="secondary" disabled={!!busy || !(status.unstaged.length || status.untracked.length)}
              loading={busy === 'stage'} onClick={() => run('stage', async () => {
                await gitAdd(workspaceId, []);
                return 'Staged all changes';
              })}>Stage all</Button>
            <form className="space-y-2" onSubmit={event => {
              event.preventDefault();
              void run('commit', async () => {
                const { hash } = await gitCommit(workspaceId, commitMessage.trim());
                setCommitMessage('');
                return `Committed ${hash.slice(0, 12)}`;
              });
            }}>
              <Input label="Commit message" value={commitMessage} onChange={event => setCommitMessage(event.target.value)} />
              <Button size="sm" type="submit" disabled={!!busy || !status.staged.length || !commitMessage.trim()}
                loading={busy === 'commit'}>Commit</Button>
            </form>
            <form className="space-y-2" onSubmit={event => {
              event.preventDefault();
              void run('branch', async () => {
                await gitBranch(workspaceId, branch.trim());
                setBranch('');
                return 'Switched branch';
              });
            }}>
              <Input label="Branch name" value={branch} onChange={event => setBranch(event.target.value)} />
              <Button size="sm" type="submit" variant="secondary" disabled={!!busy || !branch.trim()}
                loading={busy === 'branch'}>Switch or create branch</Button>
            </form>
            <div className="flex flex-wrap gap-2">
              <Button size="sm" variant="secondary" disabled={!!busy} loading={busy === 'push'}
                onClick={() => run('push', async () => {
                  await gitPush(workspaceId);
                  return 'Pushed to GitHub';
                })}><ArrowUp className="h-4 w-4" /> Push</Button>
              <Button size="sm" variant="secondary" disabled={!!busy} loading={busy === 'pull'}
                onClick={() => run('pull', async () => {
                  await gitPull(workspaceId);
                  return 'Pulled from GitHub';
                })}><ArrowDown className="h-4 w-4" /> Pull</Button>
            </div>
          </>
        )}
      </CardContent>
    </Card>
  );
}
