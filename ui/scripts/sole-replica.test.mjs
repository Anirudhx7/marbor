// Run with: npm test. Pure-logic tests for the drain/restart sole-replica
// dialog helpers; the module only has type imports so Node can load it directly.
import test from 'node:test';
import assert from 'node:assert/strict';
import {
  resolveImpactState, hasSoleModels, isChecking, confirmLabel, formatModel,
  coldLoadSummary, unknownLine, COLD_COLLAPSE_AFTER,
  impactSummary, impactVisible, shouldForce, runtimeActionQuery, runtimeActionFailureInfo, soleCheckErrorMessage,
  endSentence, readSoleReplicaImpact, createImpactChecker,
  soleActionArgs, MAX_SOLE_QUERY_BYTES, STALE_LIST_MESSAGE, SOLE_REPLICA_CODE, withoutHover,
} from '../src/lib/soleReplica.ts';
import { readFileSync } from 'node:fs';

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

// --- acknowledge_sole ---

test('the ack sent equals the displayed list, and only with force', () => {
  const two = { models: [{ model: 'm1', runtime: 'vllm' }, { model: 'org/m 2', runtime: 'ollama' }], cold_load_only: [], unknown: [] };
  const ready = { status: 'ready', impact: two };
  assert.deepEqual(soleActionArgs('drain', ready), { force: true, acknowledgedSole: ['m1', 'org/m 2'] });
  assert.deepEqual(soleActionArgs('restart', ready), { force: true, acknowledgedSole: ['m1', 'org/m 2'] });
  assert.deepEqual(soleActionArgs('stop', ready), { force: false });
  for (const s of [{ status: 'loading' }, { status: 'error' }, { status: 'idle' },
    { status: 'ready', impact: empty }, { status: 'ready', impact: cold }, { status: 'ready', impact: unk }]) {
    assert.deepEqual(soleActionArgs('drain', s), { force: false });
  }
});

test('force is only ever decided by shouldForce', () => {
  const ready = { status: 'ready', impact: sole };
  for (const action of ['drain', 'start', 'stop', 'restart']) {
    assert.equal(soleActionArgs(action, ready).force, shouldForce(action, ready));
  }
});

test('query carries one encoded acknowledge_sole per model after force', () => {
  assert.equal(runtimeActionQuery({ force: true, acknowledgedSole: ['m1', 'm2'] }), '?force=true&acknowledge_sole=m1&acknowledge_sole=m2');
  assert.equal(
    runtimeActionQuery({ acknowledgeReplica: true, force: true, acknowledgedSole: ['a+b&c=d#e;f%41/g:h', 'café'] }),
    `?acknowledge_replica=true&force=true&acknowledge_sole=${encodeURIComponent('a+b&c=d#e;f%41/g:h')}&acknowledge_sole=${encodeURIComponent('café')}`,
  );
  const q = runtimeActionQuery({ force: true, acknowledgedSole: ['a+b&c=d#e;f%41/g:h'] });
  assert.equal(new URLSearchParams(q.slice(1)).get('acknowledge_sole'), 'a+b&c=d#e;f%41/g:h');
});

test('an empty reviewed list still sends the key; no force sends no ack', () => {
  assert.equal(runtimeActionQuery({ force: true, acknowledgedSole: [] }), '?force=true&acknowledge_sole=');
  assert.equal(runtimeActionQuery({ force: true }), '?force=true');
  assert.equal(runtimeActionQuery({ force: false, acknowledgedSole: ['m1'] }), '');
});

test('the query size limit: just under passes, just over is refused', () => {
  const base = 'force=true&acknowledge_sole='.length;
  const under = 'a'.repeat(MAX_SOLE_QUERY_BYTES - base);
  assert.equal(runtimeActionQuery({ force: true, acknowledgedSole: [under] }).length - 1, MAX_SOLE_QUERY_BYTES);
  assert.throws(() => runtimeActionQuery({ force: true, acknowledgedSole: [under + 'a'] }), /Too many models/);
  assert.equal(MAX_SOLE_QUERY_BYTES, 6000);
});

test('a stale-ack refusal maps to the reopen banner; a plain sole_replica refusal keeps its wording', () => {
  const stale = runtimeActionFailureInfo(409, { error: 'server text', code: 'sole_replica', unacknowledged: ['m9'] }, 'Failed');
  assert.equal(stale.code, 'sole_replica');
  assert.ok(stale.message.startsWith(STALE_LIST_MESSAGE));
  assert.match(stale.message, /Not in your list: m9./);
  const plain = runtimeActionFailureInfo(409, { error: 'x', code: 'sole_replica' }, 'Failed');
  assert.notEqual(plain.message, STALE_LIST_MESSAGE);
  const emptyList = runtimeActionFailureInfo(409, { error: 'x', code: 'sole_replica', unacknowledged: [] }, 'Failed');
  assert.notEqual(emptyList.message, STALE_LIST_MESSAGE);
});

