import { useState, useEffect } from 'react';

const STORAGE_KEY = 'marbor-demo-mode';
const CHANGE_EVENT = 'marbor-demo-mode-change';

export const forcedDemo = import.meta.env.VITE_FORCE_DEMO === 'true';

// Router-aware "what path is actually showing right now" check, safe to call
// from inside any timer/async callback (unlike React Router's useLocation(),
// this reads the live browser URL directly, which matters because a stale
// route captured from a closure would silently drift from what's on screen).
//
// The public GitHub Pages demo (forcedDemo) runs under HashRouter, where the
// route lives in the URL hash (e.g. "#/api-keys"), not window.location.pathname
// (which stays constant at the page's real path). Every other build runs
// under BrowserRouter with basename "/", where window.location.pathname IS
// the route.
export function currentAppPath(): string {
  if (forcedDemo) {
    const hash = window.location.hash;
    const path = hash.startsWith('#') ? hash.slice(1) : hash;
    return path || '/';
  }
  return window.location.pathname;
}

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
