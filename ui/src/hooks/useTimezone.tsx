import { createContext, useCallback, useContext, useEffect, useState } from 'react';
import { fetchSettings } from '../lib/api';
import { forcedDemo } from './useDemoMode';
import { loadSession } from '../lib/api';

// Context holds the operator's configured timezone string: "Local" or an IANA
// name (e.g. "Asia/Kolkata", "UTC"). Default "Local" matches backend default.
const TimezoneContext = createContext<string>('Local');

// How often to re-poll the settings endpoint for a live timezone change.
// Settings.tsx writes via PUT /admin/settings - other pages must notice within
// this interval without a hard reload (Activity must re-render on a Settings
// toggle without a reload).
const POLL_MS = 15_000;

// Custom event fired by Settings.tsx after a successful timezone PUT so the
// App-level poller (and any existing page) can update instantly without waiting
// for the next interval tick.
export const TIMEZONE_CHANGED_EVENT = 'marbor-timezone-changed';

export function TimezoneProvider({ children }: { children: React.ReactNode }) {
  const [tz, setTz] = useState<string>(() => {
    // Demo artifact has no backend; default to UTC so preview looks sane.
    if (forcedDemo) return 'UTC';
    return 'Local';
  });

  const load = useCallback(async () => {
    if (forcedDemo) {
      setTz(prev => (prev === 'UTC' ? prev : 'UTC'));
      return;
    }
    // Skip the fetch when no session exists (login screen) - avoids a 401
    // every POLL_MS and keeps the provider inert until an operator is logged
    // in. This mirrors App.tsx's pendingCount poll guard.
    if (!loadSession()) return;
    try {
      const data = await fetchSettings();
      const next = (data as { timezone?: string })?.timezone || 'Local';
      setTz(prev => (prev === next ? prev : next));
    } catch {
      // Keep previous value on fetch failure (network / 401 during demo toggle)
    }
  }, []);

  useEffect(() => {
    let active = true;
    const safeLoad = () => { if (active) load(); };
    safeLoad();
    const onChanged = () => safeLoad();
    window.addEventListener(TIMEZONE_CHANGED_EVENT, onChanged);
    const id = setInterval(safeLoad, POLL_MS);
    return () => {
      active = false;
      window.removeEventListener(TIMEZONE_CHANGED_EVENT, onChanged);
      clearInterval(id);
    };
  }, [load]);

  return (
    <TimezoneContext.Provider value={tz}>{children}</TimezoneContext.Provider>
  );
}

export function useTimezone(): string {
  return useContext(TimezoneContext);
}

// Imperative helper for Settings.tsx to broadcast a change without importing
// the hook (avoids circular import with api.ts side effects in tests).
export function notifyTimezoneChanged() {
  window.dispatchEvent(new Event(TIMEZONE_CHANGED_EVENT));
}
