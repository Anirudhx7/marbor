// The Requests page reads /admin/audit, so the prompt-cache split must survive
// the mapping from the server row to the page row, and a missing or wrongly
// typed value must stay absent (unknown), never become 0.
import test from 'node:test';
import assert from 'node:assert/strict';
import { mapAuditEntry } from '../src/lib/auditEntry.ts';

const row = (over = {}) => ({
  id: 7, time: '2026-11-20T10:00:00Z', request_id: 'req-1', key_name: 'k', model: 'm', node: 'n',
  status: '200', latency_ms: 5, cloud: false, ...over,
});

test('a joined cache split is carried onto the page row', () => {
  const e = mapAuditEntry(row({ cachedTokens: 1820, inputTokens: 2000 }));
  assert.equal(e.cachedTokens, 1820);
  assert.equal(e.inputTokens, 2000);
  assert.equal(e.id, 'req-1');
  assert.equal(e.auditId, 7);
});

test('a real zero cached count is kept', () => {
  const e = mapAuditEntry(row({ cachedTokens: 0, inputTokens: 1450 }));
  assert.equal(e.cachedTokens, 0);
  assert.equal(e.inputTokens, 1450);
});

test('an absent, null, string or half-present split stays absent', () => {
  for (const over of [
    {},
    { cachedTokens: null, inputTokens: null },
    { cachedTokens: '5', inputTokens: '9' },
    { cachedTokens: 5 },
    { inputTokens: 9 },
  ]) {
    const e = mapAuditEntry(row(over));
    assert.equal(e.cachedTokens, undefined, JSON.stringify(over));
    assert.equal(e.inputTokens, undefined, JSON.stringify(over));
  }
});
