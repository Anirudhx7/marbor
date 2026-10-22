// Run with: npm test. The demo Requests data mirrors GET /admin/audit's
// cursor paging (before_id / after_id, newest-first, exact next/prev cursors).
// This walks the mock the way a client walks the real endpoint: a full older
// walk, the walk back, filters combined with a cursor, no duplicates, and a
// no-cursor page that still matches the static filter output.
//
// mockData.ts has extensionless imports Node cannot load, so the block under
// test is sliced out of the source text and evaluated in a temp module.
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL, fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const src = fs.readFileSync(path.join(here, '..', 'src/lib/mockData.ts'), 'utf8').replace(/\r\n/g, '\n');

const START = '// start of audit paging mock';
const END = '// end of audit paging mock';

async function loadMock() {
  const start = src.indexOf(START);
  const end = src.indexOf(END);
  assert.ok(start >= 0, 'start marker not found in mockData.ts');
  assert.ok(end > start, 'end marker not found in mockData.ts');
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'marbor-audit-paging-'));
  const file = path.join(dir, 'mock.ts');
  // The slice references the RequestEntry type only in annotations, which the
  // type stripper erases.
  fs.writeFileSync(file, src.slice(start, end));
  try {
    return await import(pathToFileURL(file).href);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

const mock = await loadMock();
const { mockAuditRows, mockRequests, filterMockRequests, pageMockRequests } = mock;
const ids = (page) => page.entries.map((e) => e.auditId);

test('mock audit rows have unique, strictly descending numeric ids', () => {
  assert.ok(mockAuditRows.length >= 100, `only ${mockAuditRows.length} rows`);
  for (const r of mockAuditRows) assert.equal(typeof r.auditId, 'number');
  for (let i = 1; i < mockAuditRows.length; i += 1) {
    assert.ok(mockAuditRows[i - 1].auditId > mockAuditRows[i].auditId, `row ${i} not descending`);
  }
  const reqIds = new Set(mockAuditRows.map((r) => r.id));
  assert.equal(reqIds.size, mockAuditRows.length, 'request ids must be unique');
});

test('older walk covers every row once, newest-first, with exact cursors', () => {
  const seen = [];
  let before;
  let pages = 0;
  for (;;) {
    const page = pageMockRequests({}, { limit: 50, beforeId: before });
    if (pages === 0) assert.equal(page.prevAfterId, undefined, 'newest page has no prev cursor');
    else assert.equal(page.prevAfterId, page.entries[0].auditId, 'prev cursor is the first id');
    seen.push(...ids(page));
    pages += 1;
    if (page.nextBeforeId === undefined) break;
    assert.equal(page.nextBeforeId, page.entries[page.entries.length - 1].auditId, 'next cursor is the last id');
    before = page.nextBeforeId;
    assert.ok(pages < 100, 'walk did not terminate');
  }
  assert.equal(seen.length, mockAuditRows.length);
  assert.equal(new Set(seen).size, seen.length, 'duplicate rows across pages');
  assert.deepEqual(seen, mockAuditRows.map((r) => r.auditId));
  assert.ok(pages > 1, 'walk should span several pages');
});

test('walking back with after_id returns the same pages newest-first', () => {
  const first = pageMockRequests({}, { limit: 40 });
  const second = pageMockRequests({}, { limit: 40, beforeId: first.nextBeforeId });
  const third = pageMockRequests({}, { limit: 40, beforeId: second.nextBeforeId });

  const backToSecond = pageMockRequests({}, { limit: 40, afterId: third.prevAfterId });
  assert.deepEqual(ids(backToSecond), ids(second));
  assert.equal(backToSecond.nextBeforeId, second.nextBeforeId);
  assert.equal(backToSecond.prevAfterId, second.prevAfterId);

  const backToFirst = pageMockRequests({}, { limit: 40, afterId: second.prevAfterId });
  assert.deepEqual(ids(backToFirst), ids(first));
  assert.equal(backToFirst.prevAfterId, undefined, 'back on the live newest page');
});

test('filters combine with a cursor', () => {
  const filters = { status: 'success', key: 'engineering' };
  const all = mockAuditRows.filter((r) => r.status >= 200 && r.status < 300 && (r.key_name ?? '').toLowerCase().includes('engineering'));
  assert.ok(all.length > 10, 'filter should hit many rows');
  const seen = [];
  let before;
  for (let guard = 0; guard < 50; guard += 1) {
    const page = pageMockRequests(filters, { limit: 7, beforeId: before });
    for (const e of page.entries) {
      assert.ok(e.status >= 200 && e.status < 300);
      assert.ok((e.key_name ?? '').toLowerCase().includes('engineering'));
    }
    seen.push(...ids(page));
    if (page.nextBeforeId === undefined) break;
    before = page.nextBeforeId;
  }
  assert.deepEqual(seen, all.map((r) => r.auditId));

  const cloud = pageMockRequests({ cloud: true }, { limit: 1000 });
  assert.ok(cloud.entries.length > 0 && cloud.entries.every((e) => e.cloud));
  assert.equal(cloud.nextBeforeId, undefined);
});

test('a stale cursor past the newest row is an empty page with no cursors', () => {
  const page = pageMockRequests({}, { limit: 10, afterId: mockAuditRows[0].auditId });
  assert.equal(page.entries.length, 0);
  assert.equal(page.nextBeforeId, undefined);
  assert.equal(page.prevAfterId, undefined);
  const tail = pageMockRequests({}, { limit: 10, beforeId: mockAuditRows[mockAuditRows.length - 1].auditId });
  assert.equal(tail.entries.length, 0);
  assert.equal(tail.nextBeforeId, undefined);
});

test('limit is clamped to 1..1000 like the server', () => {
  assert.equal(pageMockRequests({}, { limit: 0 }).entries.length, 1);
  assert.equal(pageMockRequests({}, { limit: 5000 }).entries.length, mockAuditRows.length);
});

test('a no-cursor page starts with exactly the static filter output', () => {
  const sets = [{}, { status: 'success' }, { status: 'client_error' }, { cloud: true }, { model: 'qwen' }, { node: 'gpu-node-01' }, { key: 'support' }];
  for (const f of sets) {
    const expected = filterMockRequests(f);
    const page = pageMockRequests(f, { limit: 1000 });
    const head = page.entries.slice(0, expected.length).map(({ auditId, ...rest }) => rest);
    assert.deepEqual(head, expected, `filter ${JSON.stringify(f)}`);
  }
  assert.ok(mockRequests.length > 0);
});
