import { useState, useEffect } from 'react';
import { useDemoMode, forcedDemo } from '../hooks/useDemoMode';
import { fetchSettings, updateSettings } from '../lib/api';
import { X, AlertCircle } from 'lucide-react';

// In the forced-demo (GitHub Pages) build, the "this is mock data" banner
// dismissal must never persist across visits - every surface on that build
// keeps rendering fake data forever, so the disclaimer has to keep coming
// back. sessionStorage (not localStorage/the settings API) scopes a dismiss
// to the current tab session only.
const FORCED_DEMO_HIDDEN_KEY = 'marbor_demo_banner_hidden_session';

export function DemoBanner() {
  const { demoMode } = useDemoMode();
  const [hidden, setHidden] = useState(() => {
    if (forcedDemo) {
      try {
        return sessionStorage.getItem(FORCED_DEMO_HIDDEN_KEY) === '1';
      } catch {
        return false;
      }
    }
    try {
      const stored = localStorage.getItem('demo_settings');
      if (stored) {
        const s = JSON.parse(stored);
        return !!s.hide_demo_banner;
      }
    } catch {}
    return false;
  });

  useEffect(() => {
    if (forcedDemo) return; // dismissal is session-scoped only, never fetched/persisted server-side
    const load = () => {
      fetchSettings()
        .then(s => setHidden(!!s.hide_demo_banner))
        .catch(() => {});
    };
    load();
    window.addEventListener('marbor-settings-change', load);
    return () => window.removeEventListener('marbor-settings-change', load);
  }, [demoMode]);

  if (!demoMode || hidden) return null;

  const handleDismiss = () => {
    setHidden(true);
    if (forcedDemo) {
      try { sessionStorage.setItem(FORCED_DEMO_HIDDEN_KEY, '1'); } catch {}
      return;
    }
    fetchSettings().then(s => {
      const updated = { ...s, hide_demo_banner: true };
      return updateSettings(updated);
    }).then(() => {
      window.dispatchEvent(new CustomEvent('marbor-settings-change'));
    }).catch(() => {});
  };

  return (
    <div className="relative animate-fade-in min-w-0">
      <div className="bg-brand/10 border border-brand/30 rounded-lg px-3 sm:px-4 py-3 flex items-center justify-between gap-3 sm:gap-4 min-w-0">
        <div className="flex items-center gap-3 min-w-0 flex-1">
          <div className="flex-shrink-0 w-8 h-8 rounded-full bg-brand/20 flex items-center justify-center">
            <AlertCircle className="w-4 h-4 text-brand" />
          </div>
          <div className="min-w-0">
            <p className="text-sm font-medium text-foreground">
              Demo mode active
            </p>
            <p className="text-xs text-muted-foreground leading-snug">
              All data shown is mock data, not your cluster.
              {!forcedDemo && ' Disable it in Settings.'}
            </p>
          </div>
        </div>
        <button
          onClick={handleDismiss}
          aria-label="Dismiss demo banner"
          className="flex-shrink-0 p-1 rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors min-w-[40px] min-h-[40px] sm:min-w-0 sm:min-h-0 flex items-center justify-center"
        >
          <X className="w-4 h-4" />
        </button>
      </div>
    </div>
  );
}
