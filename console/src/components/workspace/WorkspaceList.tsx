'use client';

import { Plus, Loader2, FolderGit2 } from 'lucide-react';
import { useState } from 'react';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/Card';
import { WorkspaceCard } from './WorkspaceCard';
import { cn, formatRelativeTime } from '@/lib/utils';
import type { Workspace, WorkspaceCreate } from '@/types';
import { listWorkspaces, createWorkspace, startWorkspace } from '@/lib/api';

interface WorkspaceListProps {
  workspaces: Workspace[];
  onStart: (id: string) => void;
  onStop: (id: string) => void;
  onOpen: (id: string) => void;
  onDelete: (id: string) => void;
  isLoading?: boolean;
}

export function WorkspaceList({
  workspaces,
  onStart,
  onStop,
  onOpen,
  onDelete,
  isLoading,
}: WorkspaceListProps) {
  const [showCreateForm, setShowCreateForm] = useState(false);
  const [newWorkspace, setNewWorkspace] = useState<WorkspaceCreate>({
    name: '',
    repository_url: '',
    branch: 'main',
  });
  const [errors, setErrors] = useState<Partial<WorkspaceCreate>>({});
  const [isCreating, setIsCreating] = useState(false);

  const validateForm = (): boolean => {
    const newErrors: Partial<WorkspaceCreate> = {};
    if (!newWorkspace.name.trim()) {
      newErrors.name = 'Workspace name is required';
    }
    if (!newWorkspace.repository_url.trim()) {
      newErrors.repository_url = 'Repository URL is required';
    } else if (!/^https?:\/\/.+/.test(newWorkspace.repository_url)) {
      newErrors.repository_url = 'Must be a valid HTTP/HTTPS URL';
    }
    setErrors(newErrors);
    return Object.keys(newErrors).length === 0;
  };

  const handleCreate = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!validateForm()) return;
    
    setIsCreating(true);
    try {
      const workspace = await createWorkspace(newWorkspace);
      onStart(workspace.id);
      setShowCreateForm(false);
      setNewWorkspace({ name: '', repository_url: '', branch: 'main' });
    } catch (error) {
      setErrors({ repository_url: error instanceof Error ? error.message : 'Failed to create workspace' });
    } finally {
      setIsCreating(false);
    }
  };

  const handleCancel = () => {
    setShowCreateForm(false);
    setNewWorkspace({ name: '', repository_url: '', branch: 'main' });
    setErrors({});
  };

  if (isLoading) {
    return (
      <Card>
        <CardContent className="py-12 text-center">
          <Loader2 className="h-8 w-8 animate-spin text-kamui-indigo-600 mx-auto mb-4" />
          <p className="text-neutral-500 dark:text-neutral-400">Loading workspaces...</p>
        </CardContent>
      </Card>
    );
  }

  return (
    <div className="space-y-4">
      {/* Create Workspace Form */}
      <Card>
        <CardHeader className="pb-2">
          <div className="flex items-center justify-between">
            <CardTitle className="flex items-center gap-2">
              <FolderGit2 className="h-5 w-5" />
              Workspaces
            </CardTitle>
            {workspaces.length > 0 && (
              <Button
                variant={showCreateForm ? 'secondary' : 'primary'}
                size="sm"
                onClick={() => setShowCreateForm(!showCreateForm)}
              >
                <Plus className="h-4 w-4" />
                {showCreateForm ? 'Cancel' : 'New Workspace'}
              </Button>
            )}
          </div>
        </CardHeader>

        {showCreateForm && (
          <form onSubmit={handleCreate} className="px-6 pb-6 space-y-4">
            <Input
              label="Workspace Name"
              value={newWorkspace.name}
              onChange={(e) => setNewWorkspace({ ...newWorkspace, name: e.target.value })}
              placeholder="my-project"
              error={errors.name}
              autoFocus
            />
            <Input
              label="Repository URL"
              value={newWorkspace.repository_url}
              onChange={(e) => setNewWorkspace({ ...newWorkspace, repository_url: e.target.value })}
              placeholder="https://github.com/user/repo.git"
              error={errors.repository_url}
              helperText="HTTPS URL for public or private repos (with token)"
            />
            <Input
              label="Branch"
              value={newWorkspace.branch}
              onChange={(e) => setNewWorkspace({ ...newWorkspace, branch: e.target.value })}
              placeholder="main"
            />
            <div className="flex justify-end gap-2 pt-2">
              <Button type="button" variant="secondary" onClick={handleCancel}>
                Cancel
              </Button>
              <Button type="submit" disabled={isCreating} loading={isCreating}>
                Create Workspace
              </Button>
            </div>
          </form>
        )}

        {!showCreateForm && workspaces.length === 0 && (
          <CardContent className="py-12 text-center">
            <FolderGit2 className="h-12 w-12 text-neutral-300 dark:text-neutral-600 mx-auto mb-4" />
            <h3 className="text-lg font-medium text-neutral-900 dark:text-neutral-100 mb-1">
              No workspaces yet
            </h3>
            <p className="text-neutral-500 dark:text-neutral-400 mb-4">
              Create your first workspace to start coding from your phone
            </p>
            <Button onClick={() => setShowCreateForm(true)}>
              <Plus className="h-4 w-4" />
              Create Workspace
            </Button>
          </CardContent>
        )}
      </Card>

      {/* Workspace Grid */}
      {workspaces.length > 0 && (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {workspaces.map((workspace) => (
            <WorkspaceCard
              key={workspace.id}
              workspace={workspace}
              onStart={onStart}
              onStop={onStop}
              onOpen={onOpen}
              onDelete={onDelete}
            />
          ))}
        </div>
      )}
    </div>
  );
}