// Run with: npm test. Pins the rules the Analytics page uses when a poll fails:
// keep the last good data, say how old it is, and ignore out-of-order responses.
import test from 'node:test';
import assert from 'node:assert/strict';
import path from 'node:path';
import { pathToFileURL, fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const mod = await import(pathToFileURL(path.join(here, '..', 'src/lib/pollNotice.ts')).href);
const { errorReason, pollNotice, createPollSequence, MAX_REASON_LENGTH } = mod;

const fmt = (iso) => `at ${iso}`;

test('no error means no notice, with or without data', () => {
  assert.equal(pollNotice({ hasData: false, lastOkAt: null, error: null }, 'figures', fmt).kind, 'none');
  assert.equal(pollNotice({ hasData: true, lastOkAt: 'T1', error: null }, 'figures', fmt).kind, 'none');
});

test('a failed first load reports only the reason', () => {
  const n = pollNotice({ hasData: false, lastOkAt: null, error: 'Failed to fetch analytics (HTTP 500)' }, 'figures', fmt);
  assert.equal(n.kind, 'failed');
  assert.equal(n.message, 'Failed to fetch analytics (HTTP 500).');
});

test('a failed refresh keeps the data, states the age and says it may be out of date', () => {
  const n = pollNotice({ hasData: true, lastOkAt: 'T1', error: 'Failed to fetch' }, 'rows', fmt);
  assert.equal(n.kind, 'stale');
  assert.equal(n.message, 'Failed to fetch. Showing rows last updated at T1; they may be out of date.');
});

test('data without a last-good time is treated as a failed load, not as stale', () => {
  assert.equal(pollNotice({ hasData: true, lastOkAt: null, error: 'x' }, 'rows', fmt).kind, 'failed');
});

test('errorReason trims, collapses whitespace, drops trailing full stops and caps length', () => {
  assert.equal(errorReason(new Error('  boom\n  now.. '), 'fb'), 'boom now');
  assert.equal(errorReason(new Error(''), 'fallback text'), 'fallback text');
  assert.equal(errorReason('not an error', 'fallback text'), 'fallback text');
  const long = errorReason(new Error('x'.repeat(500)), 'fb');
  assert.equal(long.length, MAX_REASON_LENGTH);
  assert.ok(long.endsWith('…'));
});

test('a slow earlier response cannot overwrite a newer one', () => {
  const seq = createPollSequence();
  const a = seq.next();
  const b = seq.next();
  assert.equal(seq.accept(b), true); // newer lands first
  assert.equal(seq.accept(a), false); // older lands late: ignored
});

test('responses arriving in order are all applied, and a response is applied once', () => {
  const seq = createPollSequence();
  const a = seq.next();
  assert.equal(seq.accept(a), true);
  const b = seq.next();
  assert.equal(seq.accept(b), true);
  assert.equal(seq.accept(b), false);
});

// The page feeds pollNotice from its state, so these walk the same sequence of
// states the Analytics page goes through.
test('first load fails, then the next poll succeeds: the notice appears, then clears', () => {
  const failed = pollNotice({ hasData: false, lastOkAt: null, error: 'Failed to fetch analytics (HTTP 503)' }, 'figures', fmt);
  assert.equal(failed.kind, 'failed');
  assert.equal(pollNotice({ hasData: true, lastOkAt: 'T2', error: null }, 'figures', fmt).kind, 'none');
});

test('good data, a failed refresh, then recovery: stale notice then none', () => {
  const stale = pollNotice({ hasData: true, lastOkAt: 'T1', error: 'Failed to fetch' }, 'figures', fmt);
  assert.equal(stale.kind, 'stale');
  assert.equal(pollNotice({ hasData: true, lastOkAt: 'T3', error: null }, 'figures', fmt).kind, 'none');
});

test('an empty table with an error reports the reason only, never "Showing rows"', () => {
  // The page passes hasData = rows.length > 0 && a last-good time.
  const n = pollNotice({ hasData: false, lastOkAt: 'T1', error: 'Failed to fetch spill counters (HTTP 500)' }, 'rows', fmt);
  assert.equal(n.kind, 'failed');
  assert.ok(!n.message.includes('Showing'));
});

test('an old failure cannot replace newer good data', () => {
  const seq = createPollSequence();
  const slowFail = seq.next();
  const fastOk = seq.next();
  assert.equal(seq.accept(fastOk), true);
  assert.equal(seq.accept(slowFail), false);
});
