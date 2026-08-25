import { useState, useEffect } from 'react';
import { useDemoMode, forcedDemo } from '../hooks/useDemoMode';
import { fetchSettings, updateSettings } from '../lib/api';
import { X, AlertCircle } from 'lucide-react';

export function DemoBanner() {
  const { demoMode } = useDemoMode();
  const [hidden, setHidden] = useState(() => {
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
    fetchSettings().then(s => {
      const updated = { ...s, hide_demo_banner: true };
      return updateSettings(updated);
    }).then(() => {
      window.dispatchEvent(new CustomEvent('marbor-settings-change'));
    }).catch(() => {});
  };

  return (
    <div className="relative animate-fade-in">
      <div className="bg-brand/10 border border-brand/30 rounded-lg px-4 py-3 flex items-center justify-between gap-4">
        <div className="flex items-center gap-3">
          <div className="flex-shrink-0 w-8 h-8 rounded-full bg-brand/20 flex items-center justify-center">
            <AlertCircle className="w-4 h-4 text-brand" />
          </div>
          <div>
            <p className="text-sm font-medium text-foreground">
              Demo mode active
            </p>
            <p className="text-xs text-muted-foreground">
              All data shown is mock data, not your cluster.
              {!forcedDemo && ' Disable it in Settings.'}
            </p>
          </div>
        </div>
        <button
          onClick={handleDismiss}
          aria-label="Dismiss demo banner"
          className="flex-shrink-0 p-1 rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors"
        >
          <X className="w-4 h-4" />
        </button>
      </div>
    </div>
  );
}
