// Run with: npm test. The Activity page refreshes itself every 30 seconds;
// the refresh must never drop events the operator loaded with Load more.
import test from 'node:test';
import assert from 'node:assert/strict';
import {
  applyRefresh,
  applyOlderPage,
  isStaleEpoch,
  startFullLoad,
  rowKeys,
  entryKey,
  EMPTY_FEED,
} from '../src/lib/activityRefresh.ts';

const LIMIT = 100;
const BASE = Date.parse('2026-10-24T12:00:00Z');
const row = (id) => ({
  id,
  time: new Date(BASE + id * 1000).toISOString(),
  username: 'admin',
  action: 'patch_node',
  target: `node-${id}`,
  details: '',
  source_ip: '10.0.0.1',
});
// newest first: from..to inclusive, descending
const range = (from, to) => Array.from({ length: from - to + 1 }, (_, i) => row(from - i));
const ids = (l) => l.map((e) => e.id);
const feed = (entries, hasMore = true) => ({ entries, hasMore });
const noId = (n) => ({ time: `t${n}`, username: 'u', action: 'a', target: 't', details: '', source_ip: '' });

test('refresh keeps older loaded pages and adds new head rows', () => {
  const out = applyRefresh(feed(range(300, 101)), range(305, 206), LIMIT);
  assert.equal(out.restarted, false);
  assert.deepEqual(ids(out.feed.entries), ids(range(305, 101)));
});

test('refresh with no new events leaves the loaded count unchanged', () => {
  const current = range(300, 101);
  const out = applyRefresh(feed(current), range(300, 201), LIMIT);
  assert.deepEqual(ids(out.feed.entries), ids(current));
});

test('an unchanged head returns the same feed object', () => {
  const current = feed(range(300, 101));
  const out = applyRefresh(current, range(300, 201), LIMIT);
  assert.equal(out.feed, current);
  assert.equal(out.restarted, false);
  assert.notEqual(applyRefresh(current, range(301, 202), LIMIT).feed, current);
});

test('hasMore is kept on an overlapping refresh, true or false', () => {
  assert.equal(applyRefresh(feed(range(300, 101), true), range(301, 202), LIMIT).feed.hasMore, true);
  assert.equal(applyRefresh(feed(range(300, 101), false), range(301, 202), LIMIT).feed.hasMore, false);
});

test('a row removed inside the overlap is dropped, the older tail stays', () => {
  const fresh = range(300, 201).filter((e) => e.id !== 250);
  const out = applyRefresh(feed(range(300, 101)), fresh, LIMIT);
  assert.equal(out.feed.entries.some((e) => e.id === 250), false);
  assert.equal(out.feed.entries.length, 199);
  assert.equal(out.feed.entries.at(-1).id, 101);
});

test('a loaded row newer than the anchor but absent from the fresh page is dropped', () => {
  const fresh = range(305, 206).filter((e) => e.id !== 299);
  const out = applyRefresh(feed(range(300, 101)), fresh, LIMIT);
  assert.equal(out.feed.entries.some((e) => e.id === 299), false);
});

test('overlap on only the oldest fresh row still joins without a gap', () => {
  const out = applyRefresh(feed(range(300, 101)), range(399, 300), LIMIT);
  assert.equal(out.restarted, false);
  assert.deepEqual(ids(out.feed.entries), ids(range(399, 101)));
});

test('no overlap restarts from the fresh page and recomputes hasMore', () => {
  const full = applyRefresh(feed(range(300, 101), false), range(900, 801), LIMIT);
  assert.equal(full.restarted, true);
  assert.deepEqual(ids(full.feed.entries), ids(range(900, 801)));
  assert.equal(full.feed.hasMore, true);
  const short = applyRefresh(feed(range(300, 101), true), range(900, 891), LIMIT);
  assert.equal(short.restarted, true);
  assert.equal(short.feed.hasMore, false);
});

test('an empty fresh page never wipes loaded rows', () => {
  const current = feed(range(300, 101), true);
  const out = applyRefresh(current, [], LIMIT);
  assert.equal(out.restarted, false);
  assert.equal(out.feed, current);
});

test('an empty list taking its first page is not a restart', () => {
  const out = applyRefresh(feed([], false), range(100, 1), LIMIT);
  assert.equal(out.restarted, false);
  assert.equal(out.feed.entries.length, 100);
  assert.equal(out.feed.hasMore, true);
  assert.deepEqual(applyRefresh(feed([], false), [], LIMIT), { feed: { entries: [], hasMore: false }, restarted: false });
});

