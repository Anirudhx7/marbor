interface StatusDotProps {
  status: 'healthy' | 'degraded' | 'down' | 'online' | 'offline' | 'active' | 'suspended' | 'rate-limited';
  size?: 'sm' | 'md' | 'lg';
  pulse?: boolean;
  // halo renders the soft status-colored ring from the marketing site's live
  // dot - reserved for the single "system is live" indicator per view.
  halo?: boolean;
  title?: string;
}

const statusColors = {
  healthy: 'bg-success',
  online: 'bg-success',
  active: 'bg-primary',
  degraded: 'bg-warning',
  down: 'bg-destructive',
  offline: 'bg-destructive',
  suspended: 'bg-muted-foreground',
  'rate-limited': 'bg-warning',
};

const sizeClasses = {
  sm: 'w-1.5 h-1.5',
  md: 'w-2 h-2',
  lg: 'w-2.5 h-2.5',
};

const haloColors: Record<StatusDotProps['status'], string> = {
  healthy: 'shadow-[0_0_0_4px_hsl(var(--success)_/_0.18)]',
  online: 'shadow-[0_0_0_4px_hsl(var(--success)_/_0.18)]',
  active: 'shadow-[0_0_0_4px_hsl(var(--primary)_/_0.18)]',
  degraded: 'shadow-[0_0_0_4px_hsl(var(--warning)_/_0.18)]',
  down: 'shadow-[0_0_0_4px_hsl(var(--destructive)_/_0.18)]',
  offline: 'shadow-[0_0_0_4px_hsl(var(--destructive)_/_0.18)]',
  suspended: 'shadow-[0_0_0_4px_hsl(var(--muted-foreground)_/_0.18)]',
  'rate-limited': 'shadow-[0_0_0_4px_hsl(var(--warning)_/_0.18)]',
};

const statusLabels = {
  healthy: 'Healthy',
  online: 'Online',
  active: 'Active',
  degraded: 'Degraded',
  down: 'Down',
  offline: 'Offline',
  suspended: 'Suspended',
  'rate-limited': 'Rate limited',
};

export function StatusDot({ status, size = 'md', pulse = false, halo = false, title }: StatusDotProps) {
  return (
    <span
      title={title ?? statusLabels[status]}
      className={`inline-block rounded-full ${statusColors[status]} ${sizeClasses[size]} ${
        pulse ? 'animate-pulse' : ''
      } ${halo ? haloColors[status] : ''}`}
    />
  );
}
