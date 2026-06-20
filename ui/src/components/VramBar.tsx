interface VramBarProps {
  used: number; // GB
  total: number; // GB, 0 = unknown
  source?: 'nvidia' | 'api' | 'declared' | 'none';
  size?: 'sm' | 'md';
}

const SOURCE_LABEL: Record<string, string> = {
  nvidia: 'nvidia-smi',
  api: 'live · /api/ps',
  declared: 'declared',
};

export function VramBar({ used, total, source, size = 'md' }: VramBarProps) {
  const barHeight = `${size === 'sm' ? 'h-1.5' : 'h-2'}`;
  const sourceTag = source && SOURCE_LABEL[source] ? (
    <span className="text-[10px] uppercase tracking-wide text-muted-foreground/70 font-mono">
      {SOURCE_LABEL[source]}
    </span>
  ) : null;

  // No usable data at all (idle node, no loaded models, no declared capacity).
  if (total === 0 && used === 0) {
    return (
      <div className="space-y-1.5">
        <div className="flex items-center justify-between text-xs">
          <span className="text-muted-foreground font-medium">VRAM Usage</span>
          <span className="text-muted-foreground font-mono">-</span>
        </div>
        <div className={`w-full bg-secondary rounded-full overflow-hidden ${barHeight}`} />
      </div>
    );
  }

  // Real used-VRAM (summed from the node's own /api/ps) but no known capacity:
  // a remote node where nvidia-smi can't reach and no vram_total_mb was declared.
  // Show the honest figure rather than faking a percentage bar.
  if (total === 0) {
    return (
      <div className="space-y-1.5">
        <div className="flex items-center justify-between text-xs">
          <span className="text-muted-foreground font-medium">VRAM in use</span>
          <span className="text-foreground/80 font-mono">{used.toFixed(1)}GB / -</span>
        </div>
        <div className={`w-full bg-secondary rounded-full overflow-hidden ${barHeight} relative`}>
          {/* Real usage, capacity unknown - neutral fill, no proportion implied. */}
          <div className="absolute inset-0 bg-primary/30" />
        </div>
        <div className="flex items-center justify-between">
          <span className="text-[10px] text-muted-foreground/70">capacity unknown</span>
          {sourceTag}
        </div>
      </div>
    );
  }

  const percentage = Math.min((used / total) * 100, 100);

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
          {used.toFixed(1)}GB / {total.toFixed(0)}GB
        </span>
      </div>
      <div className={`w-full bg-secondary rounded-full overflow-hidden ${barHeight}`}>
        <div
          className={`h-full transition-all duration-500 ease-out ${getStatusColor()}`}
          style={{ width: `${percentage}%` }}
        />
      </div>
      {sourceTag && <div className="flex justify-end">{sourceTag}</div>}
    </div>
  );
}
