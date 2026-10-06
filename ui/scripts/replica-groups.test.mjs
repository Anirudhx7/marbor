// Run with: npm test. Pure-logic tests for replica group helpers; the module
// only has type imports so Node can load it directly.
import test from 'node:test';
import assert from 'node:assert/strict';
import { mentionsNode, outsideDeclarers, pruneDismissedReady, reasonToShow } from '../src/lib/replicaGroups.ts';

test('mentionsNode matches whole node names only', () => {
  assert.equal(mentionsNode('node "gpu-node-10" declares it', 'gpu-node-1'), false);
  assert.equal(mentionsNode('node "gpu-node-1" declares it', 'gpu-node-1'), true);
  assert.equal(mentionsNode('gpu-node-10 and gpu-node-1.', 'gpu-node-1'), true);
  assert.equal(mentionsNode('', 'a'), false);
});

test('outsideDeclarers does not match a node whose name is a prefix of a mentioned one', () => {
  const s = { members: ['a', 'b'], declared: [] };
  const nodes = [{ name: 'gpu-node-1' }, { name: 'gpu-node-10' }];
  assert.deepEqual(outsideDeclarers(s, nodes, 'gpu-node-10 declares a'), ['gpu-node-10']);
});

test('pruneDismissedReady drops fingerprints that no longer exist', () => {
  assert.deepEqual(pruneDismissedReady(['a', 'b', 'c'], ['b', 'c', 'd']), ['b', 'c']);
  assert.deepEqual(pruneDismissedReady(['a'], []), []);
});

test('reasonToShow hides the reason only when missing[] already explains it', () => {
  assert.equal(reasonToShow({ reason: 'r', missing: [] }), 'r');
  assert.equal(reasonToShow({ reason: 'r', missing: ['x'] }), '');
  assert.equal(reasonToShow({ reason: '', missing: [] }), '');
});
