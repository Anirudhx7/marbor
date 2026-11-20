// Run with: npm test. The fleet-health strip counts nodes from the server's own
// signals and headlessWorker flag. These tests pin the bucket rules so a failing
// replica worker is counted once, as a worker, and the strip agrees with the
// health endpoint's totals.
import test from 'node:test';
import assert from 'node:assert/strict';
import { computeFleetHealth, isDemoHeadlessWorker, nodeDotStatus, nodeDotTitle, WORKER_DOT_TITLE, workersLabel, workersText } from '../src/lib/fleetHealth.ts';

const node = (name, signals, extra = {}) => ({ id: name, name, health: 'healthy', draining: false, signals, ...extra });

test('a flagged worker counts once, as a worker, even at Healthy=true with a failure', () => {
  // Server shape: raw health healthy (probe failure below the threshold), muted, no health signal.
  const f = computeFleetHealth([
    node('a', ['healthy']),
    node('w', [], { headlessWorker: true, health: 'healthy' }),
  ]);
  assert.equal(f.total, 1);
  assert.equal(f.healthy, 1);
  assert.equal(f.downNodes.length, 0);
  assert.equal(f.workersFailing, 1);
});

test('a worker flagged and raw down is not a down node and not in the total', () => {
  const f = computeFleetHealth([
    node('a', ['healthy']),
    node('b', ['down'], { health: 'down' }),
    node('w', [], { headlessWorker: true, health: 'down' }),
  ]);
  assert.equal(f.total, 2);
  assert.deepEqual(f.downNodes.map((n) => n.name), ['b']);
  assert.equal(f.workersFailing, 1);
});

test('a draining flagged worker is still counted as draining and as a worker, never in the total', () => {
  const f = computeFleetHealth([node('w', ['draining'], { headlessWorker: true, health: 'down', draining: true })]);
  assert.equal(f.total, 0);
  assert.equal(f.draining, 1);
  assert.equal(f.workersFailing, 1);
});

test('muting a failing probe never hides VRAM pressure or a silent agent on the worker', () => {
  const f = computeFleetHealth([node('w', ['pressure'], { headlessWorker: true, health: 'down', agentStale: true })]);
  assert.deepEqual(f.vramPressure.map((n) => n.name), ['w']);
  assert.deepEqual(f.staleAgents.map((n) => n.name), ['w']);
});

test('the flag wins over stray health signals on a worker', () => {
  const f = computeFleetHealth([node('w', ['healthy', 'degraded', 'down'], { headlessWorker: true })]);
  assert.equal(f.healthy, 0);
  assert.equal(f.degraded, 0);
  assert.equal(f.downNodes.length, 0);
  assert.equal(f.total, 0);
});

test('a degraded node is counted and a node without signals counts as nothing', () => {
  const f = computeFleetHealth([node('a', ['degraded'], { health: 'degraded' }), { id: 'b', name: 'b', health: 'healthy' }]);
  assert.equal(f.total, 2);
  assert.equal(f.degraded, 1);
  assert.equal(f.healthy, 0);
  assert.equal(f.workersFailing, 0);
});

test('an all-workers fleet has no counted nodes and shows only the worker figure', () => {
  const f = computeFleetHealth([
    node('w1', [], { headlessWorker: true }),
    node('w2', [], { headlessWorker: true }),
  ]);
  assert.equal(f.total, 0);
  assert.equal(f.workersFailing, 2);
  assert.deepEqual(f.workerNodes.map((n) => n.name), ['w1', 'w2']);
});

test('a node without the flag (older server) renders as today', () => {
  const f = computeFleetHealth([node('a', ['down'], { health: 'down' })]);
  assert.equal(f.total, 1);
  assert.equal(f.downNodes.length, 1);
  assert.equal(f.workersFailing, 0);
});

test('every node is counted exactly once: total plus workers is the node count', () => {
  const nodes = [
    node('a', ['healthy']), node('b', ['degraded'], { health: 'degraded' }), node('c', ['down'], { health: 'down' }),
    node('d', ['draining'], { draining: true }),
    node('w', [], { headlessWorker: true, health: 'down' }), node('x', [], { headlessWorker: true, health: 'healthy' }),
  ];
  const f = computeFleetHealth(nodes);
  assert.equal(f.total + f.workersFailing, nodes.length);
  assert.equal(f.healthy, 1);
  assert.equal(f.workersFailing, 2);
});

test('workersText is singular, plural and empty at zero', () => {
  assert.equal(workersText(0), '');
  assert.equal(workersText(1), '1 worker probe failing');
  assert.equal(workersText(3), '3 workers probe failing');
  assert.equal(workersLabel(1), 'worker probe failing');
  assert.equal(workersLabel(2), 'workers probe failing');
});

test('the dot is muted for a headless worker and keeps both facts when it is draining', () => {
  assert.equal(nodeDotStatus({ health: 'down', headlessWorker: true }), 'worker');
  assert.equal(nodeDotStatus({ health: 'down' }), 'down');
  assert.equal(nodeDotTitle({ draining: false }), undefined);
  assert.equal(nodeDotTitle({ draining: true }), 'Draining');
  assert.equal(nodeDotTitle({ draining: false, headlessWorker: true }), WORKER_DOT_TITLE);
  assert.equal(nodeDotTitle({ draining: true, headlessWorker: true }), `Draining. ${WORKER_DOT_TITLE}`);
});

test('the demo overlay mutes only a confirmed non-head member whose probe is failing', () => {
  assert.equal(isDemoHeadlessWorker(false, 'down'), true);
  assert.equal(isDemoHeadlessWorker(true, 'down'), false);
  assert.equal(isDemoHeadlessWorker(false, 'healthy'), false);
  assert.equal(isDemoHeadlessWorker(false, 'degraded'), false);
});
