// Shared time-formatting helpers. Previously duplicated as formatRelative
// (Requests.tsx), formatDuration (PullProgressWidget.tsx), and formatUptime
// (GPUNodes.tsx) - a rounding/pluralization fix in one wouldn't propagate to
// the others. Behavior of each function is unchanged from its original.

// ---------------------------------------------------------------------------
// Zone-aware formatting - P393: every wall-clock render must go through these
// so the operator's configured `settings.timezone` (or "Local") is honoured.
// "Local" means browser local (no timeZone param); any other value is an IANA
// name validated by config.Validate() - bad values fall back to browser local
// rather than throwing a RangeError in the UI.
// ---------------------------------------------------------------------------

const tzValidationCache = new Map<string, boolean>();
function isValidTimeZone(tz: string): boolean {
  if (tzValidationCache.has(tz)) return tzValidationCache.get(tz)!;
  try {
    new Intl.DateTimeFormat('en-US', { timeZone: tz }).format(new Date());
    tzValidationCache.set(tz, true);
    return true;
  } catch {
    tzValidationCache.set(tz, false);
    return false;
  }
}

function resolveTimeZone(tz: string | undefined): string | undefined {
  if (!tz || tz === 'Local') return undefined;
  return isValidTimeZone(tz) ? tz : undefined;
}

// Formatter cache: key = `${tz}|${JSON.stringify(options)}`, avoids creating
// a new Intl.DateTimeFormat per cell per render (Activity can have 200 rows).
const formatterCache = new Map<string, Intl.DateTimeFormat>();
function getFormatter(tz: string | undefined, options: Intl.DateTimeFormatOptions): Intl.DateTimeFormat {
  const resolved = resolveTimeZone(tz);
  const key = `${resolved ?? 'Local'}|${JSON.stringify(options)}`;
  let f = formatterCache.get(key);
  if (!f) {
    f = new Intl.DateTimeFormat('en-US', resolved ? { timeZone: resolved, ...options } : options);
    formatterCache.set(key, f);
  }
  return f;
}

// Generic wall-clock formatter: UTC/sided ISO -> wall string in `tz`.
export function formatInTimezone(
  isoString: string,
  tz: string,
  options: Intl.DateTimeFormatOptions,
): string {
  try {
    const d = new Date(isoString);
    if (isNaN(d.getTime())) return isoString;
    return getFormatter(tz, options).format(d);
  } catch {
    return isoString;
  }
}

// Activity / audit wall clock: "May 23, 2026, 02:30:45 PM" (en-US month short)
export function formatDateTimeInZone(isoString: string, tz: string): string {
  return formatInTimezone(isoString, tz, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  });
}

// Date-only wall (Users Created column)
export function formatDateInZone(isoString: string, tz: string): string {
  return formatInTimezone(isoString, tz, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
  });
}

// Time-only wall (Predictive timestamp +/- chosen)
export function formatTimeInZone(isoString: string, tz: string): string {
  return formatInTimezone(isoString, tz, {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  });
}

// Hours label for UTC hour keys: "2026-05-23T14" (UTC) -> "19:30" in Asia/Kolkata.
// 24h "14:00" wall regardless of locale 12h preference - keeps chart axis stable
// even when table cells are 12h.
export function formatHourLabelInTimezone(hourKey: string, tz: string): string {
  try {
    const tIdx = hourKey.indexOf('T');
    if (tIdx === -1) return hourKey;
    const datePart = hourKey.slice(0, tIdx);
    const hourPart = hourKey.slice(tIdx + 1);
    const [y, m, d] = datePart.split('-').map(Number);
    const h = Number(hourPart);
    if (!isFinite(y) || !isFinite(m) || !isFinite(d) || !isFinite(h)) return hourKey;
    const utc = new Date(Date.UTC(y, m - 1, d, h, 0, 0));
    return getFormatter(tz, { hour: '2-digit', minute: '2-digit', hour12: false }).format(utc);
  } catch {
    return hourKey;
  }
}

// ---------------------------------------------------------------------------
// Wall -> UTC conversion - for picker "From/Until" wall strings like
// "2026-08-28T14:30" interpreted in configured zone `tz` (not browser zone).
// ---------------------------------------------------------------------------

