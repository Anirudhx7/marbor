import { type ReactNode } from 'react';

interface TooltipProps {
  label: string;
  children: ReactNode;
  side?: 'top' | 'bottom';
}

export function Tooltip({ label, children, side = 'top' }: TooltipProps) {
  const placement = side === 'top'
    ? 'bottom-full left-1/2 -translate-x-1/2 mb-1.5'
    : 'top-full left-1/2 -translate-x-1/2 mt-1.5';

  return (
    <div className="relative group">
      {children}
      <span className={`pointer-events-none absolute ${placement} px-2 py-1 rounded-md text-xs font-medium bg-popover text-popover-foreground border border-border shadow-md whitespace-nowrap opacity-0 group-hover:opacity-100 transition-opacity duration-100 z-50`}>
        {label}
      </span>
    </div>
  );
}
