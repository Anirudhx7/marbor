import type { ReactNode } from 'react';
import type { LucideIcon } from 'lucide-react';

// EmptyState - one shared empty-state voice for the whole app: icon medallion,
// serif title (a brand moment, mirroring the marketing site's editorial type),
// muted copy, optional action. Replaces the scattered bare muted paragraphs.
interface EmptyStateProps {
  icon: LucideIcon;
  title: string;
  copy?: string;
  action?: ReactNode;
  compact?: boolean;
}

export function EmptyState({ icon: Icon, title, copy, action, compact = false }: EmptyStateProps) {
  return (
    <div className={`flex flex-col items-center text-center ${compact ? 'gap-1.5 py-8' : 'gap-2 py-12'}`}>
      <span className="mb-2 grid h-12 w-12 place-items-center rounded-full bg-secondary text-muted-foreground/60" aria-hidden="true">
        <Icon className="h-6 w-6" />
      </span>
      <h3 className="font-display text-xl font-semibold text-foreground text-balance">{title}</h3>
      {copy && <p className="max-w-sm text-sm text-muted-foreground">{copy}</p>}
      {action && <div className="mt-2">{action}</div>}
    </div>
  );
}
