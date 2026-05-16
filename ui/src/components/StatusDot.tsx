interface StatusDotProps {
  status: 'healthy' | 'degraded' | 'down' | 'online' | 'offline' | 'active' | 'suspended' | 'rate-limited';
  size?: 'sm' | 'md' | 'lg';
  pulse?: boolean;
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

export function StatusDot({ status, size = 'md', pulse = false }: StatusDotProps) {
  return (
    <span
      className={`inline-block rounded-full ${statusColors[status]} ${sizeClasses[size]} ${
        pulse ? 'animate-pulse' : ''
      }`}
    />
  );
}
