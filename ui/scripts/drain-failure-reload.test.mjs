// Run with: npm test. After a failed drain or undrain the node list is reloaded
// only when the failure may have changed router state (server errors), and a
// failed undrain shows the server's message like drain does.
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { shouldReloadAfterNodeActionFailure, runtimeActionFailureInfo } from '../src/lib/soleReplica.ts';

const page = readFileSync(new URL('../src/pages/GPUNodes.tsx', import.meta.url), 'utf8');
const api = readFileSync(new URL('../src/lib/api.ts', import.meta.url), 'utf8');

test('reload after failure only for server errors', () => {
  assert.equal(shouldReloadAfterNodeActionFailure({ name: 'RuntimeActionError', status: 500 }), true);
  assert.equal(shouldReloadAfterNodeActionFailure({ status: 502 }), true);
  for (const status of [409, 404, 400]) {
    assert.equal(shouldReloadAfterNodeActionFailure({ status }), false);
  }
  assert.equal(shouldReloadAfterNodeActionFailure(new Error('network')), false);
  assert.equal(shouldReloadAfterNodeActionFailure(null), false);
  assert.equal(shouldReloadAfterNodeActionFailure(undefined), false);
  assert.equal(shouldReloadAfterNodeActionFailure({ status: '500' }), false);
});

test('undrain failure message keeps the server text', () => {
  const text = 'Undrain is active on the node but could not be saved and the node will be drained again on restart; retry the request (request id: abc)';
  assert.equal(runtimeActionFailureInfo(500, { error: text }, 'Failed to undrain node').message, text);
  const nf = 'node "x" not found';
  assert.equal(runtimeActionFailureInfo(404, { error: nf }, 'Failed to undrain node').message, nf);
});

function handlerBlock(from, to) {
  const start = page.indexOf(from);
  const end = page.indexOf(to, start);
  assert.ok(start >= 0 && end > start, `markers ${from} / ${to} must exist`);
  return page.slice(start, end);
}

test('both catch blocks set the message first, then reload only when the helper allows', () => {
  const drain = handlerBlock('const handleDrainNode', 'const handleUndrainNode');
  const undrain = handlerBlock('const handleUndrainNode', 'const handleTogglePrewarm');
  for (const block of [drain, undrain]) {
    const set = block.indexOf('setActionError(err?.message');
    const reload = block.indexOf('if (shouldReloadAfterNodeActionFailure(err)) await loadNodes();');
    assert.ok(set >= 0, 'catch sets the error');
    assert.ok(reload > set, 'helper-gated reload comes after the message is set');
  }
  assert.match(drain, /drainImpact\.reload\(\)/);
});

test('undrain modal blocks double submit while in flight', () => {
  const modal = handlerBlock('title="Undrain node"', 'Toggle Predictive Prewarm');
  assert.match(modal, /onClose=\{\(\) => \{ if \(undrainBusy\) return;/);
  assert.match(modal, /setUndrainBusy\(true\)/);
  assert.match(modal, /finally \{\s*setUndrainBusy\(false\)/);
  assert.match(modal, /busy=\{undrainBusy\}/);
});

test('undrainNode surfaces the server message and has a demo guard', () => {
  const start = api.indexOf('export async function undrainNode');
  const block = api.slice(start, api.indexOf('export async function', start + 10));
  assert.match(block, /if \(DEMO\) return demoDelay\(undefined\);/);
  assert.match(block, /runtimeActionFailure\(res, 'Failed to undrain node'\)/);
  assert.doesNotMatch(block, /new Error\('Failed to undrain node'\)/);
});
