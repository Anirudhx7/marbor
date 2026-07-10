import { useState, useEffect } from 'react';
import { useDemoMode, forcedDemo } from '../hooks/useDemoMode';
import { fetchSettings } from '../lib/api';

export function DemoBanner() {
  const { demoMode } = useDemoMode();
  const [hidden, setHidden] = useState(false);

  useEffect(() => {
    if (forcedDemo) return;
    const load = () => {
      fetchSettings()
        .then(s => setHidden(!!s.hide_demo_banner))
        .catch(() => {});
    };
    load();
    window.addEventListener('ollama-mesh-settings-change', load);
    return () => window.removeEventListener('ollama-mesh-settings-change', load);
  }, [demoMode]);

  if (!demoMode || hidden) return null;
  
  return (
    <div className="bg-amber-500 text-black text-sm font-medium text-center py-1.5 pl-14 pr-4 md:px-4">
      Demo mode - all data shown is mock data, not your cluster.
      {!forcedDemo && ' Disable it in Settings.'}
    </div>
  );
}
