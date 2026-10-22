// The Requests page's shareable view state: filters, quick range, page size and
// the page cursor, read from and written to the URL query string. Kept free of
// imports so the parse/serialize rules can be tested directly with node.

export const PAGE_SIZES = [50, 100, 250, 500] as const;
export const DEFAULT_PAGE_SIZE = 100;

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
const WALL_TIME = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/;

function parseCursorId(raw: string | null): number | null {
  if (raw === null || !/^\d+$/.test(raw)) return null;
  const n = Number(raw);
  return Number.isSafeInteger(n) && n > 0 ? n : null;
}

function wallTime(raw: string | null): string {
  return raw !== null && WALL_TIME.test(raw) ? raw : '';
}

// Anything unrecognised falls back to its default, so a stale or hand-edited
// link opens the unfiltered newest page instead of an error.
export function parseRequestsView(params: URLSearchParams): RequestsView {
  const cloud = params.get('cloud');
  const status = params.get('status');
  const rangeParam = params.get('range');
  const size = Number(params.get('size'));
  const from = wallTime(params.get('from'));
  const before = parseCursorId(params.get('before'));
  const after = parseCursorId(params.get('after'));

  let cursor: PageCursor | null = null;
  if (before !== null) cursor = { dir: 'before', id: before };
  else if (after !== null) cursor = { dir: 'after', id: after };

  return {
    model: (params.get('model') ?? '').trim(),
    key: (params.get('key') ?? '').trim(),
    node: (params.get('node') ?? '').trim(),
    cloud: cloud !== null && CLOUD_VALUES.includes(cloud) ? (cloud as CloudFilter) : 'all',
    status: status !== null && STATUS_VALUES.includes(status) ? (status as StatusFilter) : 'all',
    // A custom From wins over a preset, exactly as it does in the filter bar.
    range: from
      ? 'custom'
      : rangeParam !== null && RANGE_VALUES.includes(rangeParam)
        ? (rangeParam as SincePreset)
        : 'all',
    from,
    until: wallTime(params.get('until')),
    size: (PAGE_SIZES as readonly number[]).includes(size) ? size : DEFAULT_PAGE_SIZE,
    cursor,
  };
}

// Identity of the filter set and page size a cursor was issued under. A cursor
// is only meaningful with the filters that produced it, so the page keeps it
// next to this key and ignores it as soon as the key changes.
export function filtersKey(v: Omit<RequestsView, 'cursor'>): string {
  return JSON.stringify([v.model, v.key, v.node, v.cloud, v.status, v.range, v.from, v.until, v.size]);
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

// One line for the page header: how many rows are loaded, and whether older
// ones exist. The server returns no match count (a count over millions of
// rows is slow and stale at request rates), so with older rows present the
// honest statement is a lower bound.
export function loadedSummary(rows: number, hasOlder: boolean): string {
  if (rows === 0) return 'No requests loaded';
  const noun = rows === 1 ? 'request' : 'requests';
  return hasOlder ? `At least ${rows} ${noun} loaded, older ones available` : `${rows} ${noun} loaded, end of log`;
}
