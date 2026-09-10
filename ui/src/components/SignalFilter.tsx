import { Check } from 'lucide-react';

// SignalFilter - interactive placement-signal chips, ported from the landing
// page's live-trace signal grid, but as a true filter: non-matching items are
// removed so large fleets stay scannable. Controlled: the parent owns active
// ids and computes per-signal counts + the result line.
export interface SignalOption {
  id: string;
  label: string;
  count: number;
}

interface SignalFilterProps {
  signals: SignalOption[];
  activeIds: Set<string>;
  onToggle: (id: string) => void;
  onClear: () => void;
  // e.g. "4 of 6 nodes" - announced via the live region when it changes.
  resultText: string;
  label?: string;
}

export function SignalFilter({ signals, activeIds, onToggle, onClear, resultText, label = 'Filter by signal' }: SignalFilterProps) {
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-2" role="group" aria-label={label}>
        {signals.map((s) => {
          const on = activeIds.has(s.id);
          return (
            <button
              key={s.id}
              type="button"
              onClick={() => onToggle(s.id)}
              aria-pressed={on}
              className={`inline-flex min-h-[36px] items-center gap-1.5 rounded-full border px-3.5 text-xs font-semibold transition-colors ${
                on
                  ? 'border-primary text-primary bg-primary/10'
                  : 'border-border bg-card text-muted-foreground hover:border-primary/50 hover:text-foreground'
              }`}
            >
              {s.label}
              <span className="tabular-nums opacity-70" aria-hidden="true">
                {s.count}
              </span>
              {on && <Check className="w-3.5 h-3.5" aria-hidden="true" />}
            </button>
          );
        })}
      </div>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1" role="status" aria-live="polite">
        <span className="text-xs text-muted-foreground tabular-nums">{resultText}</span>
        {activeIds.size > 0 && (
          <button
            type="button"
            onClick={onClear}
            className="text-xs font-medium text-primary hover:underline"
          >
            Clear signals
          </button>
        )}
      </div>
    </div>
  );
}
