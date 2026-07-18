import { useState } from 'react';
import { KeyRound } from 'lucide-react';
import { changePassword, saveSession, skipPasswordChangeThisSession } from '../lib/api';
import type { SessionData } from '../types';

interface Props {
  session: SessionData;
  onSuccess: (updated: SessionData) => void;
}

export function ForceChangePassword({ session, onSuccess }: Props) {
  const [newPw, setNewPw] = useState('');
  const [confirmPw, setConfirmPw] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [skipLimitReached, setSkipLimitReached] = useState(false);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (!newPw) { setError('New password is required'); return; }
    if (newPw !== confirmPw) { setError('Passwords do not match'); return; }
    if (newPw.length < 8) { setError('Password must be at least 8 characters'); return; }
    setSaving(true);
    setError(null);
    try {
      await changePassword('', newPw);
      const updated: SessionData = { ...session, mustChangePassword: false };
      saveSession({
        role: updated.role,
        username: updated.username,
        must_change_password: false,
        expires_at: '',
      });
      onSuccess(updated);
    } catch (err: any) {
      setError(err.message || 'Failed to change password');
    } finally {
      setSaving(false);
    }
  }

  // Grafana-style skip: lets the admin explore this session without
  // changing the password. Session-only - closing the tab or logging back
  // in re-prompts, since the user's own must_change_password flag is
  // untouched; only this session is reissued without the gate. Capped
  // server-side (3 skips) so a default/known password can't be dismissed
  // forever - once the cap is hit, the skip option is removed here too.
  async function handleSkip() {
    setSaving(true);
    setError(null);
    try {
      await skipPasswordChangeThisSession();
      onSuccess({ ...session, mustChangePassword: false });
    } catch (err: any) {
      const message = err.message || 'Failed to skip password change';
      setError(message);
      if (message.includes('Skip limit reached')) {
        setSkipLimitReached(true);
      }
      setSaving(false);
    }
  }

  return (
    <div className="min-h-screen bg-background flex items-center justify-center p-4">
      <div className="w-full max-w-sm">
        <div className="flex justify-center mb-6">
          <div className="w-12 h-12 rounded-xl flex items-center justify-center" style={{ background: '#1a1714' }}>
            <svg width="28" height="28" viewBox="0 0 100 100" fill="none" aria-hidden="true">
              <path d="M30 35 L30 65 M30 50 L50 35 L50 65 M50 50 L70 35 L70 65" stroke="#d4a853" strokeWidth="8" strokeLinecap="round" strokeLinejoin="round"/>
              <circle cx="75" cy="75" r="8" fill="#a87f3a" />
            </svg>
          </div>
        </div>

        <div className="bg-card border border-border rounded-xl p-6 shadow-sm">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-amber-500/10 rounded-lg">
              <KeyRound className="w-5 h-5 text-amber-600 dark:text-amber-400" />
            </div>
            <div>
              <h2 className="text-sm font-semibold text-foreground">Set New Password</h2>
              <p className="text-xs text-muted-foreground">Required before continuing</p>
            </div>
          </div>

          <p className="text-xs text-muted-foreground mb-4">
            Your account requires a password change. Choose a strong password to continue.
          </p>

          <form onSubmit={handleSubmit} className="space-y-4">
            <div>
              <label className="block text-xs font-medium text-muted-foreground mb-1.5">New Password</label>
              <input
                type="password"
                value={newPw}
                onChange={e => setNewPw(e.target.value)}
                autoFocus
                autoComplete="new-password"
                className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
                placeholder="Min. 8 characters"
              />
            </div>
            <div>
              <label className="block text-xs font-medium text-muted-foreground mb-1.5">Confirm Password</label>
              <input
                type="password"
                value={confirmPw}
                onChange={e => setConfirmPw(e.target.value)}
                autoComplete="new-password"
                className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
                placeholder="Repeat password"
              />
            </div>

            {error && (
              <p className="text-xs text-destructive bg-destructive/10 px-3 py-2 rounded-lg">{error}</p>
            )}

            <button
              type="submit"
              disabled={saving}
              className="w-full py-2 px-4 bg-primary text-primary-foreground text-sm font-medium rounded-lg hover:bg-primary/90 transition-colors disabled:opacity-50"
            >
              {saving ? 'Saving...' : 'Set Password & Continue'}
            </button>

            {!skipLimitReached && (
              <button
                type="button"
                onClick={handleSkip}
                disabled={saving}
                className="w-full text-center text-xs text-muted-foreground hover:text-foreground transition-colors disabled:opacity-50"
              >
                Skip for now
              </button>
            )}
          </form>
        </div>
      </div>
    </div>
  );
}
