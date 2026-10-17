// Run with: npm test. The demo node fixtures carry static `signals` arrays
// (the real server computes them); this checks each fixture's array against
// the documented signal rules so the demo's chips and counts stay honest.
//
// The fixture arrays live in .ts files with extensionless imports Node cannot
// load, so the array literal is sliced out of the source text and evaluated.
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL, fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const read = (rel) => fs.readFileSync(path.join(here, '..', rel), 'utf8').replace(/\r\n/g, '\n');

const CANONICAL = ['healthy', 'degraded', 'draining', 'down', 'agent', 'warm', 'pressure'];

// The documented rules, in canonical order.
function expectedSignals(n) {
  const out = [];
  if (n.health === 'healthy' && !n.draining) out.push('healthy');
  if (n.health === 'degraded' && !n.draining) out.push('degraded');
  if (n.draining) out.push('draining');
  if (n.health === 'down') out.push('down');
  if (n.agentPresent) out.push('agent');
  if ((n.loadedModels ?? []).length > 0) out.push('warm');
  if (n.vramSource !== 'none' && n.vramTotalMB > 0 && n.vramUsedMB / n.vramTotalMB >= 0.9) out.push('pressure');
  return out;
}

// Size constants the fixture literals reference (declared above the array).
const prelude = (src) => (src.match(/^const (?:GB|GiB) = .*;$/gm) ?? []).join('\n');

async function evalArray(literal, name, src = '') {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'marbor-signals-'));
  const file = path.join(dir, `${name}.ts`);
  fs.writeFileSync(file, `${prelude(src)}\nexport const nodes = ${literal};\n`);
  try {
    return (await import(pathToFileURL(file).href)).nodes;
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

function sliceArray(src, startMarker, endMarker) {
  const start = src.indexOf(startMarker);
  assert.ok(start >= 0, `marker not found: ${startMarker}`);
  const open = src.indexOf('[', start + startMarker.length - 1);
  const end = src.indexOf(endMarker, open);
  assert.ok(end > open, `end marker not found: ${endMarker}`);
  return src.slice(open, end + endMarker.length);
}

const fixtures = {
  'mockData.ts mockGPUNodes': async () => {
    const src = read('src/lib/mockData.ts');
    return evalArray(sliceArray(src, 'export const mockGPUNodes: GPUNode[] = [', '\n];'), 'mock', src);
  },
};

for (const [label, load] of Object.entries(fixtures)) {
  test(`${label}: every node's signals match the documented rules`, async () => {
    const nodes = await load();
    assert.ok(nodes.length > 0, 'no fixtures found');
    for (const n of nodes) {
      assert.ok(Array.isArray(n.signals), `${n.name} has no signals array`);
      assert.deepEqual(n.signals, expectedSignals(n), `${n.name} signals`);
      for (const id of n.signals) assert.ok(CANONICAL.includes(id), `${n.name}: unknown signal ${id}`);
    }
  });
}

async function loadWithDemoSignals() {
  const src = read('src/lib/mockData.ts');
  const start = src.indexOf('export function withDemoSignals');
  assert.ok(start >= 0, 'withDemoSignals not found');
  const end = src.indexOf('\n}\n', start) + 3;
  const fnSrc = src.slice(start, end).replace('GPUNode', 'any').replace('GPUNode', 'any');
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'marbor-signals-'));
  const file = path.join(dir, 'fn.ts');
  fs.writeFileSync(file, fnSrc);
  try {
    return (await import(pathToFileURL(file).href)).withDemoSignals;
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

const base = { health: 'healthy', draining: false, agentPresent: false, loadedModels: [], vramSource: 'nvidia', vramTotalMB: 10000, vramUsedMB: 1000, signals: [] };

test('withDemoSignals (the demo-only recompute after a local action) agrees with every fixture', async () => {
  const withDemoSignals = await loadWithDemoSignals();
  const nodes = await fixtures['mockData.ts mockGPUNodes']();
  for (const n of nodes) {
    assert.deepEqual(withDemoSignals({ ...n, signals: [] }).signals, n.signals, n.name);
  }
});

test('withDemoSignals: drain then undrain restores healthy', async () => {
  const f = await loadWithDemoSignals();
  assert.deepEqual(f(base).signals, ['healthy']);
  assert.deepEqual(f({ ...base, draining: true }).signals, ['draining']);
  assert.deepEqual(f(f({ ...base, draining: true })).signals, ['draining']);
  assert.deepEqual(f({ ...base, draining: false }).signals, ['healthy']);
});

test('withDemoSignals: agent toggling adds and removes agent', async () => {
  const f = await loadWithDemoSignals();
  assert.deepEqual(f({ ...base, agentPresent: true }).signals, ['healthy', 'agent']);
  assert.deepEqual(f({ ...base, agentPresent: false }).signals, ['healthy']);
});

test('withDemoSignals: unloading the last model removes warm', async () => {
  const f = await loadWithDemoSignals();
  const warm = { ...base, loadedModels: [{ name: 'm' }] };
  assert.deepEqual(f(warm).signals, ['healthy', 'warm']);
  assert.deepEqual(f({ ...warm, loadedModels: [] }).signals, ['healthy']);
});

test('withDemoSignals: pressure starts at exactly 90% of VRAM', async () => {
  const f = await loadWithDemoSignals();
  assert.ok(f({ ...base, vramUsedMB: 9000 }).signals.includes('pressure'), 'exactly 0.9');
  assert.ok(!f({ ...base, vramUsedMB: 8999 }).signals.includes('pressure'), 'just below 0.9');
});

test('withDemoSignals: unknown VRAM never shows pressure', async () => {
  const f = await loadWithDemoSignals();
  assert.ok(!f({ ...base, vramSource: 'none', vramUsedMB: 10000 }).signals.includes('pressure'), 'vramSource none');
  assert.ok(!f({ ...base, vramTotalMB: 0, vramUsedMB: 500 }).signals.includes('pressure'), 'zero total');
});
