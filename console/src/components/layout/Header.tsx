'use client';

import { Wifi, WifiOff, Server, Smartphone, Menu, X } from 'lucide-react';
import { useState } from 'react';
import { Button } from '@/components/ui/Button';
import { Badge } from '@/components/ui/Badge';
import { cn } from '@/lib/utils';
import type { ConnectionState } from '@/types';

interface HeaderProps {
  connectionState: ConnectionState;
  hostName: string;
  onMenuClick?: () => void;
  isMobileMenuOpen: boolean;
  onMobileMenuToggle: () => void;
}

export function Header({
  connectionState,
  hostName,
  onMenuClick,
  isMobileMenuOpen,
  onMobileMenuToggle,
}: HeaderProps) {
  const getConnectionIndicator = () => {
    switch (connectionState) {
      case 'connected':
        return (
          <>
            <Wifi className="h-4 w-4 text-green-600" />
            <span className="text-green-600 dark:text-green-400">Connected</span>
          </>
        );
      case 'connecting':
        return (
          <>
            <Wifi className="h-4 w-4 text-yellow-600 animate-pulse" />
            <span className="text-yellow-600 dark:text-yellow-400">Connecting...</span>
          </>
        );
      case 'error':
        return (
          <>
            <WifiOff className="h-4 w-4 text-red-600" />
            <span className="text-red-600 dark:text-red-400">Error</span>
          </>
        );
      default:
        return (
          <>
            <WifiOff className="h-4 w-4 text-neutral-500" />
            <span className="text-neutral-500 dark:text-neutral-400">Disconnected</span>
          </>
        );
    }
  };

  return (
    <header className={cn(
      'fixed top-0 left-0 right-0 z-40 border-b border-neutral-200 dark:border-neutral-700 bg-white/80 dark:bg-neutral-900/80 backdrop-blur-sm',
      isMobileMenuOpen && 'shadow-lg'
    )}>
      <div className="container-narrow">
        <div className="flex items-center justify-between h-14">
          {/* Left: Logo and Host */}
          <div className="flex items-center gap-4">
            <button
              onClick={onMobileMenuToggle}
              className="lg:hidden p-2 rounded-md hover:bg-neutral-100 dark:hover:bg-neutral-800"
              aria-label={isMobileMenuOpen ? 'Close menu' : 'Open menu'}
              aria-expanded={isMobileMenuOpen}
            >
              {isMobileMenuOpen ? <X className="h-5 w-5" /> : <Menu className="h-5 w-5" />}
            </button>
            
            <div className="flex items-center gap-2">
              <Server className="h-6 w-6 text-kamui-indigo-600 dark:text-kamui-indigo-400" />
              <span className="text-xl font-semibold text-neutral-900 dark:text-neutral-100">
                Kamui Cloud
              </span>
            </div>
            
            <div className="hidden sm:flex items-center gap-1.5 px-2 py-1 rounded-md bg-neutral-100 dark:bg-neutral-800">
              <Smartphone className="h-3.5 w-3.5 text-neutral-500" />
              <span className="text-xs font-medium text-neutral-700 dark:text-neutral-300">
                {hostName}
              </span>
            </div>
          </div>

          {/* Center: Connection State */}
          <div className="hidden md:flex items-center gap-2 px-3 py-1.5 rounded-md bg-neutral-50 dark:bg-neutral-800/50">
            {getConnectionIndicator()}
          </div>

          {/* Right: Actions */}
          <div className="flex items-center gap-2">
            <Button variant="ghost" size="sm" onClick={onMenuClick}>
              <Wifi className="h-4 w-4" />
              <span className="hidden sm:inline">Network</span>
            </Button>
          </div>
        </div>

        {/* Mobile Menu */}
        {isMobileMenuOpen && (
          <div className="lg:hidden border-t border-neutral-200 dark:border-neutral-700 py-4 animate-slide-down">
            <div className="flex flex-col gap-3">
              <div className="flex items-center gap-2 px-2 py-2 rounded-md bg-neutral-50 dark:bg-neutral-800/50">
                {getConnectionIndicator()}
              </div>
              <div className="flex items-center gap-2 px-2 py-2 rounded-md bg-neutral-50 dark:bg-neutral-800/50">
                <Smartphone className="h-4 w-4 text-neutral-500" />
                <span className="text-sm font-medium text-neutral-700 dark:text-neutral-300">{hostName}</span>
              </div>
              <Button variant="secondary" className="w-full justify-center">
                <Wifi className="h-4 w-4" />
                Network Settings
              </Button>
            </div>
          </div>
        )}
      </div>
    </header>
  );
}