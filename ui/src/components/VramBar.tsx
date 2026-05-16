interface VramBarProps {
  used: number;
  total: number;
  size?: 'sm' | 'md';
}

export function VramBar({ used, total, size = 'md' }: VramBarProps) {
  const percentage = (used / total) * 100;
  
  const getStatusColor = () => {
    if (percentage > 90) return 'bg-destructive';
    if (percentage > 70) return 'bg-amber-500';
    return 'bg-primary';
  };

  return (
    <div className="space-y-1.5">
      <div className="flex items-center justify-between text-xs">
        <span className="text-muted-foreground font-medium">VRAM Usage</span>
        <span className="text-foreground/80 font-mono">
          {used.toFixed(1)}GB / {total}GB
        </span>
      </div>
      <div className={`w-full bg-secondary rounded-full overflow-hidden ${size === 'sm' ? 'h-1.5' : 'h-2'}`}>
        <div 
          className={`h-full transition-all duration-500 ease-out ${getStatusColor()}`}
          style={{ width: `${percentage}%` }}
        />
      </div>
    </div>
  );
}
