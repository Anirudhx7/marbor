import type { InputHTMLAttributes, ReactNode } from 'react';
import { X } from 'lucide-react';

// FilterField adds a small label above a filter control - a bare input or
// select is impossible to tell apart from its neighbors at a glance without
// one. Shared by every page with a multi-field filter bar (Requests,
// Activity) so labeling stays visually consistent across them.
export function FilterField({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <label className="block text-xs font-medium text-muted-foreground mb-1">{label}</label>
      {children}
    </div>
  );
}

// ClearableInput wraps a text/datetime filter input with an inline "x"
// button once it has a value, so undoing a filter (including one picked
// from a datalist) doesn't require manually backspacing the whole thing.
// `icon` is optional - pages that prefix the field with a lucide icon (e.g.
// Activity's Search/Globe icons) pass it here instead of hand-rolling the
// absolute-positioned icon + padding themselves.
export function ClearableInput(
  props: InputHTMLAttributes<HTMLInputElement> & { onClear: () => void; icon?: ReactNode }
) {
  const { onClear, className, value, icon, ...rest } = props;
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
        className={`${className ?? ''} w-full ${icon ? 'pl-9' : 'pl-3'} ${value ? 'pr-7' : 'pr-3'}`}
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
