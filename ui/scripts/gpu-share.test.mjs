// Run with: npm test. Badge rules for models partly on CPU.
import test from 'node:test';
import assert from 'node:assert/strict';
import { showPartialGpuBadge, formatContext, PARTIAL_GPU_TOOLTIP } from '../src/lib/gpuShare.ts';

const node = (models, vramTotalMB) => ({ vramTotalMB, loadedModels: models });

test('partial share warns on any node, even with no known VRAM', () => {
  const m = { gpuPercent: 62 };
  assert.equal(showPartialGpuBadge(m, node([m], 0)), true);
  assert.equal(showPartialGpuBadge(m, node([m], undefined)), true);
  assert.equal(showPartialGpuBadge(m, node([m], 24000)), true);
});

test('full GPU and unreported show nothing', () => {
  for (const m of [{ gpuPercent: 100 }, { gpuPercent: null }, {}, { contextLength: 4096 }]) {
    assert.equal(showPartialGpuBadge(m, node([m], 24000)), false);
  }
});

test('0 is a real reading, not falsy', () => {
  const m = { gpuPercent: 0 };
  assert.equal(showPartialGpuBadge(m, node([m], 24000)), true);
});

test('0 on a node with no known VRAM and no other GPU model is not flagged', () => {
  const m = { gpuPercent: 0 };
  assert.equal(showPartialGpuBadge(m, node([m], 0)), false);
  assert.equal(showPartialGpuBadge(m, node([m], null)), false);
});

test('0 on a node with no known VRAM is flagged when a sibling is on the GPU', () => {
  const m = { gpuPercent: 0 };
  const sibling = { gpuPercent: 100 };
  assert.equal(showPartialGpuBadge(m, node([m, sibling], 0)), true);
  const unreportedSibling = { };
  assert.equal(showPartialGpuBadge(m, node([m, unreportedSibling], 0)), false);
  const anotherZero = { gpuPercent: 0 };
  assert.equal(showPartialGpuBadge(m, node([m, anotherZero], 0)), false);
});

test('context formatting', () => {
  assert.equal(formatContext(4096), '4k');
  assert.equal(formatContext(32768), '32k');
  assert.equal(formatContext(131072), '128k');
  assert.equal(formatContext(2048 + 512), '2.5k');
  assert.equal(formatContext(512), '512');
  assert.equal(formatContext(null), null);
  assert.equal(formatContext(undefined), null);
  assert.equal(formatContext(0), null);
});

test('tooltip says what happens, what to do and the CPU-only reading', () => {
  assert.match(PARTIAL_GPU_TOOLTIP, /runs on CPU/);
  assert.match(PARTIAL_GPU_TOOLTIP, /Lower num_ctx or free GPU memory/);
  assert.match(PARTIAL_GPU_TOOLTIP, /no GPU for Ollama/);
});
