// Run with: npm test. Pure-logic tests for the drain/restart sole-replica
// dialog helpers; the module only has type imports so Node can load it directly.
import test from 'node:test';
import assert from 'node:assert/strict';
import {
  resolveImpactState, hasSoleModels, isChecking, confirmLabel, formatModel,
  coldLoadSummary, unknownLine, COLD_COLLAPSE_AFTER,
} from '../src/lib/soleReplica.ts';

const empty = { models: [], cold_load_only: [], unknown: [] };
const sole = { models: [{ model: 'm1', runtime: 'vllm' }], cold_load_only: [], unknown: [] };

test('state for a different node than the dialog target counts as checking', () => {
  const stale = { node: 'a', status: 'ready', impact: sole };
  assert.equal(resolveImpactState(stale, 'b').status, 'loading');
  assert.equal(resolveImpactState(stale, 'a').status, 'ready');
  assert.equal(resolveImpactState(stale, null).status, 'idle');
});

test('hasSoleModels is true only for a ready state with models', () => {
  assert.equal(hasSoleModels({ status: 'ready', impact: sole }), true);
  assert.equal(hasSoleModels({ status: 'ready', impact: empty }), false);
  assert.equal(hasSoleModels({ status: 'loading' }), false);
  assert.equal(hasSoleModels({ status: 'error' }), false);
  assert.equal(hasSoleModels({ status: 'idle' }), false);
});

test('only loading blocks confirm; error and idle do not', () => {
  assert.equal(isChecking({ status: 'loading' }), true);
  assert.equal(isChecking({ status: 'error' }), false);
  assert.equal(isChecking({ status: 'idle' }), false);
});

test('confirm label follows busy and sole state', () => {
  assert.equal(confirmLabel('Drain', false, { status: 'ready', impact: sole }), 'Drain anyway');
  assert.equal(confirmLabel('Drain', false, { status: 'ready', impact: empty }), 'Drain node');
  assert.equal(confirmLabel('Restart', false, { status: 'ready', impact: sole }), 'Restart anyway');
  assert.equal(confirmLabel('Restart', false, { status: 'error' }), 'Restart');
  assert.equal(confirmLabel('Drain', true, { status: 'ready', impact: sole }), 'Draining...');
  assert.equal(confirmLabel('Restart', true, { status: 'idle' }), 'Restarting...');
});

test('formatting helpers', () => {
  assert.equal(formatModel({ model: 'm1', runtime: 'vllm' }), 'm1 (vllm)');
  assert.equal(formatModel({ model: 'm1', runtime: '' }), 'm1');
  assert.equal(unknownLine({ node: 'n1', reason: 'r' }), 'n1: r');
  assert.equal(unknownLine({ node: 'n1', model: 'm', reason: 'r' }), 'n1, m: r');
  assert.equal(coldLoadSummary(1), '1 model would have to cold-load on another node');
  assert.equal(coldLoadSummary(4), '4 models would have to cold-load on other nodes');
  assert.equal(COLD_COLLAPSE_AFTER, 3);
});
