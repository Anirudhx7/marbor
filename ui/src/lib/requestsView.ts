// The Requests page's shareable view state: filters, quick range, page size and
// the page cursor, read from and written to the URL query string, plus the
// pure rules for paging and chip removal. Kept free of imports so all of it can
// be tested directly with node.

export const PAGE_SIZES = [50, 100, 250, 500] as const;
export const DEFAULT_PAGE_SIZE = 100;
export const LIVE_POLL_MS = 3000;

export type CloudFilter = 'all' | 'local' | 'cloud';
export type StatusFilter = 'all' | 'success' | 'client_error' | 'server_error';
// 'custom' is not a selectable preset: it marks a hand-picked From value, so
// the Quick range select shows its placeholder instead of a stale preset.
export type SincePreset = 'all' | '15m' | '1h' | '24h' | 'custom';

// before: the page of rows older than id; after: the page newer than id.
export interface PageCursor {
  dir: 'before' | 'after';
  id: number;
}

export interface RequestsView {
  model: string;
  key: string;
  node: string;
  cloud: CloudFilter;
  status: StatusFilter;
  range: SincePreset;
  from: string; // wall time "YYYY-MM-DDTHH:MM" in the display timezone
  until: string;
  size: number;
  cursor: PageCursor | null;
}

const CLOUD_VALUES: readonly string[] = ['local', 'cloud'];
const STATUS_VALUES: readonly string[] = ['success', 'client_error', 'server_error'];
const RANGE_VALUES: readonly string[] = ['15m', '1h', '24h'];
const WALL_TIME = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})$/;

function parseCursorId(raw: string | null): number | null {
  if (raw === null || !/^\d+$/.test(raw)) return null;
  const n = Number(raw);
  return Number.isSafeInteger(n) && n > 0 ? n : null;
}

// A wall time must be a real calendar time: 2026-02-31 or 25:61 would roll
// over to a different instant when converted, so they are rejected instead.
function validWallTime(raw: string | null): string {
  if (raw === null) return '';
  const m = WALL_TIME.exec(raw);
  if (!m) return '';
  const [y, mo, d, h, mi] = m.slice(1).map(Number);
  const t = new Date(Date.UTC(y, mo - 1, d, h, mi));
  const ok =
    t.getUTCFullYear() === y && t.getUTCMonth() === mo - 1 && t.getUTCDate() === d && t.getUTCHours() === h && t.getUTCMinutes() === mi;
  return ok ? raw : '';
}

export interface ParsedRequestsView {
  view: RequestsView;
  // Query parameters that were present but unusable and so were ignored.
  ignored: string[];
}

// Anything unrecognised falls back to its default, so a stale or hand-edited
// link opens a usable view instead of an error; `ignored` names what was
// dropped so the page can say so.
export function parseRequestsViewNotes(params: URLSearchParams): ParsedRequestsView {
  const ignored: string[] = [];
  const present = (name: string) => {
    const v = params.get(name);
    return v !== null && v !== '';
  };

  const cloudParam = params.get('cloud');
  const statusParam = params.get('status');
  const rangeParam = params.get('range');
  const sizeParam = params.get('size');
  const from = validWallTime(params.get('from'));
  const until = validWallTime(params.get('until'));
  const before = parseCursorId(params.get('before'));
  const after = parseCursorId(params.get('after'));

  const cloud: CloudFilter = cloudParam !== null && CLOUD_VALUES.includes(cloudParam) ? (cloudParam as CloudFilter) : 'all';
  if (present('cloud') && cloudParam !== 'all' && cloud === 'all') ignored.push('cloud');
  const status: StatusFilter = statusParam !== null && STATUS_VALUES.includes(statusParam) ? (statusParam as StatusFilter) : 'all';
  if (present('status') && statusParam !== 'all' && status === 'all') ignored.push('status');
  const rangeValid = rangeParam !== null && RANGE_VALUES.includes(rangeParam);
  if (present('range') && rangeParam !== 'all' && !rangeValid) ignored.push('range');
  const sizeNum = Number(sizeParam);
  const sizeValid = sizeParam !== null && (PAGE_SIZES as readonly number[]).includes(sizeNum);
  if (present('size') && !sizeValid) ignored.push('size');
  if (present('from') && !from) ignored.push('from');
  if (present('until') && !until) ignored.push('until');
  if (present('before') && before === null) ignored.push('before');
  if (present('after') && after === null) ignored.push('after');

  let cursor: PageCursor | null = null;
  if (before !== null) cursor = { dir: 'before', id: before };
  else if (after !== null) cursor = { dir: 'after', id: after };
  if (before !== null && after !== null) ignored.push('after');

  const view: RequestsView = {
    model: (params.get('model') ?? '').trim(),
    key: (params.get('key') ?? '').trim(),
    node: (params.get('node') ?? '').trim(),
    cloud,
    status,
    // A custom From wins over a preset, exactly as it does in the filter bar.
    range: from ? 'custom' : rangeValid ? (rangeParam as SincePreset) : 'all',
    from,
    until,
    size: sizeValid ? sizeNum : DEFAULT_PAGE_SIZE,
    cursor,
  };
  return { view, ignored };
}

export function parseRequestsView(params: URLSearchParams): RequestsView {
  return parseRequestsViewNotes(params).view;
}

// Identity of the filter set and page size a cursor was issued under. A cursor
// is only meaningful with the filters that produced it, so the page keeps it
// next to this key and discards it as soon as the key changes.
export function filtersKey(v: Omit<RequestsView, 'cursor'>): string {
  return JSON.stringify([v.model, v.key, v.node, v.cloud, v.status, v.range, v.from, v.until, v.size]);
}