test('both dialogs take force and the ack from soleActionArgs and never retry on a refusal', () => {
  const src = readFileSync(new URL('../src/pages/GPUNodes.tsx', import.meta.url), 'utf8');
  assert.equal(src.match(/soleActionArgs\(/g).length, 2);
  assert.doesNotMatch(src, /shouldForce\(/);
  const drainStart = src.indexOf('const handleDrainNode');
  const drainEnd = src.indexOf('const handleUndrainNode');
  assert.ok(drainStart >= 0 && drainEnd > drainStart, 'drain handler markers must exist');
  const drainCatch = src.slice(drainStart, drainEnd);
  assert.equal(drainCatch.match(/drainNode\(/g).length, 1, 'handleDrainNode calls the API once and never retries');
  assert.match(drainCatch, /drainImpact\.reload\(\)/);
  const restartStart = src.indexOf('const runRuntimeAction');
  const restartEnd = src.indexOf('const viewRuntimeLogs');
  assert.ok(restartStart >= 0 && restartEnd > restartStart, 'restart handler markers must exist');
  const restartBlock = src.slice(restartStart, restartEnd);
  assert.equal(restartBlock.match(/restartNodeRuntime\(/g).length, 1);
  assert.match(restartBlock, /setRuntimeActionConfirm\(action\)/);
});

test('the same model under two runtimes is acknowledged once', () => {
  const dup = { models: [{ model: 'm1', runtime: 'ollama' }, { model: 'm1', runtime: 'vllm' }, { model: 'm2', runtime: 'vllm' }], cold_load_only: [], unknown: [] };
  assert.deepEqual(soleActionArgs('drain', { status: 'ready', impact: dup }), { force: true, acknowledgedSole: ['m1', 'm2'] });
});

test('force by action and state, spelled out', () => {
  const ready = { status: 'ready', impact: sole };
  const expected = { drain: true, restart: true, start: false, stop: false };
  for (const [action, want] of Object.entries(expected)) {
    assert.equal(soleActionArgs(action, ready).force, want, action);
    assert.equal(soleActionArgs(action, { status: 'ready', impact: empty }).force, false, action);
  }
});

test('the sole_replica code is one shared constant and the stale banner is accurate for any list', () => {
  assert.equal(SOLE_REPLICA_CODE, 'sole_replica');
  assert.doesNotMatch(STALE_LIST_MESSAGE, /the list changed/i);
  assert.match(STALE_LIST_MESSAGE, /Review the current list and confirm again/);
  assert.doesNotMatch(STALE_LIST_MESSAGE, /below|above/);
  const page = readFileSync(new URL('../src/pages/GPUNodes.tsx', import.meta.url), 'utf8');
  assert.doesNotMatch(page, /'sole_replica'/, 'GPUNodes uses SOLE_REPLICA_CODE');
});

test('an encoded query is ASCII, so its length is its byte length', () => {
  const q = runtimeActionQuery({ force: true, acknowledgedSole: ['café-中文', 'x y'] });
  assert.ok(/^[ -~]*$/.test(q));
  assert.equal(new TextEncoder().encode(q).length, q.length);
});

test('withoutHover removes hover variants only', () => {
  assert.equal(
    withoutHover('px-4 bg-amber-600 hover:bg-amber-600/90 sm:hover:shadow-lg group-hover:opacity-100 transition-colors'),
    'px-4 bg-amber-600 group-hover:opacity-100 transition-colors',
  );
  assert.equal(withoutHover(''), '');
});

test('the drain dialog drops hover while the confirm button is held, and the restart size check runs before the dialog closes', () => {
  const src = readFileSync(new URL('../src/pages/GPUNodes.tsx', import.meta.url), 'utf8');
  assert.ok(src.includes('busy || confirmDisabled ? withoutHover(confirmClassName)'));
  const start = src.indexOf('const runRuntimeAction');
  assert.ok(start >= 0);
  const block = src.slice(start, src.indexOf('const viewRuntimeLogs'));
  const check = block.indexOf('runtimeActionQuery({ acknowledgeReplica: isReplicaMember(agentNode), force, acknowledgedSole })');
  const close = block.indexOf('setRuntimeActionConfirm(null)');
  assert.ok(check >= 0 && close > check, 'the size check comes before the dialog closes');
  assert.match(src, /aria-disabled:hover:bg-destructive/);
});

test('api.ts sends the acknowledgement for both actions and keeps the unacknowledged list for the failure mapping', () => {
  const api = readFileSync(new URL('../src/lib/api.ts', import.meta.url), 'utf8');
  assert.ok(api.includes('/drain${runtimeActionQuery({ force, acknowledgedSole })}'));
  assert.ok(api.includes('/runtime/restart${runtimeActionQuery({ acknowledgeReplica, force, acknowledgedSole })}'));
  assert.ok(api.includes('unacknowledged?: string[] };'));
  assert.ok(api.includes('runtimeActionFailureInfo(res.status, j, fallback)'));
  const drainStart = api.indexOf('export async function drainNode');
  const drainEnd = api.indexOf('export async function undrainNode');
  assert.ok(drainStart >= 0 && drainEnd > drainStart);
  assert.ok(api.slice(drainStart, drainEnd).includes('throw await runtimeActionFailure(res'));
  assert.ok(runtimeActionFailureInfo(409, { error: 'x', code: 'sole_replica', unacknowledged: ['m'] }, 'Failed to drain node').message.startsWith(STALE_LIST_MESSAGE));
});

test('messages never point in a direction, and the restart pre-check carries the replica acknowledgement', () => {
  for (const m of [
    runtimeActionFailureInfo(409, { code: 'replica_member' }, 'F').message,
    runtimeActionFailureInfo(409, { code: 'sole_replica' }, 'F').message,
  ]) assert.doesNotMatch(m, /below|above/);
  const page = readFileSync(new URL('../src/pages/GPUNodes.tsx', import.meta.url), 'utf8');
  assert.ok(page.includes('runtimeActionQuery({ acknowledgeReplica: isReplicaMember(agentNode), force, acknowledgedSole })'));
  assert.ok(page.includes('role="alert" className="text-sm text-destructive">{actionError}'));
  assert.ok(page.includes('role="alert" className="text-sm text-destructive">{runtimeActionError}'));
  assert.ok(page.includes("setRuntimeActionNotice(null); setRuntimeActionError(null); setRuntimeActionConfirm('restart');"));
});
