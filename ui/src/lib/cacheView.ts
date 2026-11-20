// Display rules for prompt-cache figures. Kept free of imports so they can be
// tested directly with node. An unreported value is always a dash: a missing
// count is unknown, and unknown is never shown as 0.

function groupDigits(n: number): string {
  return n.toLocaleString('en-US');
}

export interface CacheCell {
  text: string;
  title: string;
}

// One request's cache split as "cached / input (pct)".
export function cacheCell(cached?: number | null, input?: number | null): CacheCell {
  if (cached == null || input == null || input <= 0 || cached < 0) {
    return {
      text: '-',
      title: 'Not reported: this runtime did not return a cached-token count for this request',
    };
  }
  const pct = Math.round((cached * 100) / input);
  return {
    text: `${groupDigits(cached)} / ${groupDigits(input)} (${pct}%)`,
    title: `${groupDigits(cached)} of ${groupDigits(input)} prompt tokens were served from the engine's prefix cache`,
  };
}

// A group's hit rate; null (no request in the group reported) is a dash.
export function hitPctLabel(pct?: number | null): string {
  if (pct == null) return '-';
  return `${Number(pct.toFixed(1))}%`;
}

// The window caption shown beside every cache figure.
export function cacheWindowLabel(windowRequests: number, reporting: number): string {
  const noun = windowRequests === 1 ? 'request' : 'requests';
  return `last ${groupDigits(windowRequests)} ${noun}, ${groupDigits(reporting)} reported`;
}
