import { useDemoMode, forcedDemo } from '../hooks/useDemoMode';

export function DemoBanner() {
  const { demoMode } = useDemoMode();
  if (!demoMode) return null;
  return (
    <div className="bg-amber-500 text-black text-sm font-medium text-center py-1.5 pl-14 pr-4 md:px-4">
      Demo mode - all data shown is mock data, not your cluster.
      {!forcedDemo && ' Disable it in Settings.'}
    </div>
  );
}
