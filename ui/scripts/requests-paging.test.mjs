// Run with: npm test. The Requests page keeps its whole view (filters, quick
// range, page size, page cursor) in the URL so a view can be shared or
// bookmarked. These tests pin the parse/serialize rules, including that a
// stale or hand-edited link degrades to the default view instead of breaking,
// and the pure paging rules the page builds on.
import test from 'node:test';
import assert from 'node:assert/strict';
import path from 'node:path';
import { pathToFileURL, fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const mod = await import(pathToFileURL(path.join(here, '..', 'src/lib/requestsView.ts')).href);
const {
  parseRequestsView, parseRequestsViewNotes, serializeRequestsView, activeFilterChips, loadedSummary,
  filtersKey, resolveCursor, shouldGoLive, pollIntervalMs, cursorParams, removeChipFromView, clearedView,
  emptyStateKind, classifyUrlChange, PAGE_SIZES, DEFAULT_PAGE_SIZE, LIVE_POLL_MS,
} = mod;

const parse = (qs) => parseRequestsView(new URLSearchParams(qs));
const notes = (qs) => parseRequestsViewNotes(new URLSearchParams(qs));

test('an empty query is the default view: newest page, 100 rows, no filters', () => {
  const v = parse('');
  assert.equal(v.size, DEFAULT_PAGE_SIZE);
  assert.equal(DEFAULT_PAGE_SIZE, 100);
  assert.deepEqual([...PAGE_SIZES], [50, 100, 250, 500]);
  assert.equal(v.cursor, null);
  assert.equal(v.cloud, 'all');
  assert.equal(v.status, 'all');
  assert.equal(v.range, 'all');
  assert.equal(serializeRequestsView(v).toString(), '');
  assert.deepEqual(activeFilterChips(v), []);
  assert.deepEqual(notes('').ignored, []);
});

test('a full view survives a serialize and parse round trip', () => {
  const view = {
    model: 'llama3', key: 'Data Platform', node: 'gpu-node-02', cloud: 'local', status: 'server_error',
    range: 'custom', from: '2026-10-22T09:30', until: '2026-10-22T10:45', size: 250,
    cursor: { dir: 'before', id: 7300 },
  };
  const qs = serializeRequestsView(view).toString();
  assert.deepEqual(parse(qs), view);
  // The newer-direction cursor round trips too.
  const newer = { ...view, cursor: { dir: 'after', id: 12 } };
  assert.deepEqual(parse(serializeRequestsView(newer).toString()), newer);
});

test('serialized parameter order is fixed so equal views give equal URLs', () => {
  const a = parse('size=250&status=client_error&model=x&before=9');
  const b = parse('before=9&model=x&size=250&status=client_error');
  assert.equal(serializeRequestsView(a).toString(), serializeRequestsView(b).toString());
  assert.equal(serializeRequestsView(a).toString(), 'model=x&status=client_error&size=250&before=9');
});

test('a preset range round trips and a custom From takes precedence over it', () => {
  const preset = parse('range=1h');
  assert.equal(preset.range, '1h');
  assert.equal(serializeRequestsView(preset).get('range'), '1h');

  const both = parse('range=1h&from=2026-10-22T09:30');
  assert.equal(both.range, 'custom');
  const out = serializeRequestsView(both);
  assert.equal(out.get('from'), '2026-10-22T09:30');
  assert.equal(out.get('range'), null);
});

test('unrecognised values fall back to defaults and are reported', () => {
  const { view: v, ignored } = notes('cloud=maybe&status=teapot&range=7d&size=77&from=yesterday&until=2026-10-22');
  assert.equal(v.cloud, 'all');
  assert.equal(v.status, 'all');
  assert.equal(v.range, 'all');
  assert.equal(v.size, DEFAULT_PAGE_SIZE);
  assert.equal(v.from, '');
  assert.equal(v.until, '');
  assert.deepEqual(ignored.sort(), ['cloud', 'from', 'range', 'size', 'status', 'until']);
});

test('explicit default values are accepted, not reported as ignored', () => {
  assert.deepEqual(notes('cloud=all&status=all&range=all&size=100').ignored, []);
});

test('wall times must be real calendar times', () => {
  for (const bad of ['2026-02-31T10:00', '2026-13-01T10:00', '2026-10-22T25:00', '2026-10-22T10:61', '2026-10-22 10:00']) {
    assert.equal(parse(`from=${encodeURIComponent(bad)}`).from, '', bad);
  }
  assert.equal(parse('from=2028-02-29T23:59').from, '2028-02-29T23:59');
  assert.equal(parse('from=2027-02-29T00:00').from, '');
});

test('cursors must be positive safe integers and only one is kept', () => {
  assert.deepEqual(parse('before=7300').cursor, { dir: 'before', id: 7300 });
  assert.deepEqual(parse('after=12').cursor, { dir: 'after', id: 12 });
  for (const bad of ['before=0', 'before=-5', 'before=1.5', 'before=abc', 'before=', 'after=1e3', 'before=99999999999999999999']) {
    assert.equal(parse(bad).cursor, null, bad);
  }
  // Both present: before wins, so the API's mutual exclusion is never violated.
  const both = notes('before=10&after=20');
  assert.deepEqual(both.view.cursor, { dir: 'before', id: 10 });
  assert.deepEqual(both.ignored, ['after']);
  assert.equal(serializeRequestsView(both.view).get('after'), null);
});

test('text filters are trimmed and the page size is one of the offered sizes', () => {
  assert.equal(parse('model=%20llama3%20').model, 'llama3');
  for (const n of PAGE_SIZES) assert.equal(parse(`size=${n}`).size, n);
  assert.equal(parse('size=1000').size, DEFAULT_PAGE_SIZE);
  assert.equal(serializeRequestsView(parse('size=100')).get('size'), null);
});

test('filter chips name every active filter in filter-bar order', () => {
  const v = parse('model=llama3&key=CI&node=gpu-node-01&cloud=cloud&status=client_error&range=15m&until=2026-10-22T10:45');
  assert.deepEqual(
    activeFilterChips(v).map((c) => [c.id, c.label]),
    [
      ['model', 'Model: llama3'],
      ['key', 'Key: CI'],
      ['node', 'Node: gpu-node-01'],
      ['cloud', 'Cloud only'],
      ['status', 'Client error (4xx)'],
      ['range', 'Last 15 min'],
      ['until', 'Until 2026-10-22 10:45'],
    ],
  );
  const custom = parse('from=2026-10-22T09:30');
  assert.deepEqual(activeFilterChips(custom).map((c) => c.id), ['from']);
});

test('the loaded line is a lower bound while older rows exist', () => {
  assert.equal(loadedSummary(100, true), 'At least 100 requests loaded, older ones available');
  assert.equal(loadedSummary(37, false), '37 requests loaded, no older requests match');
  assert.equal(loadedSummary(1, false), '1 request loaded, no older requests match');
  assert.equal(loadedSummary(0, false), 'No requests loaded');
});

test('the filter key changes with every filter and the page size, and with nothing else', () => {
  const base = parse('');
  const key = filtersKey(base);
  const variants = [
    { model: 'x' }, { key: 'x' }, { node: 'x' }, { cloud: 'local' }, { status: 'success' },
    { range: '1h' }, { from: '2026-10-22T09:30' }, { until: '2026-10-22T09:30' }, { size: 50 },
  ];
  for (const change of variants) assert.notEqual(filtersKey({ ...base, ...change }), key, JSON.stringify(change));
  assert.equal(filtersKey({ ...base, cursor: { dir: 'before', id: 5 } }), key);
});

test('a stored cursor is used only under the filters it was issued for', () => {
  const stored = { key: 'k1', cursor: { dir: 'before', id: 9 } };
  assert.deepEqual(resolveCursor(stored, 'k1'), { dir: 'before', id: 9 });
  assert.equal(resolveCursor(stored, 'k2'), null);
});

test('Newer goes live only when it reaches the top of the log', () => {
  const after = { dir: 'after', id: 5 };
  assert.equal(shouldGoLive(after, {}), true);
  assert.equal(shouldGoLive(after, { prevAfterId: 9 }), false);
  assert.equal(shouldGoLive({ dir: 'before', id: 5 }, {}), false);
  assert.equal(shouldGoLive(null, {}), false);
});

test('only the newest page refreshes', () => {
  assert.equal(pollIntervalMs(null), LIVE_POLL_MS);
  assert.equal(pollIntervalMs({ dir: 'before', id: 1 }), null);
  assert.equal(pollIntervalMs({ dir: 'after', id: 1 }), null);
});

test('cursor request parameters are mutually exclusive', () => {
  assert.deepEqual(cursorParams(null), {});
  assert.deepEqual(cursorParams({ dir: 'before', id: 4 }), { beforeId: 4 });
  assert.deepEqual(cursorParams({ dir: 'after', id: 4 }), { afterId: 4 });
});

test('removing a chip returns to the newest page and Clear all keeps the page size', () => {
  const v = parse('model=x&status=server_error&from=2026-10-22T09:30&size=250&before=9');
  const noModel = removeChipFromView(v, 'model');
  assert.equal(noModel.model, '');
  assert.equal(noModel.cursor, null);
  assert.equal(noModel.status, 'server_error');
  const noFrom = removeChipFromView(v, 'from');
  assert.equal(noFrom.from, '');
  assert.equal(noFrom.range, 'all');
  const cleared = clearedView(v);
  assert.deepEqual(activeFilterChips(cleared), []);
  assert.equal(cleared.size, 250);
  assert.equal(cleared.cursor, null);
});

test('a bare link after earlier writes is adopted, not mistaken for a late echo', () => {
  const written = new Set();
  // The page wrote "", then "model=x"; the router echoed the latest one.
  written.add('');
  written.add('model=x');
  assert.equal(classifyUrlChange('model=x', 'model=x', written), 'same');
  assert.equal(written.size, 0);
  // The sidebar link to bare /requests is now an external change.
  assert.equal(classifyUrlChange('', 'model=x', written), 'external');
  // A genuine late echo of a write still in flight is ignored once.
  written.add('status=success');
  assert.equal(classifyUrlChange('status=success', 'size=50', written), 'echo');
  assert.equal(classifyUrlChange('status=success', 'size=50', written), 'external');
});

test('an empty page is named for what caused it', () => {
  assert.equal(emptyStateKind({ dir: 'before', id: 1 }, true), 'pastEnd');
  assert.equal(emptyStateKind(null, true), 'filtered');
  assert.equal(emptyStateKind(null, false), 'none');
});
