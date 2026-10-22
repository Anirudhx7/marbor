import { useEffect, useState } from 'react';

// useMediaQuery tracks a CSS media query so a page can render ONE of two
// layouts instead of rendering both and hiding one with CSS (which keeps both
// sets of nodes in the DOM). Falls back to `fallback` where matchMedia is
// missing.
export function useMediaQuery(query: string, fallback = false): boolean {
  const read = () => (typeof window !== 'undefined' && typeof window.matchMedia === 'function' ? window.matchMedia(query).matches : fallback);
  const [matches, setMatches] = useState<boolean>(read);

  useEffect(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return;
    const mql = window.matchMedia(query);
    const onChange = () => setMatches(mql.matches);
    onChange();
    mql.addEventListener('change', onChange);
    return () => mql.removeEventListener('change', onChange);
  }, [query]);

  return matches;
}
