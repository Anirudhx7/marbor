// Run with: npm test. Pure helpers behind the small UI consistency fixes:
// unlimited rate-limit label, decision hour label, in-flight request sharing.
import test from 'node:test';
import assert from 'node:assert/strict';
import { formatRateLimit } from '../src/lib/roundPrice.ts';
import { decisionHourLabel } from '../src/lib/time.ts';
import { dedupeInflight } from '../src/lib/inflight.ts';

test('formatRateLimit shows Unlimited for no limit', () => {
  assert.equal(formatRateLimit(0), 'Unlimited');
  assert.equal(formatRateLimit(undefined), 'Unlimited');
  assert.equal(formatRateLimit(null), 'Unlimited');
  assert.equal(formatRateLimit(1000), '1,000/hr');
});

test('decisionHourLabel names the zone of the hour', () => {
  assert.equal(decisionHourLabel(17, 'UTC'), 'hour 17 (UTC)');
  assert.equal(decisionHourLabel(9, 'Asia/Kolkata'), 'hour 9 (Asia/Kolkata)');
  assert.equal(decisionHourLabel(3, 'Local'), 'hour 3 (server time)');
  assert.equal(decisionHourLabel(3, ''), 'hour 3 (server time)');
});

const deferred = () => {
  let resolve, reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
};

test('dedupeInflight shares one in-flight call', async () => {
  let calls = 0;
  const d = deferred();
  const { call } = dedupeInflight(() => { calls++; return d.promise; });
  const ps = [call(), call(), call(), call(), call()];
  assert.equal(calls, 1);
  d.resolve({ a: 1 });
  const out = await Promise.all(ps);
  assert.equal(out.length, 5);
  for (const o of out) assert.deepEqual(o, { a: 1 });
});

test('dedupeInflight starts a new call after settle', async () => {
  let calls = 0;
  const { call } = dedupeInflight(async () => ({ n: ++calls }));
  assert.deepEqual(await call(), { n: 1 });
  assert.deepEqual(await call(), { n: 2 });
});

test('dedupeInflight reset mid-flight forces a new call', async () => {
  let calls = 0;
  const ds = [deferred(), deferred()];
  const { call, reset } = dedupeInflight(() => ds[calls++].promise);
  const a = call();
  reset();
  const b = call();
  assert.equal(calls, 2);
  ds[0].resolve({ v: 'a' });
  ds[1].resolve({ v: 'b' });
  assert.deepEqual(await a, { v: 'a' });
  assert.deepEqual(await b, { v: 'b' });
});

test('dedupeInflight identity guard keeps the newer slot', async () => {
  let calls = 0;
  const ds = [deferred(), deferred()];
  const { call, reset } = dedupeInflight(() => ds[calls++].promise);
  const a = call();
  reset();
  const b = call();
  ds[0].resolve({ v: 'a' });
  await a;
  const c = call();
  assert.equal(calls, 2, 'old settle must not clear the newer slot');
  ds[1].resolve({ v: 'b' });
  assert.deepEqual(await b, { v: 'b' });
  assert.deepEqual(await c, { v: 'b' });
});

test('dedupeInflight hands each caller an independent clone', async () => {
  const { call } = dedupeInflight(async () => ({ list: [1] }));
  const [x, y] = await Promise.all([call(), call()]);
  x.list.push(2);
  assert.deepEqual(y.list, [1]);
});

test('dedupeInflight rejection clears the slot and rejects sharers', async () => {
  let calls = 0;
  const d = deferred();
  const { call } = dedupeInflight(() => (++calls === 1 ? d.promise : Promise.resolve({ ok: true })));
  const a = call();
  const b = call();
  d.reject(new Error('boom'));
  await assert.rejects(a, /boom/);
  await assert.rejects(b, /boom/);
  assert.deepEqual(await call(), { ok: true });
});
