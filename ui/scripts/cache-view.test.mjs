import test from 'node:test';
import assert from 'node:assert/strict';
import { cacheCell, hitPctLabel, cacheWindowLabel } from '../src/lib/cacheView.ts';

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

test('cacheCell explains the unknown state in its tooltip', () => {
  assert.match(cacheCell(undefined, undefined).title, /not reported/i);
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
