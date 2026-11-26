'use client';

import { Plus, Loader2, FolderGit2 } from 'lucide-react';
import { useState } from 'react';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/Card';
import { WorkspaceCard } from './WorkspaceCard';
import type { Workspace, WorkspaceCreate } from '@/types';
import { createWorkspace } from '@/lib/api';

interface WorkspaceListProps {
  workspaces: Workspace[];
  onStart: (id: string) => void;
  onStop: (id: string) => void;
  onRestart: (id: string) => void;
  onGit: (id: string) => void;
  onCreate: (workspace: Workspace) => void;
  busyId: string | null;
  onDelete: (id: string) => void;
  isLoading?: boolean;
}

export function WorkspaceList({
  workspaces,
  onStart,
  onStop,
  onRestart,
  onGit,
  onCreate,
  busyId,
  onDelete,
  isLoading,
}: WorkspaceListProps) {
  const [showCreateForm, setShowCreateForm] = useState(false);
  const [newWorkspace, setNewWorkspace] = useState<WorkspaceCreate>({
    name: '',
    repository_url: '',
    branch: '',
  });
  const [errors, setErrors] = useState<Partial<WorkspaceCreate>>({});
  const [createError, setCreateError] = useState<string | null>(null);
  const [isCreating, setIsCreating] = useState(false);

  const validateForm = (): boolean => {
    const newErrors: Partial<WorkspaceCreate> = {};
    if (!newWorkspace.name.trim()) {
      newErrors.name = 'Workspace name is required';
    }
    if (!newWorkspace.repository_url.trim()) {
      newErrors.repository_url = 'Repository URL is required';
    } else if (!/^https:\/\/github\.com\/[\w.-]+\/[\w.-]+\/?$/i.test(newWorkspace.repository_url.trim())) {
      newErrors.repository_url = 'Enter a GitHub HTTPS repository URL';
    }
    setErrors(newErrors);
    return Object.keys(newErrors).length === 0;
  };

  const handleCreate = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!validateForm()) return;
    
    setCreateError(null);
    setIsCreating(true);
    try {
      const workspace = await createWorkspace({
        name: newWorkspace.name.trim(),
        repository_url: newWorkspace.repository_url.trim(),
        branch: newWorkspace.branch?.trim() || undefined,
      });
      onCreate(workspace);
      setShowCreateForm(false);
      setNewWorkspace({ name: '', repository_url: '', branch: '' });
    } catch (error) {
      setCreateError(error instanceof Error ? error.message : 'Failed to create workspace');
    } finally {
      setIsCreating(false);
    }
  };

  const handleCancel = () => {
    setShowCreateForm(false);
    setNewWorkspace({ name: '', repository_url: '', branch: '' });
    setErrors({});
    setCreateError(null);
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
          <div className="flex flex-wrap items-center justify-between gap-2">
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
            {createError && <p role="alert" className="text-sm text-red-600">{createError}</p>}
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
              helperText="Public GitHub repository URL"
            />
            <Input
              label="Branch"
              value={newWorkspace.branch}
              onChange={(e) => setNewWorkspace({ ...newWorkspace, branch: e.target.value })}
              placeholder="Repository default"
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
        <div className="grid gap-4 md:grid-cols-2">
          {workspaces.map((workspace) => (
            <WorkspaceCard
              key={workspace.id}
              workspace={workspace}
              onStart={onStart}
              onStop={onStop}
              onRestart={onRestart}
              onGit={onGit}
              busy={busyId === workspace.id}
              onDelete={onDelete}
            />
          ))}
        </div>
      )}
    </div>
  );
}