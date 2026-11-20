// Run with: npm test. The fleet-health strip counts nodes from the server's own
// signals and headlessWorker flag. These tests pin the bucket rules so a failing
// replica worker is counted once, as a worker, and the strip agrees with the
// health endpoint's totals.
import test from 'node:test';
import assert from 'node:assert/strict';
import { computeFleetHealth, workersText } from '../src/lib/fleetHealth.ts';

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

test('a draining flagged worker is only a worker, never also draining', () => {
  const f = computeFleetHealth([node('w', ['draining'], { headlessWorker: true, health: 'down', draining: true })]);
  assert.equal(f.draining, 0);
  assert.equal(f.workersFailing, 1);
});

test('an all-workers fleet has no counted nodes and shows only the worker figure', () => {
  const f = computeFleetHealth([
    node('w1', [], { headlessWorker: true }),
    node('w2', [], { headlessWorker: true }),
  ]);
  assert.equal(f.total, 0);
  assert.equal(f.workersFailing, 2);
});

test('a node without the flag (older server) renders as today', () => {
  const f = computeFleetHealth([node('a', ['down'], { health: 'down' })]);
  assert.equal(f.total, 1);
  assert.equal(f.downNodes.length, 1);
  assert.equal(f.workersFailing, 0);
});

test('strip totals equal the health endpoint fixture for the same fleet', () => {
  const nodes = [
    node('a', ['healthy']), node('b', ['degraded'], { health: 'degraded' }), node('c', ['down'], { health: 'down' }),
    node('w', [], { headlessWorker: true, health: 'down' }),
  ];
  // Mirror of /health: total and healthy leave out the failing worker, workers counts it.
  const healthFixture = { total: 3, healthy: 1, workers: 1 };
  const f = computeFleetHealth(nodes);
  assert.equal(f.total, healthFixture.total);
  assert.equal(f.healthy, healthFixture.healthy);
  assert.equal(f.workersFailing, healthFixture.workers);
});

test('workersText is singular, plural and empty at zero', () => {
  assert.equal(workersText(0), '');
  assert.equal(workersText(1), '1 worker probe failing');
  assert.equal(workersText(3), '3 workers probe failing');
});
