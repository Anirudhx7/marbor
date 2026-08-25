import { useState, FormEvent } from 'react';
import { login, userLogin, saveSession } from '../lib/api';
import { forcedDemo } from '../hooks/useDemoMode';
import type { SessionData } from '../types';

interface LoginProps {
  onSuccess: (session: SessionData) => void;
  mode?: 'admin' | 'user';
}

export function Login({ onSuccess, mode = 'admin' }: LoginProps) {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);

  async function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    if (!username.trim() || !password.trim()) return;

    setLoading(true);
    setError('');

    try {
      const data = mode === 'user'
        ? await userLogin(username.trim(), password)
        : await login(username.trim(), password);
      saveSession(data);
      onSuccess({
        role: data.role,
        username: data.username,
        mustChangePassword: data.must_change_password,
      });
    } catch {
      setError('Invalid username or password');
      setPassword('');
      setLoading(false);
    }
  }

  return (
    <div className="min-h-screen bg-background text-foreground flex items-center justify-center p-4">
      <div className="w-full max-w-sm">
        <div className="glass-panel rounded-xl p-8">
          <div className="mb-8 text-center">
            <div className="inline-flex items-center justify-center mb-4">
              <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100" width="56" height="56">
                <rect width="100" height="100" rx="20" fill="#0a0a0a"/>
                <path d="M30 35 L30 65 M30 50 L50 35 L50 65 M50 50 L70 35 L70 65"
                  stroke="#d4a853" strokeWidth="8" strokeLinecap="round" strokeLinejoin="round" fill="none"/>
                <circle cx="75" cy="75" r="8" fill="#a87f3a"/>
              </svg>
            </div>
            <h1 className="text-xl font-semibold text-foreground">Marbor</h1>
            <p className="text-sm text-muted-foreground mt-1">
              {mode === 'user' ? 'Sign in to the user portal' : 'Sign in to the admin dashboard'}
            </p>
          </div>

          <form onSubmit={handleSubmit} className="space-y-4">
            <div>
              <label htmlFor="username" className="block text-sm font-medium text-muted-foreground mb-1.5">
                Username
              </label>
              <input
                id="username"
                type="text"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                placeholder={mode === 'user' ? 'username' : 'admin'}
                autoFocus
                autoComplete="username"
                disabled={loading}
                className="w-full px-3 py-2 rounded-lg bg-secondary border border-border text-foreground placeholder-muted-foreground text-sm focus:outline-none focus:ring-2 focus:ring-primary/50 focus:border-primary transition-colors disabled:opacity-50"
              />
            </div>

            <div>
              <label htmlFor="password" className="block text-sm font-medium text-muted-foreground mb-1.5">
                Password
              </label>
              <input
                id="password"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="••••••••"
                autoComplete="current-password"
                disabled={loading}
                className="w-full px-3 py-2 rounded-lg bg-secondary border border-border text-foreground placeholder-muted-foreground text-sm focus:outline-none focus:ring-2 focus:ring-primary/50 focus:border-primary transition-colors disabled:opacity-50"
              />
            </div>

            {error && (
              <p className="text-sm text-destructive">{error}</p>
            )}

            <button
              type="submit"
              disabled={loading || !username.trim() || !password.trim()}
              className="w-full px-4 py-2 rounded-lg bg-primary text-primary-foreground text-sm font-medium hover:bg-primary/90 transition-colors disabled:opacity-50 disabled:cursor-not-allowed flex items-center justify-center gap-2"
            >
              {loading ? (
                <>
                  <svg
                    className="animate-spin h-4 w-4"
                    xmlns="http://www.w3.org/2000/svg"
                    fill="none"
                    viewBox="0 0 24 24"
                  >
                    <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" />
                    <path className="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z" />
                  </svg>
                  Signing in...
                </>
              ) : (
                'Sign in'
              )}
            </button>
          </form>

          {forcedDemo && (
            <p className="mt-4 text-center text-xs text-muted-foreground">
              Demo credentials: <span className="font-mono text-foreground">admin</span> / <span className="font-mono text-foreground">admin</span>
            </p>
          )}
        </div>
      </div>
    </div>
  );
}
