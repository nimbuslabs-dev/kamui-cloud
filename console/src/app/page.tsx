'use client';

import { useState, useEffect, useCallback, useRef } from 'react';
import { useRouter } from 'next/navigation';
import { Header } from '@/components/layout/Header';
import { WorkspaceList } from '@/components/workspace/WorkspaceList';
import { GitPanel } from '@/components/workspace/GitPanel';
import { Card, CardContent } from '@/components/ui/Card';
import { Button } from '@/components/ui/Button';
import { AlertTriangle, Loader2 } from 'lucide-react';
import {
  ApiRequestError, listWorkspaces, startWorkspace, stopWorkspace,
  restartWorkspace, deleteWorkspace, checkHealth, checkAuth, logout,
} from '@/lib/api';
import type { Workspace, ConnectionState } from '@/types';

export default function Dashboard() {
  const router = useRouter();
  const [workspaces, setWorkspaces] = useState<Workspace[]>([]);
  const [isLoading, setIsLoading] = useState(true);
  const [authenticated, setAuthenticated] = useState(false);
  const [connectionState, setConnectionState] = useState<ConnectionState>('connecting');
  const [connectionError, setConnectionError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [selectedWorkspaceId, setSelectedWorkspaceId] = useState<string | null>(null);
  const [busyId, setBusyId] = useState<string | null>(null);
  const currentAction = useRef<string | null>(null);
  const revision = useRef(0);
  const gitPanel = useRef<HTMLElement>(null);

  const handleError = useCallback((error: unknown, setError: (message: string) => void) => {
    if (error instanceof ApiRequestError && error.status === 401) {
      setAuthenticated(false);
      router.replace('/login');
      return;
    }
    setError(error instanceof Error ? error.message : 'Kamui could not complete the request');
  }, [router]);

  const fetchData = useCallback(async () => {
    const requestRevision = ++revision.current;
    try {
      await checkAuth();
      setAuthenticated(true);
      const [, active] = await Promise.all([checkHealth(), listWorkspaces()]);
      if (requestRevision === revision.current) setWorkspaces(active);
      setConnectionState('connected');
      setConnectionError(null);
    } catch (error) {
      setConnectionState('error');
      handleError(error, setConnectionError);
    } finally {
      setIsLoading(false);
    }
  }, [handleError]);

  useEffect(() => {
    const initial = setTimeout(fetchData, 0);
    const interval = setInterval(() => {
      if (!currentAction.current) void fetchData();
    }, 30000);
    return () => {
      clearTimeout(initial);
      clearInterval(interval);
    };
  }, [fetchData]);

  async function runAction(id: string, action: (id: string) => Promise<Workspace | void>) {
    if (currentAction.current) return;
    currentAction.current = id;
    ++revision.current;
    setBusyId(id);
    setActionError(null);
    try {
      const updated = await action(id);
      setWorkspaces(items => updated
        ? items.map(workspace => workspace.id === id ? updated : workspace)
        : items.filter(workspace => workspace.id !== id));
      if (!updated) setSelectedWorkspaceId(selected => selected === id ? null : selected);
    } catch (error) {
      handleError(error, setActionError);
      void fetchData();
    } finally {
      currentAction.current = null;
      setBusyId(null);
    }
  }

  function handleCreate(workspace: Workspace) {
    ++revision.current;
    setWorkspaces(items => [...items, workspace]);
  }

  async function handleLogout() {
    setActionError(null);
    try {
      await logout();
      setAuthenticated(false);
      router.replace('/login');
    } catch (error) {
      handleError(error, setActionError);
    }
  }

  useEffect(() => {
    if (!selectedWorkspaceId) return;
    gitPanel.current?.focus();
  }, [selectedWorkspaceId]);

  const selectedWorkspace = workspaces.find(workspace => workspace.id === selectedWorkspaceId);

  if (!authenticated && !connectionError) {
    return (
      <div className="flex min-h-screen items-center justify-center bg-neutral-50 dark:bg-neutral-950">
        <Loader2 aria-label="Checking session" className="h-8 w-8 animate-spin text-kamui-indigo-600" />
      </div>
    );
  }

  return (
    <div className="min-h-screen">
      <Header connectionState={connectionState} onLogout={authenticated ? handleLogout : undefined} />
      <main className="px-4 pb-4 pt-[4.5rem]">
        <div className="container-narrow space-y-6">
          {connectionError && (
            <div role="alert" className="flex items-center gap-3 rounded-lg border border-yellow-200 bg-yellow-50 p-4 text-yellow-800 dark:border-yellow-800 dark:bg-yellow-900/30 dark:text-yellow-200">
              <AlertTriangle className="h-5 w-5 shrink-0" />
              <p className="flex-1 text-sm">{connectionError}</p>
              <Button variant="secondary" size="sm" onClick={fetchData}>Retry</Button>
            </div>
          )}
          {actionError && <p role="alert" className="rounded-lg bg-red-50 p-4 text-sm text-red-700 dark:bg-red-900/20 dark:text-red-300">{actionError}</p>}
          {authenticated && (
            <div className="grid gap-6 lg:grid-cols-3">
              <div className="min-w-0 lg:col-span-2">
                <WorkspaceList
                  workspaces={workspaces}
                  onStart={id => runAction(id, startWorkspace)}
                  onStop={id => runAction(id, stopWorkspace)}
                  onRestart={id => runAction(id, restartWorkspace)}
                  onDelete={id => runAction(id, deleteWorkspace)}
                  onCreate={handleCreate}
                  onGit={setSelectedWorkspaceId}
                  busyId={busyId}
                  isLoading={isLoading}
                />
              </div>
              <section ref={gitPanel} tabIndex={-1} aria-label="Workspace Git" className="min-w-0 scroll-mt-20">
                {selectedWorkspace ? (
                  <GitPanel key={selectedWorkspace.id + selectedWorkspace.status}
                    workspaceId={selectedWorkspace.id} isWorkspaceRunning={selectedWorkspace.status === 'running'} />
                ) : (
                  <Card><CardContent className="py-8 text-center text-sm text-neutral-500 dark:text-neutral-400">
                    Choose Git on a running workspace to view changes.
                  </CardContent></Card>
                )}
              </section>
            </div>
          )}
        </div>
      </main>
    </div>
  );
}
