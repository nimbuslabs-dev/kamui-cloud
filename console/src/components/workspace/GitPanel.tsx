'use client';

import { 
  GitBranch, 
  FileText, 
  Plus, 
  Minus, 
  MessageSquare,
  ArrowUp,
  ArrowDown,
  RefreshCw,
  Check,
  X,
  Loader2,
  AlertCircle
} from 'lucide-react';
import { useState } from 'react';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/Card';
import { Badge } from '@/components/ui/Badge';
import { cn } from '@/lib/utils';
import type { GitStatus } from '@/types';
import { getGitStatus, gitAdd, gitCommit, gitPush, gitPull } from '@/lib/api';

interface GitPanelProps {
  workspaceId: string;
  isWorkspaceRunning: boolean;
}

export function GitPanel({ workspaceId, isWorkspaceRunning }: GitPanelProps) {
  const [gitStatus, setGitStatus] = useState<GitStatus | null>(null);
  const [isLoading, setIsLoading] = useState(false);
  const [commitMessage, setCommitMessage] = useState('');
  const [stagedFiles, setStagedFiles] = useState<Set<string>>(new Set());
  const [operation, setOperation] = useState<'commit' | 'push' | 'pull' | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);

  const fetchStatus = async () => {
    if (!isWorkspaceRunning) return;
    setIsLoading(true);
    setError(null);
    try {
      const status = await getGitStatus(workspaceId);
      setGitStatus(status);
      // Reset staged files to match current status
      setStagedFiles(new Set(status.staged));
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to fetch git status');
    } finally {
      setIsLoading(false);
    }
  };

  const toggleStage = (file: string) => {
    const newStaged = new Set(stagedFiles);
    if (newStaged.has(file)) {
      newStaged.delete(file);
    } else {
      newStaged.add(file);
    }
    setStagedFiles(newStaged);
  };

  const handleStageAll = async () => {
    const allFiles = [...(gitStatus?.staged || []), ...(gitStatus?.unstaged || []), ...(gitStatus?.untracked || [])];
    setOperation('commit');
    try {
      await gitAdd(workspaceId, allFiles);
      setStagedFiles(new Set(allFiles));
      setSuccess('All files staged');
      fetchStatus();
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to stage files');
    } finally {
      setOperation(null);
    }
  };

  const handleUnstageAll = async () => {
    setOperation('commit');
    try {
      await gitAdd(workspaceId, []);
      setStagedFiles(new Set());
      setSuccess('All files unstaged');
      fetchStatus();
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to unstage files');
    } finally {
      setOperation(null);
    }
  };

  const handleCommit = async () => {
    if (!commitMessage.trim()) {
      setError('Commit message is required');
      return;
    }
    if (stagedFiles.size === 0) {
      setError('No files staged for commit');
      return;
    }
    
    setOperation('commit');
    setError(null);
    try {
      const result = await gitCommit(workspaceId, commitMessage);
      setSuccess(`Committed: ${result.hash.slice(0, 7)}`);
      setCommitMessage('');
      setStagedFiles(new Set());
      fetchStatus();
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Commit failed');
    } finally {
      setOperation(null);
    }
  };

  const handlePush = async () => {
    setOperation('push');
    setError(null);
    try {
      await gitPush(workspaceId);
      setSuccess('Pushed to remote');
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Push failed');
    } finally {
      setOperation(null);
    }
  };

  const handlePull = async () => {
    setOperation('pull');
    setError(null);
    try {
      await gitPull(workspaceId);
      setSuccess('Pulled from remote');
      fetchStatus();
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Pull failed');
    } finally {
      setOperation(null);
    }
  };

  // Auto-fetch on mount and when workspace becomes running
  // Note: In a real app, you'd use useEffect with proper dependencies

  const renderFileList = (files: string[], label: string, icon: React.ReactNode, defaultStaged: boolean) => {
    if (files.length === 0) return null;
    return (
      <div className="mb-4">
        <div className="flex items-center gap-2 text-sm font-medium text-neutral-700 dark:text-neutral-300 mb-2">
          {icon}
          <span>{label} ({files.length})</span>
        </div>
        <div className="space-y-1 max-h-40 overflow-y-auto">
          {files.map((file) => (
            <div
              key={file}
              className="flex items-center gap-2 px-2 py-1.5 rounded text-sm bg-neutral-50 dark:bg-neutral-800/50 hover:bg-neutral-100 dark:hover:bg-neutral-700 cursor-pointer transition-colors"
              onClick={() => toggleStage(file)}
            >
              {stagedFiles.has(file) ? (
                <Minus className="h-4 w-4 text-green-600 flex-shrink-0" />
              ) : (
                <Plus className="h-4 w-4 text-kamui-indigo-600 flex-shrink-0" />
              )}
              <FileText className="h-4 w-4 text-neutral-500 flex-shrink-0" />
              <span className="truncate font-mono text-xs">{file}</span>
              {stagedFiles.has(file) && (
                <Badge variant="success" className="text-xs">staged</Badge>
              )}
            </div>
          ))}
        </div>
      </div>
    );
  };

  if (!isWorkspaceRunning) {
    return (
      <Card>
        <CardContent className="py-8 text-center">
          <GitBranch className="h-12 w-12 text-neutral-300 dark:text-neutral-600 mx-auto mb-4" />
          <h3 className="text-lg font-medium text-neutral-900 dark:text-neutral-100 mb-1">
            Workspace not running
          </h3>
          <p className="text-neutral-500 dark:text-neutral-400">
            Start the workspace to access Git controls
          </p>
        </CardContent>
      </Card>
    );
  }

  return (
    <Card>
      <CardHeader>
        <div className="flex items-center justify-between">
          <CardTitle className="flex items-center gap-2">
            <GitBranch className="h-5 w-5" />
            Git
          </CardTitle>
          <Button variant="ghost" size="sm" onClick={fetchStatus} disabled={isLoading} loading={isLoading}>
            <RefreshCw className="h-4 w-4" />
          </Button>
        </div>
      </CardHeader>
      
      <CardContent className="space-y-4">
        {error && (
          <div className="flex items-center gap-2 p-3 rounded-md bg-red-50 dark:bg-red-900/20 border border-red-200 dark:border-red-800">
            <AlertCircle className="h-4 w-4 text-red-600 flex-shrink-0" />
            <span className="text-sm text-red-700 dark:text-red-300">{error}</span>
          </div>
        )}
        
        {success && (
          <div className="flex items-center gap-2 p-3 rounded-md bg-green-50 dark:bg-green-900/20 border border-green-200 dark:border-green-800">
            <Check className="h-4 w-4 text-green-600 flex-shrink-0" />
            <span className="text-sm text-green-700 dark:text-green-300">{success}</span>
          </div>
        )}

        {gitStatus && (
          <>
            {/* Branch Info */}
            <div className="flex items-center gap-2 px-3 py-2 rounded-md bg-neutral-50 dark:bg-neutral-800/50">
              <GitBranch className="h-4 w-4 text-kamui-indigo-600" />
              <span className="font-medium text-neutral-900 dark:text-neutral-100">
                {gitStatus.branch}
              </span>
              {gitStatus.ahead > 0 && (
                <Badge variant="info">
                  <ArrowUp className="h-3 w-3 mr-1" /> {gitStatus.ahead} ahead
                </Badge>
              )}
              {gitStatus.behind > 0 && (
                <Badge variant="warning">
                  <ArrowDown className="h-3 w-3 mr-1" /> {gitStatus.behind} behind
                </Badge>
              )}
            </div>

            {/* Staged Files */}
            {renderFileList(gitStatus.staged, 'Staged Changes', <Check className="h-4 w-4" />, true)}

            {/* Unstaged Files */}
            {renderFileList(gitStatus.unstaged, 'Unstaged Changes', <FileText className="h-4 w-4" />, false)}

            {/* Untracked Files */}
            {renderFileList(gitStatus.untracked, 'Untracked Files', <FileText className="h-4 w-4" />, false)}

            {/* Commit Form */}
            {(gitStatus.staged.length > 0 || gitStatus.unstaged.length > 0 || gitStatus.untracked.length > 0) && (
              <div className="border-t border-neutral-200 dark:border-neutral-700 pt-4">
                <div className="space-y-3">
                  <Input
                    label="Commit Message"
                    value={commitMessage}
                    onChange={(e) => setCommitMessage(e.target.value)}
                    placeholder="Describe your changes..."
                    helperText={`${stagedFiles.size} file(s) staged`}
                  />
                  <div className="flex gap-2">
                    <Button 
                      onClick={handleCommit} 
                      disabled={operation !== null || stagedFiles.size === 0 || !commitMessage.trim()}
                      loading={operation === 'commit'}
                    >
                      <MessageSquare className="h-4 w-4" />
                      Commit
                    </Button>
                    {stagedFiles.size > 0 && (
                      <Button 
                        variant="secondary" 
                        onClick={handlePush} 
                        disabled={operation !== null}
                        loading={operation === 'push'}
                      >
                        <ArrowUp className="h-4 w-4" />
                        Push
                      </Button>
                    )}
                    <Button 
                      variant="secondary" 
                      onClick={handlePull} 
                      disabled={operation !== null}
                      loading={operation === 'pull'}
                    >
                      <ArrowDown className="h-4 w-4" />
                      Pull
                    </Button>
                  </div>
                </div>
              </div>
            )}

            {/* No changes */}
            {gitStatus.staged.length === 0 && gitStatus.unstaged.length === 0 && gitStatus.untracked.length === 0 && (
              <div className="text-center py-8 text-neutral-500 dark:text-neutral-400">
                <Check className="h-12 w-12 mx-auto mb-3 text-green-500" />
                <p className="font-medium">Working tree clean</p>
                <p className="text-sm mt-1">No changes to commit</p>
              </div>
            )}
          </>
        )}
      </CardContent>
    </Card>
  );
}