'use client';

import { useState, useEffect, useCallback } from 'react';
import { Header } from '@/components/layout/Header';
import { WorkspaceList } from '@/components/workspace/WorkspaceList';
import { GitPanel } from '@/components/workspace/GitPanel';
import { Card, CardContent } from '@/components/ui/Card';
import { Badge } from '@/components/ui/Badge';
import { Button } from '@/components/ui/Button';
import { Wifi, WifiOff, Loader2, Info, AlertTriangle } from 'lucide-react';
import { cn, formatRelativeTime } from '@/lib/utils';
import { listWorkspaces, createWorkspace, startWorkspace, stopWorkspace, deleteWorkspace, checkHealth } from '@/lib/api';
import type { Workspace, ConnectionState, HealthResponse } from '@/types';

export default function Dashboard() {
  const [workspaces, setWorkspaces] = useState<Workspace[]>([]);
  const [isLoading, setIsLoading] = useState(true);
  const [connectionState, setConnectionState] = useState<ConnectionState>('disconnected');
  const [hostName, setHostName] = useState('Loading...');
  const [localIP, setLocalIP] = useState('Detecting...');
  const [health, setHealth] = useState<HealthResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [isMobileMenuOpen, setIsMobileMenuOpen] = useState(false);
  const [selectedWorkspaceId, setSelectedWorkspaceId] = useState<string | null>(null);
  const [showGitPanel, setShowGitPanel] = useState(false);
  const [refreshInterval, setRefreshInterval] = useState<NodeJS.Timeout | null>(null);

  const fetchData = useCallback(async () => {
    try {
      setConnectionState('connecting');
      const [healthData, workspacesData] = await Promise.all([
        checkHealth(),
        listWorkspaces(),
      ]);
      setHealth(healthData);
      setWorkspaces(workspacesData);
      setConnectionState('connected');
      setError(null);
    } catch (err) {
      setConnectionState('error');
      setError(err instanceof Error ? err.message : 'Failed to connect to API');
    } finally {
      setIsLoading(false);
    }
  }, []);

  const fetchLocalIP = useCallback(async () => {
    try {
      const response = await fetch('https://api.ipify.org?format=json');
      const data = await response.json();
      setLocalIP(data.ip);
    } catch {
      setLocalIP('Unknown');
    }
  }, []);

  const getHostName = useCallback(async () => {
    try {
      const response = await fetch('https://api.hostname.com/');
      // This won't work cross-origin, fallback to a generic name
      setHostName('kamui-server');
    } catch {
      setHostName('kamui-server');
    }
  }, []);

  // Initial load
  useEffect(() => {
    fetchData();
    fetchLocalIP();
    getHostName();
    
    // Set up periodic refresh
    const interval = setInterval(fetchData, 30000);
    setRefreshInterval(interval);
    
    return () => {
      if (interval) clearInterval(interval);
    };
  }, [fetchData, fetchLocalIP, getHostName]);

  const handleStartWorkspace = async (id: string) => {
    try {
      const workspace = await startWorkspace(id);
      setWorkspaces(workspaces.map(w => w.id === id ? workspace : w));
    } catch (err) {
      alert(err instanceof Error ? err.message : 'Failed to start workspace');
    }
  };

  const handleStopWorkspace = async (id: string) => {
    try {
      const workspace = await stopWorkspace(id);
      setWorkspaces(workspaces.map(w => w.id === id ? workspace : w));
      if (selectedWorkspaceId === id) {
        setSelectedWorkspaceId(null);
        setShowGitPanel(false);
      }
    } catch (err) {
      alert(err instanceof Error ? err.message : 'Failed to stop workspace');
    }
  };

  const handleOpenWorkspace = (id: string) => {
    const workspace = workspaces.find(w => w.id === id);
    if (workspace?.code_server_url) {
      window.open(workspace.code_server_url, '_blank');
    }
  };

  const handleDeleteWorkspace = async (id: string) => {
    try {
      await deleteWorkspace(id);
      setWorkspaces(workspaces.filter(w => w.id !== id));
      if (selectedWorkspaceId === id) {
        setSelectedWorkspaceId(null);
        setShowGitPanel(false);
      }
    } catch (err) {
      alert(err instanceof Error ? err.message : 'Failed to delete workspace');
    }
  };

  const handleWorkspaceSelect = (id: string) => {
    setSelectedWorkspaceId(id);
    setShowGitPanel(true);
  };

  const selectedWorkspace = workspaces.find(w => w.id === selectedWorkspaceId);

  return (
    <div className="min-h-screen">
      {/* Header */}
      <Header
        connectionState={connectionState}
        hostName={hostName}
        isMobileMenuOpen={isMobileMenuOpen}
        onMobileMenuToggle={() => setIsMobileMenuOpen(!isMobileMenuOpen)}
      />

      {/* Main Content */}
      <main className="pt-14 pb-4 px-4">
        <div className="container-narrow space-y-6">
          {/* Connection Status Banner */}
          {(connectionState === 'error' || connectionState === 'disconnected') && (
            <div className="flex items-center gap-3 p-4 rounded-lg bg-yellow-50 dark:bg-yellow-900/30 border border-yellow-200 dark:border-yellow-800">
              {connectionState === 'error' ? (
                <AlertTriangle className="h-5 w-5 text-yellow-600 flex-shrink-0" />
              ) : (
                <WifiOff className="h-5 w-5 text-yellow-600 flex-shrink-0" />
              )}
              <div className="flex-1">
                <p className="font-medium text-yellow-800 dark:text-yellow-200">
                  {connectionState === 'error' ? 'Connection Error' : 'Not Connected'}
                </p>
                <p className="text-sm text-yellow-700 dark:text-yellow-300">
                  {error || 'Unable to reach Kamui API. Check if the server is running.'}
                </p>
              </div>
              <Button variant="secondary" size="sm" onClick={fetchData}>
                Retry
              </Button>
            </div>
          )}

          {/* Server Info */}
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
            <Card>
              <CardContent className="p-4">
                <div className="flex items-center gap-3">
                  <div className="p-2 rounded-lg bg-kamui-indigo-100 dark:bg-kamui-indigo-900/30">
                    <Wifi className="h-5 w-5 text-kamui-indigo-600 dark:text-kamui-indigo-400" />
                  </div>
                  <div>
                    <p className="text-sm text-neutral-500 dark:text-neutral-400">API Status</p>
                    <p className="font-medium text-neutral-900 dark:text-neutral-100 capitalize">
                      {health?.status || 'unknown'}
                    </p>
                  </div>
                </div>
              </CardContent>
            </Card>
            
            <Card>
              <CardContent className="p-4">
                <div className="flex items-center gap-3">
                  <div className="p-2 rounded-lg bg-kamui-teal-100 dark:bg-kamui-teal-900/30">
                    <span className="text-2xl">📱</span>
                  </div>
                  <div>
                    <p className="text-sm text-neutral-500 dark:text-neutral-400">Host</p>
                    <p className="font-medium text-neutral-900 dark:text-neutral-100 truncate">
                      {hostName}
                    </p>
                  </div>
                </div>
              </CardContent>
            </Card>
            
            <Card>
              <CardContent className="p-4">
                <div className="flex items-center gap-3">
                  <div className="p-2 rounded-lg bg-orange-100 dark:bg-orange-900/30">
                    <span className="text-2xl">🌐</span>
                  </div>
                  <div>
                    <p className="text-sm text-neutral-500 dark:text-neutral-400">Public IP</p>
                    <p className="font-medium text-neutral-900 dark:text-neutral-100 truncate font-mono text-sm">
                      {localIP}
                    </p>
                  </div>
                </div>
              </CardContent>
            </Card>
            
            <Card>
              <CardContent className="p-4">
                <div className="flex items-center gap-3">
                  <div className="p-2 rounded-lg bg-purple-100 dark:bg-purple-900/30">
                    <span className="text-2xl">📦</span>
                  </div>
                  <div>
                    <p className="text-sm text-neutral-500 dark:text-neutral-400">Workspaces</p>
                    <p className="font-medium text-neutral-900 dark:text-neutral-100">
                      {workspaces.length} total
                    </p>
                  </div>
                </div>
              </CardContent>
            </Card>
          </div>

          {/* Workspace List & Git Panel */}
          <div className="grid gap-6 lg:grid-cols-3">
            <div className="lg:col-span-2">
              <WorkspaceList
                workspaces={workspaces}
                onStart={handleStartWorkspace}
                onStop={handleStopWorkspace}
                onOpen={handleOpenWorkspace}
                onDelete={handleDeleteWorkspace}
                isLoading={isLoading}
              />
            </div>
            
            <div className="lg:col-span-1">
              {selectedWorkspace && (
                <div className="sticky top-20">
                  <GitPanel
                    workspaceId={selectedWorkspace.id}
                    isWorkspaceRunning={selectedWorkspace.status === 'running'}
                  />
                </div>
              )}
              {!selectedWorkspace && (
                <Card>
                  <CardContent className="py-8 text-center">
                    <Info className="h-12 w-12 text-neutral-300 dark:text-neutral-600 mx-auto mb-4" />
                    <h3 className="text-lg font-medium text-neutral-900 dark:text-neutral-100 mb-1">
                      Select a workspace
                    </h3>
                    <p className="text-neutral-500 dark:text-neutral-400">
                      Click on a workspace to view Git status and controls
                    </p>
                  </CardContent>
                </Card>
              )}
            </div>
          </div>
        </div>
      </main>

      {/* Mobile Git Panel Modal */}
      {showGitPanel && selectedWorkspace && (
        <div className="fixed inset-0 z-50 lg:hidden">
          <div className="fixed inset-0 bg-black/50" onClick={() => setShowGitPanel(false)} />
          <div className="fixed bottom-0 left-0 right-0 bg-white dark:bg-neutral-900 rounded-t-2xl shadow-xl max-h-[80vh] overflow-y-auto animate-slide-up">
            <div className="flex items-center justify-between p-4 border-b border-neutral-200 dark:border-neutral-700 sticky top-0 bg-white dark:bg-neutral-900 rounded-t-2xl">
              <h3 className="font-semibold text-neutral-900 dark:text-neutral-100">
                Git: {selectedWorkspace.name}
              </h3>
              <Button variant="ghost" size="sm" onClick={() => setShowGitPanel(false)}>
                ✕
              </Button>
            </div>
            <div className="p-4">
              <GitPanel
                workspaceId={selectedWorkspace.id}
                isWorkspaceRunning={selectedWorkspace.status === 'running'}
              />
            </div>
          </div>
        </div>
      )}
    </div>
  );
}