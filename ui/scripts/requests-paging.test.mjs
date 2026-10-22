// Run with: npm test. The Requests page keeps its whole view (filters, quick
// range, page size, page cursor) in the URL so a view can be shared or
// bookmarked. These tests pin the parse/serialize rules, including that a
// stale or hand-edited link degrades to the default view instead of breaking.
import test from 'node:test';
import assert from 'node:assert/strict';
import path from 'node:path';
import { pathToFileURL, fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const mod = await import(pathToFileURL(path.join(here, '..', 'src/lib/requestsView.ts')).href);
const { parseRequestsView, serializeRequestsView, activeFilterChips, loadedSummary, PAGE_SIZES, DEFAULT_PAGE_SIZE } = mod;

const parse = (qs) => parseRequestsView(new URLSearchParams(qs));

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
});

test('a full view survives a serialize and parse round trip', () => {
  const view = {
    model: 'llama3', key: 'Data Platform', node: 'gpu-node-02', cloud: 'local', status: 'server_error',
    range: 'custom', from: '2026-10-22T09:30', until: '2026-10-22T10:45', size: 250,
    cursor: { dir: 'before', id: 7300 },
  };
  const qs = serializeRequestsView(view).toString();
  assert.deepEqual(parse(qs), view);
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

test('unrecognised values fall back to defaults', () => {
  const v = parse('cloud=maybe&status=teapot&range=7d&size=77&from=yesterday&until=2026-10-22');
  assert.equal(v.cloud, 'all');
  assert.equal(v.status, 'all');
  assert.equal(v.range, 'all');
  assert.equal(v.size, DEFAULT_PAGE_SIZE);
  assert.equal(v.from, '');
  assert.equal(v.until, '');
});

test('cursors must be positive integers and only one is kept', () => {
  assert.deepEqual(parse('before=7300').cursor, { dir: 'before', id: 7300 });
  assert.deepEqual(parse('after=12').cursor, { dir: 'after', id: 12 });
  for (const bad of ['before=0', 'before=-5', 'before=1.5', 'before=abc', 'before=', 'after=1e3', 'before=99999999999999999999']) {
    assert.equal(parse(bad).cursor, null, bad);
  }
  // Both present: before wins, so the API's mutual exclusion is never violated.
  const both = parse('before=10&after=20');
  assert.deepEqual(both.cursor, { dir: 'before', id: 10 });
  assert.equal(serializeRequestsView(both).get('after'), null);
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
  assert.equal(loadedSummary(37, false), '37 requests loaded, end of log');
  assert.equal(loadedSummary(1, false), '1 request loaded, end of log');
  assert.equal(loadedSummary(0, false), 'No requests loaded');
});