test('rows without ids match by fields, and identical id-less rows count as one event', () => {
  const out = applyRefresh(feed([noId(5), noId(4), noId(3), noId(2)]), [noId(6), noId(5), noId(4)], LIMIT);
  assert.deepEqual(out.feed.entries.map(entryKey), [6, 5, 4, 3, 2].map((n) => entryKey(noId(n))));
  const dup = applyRefresh(feed([noId(1), noId(1), noId(0)]), [noId(2), noId(1)], LIMIT);
  assert.equal(dup.restarted, false);
  assert.deepEqual(dup.feed.entries.map(entryKey), [2, 1, 1, 0].map((n) => entryKey(noId(n))));
});

test('applyOlderPage skips loaded rows and sets hasMore from the page size', () => {
  const full = applyOlderPage(feed(range(300, 201), false), range(200, 101), LIMIT);
  assert.deepEqual(ids(full.entries), ids(range(300, 101)));
  assert.equal(full.hasMore, true);
  const overlap = applyOlderPage(feed(range(300, 201)), range(210, 150), LIMIT);
  assert.deepEqual(ids(overlap.entries), ids(range(300, 150)));
  assert.equal(overlap.hasMore, false);
});

test('a refresh that restarts the list invalidates a Load more in flight', () => {
  let epoch = 7;
  const captured = epoch;
  assert.equal(isStaleEpoch(captured, epoch), false);
  const out = applyRefresh(feed(range(300, 201)), range(900, 801), LIMIT);
  if (out.restarted) epoch += 1;
  assert.equal(isStaleEpoch(captured, epoch), true);
});

test('a full load (filter change) invalidates a Load more or older load in flight', () => {
  const loadMoreEpoch = 5;
  const first = startFullLoad(5, 0);
  assert.equal(isStaleEpoch(loadMoreEpoch, first.epoch), true);
  assert.equal(isStaleEpoch(first.fullSeq, first.fullSeq), false);
  // A slow earlier full load is stale once a newer one starts.
  const newer = startFullLoad(first.epoch, first.fullSeq);
  assert.equal(isStaleEpoch(first.fullSeq, newer.fullSeq), true);
});

test('a full load empties the feed so a failed load cannot be merged against old rows', () => {
  const start = startFullLoad(1, 0);
  assert.equal(start.feed, EMPTY_FEED);
  assert.deepEqual(start.feed, { entries: [], hasMore: false });
  // The load failed; the next background refresh sees an empty list, so it
  // takes the first page of the new filter instead of merging into old rows.
  const out = applyRefresh(start.feed, range(900, 801), LIMIT);
  assert.equal(out.restarted, false);
  assert.deepEqual(ids(out.feed.entries), ids(range(900, 801)));
});

test('rowKeys are stable for ids and suffix id-less duplicates', () => {
  assert.deepEqual(rowKeys(range(3, 1)), ['id:3', 'id:2', 'id:1']);
  const keys = rowKeys([noId(1), noId(1), noId(1)]);
  assert.equal(new Set(keys).size, 3);
  assert.equal(keys[0], entryKey(noId(1)));
});

test('a restart bump never makes an in-flight full load look superseded', () => {
  const start = startFullLoad(3, 0);
  // A refresh restarts the list and moves only the epoch.
  const epochAfterRestart = start.epoch + 1;
  assert.equal(isStaleEpoch(start.epoch, epochAfterRestart), true); // Load more / refresh are dropped
  assert.equal(isStaleEpoch(start.fullSeq, start.fullSeq), false); // the full load still applies and clears loading
});

test('an empty refresh on an empty feed returns the same object', () => {
  const current = feed([], false);
  assert.equal(applyRefresh(current, [], LIMIT).feed, current);
});

test('rowKeys separator cannot collide with a key that already ends in a counter', () => {
  const a = { ...noId(1), details: 'x#1' };
  const b = { ...noId(1), details: 'x' };
  const keys = rowKeys([b, b, a]);
  assert.equal(new Set(keys).size, 3);
});

test('a manual Refresh goes through applyRefresh: loaded pages stay and a Load more in flight still applies', () => {
  // Load more starts against epoch 4, then the Refresh button folds in new rows.
  const epoch = 4;
  const refreshed = applyRefresh(feed(range(300, 101)), range(305, 206), LIMIT);
  assert.equal(refreshed.restarted, false);
  assert.deepEqual(ids(refreshed.feed.entries), ids(range(305, 101)));
  // No epoch move without a restart, so the late older page is applied and deduped.
  assert.equal(isStaleEpoch(epoch, epoch), false);
  const late = applyOlderPage(refreshed.feed, range(100, 1), LIMIT);
  assert.deepEqual(ids(late.entries), ids(range(305, 1)));
});

test('a manual Refresh that restarts the list drops the Load more in flight', () => {
  const epoch = 4;
  const refreshed = applyRefresh(feed(range(300, 201)), range(900, 801), LIMIT);
  assert.equal(refreshed.restarted, true);
  assert.equal(isStaleEpoch(epoch, epoch + 1), true);
});
