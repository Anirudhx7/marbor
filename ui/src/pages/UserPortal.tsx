import { useState, FormEvent } from 'react';
import { changePassword, clearSession } from '../lib/api';
import type { SessionData } from '../types';

interface UserPortalProps {
  session: SessionData;
  onLogout: () => void;
}

export function UserPortal({ session, onLogout }: UserPortalProps) {
  const [showPwForm, setShowPwForm] = useState(false);
  const [currentPw, setCurrentPw] = useState('');
  const [newPw, setNewPw] = useState('');
  const [confirmPw, setConfirmPw] = useState('');
  const [pwError, setPwError] = useState('');
  const [pwSuccess, setPwSuccess] = useState(false);
  const [loading, setLoading] = useState(false);

  function handleLogout() {
    clearSession();
    onLogout();
  }

  async function handleChangePw(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    if (newPw !== confirmPw) {
      setPwError('Passwords do not match');
      return;
    }
    if (newPw.length < 8) {
      setPwError('Password must be at least 8 characters');
      return;
    }
    setLoading(true);
    setPwError('');
    try {
      await changePassword(currentPw, newPw);
      setPwSuccess(true);
      setCurrentPw('');
      setNewPw('');
      setConfirmPw('');
      setShowPwForm(false);
    } catch (err: any) {
      setPwError(err.message || 'Failed to change password');
    } finally {
      setLoading(false);
    }
  }

  return (
    <div className="min-h-screen bg-background text-foreground flex flex-col items-center justify-start pt-16 p-4">
      <div className="w-full max-w-lg space-y-4">
        {/* Header */}
        <div className="glass-panel rounded-xl p-6 flex items-center gap-4">
          <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100" width="44" height="44">
            <rect width="100" height="100" rx="20" fill="#0a0a0a"/>
            <path d="M30 35 L30 65 M30 50 L50 35 L50 65 M50 50 L70 35 L70 65"
              stroke="#d4a853" strokeWidth="8" strokeLinecap="round" strokeLinejoin="round" fill="none"/>
            <circle cx="75" cy="75" r="8" fill="#a87f3a"/>
          </svg>
          <div className="flex-1">
            <h1 className="text-lg font-semibold text-foreground">Marbor</h1>
            <p className="text-sm text-muted-foreground">User Portal</p>
          </div>
          <button
            onClick={handleLogout}
            className="px-3 py-1.5 text-xs rounded-lg bg-secondary text-foreground hover:bg-secondary/80 transition-colors cursor-pointer"
          >
            Sign out
          </button>
        </div>

        {/* Account info */}
        <div className="glass-panel rounded-xl p-6 space-y-3">
          <h2 className="text-sm font-semibold text-foreground">Account</h2>
          <div className="grid grid-cols-2 gap-2 text-sm">
            <span className="text-muted-foreground">Username</span>
            <span className="font-mono text-foreground">{session.username}</span>
            <span className="text-muted-foreground">Role</span>
            <span className="font-mono text-foreground capitalize">{session.role}</span>
          </div>
        </div>

        {/* How to connect */}
        <div className="glass-panel rounded-xl p-6 space-y-3">
          <h2 className="text-sm font-semibold text-foreground">How to connect</h2>
          <p className="text-sm text-muted-foreground">
            Ask your admin for the marbor endpoint URL and your API key. Then point your tools at it:
          </p>
          <div className="space-y-2 text-xs">
            <div className="bg-secondary rounded-lg p-3 font-mono overflow-x-auto">
              <p className="text-muted-foreground mb-1"># OpenAI SDK / any LLM client</p>
              <p className="text-foreground">OPENAI_BASE_URL=http://&lt;marbor-host&gt;:11434/v1</p>
              <p className="text-foreground">OPENAI_API_KEY=&lt;your-api-key&gt;</p>
            </div>
            <div className="bg-secondary rounded-lg p-3 font-mono overflow-x-auto">
              <p className="text-muted-foreground mb-1"># Ollama CLI</p>
              <p className="text-foreground">OLLAMA_HOST=http://&lt;marbor-host&gt;:11434</p>
            </div>
          </div>
        </div>

        {/* Change password */}
        <div className="glass-panel rounded-xl p-6 space-y-3">
          <div className="flex items-center justify-between">
            <h2 className="text-sm font-semibold text-foreground">Change password</h2>
            {!showPwForm && (
              <button
                onClick={() => { setShowPwForm(true); setPwSuccess(false); }}
                className="px-3 py-1.5 text-xs rounded-lg bg-secondary text-foreground hover:bg-secondary/80 transition-colors cursor-pointer"
              >
                Change
              </button>
            )}
          </div>
          {pwSuccess && !showPwForm && (
            <p className="text-sm text-green-600 dark:text-green-400">Password changed successfully.</p>
          )}
          {showPwForm && (
            <form onSubmit={handleChangePw} className="space-y-3">
              <input
                type="password"
                placeholder="Current password"
                value={currentPw}
                onChange={e => setCurrentPw(e.target.value)}
                autoComplete="current-password"
                disabled={loading}
                className="w-full px-3 py-2 rounded-lg bg-secondary border border-border text-foreground placeholder-muted-foreground text-sm focus:outline-none focus:ring-2 focus:ring-primary/50 focus:border-primary transition-colors disabled:opacity-50"
              />
              <input
                type="password"
                placeholder="New password (min 8 chars)"
                value={newPw}
                onChange={e => setNewPw(e.target.value)}
                autoComplete="new-password"
                disabled={loading}
                className="w-full px-3 py-2 rounded-lg bg-secondary border border-border text-foreground placeholder-muted-foreground text-sm focus:outline-none focus:ring-2 focus:ring-primary/50 focus:border-primary transition-colors disabled:opacity-50"
              />
              <input
                type="password"
                placeholder="Confirm new password"
                value={confirmPw}
                onChange={e => setConfirmPw(e.target.value)}
                autoComplete="new-password"
                disabled={loading}
                className="w-full px-3 py-2 rounded-lg bg-secondary border border-border text-foreground placeholder-muted-foreground text-sm focus:outline-none focus:ring-2 focus:ring-primary/50 focus:border-primary transition-colors disabled:opacity-50"
              />
              {pwError && <p className="text-sm text-destructive">{pwError}</p>}
              <div className="flex gap-2">
                <button
                  type="submit"
                  disabled={loading || !currentPw || !newPw || !confirmPw}
                  className="flex-1 px-4 py-2 rounded-lg bg-primary text-primary-foreground text-sm font-medium hover:bg-primary/90 transition-colors disabled:opacity-50 disabled:cursor-not-allowed cursor-pointer"
                >
                  {loading ? 'Saving...' : 'Save'}
                </button>
                <button
                  type="button"
                  onClick={() => { setShowPwForm(false); setPwError(''); setCurrentPw(''); setNewPw(''); setConfirmPw(''); }}
                  className="px-4 py-2 rounded-lg bg-secondary text-foreground text-sm hover:bg-secondary/80 transition-colors cursor-pointer"
                >
                  Cancel
                </button>
              </div>
            </form>
          )}
        </div>
      </div>
    </div>
  );
}
