'use client';

import { Wifi, WifiOff, Server, LogOut } from 'lucide-react';
import { Button } from '@/components/ui/Button';
import type { ConnectionState } from '@/types';

interface HeaderProps {
  connectionState: ConnectionState;
  onLogout?: () => void;
}

export function Header({ connectionState, onLogout }: HeaderProps) {
  const connected = connectionState === 'connected';
  const NetworkIcon = connected ? Wifi : WifiOff;

  return (
    <header className="fixed inset-x-0 top-0 z-40 border-b border-neutral-200 bg-white/80 backdrop-blur-sm dark:border-neutral-700 dark:bg-neutral-900/80">
      <div className="container-narrow flex h-14 items-center justify-between gap-2">
        <div className="flex min-w-0 items-center gap-2 font-semibold">
          <Server className="h-5 w-5 shrink-0 text-kamui-indigo-600 dark:text-kamui-indigo-400" />
          <span className="truncate">Kamui Cloud</span>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <span aria-label={connectionState} className="flex items-center gap-1 text-sm text-neutral-500 dark:text-neutral-400">
            <NetworkIcon aria-hidden="true" className="h-4 w-4" />
            <span className="hidden capitalize sm:inline">{connectionState}</span>
          </span>
          {onLogout && (
            <Button variant="ghost" size="sm" onClick={onLogout}>
              <LogOut className="h-4 w-4" /> Sign out
            </Button>
          )}
        </div>
      </div>
    </header>
  );
}
