import { useState, useEffect } from 'react';

const STORAGE_KEY = 'marbor-sidebar-collapsed';

export function useSidebarCollapsed() {
  const [collapsed, setCollapsed] = useState(() => {
    try {
      return localStorage.getItem(STORAGE_KEY) === 'true';
    } catch {
      return false;
    }
  });

  useEffect(() => {
    try {
      localStorage.setItem(STORAGE_KEY, String(collapsed));
    } catch {
      // ignore - private browsing or quota
    }
  }, [collapsed]);

  const toggle = () => setCollapsed((c) => !c);

  return { collapsed, setCollapsed, toggle };
}
