import { useState, useEffect } from 'react';

const STORAGE_KEY = 'ollama-mesh-demo-mode';
const CHANGE_EVENT = 'ollama-mesh-demo-mode-change';

export const forcedDemo = import.meta.env.VITE_FORCE_DEMO === 'true';

function readDemoMode(): boolean {
  if (forcedDemo) return true;
  return localStorage.getItem(STORAGE_KEY) === 'true'; // Default to false (live data)
}

export function useDemoMode() {
  const [demoMode, setDemoMode] = useState<boolean>(readDemoMode);

  useEffect(() => {
    if (!forcedDemo) {
      localStorage.setItem(STORAGE_KEY, demoMode.toString());
    }
    window.dispatchEvent(new Event(CHANGE_EVENT));
  }, [demoMode]);

  // Keep all hook instances (pages, banner) in sync when any of them toggles
  useEffect(() => {
    const sync = () => setDemoMode(readDemoMode());
    window.addEventListener(CHANGE_EVENT, sync);
    return () => window.removeEventListener(CHANGE_EVENT, sync);
  }, []);

  return { demoMode, setDemoMode };
}
