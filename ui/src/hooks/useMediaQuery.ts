import { useCallback, useSyncExternalStore } from 'react';

// useMediaQuery tracks a CSS media query so a page can render ONE of two
// layouts instead of rendering both and hiding one with CSS (which keeps both
// sets of nodes in the DOM). Falls back to `fallback` where matchMedia is
// missing.
export function useMediaQuery(query: string, fallback = false): boolean {
  const hasMatchMedia = typeof window !== 'undefined' && typeof window.matchMedia === 'function';
  const subscribe = useCallback(
    (onChange: () => void) => {
      if (!hasMatchMedia) return () => {};
      const mql = window.matchMedia(query);
      mql.addEventListener('change', onChange);
      return () => mql.removeEventListener('change', onChange);
    },
    [query, hasMatchMedia],
  );
  const getSnapshot = () => (hasMatchMedia ? window.matchMedia(query).matches : fallback);
  return useSyncExternalStore(subscribe, getSnapshot, () => fallback);
}
