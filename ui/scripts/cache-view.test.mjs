import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import { cacheCell, hitPctLabel, cacheWindowLabel, cacheWindowState, CACHE_EMPTY_HINT } from '../src/lib/cacheView.ts';

test('cacheCell shows cached / input (pct) when both are reported', () => {
  assert.equal(cacheCell(1820, 2000).text, '1,820 / 2,000 (91%)');
});

test('cacheCell keeps a reported zero as a real 0', () => {
  assert.equal(cacheCell(0, 1450).text, '0 / 1,450 (0%)');
});

test('cacheCell is a dash when either side is missing, never 0', () => {
  assert.equal(cacheCell(undefined, undefined).text, '-');
  assert.equal(cacheCell(5, undefined).text, '-');
  assert.equal(cacheCell(undefined, 5).text, '-');
  assert.equal(cacheCell(null, null).text, '-');
});

test('cacheCell is a dash for an unusable denominator', () => {
  assert.equal(cacheCell(0, 0).text, '-');
});

test('cacheCell explains the unknown state, covering every reason it can be unknown', () => {
  const c = cacheCell(undefined, undefined);
  assert.equal(c.title, 'No cache split recorded for this request (not reported, or not readable)');
  assert.equal(c.known, false);
  assert.equal(cacheCell(1, 2).known, true);
});

test('cacheCell is a dash when cached exceeds input, as the server also treats it', () => {
  assert.equal(cacheCell(150, 100).text, '-');
  assert.equal(cacheCell(100, 100).text, '100 / 100 (100%)');
});

test('cacheCell never rounds a small real share down to zero or a partial share up to 100', () => {
  assert.equal(cacheCell(4, 1000).text, '4 / 1,000 (<1%)');
  assert.equal(cacheCell(0, 1000).text, '0 / 1,000 (0%)');
  assert.equal(cacheCell(999, 1000).text, '999 / 1,000 (>99%)');
  assert.equal(cacheCell(10, 1000).text, '10 / 1,000 (1%)');
});

test('hitPctLabel is a dash for null and keeps one decimal when needed', () => {
  assert.equal(hitPctLabel(null), '-');
  assert.equal(hitPctLabel(undefined), '-');
  assert.equal(hitPctLabel(0), '0%');
  assert.equal(hitPctLabel(72), '72%');
  assert.equal(hitPctLabel(40.5), '40.5%');
});

test('cacheWindowLabel states the window size and the reporting count', () => {
  assert.equal(cacheWindowLabel(1000, 212), 'last 1,000 requests, 212 reported');
  assert.equal(cacheWindowLabel(1, 0), 'last 1 request, 0 reported');
});

const cw = (over = {}) => ({
  window_size: 1000, window_requests: 10, reporting_requests: 4, cached_tokens: 5, input_tokens: 9,
  window_oldest: null, by_model: [], by_key: [], ...over,
});
const analytics = (over = {}) => ({ local_requests: 0, cloud_requests: 0, hourly: [], by_model: [], ...over });

test('cacheWindowState tells a failed read, an old server and an empty window apart', () => {
  assert.equal(cacheWindowState(null, true, false), 'loading');
  assert.equal(cacheWindowState(null, false, true), 'unavailable');
  assert.equal(cacheWindowState(analytics({ cache_window: null, cache_window_error: 'cache window unavailable' }), false, false), 'unavailable');
  assert.equal(cacheWindowState(analytics({ cache_window: null }), false, false), 'unavailable');
  assert.equal(cacheWindowState(analytics(), false, false), 'unsupported');
  assert.equal(cacheWindowState(analytics({ cache_window: cw({ window_requests: 0, reporting_requests: 0 }) }), false, false), 'empty');
  assert.equal(cacheWindowState(analytics({ cache_window: cw({ reporting_requests: 0 }) }), false, false), 'unreported');
  assert.equal(cacheWindowState(analytics({ cache_window: cw() }), false, false), 'ready');
});

test('the empty hint names only what is verified and is not a capability promise', () => {
  assert.match(CACHE_EMPTY_HINT, /--enable-prompt-tokens-details/);
  assert.match(CACHE_EMPTY_HINT, /non-streaming/);
  assert.match(CACHE_EMPTY_HINT, /Ollama and MLX do not report/);
});

test('the card renders a distinct message for each non-ready state', () => {
  const src = fs.readFileSync(new URL('../src/components/CacheWindowCard.tsx', import.meta.url), 'utf8');
  assert.match(src, /state === 'unavailable'[^\n]*Cache data unavailable/);
  assert.match(src, /state === 'unsupported'[^\n]*not supported by this server/);
  assert.match(src, /state === 'empty'[^\n]*No requests on record yet\./);
  assert.match(src, /state === 'unreported'[^\n]*CACHE_EMPTY_HINT/);
  const page = fs.readFileSync(new URL('../src/pages/Analytics.tsx', import.meta.url), 'utf8');
  assert.match(page, /<CacheWindowCard[^>]*loadError=\{!!error\}/);
});

test('the Requests table has exactly one body cell per column', () => {
  const cols = fs.readFileSync(new URL('../src/components/RequestLogParts.tsx', import.meta.url), 'utf8');
  const colsStart = cols.indexOf('= [', cols.indexOf('export const COLUMNS'));
  const colsBlock = cols.slice(colsStart, cols.indexOf('];', colsStart));
  const columnCount = (colsBlock.match(/\{ label:/g) || []).length;
  const rows = fs.readFileSync(new URL('../src/components/RequestRows.tsx', import.meta.url), 'utf8');
  const rowFn = rows.slice(rows.indexOf('export const RequestRow ='), rows.indexOf('export const RequestCard ='));
  const firstTr = rowFn.slice(rowFn.indexOf('<tr'), rowFn.indexOf('</tr>'));
  const cellCount = (firstTr.match(/<td\b/g) || []).length;
  assert.equal(cellCount, columnCount);
  const skeleton = cols.slice(cols.indexOf('export function SkeletonRow'));
  assert.match(skeleton, /COLUMNS\.map/);
});

test('the demo cache window adds up: model, key and window totals agree', () => {
  const src = fs.readFileSync(new URL('../src/lib/mockData.ts', import.meta.url), 'utf8');
  const block = src.slice(src.indexOf('cache_window: {'));
  const grab = (section, field) => {
    const start = block.indexOf(section);
    const end = block.indexOf('],', start);
    return [...block.slice(start, end).matchAll(new RegExp(field + ': (\\d+)', 'g'))].map((m) => Number(m[1]));
  };
  const sum = (a) => a.reduce((x, y) => x + y, 0);
  const top = (field) => Number(block.match(new RegExp('\\n    ' + field + ': (\\d+)'))[1]);
  for (const f of ['window_requests', 'reporting_requests', 'cached_tokens', 'input_tokens']) {
    assert.equal(sum(grab('by_model: [', f)), sum(grab('by_key: [', f)), f + ' model vs key');
    assert.equal(sum(grab('by_model: [', f)), top(f), f + ' vs window total');
  }
});
