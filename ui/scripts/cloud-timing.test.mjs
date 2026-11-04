import test from 'node:test';
import assert from 'node:assert/strict';
import { formatDurationMs, waitCloudSummary } from '../src/lib/requestsView.ts';

test('formatDurationMs shows ms under a second and seconds above', () => {
  assert.equal(formatDurationMs(320), '320ms');
  assert.equal(formatDurationMs(1500), '1.5s');
});

test('formatDurationMs never turns a missing value into 0', () => {
  assert.equal(formatDurationMs(undefined), '-');
  assert.equal(formatDurationMs(-1), '-');
  assert.equal(formatDurationMs(0), '0ms');
});

test('waitCloudSummary is empty when nothing was measured', () => {
  assert.equal(waitCloudSummary(undefined, undefined), undefined);
});

test('waitCloudSummary shows - for the unmeasured half', () => {
  assert.equal(waitCloudSummary(1500, 320), 'waited 1.5s, cloud 320ms');
  assert.equal(waitCloudSummary(undefined, 320), 'waited -, cloud 320ms');
});
