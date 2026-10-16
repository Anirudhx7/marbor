// Run with: npm test. Pure-logic tests for the drain/restart sole-replica
// dialog helpers; the module only has type imports so Node can load it directly.
import test from 'node:test';
import assert from 'node:assert/strict';
import {
  resolveImpactState, hasSoleModels, isChecking, confirmLabel, formatModel,
  coldLoadSummary, unknownLine, COLD_COLLAPSE_AFTER,
  impactSummary, impactVisible, shouldForce, runtimeActionQuery, runtimeActionFailureInfo, soleCheckErrorMessage,
  endSentence, readSoleReplicaImpact, createImpactChecker,
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

const cold = { models: [], cold_load_only: [{ model: 'm', runtime: 'ollama', peers: ['p'] }], unknown: [] };
const unk = { models: [], cold_load_only: [], unknown: [{ node: 'n', reason: 'r' }] };

test('the notice and the confirm button agree on when the impact is visible', () => {
  assert.equal(impactVisible({ status: 'idle' }), false);
  assert.equal(impactVisible({ status: 'ready', impact: empty }), false);
  assert.equal(impactVisible({ status: 'loading' }), true);
  assert.equal(impactVisible({ status: 'error' }), true);
  assert.equal(impactVisible({ status: 'ready', impact: sole }), true);
  assert.equal(impactVisible({ status: 'ready', impact: cold }), true);
  assert.equal(impactVisible({ status: 'ready', impact: unk }), true);
});

test('impact summary names the most serious finding', () => {
  assert.match(impactSummary({ status: 'ready', impact: sole }), /Only this node can serve 1 model/);
  assert.match(impactSummary({ status: 'ready', impact: cold }), /cold-load/);
  assert.match(impactSummary({ status: 'ready', impact: unk }), /Could not verify/);
  assert.equal(impactSummary({ status: 'ready', impact: empty }), null);
});

test('force is sent only for a drain or restart whose dialog listed sole models', () => {
  const ready = { status: 'ready', impact: sole };
  assert.equal(shouldForce('drain', ready), true);
  assert.equal(shouldForce('restart', ready), true);
  assert.equal(shouldForce('stop', ready), false);
  assert.equal(shouldForce('start', ready), false);
  for (const s of [{ status: 'loading' }, { status: 'error' }, { status: 'idle' },
    { status: 'ready', impact: empty }, { status: 'ready', impact: cold }, { status: 'ready', impact: unk }]) {
    assert.equal(shouldForce('drain', s), false);
    assert.equal(shouldForce('restart', s), false);
  }
});

test('query string carries both acknowledgements in one request and only exact true', () => {
  assert.equal(runtimeActionQuery({}), '');
  assert.equal(runtimeActionQuery({ acknowledgeReplica: false, force: false }), '');
  assert.equal(runtimeActionQuery({ force: true }), '?force=true');
  assert.equal(runtimeActionQuery({ acknowledgeReplica: true }), '?acknowledge_replica=true');
  assert.equal(runtimeActionQuery({ acknowledgeReplica: true, force: true }), '?acknowledge_replica=true&force=true');
});

test('failure mapping keeps the code and uses neutral wording for sole_replica', () => {
  const sr = runtimeActionFailureInfo(409, { error: 'server text with --force', code: 'sole_replica' }, 'Failed');
  assert.equal(sr.code, 'sole_replica');
  assert.doesNotMatch(sr.message, /--force|since you opened/);
  const rm = runtimeActionFailureInfo(409, { error: 'x', code: 'replica_member' }, 'Failed');
  assert.equal(rm.code, 'replica_member');
  assert.match(rm.message, /multi-host replica/);
  assert.deepEqual(runtimeActionFailureInfo(502, { error: 'agent down' }, 'Failed'), { message: 'agent down', code: undefined });
  assert.equal(runtimeActionFailureInfo(500, {}, 'Failed to drain node').message, 'Failed to drain node (HTTP 500)');
});

test('check error text keeps the reason', () => {
  assert.equal(soleCheckErrorMessage(new Error('Failed to check other replicas (HTTP 502)')), 'Failed to check other replicas (HTTP 502)');
  const abort = new Error('aborted');
  abort.name = 'AbortError';
  assert.equal(soleCheckErrorMessage(abort), 'Timed out checking other replicas');
  assert.equal(soleCheckErrorMessage('boom'), 'Could not check other replicas');
});

test('error summary uses the message and never doubles its punctuation', () => {
  assert.equal(impactSummary({ status: 'error' }), 'Could not check other replicas.');
  assert.equal(impactSummary({ status: 'error', message: 'Timed out checking other replicas' }), 'Timed out checking other replicas.');
  assert.equal(impactSummary({ status: 'error', message: 'Could not reach marbor.' }), 'Could not reach marbor.');
  assert.equal(endSentence('Failed?'), 'Failed?');
  assert.equal(endSentence('Failed'), 'Failed.');
});

const jsonResponse = (body, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

test('readSoleReplicaImpact throws on a missing sole_replica and on a bad status', async () => {
  await assert.rejects(readSoleReplicaImpact(jsonResponse({ node: 'a' })), /Unexpected response/);
  await assert.rejects(readSoleReplicaImpact(new Response('not json', { status: 200 })), /Unexpected response/);
  await assert.rejects(readSoleReplicaImpact(jsonResponse({ error: 'x' }, 503)), /HTTP 503/);
});

test('readSoleReplicaImpact fills missing lists with empty ones', async () => {
  const impact = await readSoleReplicaImpact(jsonResponse({ sole_replica: { models: [{ model: 'm', runtime: 'vllm' }] } }));
  assert.deepEqual(impact, { models: [{ model: 'm', runtime: 'vllm' }], cold_load_only: [], unknown: [] });
});

const abortable = (signal) => new Promise((_, reject) => {
  signal.addEventListener('abort', () => reject(Object.assign(new Error('aborted'), { name: 'AbortError' })));
});

test('a check that hangs is aborted at the timeout and reports an error', async () => {
  const seen = [];
  const checker = createImpactChecker((_name, signal) => abortable(signal), s => seen.push(s), 20);
  await checker.load('a');
  assert.deepEqual(seen.map(s => s.status), ['loading', 'error']);
  assert.equal(seen[1].message, 'Timed out checking other replicas');
});

test('a slow answer for an earlier node never overwrites a later one', async () => {
  const seen = [];
  const resolvers = {};
  const checker = createImpactChecker((name) => new Promise(resolve => { resolvers[name] = () => resolve(name === 'a' ? sole : empty); }), s => seen.push(s), 1000);
  const first = checker.load('a');
  const second = checker.load('b');
  resolvers.b();
  await second;
  resolvers.a();
  await first;
  const finals = seen.filter(s => s.status === 'ready');
  assert.equal(finals.length, 1);
  assert.equal(finals[0].node, 'b');
});

test('cancel aborts the request and drops its result', async () => {
  const seen = [];
  let signalSeen;
  const checker = createImpactChecker((_n, signal) => { signalSeen = signal; return abortable(signal); }, s => seen.push(s), 1000);
  const p = checker.load('a');
  checker.cancel();
  await p;
  assert.equal(signalSeen.aborted, true);
  assert.deepEqual(seen.map(s => s.status), ['loading']);
});
