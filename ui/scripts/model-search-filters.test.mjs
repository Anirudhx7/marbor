// Run with: npm test. Guards the Model Advisor search filters: only values the
// server accepts are ever sent (whole numbers above zero, real YYYY-MM-DD dates).
import test from 'node:test';
import assert from 'node:assert/strict';
import { wholeNumber, realDate } from '../src/lib/modelSearchFilters.ts';

test('wholeNumber accepts whole numbers above zero', () => {
  assert.equal(wholeNumber('5'), 5);
  assert.equal(wholeNumber('007'), 7);
  assert.equal(wholeNumber('1e3'), 1000);
  assert.equal(wholeNumber(String(Number.MAX_SAFE_INTEGER)), Number.MAX_SAFE_INTEGER);
});

test('wholeNumber drops everything the server would reject', () => {
  for (const raw of ['', '0', '-5', '1.5', 'abc', '1e+21', '9007199254740993', ' ']) {
    assert.equal(wholeNumber(raw), undefined, raw);
  }
});

test('realDate accepts real calendar dates only', () => {
  assert.equal(realDate('2024-02-29'), '2024-02-29');
  for (const raw of ['', '2024-1-5', '2024-02-30', '2024-13-01', '275760-09-13', '2024-01-01T00:00:00Z']) {
    assert.equal(realDate(raw), undefined, raw);
  }
});
