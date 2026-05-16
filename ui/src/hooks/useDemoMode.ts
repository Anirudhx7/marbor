import { useState, useEffect } from 'react';

export function useDemoMode() {
  const [demoMode, setDemoMode] = useState<boolean>(() => {
    const saved = localStorage.getItem('ollama-mesh-demo-mode');
    return saved !== null ? saved === 'true' : true; // Default to true if not set
  });

  useEffect(() => {
    localStorage.setItem('ollama-mesh-demo-mode', demoMode.toString());
  }, [demoMode]);

  return { demoMode, setDemoMode };
}