// The cursor to use for the key now in effect: the stored one only while the
// filters are still the ones it was issued under.
export function resolveCursor(stored: { key: string; cursor: PageCursor | null }, key: string): PageCursor | null {
  return stored.key === key ? stored.cursor : null;
}

// Newer reached the top of the log when the server returns no newer cursor for
// an after-cursor page; the page then goes live instead of showing a short one.
export function shouldGoLive(cursor: PageCursor | null, page: { prevAfterId?: number }): boolean {
  return cursor !== null && cursor.dir === 'after' && page.prevAfterId === undefined;
}

// Only the newest page (no cursor) refreshes; a paged-back view is fixed so a
// refresh never moves the rows being read.
export function pollIntervalMs(cursor: PageCursor | null): number | null {
  return cursor === null ? LIVE_POLL_MS : null;
}

export function cursorParams(cursor: PageCursor | null): { beforeId?: number; afterId?: number } {
  if (cursor === null) return {};
  return cursor.dir === 'before' ? { beforeId: cursor.id } : { afterId: cursor.id };
}

// Defaults are omitted, so the unfiltered newest page has a clean URL.
export function serializeRequestsView(view: RequestsView): URLSearchParams {
  const p = new URLSearchParams();
  if (view.model) p.set('model', view.model);
  if (view.key) p.set('key', view.key);
  if (view.node) p.set('node', view.node);
  if (view.cloud !== 'all') p.set('cloud', view.cloud);
  if (view.status !== 'all') p.set('status', view.status);
  if (view.from) p.set('from', view.from);
  else if (view.range !== 'all' && view.range !== 'custom') p.set('range', view.range);
  if (view.until) p.set('until', view.until);
  if (view.size !== DEFAULT_PAGE_SIZE) p.set('size', String(view.size));
  if (view.cursor) p.set(view.cursor.dir, String(view.cursor.id));
  return p;
}

export interface FilterChip {
  id: 'model' | 'key' | 'node' | 'cloud' | 'status' | 'range' | 'from' | 'until';
  label: string;
}

const STATUS_CHIP: Record<Exclude<StatusFilter, 'all'>, string> = {
  success: 'Success (2xx)',
  client_error: 'Client error (4xx)',
  server_error: 'Server error (5xx)',
};
const RANGE_CHIP: Record<'15m' | '1h' | '24h', string> = {
  '15m': 'Last 15 min',
  '1h': 'Last hour',
  '24h': 'Last 24h',
};

// The active filters as labelled chips, in filter-bar order. The empty state
// names the same list, so the two can never disagree about what is filtering.
export function activeFilterChips(view: RequestsView): FilterChip[] {
  const chips: FilterChip[] = [];
  if (view.model) chips.push({ id: 'model', label: `Model: ${view.model}` });
  if (view.key) chips.push({ id: 'key', label: `Key: ${view.key}` });
  if (view.node) chips.push({ id: 'node', label: `Node: ${view.node}` });
  if (view.cloud !== 'all') chips.push({ id: 'cloud', label: view.cloud === 'cloud' ? 'Cloud only' : 'Local only' });
  if (view.status !== 'all') chips.push({ id: 'status', label: STATUS_CHIP[view.status] });
  if (view.range !== 'all' && view.range !== 'custom') chips.push({ id: 'range', label: RANGE_CHIP[view.range] });
  if (view.from) chips.push({ id: 'from', label: `From ${view.from.replace('T', ' ')}` });
  if (view.until) chips.push({ id: 'until', label: `Until ${view.until.replace('T', ' ')}` });
  return chips;
}

// Removing a filter returns to the newest page: the cursor belonged to the
// old filter set. Removing From also drops the 'custom' range marker.
export function removeChipFromView(view: RequestsView, id: FilterChip['id']): RequestsView {
  const next: RequestsView = { ...view, cursor: null };
  switch (id) {
    case 'model': next.model = ''; break;
    case 'key': next.key = ''; break;
    case 'node': next.node = ''; break;
    case 'cloud': next.cloud = 'all'; break;
    case 'status': next.status = 'all'; break;
    case 'range': next.range = 'all'; break;
    case 'from': next.from = ''; next.range = 'all'; break;
    case 'until': next.until = ''; break;
  }
  return next;
}

// Clear all keeps the page size, which is a display choice, not a filter.
export function clearedView(view: RequestsView): RequestsView {
  return {
    model: '', key: '', node: '', cloud: 'all', status: 'all', range: 'all', from: '', until: '',
    size: view.size, cursor: null,
  };
}

export type EmptyStateKind = 'pastEnd' | 'filtered' | 'none';

// What an empty page means: a cursor past the data, filters that match
// nothing, or a log with no requests at all.
export function emptyStateKind(cursor: PageCursor | null, hasFilters: boolean): EmptyStateKind {
  if (cursor !== null) return 'pastEnd';
  return hasFilters ? 'filtered' : 'none';
}

// One line for the page header: how many rows are loaded, and whether older
// ones exist. The server returns no match count (a count over millions of
// rows is slow and stale at request rates), so with older rows present the
// honest statement is a lower bound.
export function loadedSummary(rows: number, hasOlder: boolean): string {
  if (rows === 0) return 'No requests loaded';
  const noun = rows === 1 ? 'request' : 'requests';
  return hasOlder ? `At least ${rows} ${noun} loaded, older ones available` : `${rows} ${noun} loaded, no older requests match`;
}
