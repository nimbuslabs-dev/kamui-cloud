'use client';

import { useState, FormEvent } from 'react';
import { useRouter } from 'next/navigation';
import { Lock, AlertCircle } from 'lucide-react';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from '@/components/ui/Card';
import { login } from '@/lib/api';

export default function LoginPage() {
  const router = useRouter();
  const [password, setPassword] = useState('');
  const [showPassword, setShowPassword] = useState(false);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setError('');
    setLoading(true);

    try {
      await login(password);
      router.push('/');
      router.refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Login failed');
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="min-h-screen flex items-center justify-center bg-neutral-50 dark:bg-neutral-950 px-4">
      <Card className="w-full max-w-md">
        <CardHeader className="text-center">
          <div className="mx-auto mb-4 p-3 rounded-full bg-kamui-indigo-100 dark:bg-kamui-indigo-900/30 w-12 h-12 flex items-center justify-center">
            <Lock className="h-6 w-6 text-kamui-indigo-600 dark:text-kamui-indigo-400" />
          </div>
          <CardTitle className="text-xl">Sign in to Kamui Cloud</CardTitle>
          <CardDescription>
            Enter your admin password to access the workspace dashboard
          </CardDescription>
        </CardHeader>
        
        <CardContent>
          <form onSubmit={handleSubmit} className="space-y-4">
            {error && (
              <div className="flex items-center gap-2 p-3 rounded-md bg-red-50 dark:bg-red-900/20 border border-red-200 dark:border-red-800">
                <AlertCircle className="h-4 w-4 text-red-600 flex-shrink-0" />
                <p className="text-sm text-red-700 dark:text-red-300">{error}</p>
              </div>
            )}

            <Input
              label="Admin Password"
              type={showPassword ? 'text' : 'password'}
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="Enter admin password"
              autoComplete="current-password"
              autoFocus
              disabled={loading}
            />
            
            <div className="flex items-center">
              <input
                type="checkbox"
                id="show-password"
                checked={showPassword}
                onChange={(e) => setShowPassword(e.target.checked)}
                className="h-4 w-4 rounded border-neutral-300 text-kamui-indigo-600 focus:ring-kamui-indigo-500"
              />
              <label htmlFor="show-password" className="ml-2 text-sm text-neutral-600 dark:text-neutral-400">
                Show password
              </label>
            </div>

            <Button type="submit" className="w-full" loading={loading}>
              <Lock className="h-4 w-4" />
              Sign In
            </Button>
          </form>

        </CardContent>
      </Card>
    </div>
  );
}