// Shared time-formatting helpers. Previously duplicated as formatRelative
// (Requests.tsx), formatDuration (PullProgressWidget.tsx), and formatUptime
// (GPUNodes.tsx) - a rounding/pluralization fix in one wouldn't propagate to
// the others. Behavior of each function is unchanged from its original.

// "Xs/Xm/Xh/Xd ago" relative to now, from an ISO timestamp.
export function formatRelativeTime(isoString: string): string {
  const diffMs = Date.now() - new Date(isoString).getTime();
  const diffSecs = Math.floor(diffMs / 1000);
  if (diffSecs < 60) return `${diffSecs}s ago`;
  const diffMins = Math.floor(diffSecs / 60);
  if (diffMins < 60) return `${diffMins}m ago`;
  const diffHours = Math.floor(diffMins / 60);
  if (diffHours < 24) return `${diffHours}h ago`;
  return `${Math.floor(diffHours / 24)}d ago`;
}

// "Xm Ys" or "Ys" - short-form duration, no hours/days tier.
export function formatDurationShort(seconds: number): string {
  if (!isFinite(seconds) || seconds < 0) return '';
  const m = Math.floor(seconds / 60);
  const s = Math.floor(seconds % 60);
  return m > 0 ? `${m}m ${s}s` : `${s}s`;
}

// "Xd Yh" / "Xh Ym" / "Xm" - cascading long-form duration (uptime).
export function formatDurationLong(seconds: number): string {
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${minutes}m`;
  return `${minutes}m`;
}
