// Run with: npm test. Pure-logic tests for the tokens per second series helper.
import test from 'node:test';
import assert from 'node:assert/strict';
import { toTpsSeries } from '../src/lib/tokensPerSec.ts';

test('zero generation duration is unmeasured (null), never 0', () => {
  const out = toTpsSeries([{ hour: 'h1', tokens_per_sec: 0, gen_duration_ms: 0 }]);
  assert.equal(out[0].tps, null);
});

test('a stale non-zero rate with zero duration is still unmeasured', () => {
  const out = toTpsSeries([{ hour: 'h1', tokens_per_sec: 50, gen_duration_ms: 0 }]);
  assert.equal(out[0].tps, null);
});

test('an hour with no fields is unmeasured', () => {
  assert.equal(toTpsSeries([{ hour: 'h1' }])[0].tps, null);
});

test('a rate with no duration field is unmeasured', () => {
  assert.equal(toTpsSeries([{ hour: 'h2', tokens_per_sec: 40 }])[0].tps, null);
});

test('positive duration with the rate absent is unmeasured', () => {
  assert.equal(toTpsSeries([{ hour: 'h3', gen_duration_ms: 900 }])[0].tps, null);
});

test('NaN rate with positive duration is unmeasured', () => {
  assert.equal(toTpsSeries([{ hour: 'h', tokens_per_sec: NaN, gen_duration_ms: 900 }])[0].tps, null);
});

test('Infinity rate is unmeasured', () => {
  assert.equal(toTpsSeries([{ hour: 'h', tokens_per_sec: Infinity, gen_duration_ms: 900 }])[0].tps, null);
});

test('negative duration is unmeasured', () => {
  assert.equal(toTpsSeries([{ hour: 'h', tokens_per_sec: 40, gen_duration_ms: -5 }])[0].tps, null);
});

test('NaN duration is unmeasured', () => {
  assert.equal(toTpsSeries([{ hour: 'h', tokens_per_sec: 40, gen_duration_ms: NaN }])[0].tps, null);
});

test('an empty array yields an empty series', () => {
  assert.deepEqual(toTpsSeries([]), []);
});

test('a measured zero rate with positive duration is a real value, not a gap', () => {
  assert.equal(toTpsSeries([{ hour: 'h', tokens_per_sec: 0, gen_duration_ms: 900 }])[0].tps, 0);
});

test('a measured hour passes its value through', () => {
  const out = toTpsSeries([{ hour: 'h1', tokens_per_sec: 52.5, gen_duration_ms: 12000 }]);
  assert.deepEqual(out, [{ hour: 'h1', tps: 52.5 }]);
});

test('order and length are preserved', () => {
  const out = toTpsSeries([
    { hour: 'a', tokens_per_sec: 41, gen_duration_ms: 10 },
    { hour: 'b', tokens_per_sec: 0, gen_duration_ms: 0 },
    { hour: 'c', tokens_per_sec: 60, gen_duration_ms: 5 },
  ]);
  assert.deepEqual(out.map(p => [p.hour, p.tps]), [['a', 41], ['b', null], ['c', 60]]);
});
