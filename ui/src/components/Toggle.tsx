// Compact toggle switch shared by boolean controls (Settings, Routing).
// `label` is the accessible name: the switch has no visible text of its own.
export function Toggle({ on, onToggle, label }: { on: boolean; onToggle: () => void; label: string }) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={on}
      aria-label={label}
      onClick={onToggle}
      // The hit area grows to 44px tall on touch screens without changing the
      // drawn switch.
      className={`relative inline-flex h-6 w-11 shrink-0 items-center rounded-full transition-colors pointer-coarse:after:absolute pointer-coarse:after:-inset-y-[10px] pointer-coarse:after:inset-x-0 pointer-coarse:after:content-[''] ${on ? 'bg-primary' : 'bg-muted-foreground/30'}`}
    >
      <span className={`inline-block h-4 w-4 transform rounded-full bg-white transition-transform ${on ? 'translate-x-6' : 'translate-x-1'}`} />
    </button>
  );
}