function tzOffsetMinutesAt(date: Date, tz: string): number {
  const resolved = resolveTimeZone(tz);
  if (!resolved) return -date.getTimezoneOffset(); // browser offset (minutes east of UTC)
  // Wall millis of `date` as if its wall components were UTC, minus actual UTC
  const parts = new Intl.DateTimeFormat('en-US', {
    timeZone: resolved,
    year: 'numeric', month: 'numeric', day: 'numeric',
    hour: 'numeric', minute: 'numeric', second: 'numeric',
    hourCycle: 'h23',
  }).formatToParts(date);
  const map: Record<string, number> = {};
  for (const p of parts) if (p.type !== 'literal') map[p.type] = Number(p.value);
  const wallAsUtc = Date.UTC(map.year, map.month - 1, map.day, map.hour, map.minute, map.second);
  return (wallAsUtc - date.getTime()) / 60000;
}

export function wallDateTimeToUtcIso(wall: string, tz: string): string | null {
  // Accepts "YYYY-MM-DDTHH:MM" or "YYYY-MM-DDTHH:MM:SS" (wall in `tz`)
  const m = wall.match(/^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2}))?$/);
  if (!m) return null;
  const y = Number(m[1]), mo = Number(m[2]), d = Number(m[3]), hh = Number(m[4]), mm = Number(m[5]), ss = Number(m[6] ?? 0);
  if (!isFinite(y) || !isFinite(mo) || !isFinite(d) || !isFinite(hh) || !isFinite(mm)) return null;
  if (tz === 'Local' || !resolveTimeZone(tz)) {
    // Browser-local interpretation (preserves existing behaviour for "Local")
    const local = new Date(y, mo - 1, d, hh, mm, ss);
    return isNaN(local.getTime()) ? null : local.toISOString();
  }
  // Iterative convergence: wallAsUtc - offset => UTC
  let utc = Date.UTC(y, mo - 1, d, hh, mm, ss);
  for (let i = 0; i < 2; i++) {
    const off = tzOffsetMinutesAt(new Date(utc), tz);
    const next = Date.UTC(y, mo - 1, d, hh, mm, ss) - off * 60 * 1000;
    if (next === utc) break;
    utc = next;
  }
  const out = new Date(utc);
  return isNaN(out.getTime()) ? null : out.toISOString();
}

// Formats a wall string "YYYY-MM-DDTHH:MM" (in `tz`) as a localized wall
// like "Aug 28, 2026, 02:30 PM" - goes wall->UTC->formatInTimezone so the wall
// numbers survive the browser-zone indirection.
export function formatWallInZone(wall: string, tz: string): string {
  const iso = wallDateTimeToUtcIso(wall, tz);
  if (!iso) return wall;
  return formatDateTimeInZone(iso, tz);
}

// Returns now's wall components in `tz` (used for picker default "now" view).
export function nowWallInZone(tz: string): { y: number; m: number; d: number; h: number; min: number } {
  const now = new Date();
  const resolved = resolveTimeZone(tz);
  if (!resolved) {
    return { y: now.getFullYear(), m: now.getMonth() + 1, d: now.getDate(), h: now.getHours(), min: now.getMinutes() };
  }
  const parts = new Intl.DateTimeFormat('en-US', {
    timeZone: resolved,
    year: 'numeric', month: 'numeric', day: 'numeric',
    hour: 'numeric', minute: 'numeric',
    hourCycle: 'h23',
  }).formatToParts(now);
  const mp: Record<string, number> = {};
  for (const p of parts) if (p.type !== 'literal') mp[p.type] = Number(p.value);
  return { y: mp.year, m: mp.month, d: mp.day, h: mp.hour, min: mp.minute };
}

// "Xs/Xm/Xh/Xd ago" relative to now, from an ISO timestamp.
export function formatRelativeTime(isoString: string): string {
  const diffMs = Date.now() - new Date(isoString).getTime();
  const diffSecs = Math.floor(diffMs / 1000);
  if (diffSecs < 0) return 'just now';
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
