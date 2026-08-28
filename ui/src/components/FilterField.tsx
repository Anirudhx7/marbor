import type { InputHTMLAttributes, ReactNode } from 'react';
import { X } from 'lucide-react';

// FilterField adds a small label above a filter control - a bare input or
// select is impossible to tell apart from its neighbors at a glance without
// one. Shared by every page with a multi-field filter bar (Requests,
// Activity) so labeling stays visually consistent across them.
// Unified filter bar primitives - single source for every page's filter bar
// (Requests, Activity, Models, etc.) so spacing, card style, grid, and the
// "Clear all filters" row cannot drift per-page.

export function FilterField({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <label className="block text-[10px] uppercase tracking-wider font-semibold text-muted-foreground mb-1">{label}</label>
      {children}
    </div>
  );
}

// FilterBar is the outer card for every filter bar - use for ALL pages
export function FilterBar({ children }: { children: ReactNode }) {
  return (
    <div className="bg-card border border-border rounded-xl p-4 shadow-sm space-y-4">
      {children}
    </div>
  );
}

// FilterBarGrid - unified grid for filter controls (gap-3 everywhere)
export function FilterBarGrid({ children, colsClass }: { children: ReactNode; colsClass?: string }) {
  // colsClass override only when a page needs a different column count, but
  // gap and spacing stay locked here.
  return (
    <div className={colsClass ?? "grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-3"}>
      {children}
    </div>
  );
}

// FilterBarClear - single style for "Clear all filters" (count + solid button + border-t)
export function FilterBarClear({ countText, onClear }: { countText?: string; onClear: () => void }) {
  return (
    <div className="flex items-center justify-between pt-3 border-t border-border">
      <span className="text-xs text-muted-foreground">{countText ?? ""}</span>
      <button
        onClick={onClear}
        className="px-3 py-1.5 bg-secondary text-foreground hover:bg-secondary/80 rounded-lg text-xs font-medium transition-colors"
      >
        Clear all filters
      </button>
    </div>
  );
}

// ClearableInput wraps a text/datetime filter input with an inline "x"
// button once it has a value, so undoing a filter (including one picked
// from a datalist) doesn't require manually backspacing the whole thing.
// `icon` is optional - pages that prefix the field with a lucide icon (e.g.
// Activity's Search/Globe icons) pass it here instead of hand-rolling the
// absolute-positioned icon + padding themselves.
// Visuals are unified here (bg-secondary/30, focus:ring-2) to match
// CustomSelect/CustomCombobox/CustomDateTimePicker - callers should NOT
// pass a custom className with divergent bg/ring/padding.
const CLEARABLE_INPUT_BASE =
  "w-full py-2 text-sm rounded-lg border border-border bg-secondary/30 text-foreground placeholder:text-muted-foreground/60 focus:outline-none focus:ring-2 focus:ring-primary/40 transition-colors";

export function ClearableInput(
  props: InputHTMLAttributes<HTMLInputElement> & { onClear: () => void; icon?: ReactNode }
) {
  const { onClear, className, value, icon, ...rest } = props;
  const inputClass = `${CLEARABLE_INPUT_BASE} ${icon ? 'pl-9' : 'pl-3'} ${value ? 'pr-7' : 'pr-3'} ${className ?? ''}`;
  return (
    <div className="relative w-full">
      {icon && (
        <span className="absolute left-3 top-1/2 -translate-y-1/2 text-muted-foreground pointer-events-none">
          {icon}
        </span>
      )}
      <input
        {...rest}
        value={value}
        className={inputClass}
      />
      {!!value && (
        <button
          type="button"
          onClick={onClear}
          title="Clear"
          className="absolute right-1.5 top-1/2 -translate-y-1/2 p-0.5 rounded text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors"
        >
          <X className="w-3.5 h-3.5" />
        </button>
      )}
    </div>
  );
}
