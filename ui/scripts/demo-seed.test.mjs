// Run with: npm test. One seed, derived views: the demo fleet's numbers must
// agree across Models, GPU Nodes, requests and analytics. mockData.ts is
// compiled with vite's transformWithOxc into a temp folder and imported, so the
// checks run against the real fixtures rather than their source text.
import test, { before, after } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, writeFileSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';

const GiB = 1024 ** 3;
let root;
let m;

before(async () => {
  const { transformWithOxc } = await import('vite');
  root = mkdtempSync(join(tmpdir(), 'demo-seed-test-'));
  const src = readFileSync(new URL('../src/lib/mockData.ts', import.meta.url), 'utf8');
  const out = await transformWithOxc(src, 'mockData.ts');
  const file = join(root, 'mockData.mjs');
  writeFileSync(file, out.code);
  m = await import(pathToFileURL(file).href);
});
after(() => { if (root) rmSync(root, { recursive: true, force: true }); });

const catalogNames = () => new Set(m.mockModelCatalog.models.map((x) => x.name));

test('each model warm count and VRAM total are derived from its warm nodes', () => {
  for (const model of m.mockModelCatalog.models) {
    const warm = (model.nodes ?? []).filter((n) => n.warm);
    assert.equal(model.warm_count, warm.length, `${model.name} warm_count`);
    const sum = warm.reduce((s, n) => s + (n.vram_bytes ?? model.size_vram), 0);
    assert.equal(model.total_vram_bytes ?? 0, sum, `${model.name} total VRAM`);
    if (warm.length === 0) assert.equal(model.total_vram_bytes ?? 0, 0, `${model.name} is cold so it holds no VRAM`);
  }
});

test('a node warm for a model has it loaded, itself or through its replica head', () => {
  for (const model of m.mockModelCatalog.models) {
    for (const entry of model.nodes ?? []) {
      if (!entry.warm) continue;
      const node = m.mockGPUNodes.find((n) => n.name === entry.name);
      assert.ok(node, `${entry.name} exists`);
      const holder = (node.loadedModels ?? []).some((l) => l.name === model.name)
        ? node
        : m.mockGPUNodes.find((n) => n.name === node.replicaHead);
      assert.ok(holder && (holder.loadedModels ?? []).some((l) => l.name === model.name), `${model.name} is loaded behind ${entry.name}`);
    }
  }
});

test('models loaded on a node fit its VRAM and stay within the used figure', () => {
  for (const n of m.mockGPUNodes) {
    const loaded = (n.loadedModels ?? []).reduce((s, l) => s + l.sizeVram, 0);
    const usedBytes = n.vramUsedMB * 1024 * 1024;
    const totalBytes = n.vramTotalMB * 1024 * 1024;
    assert.ok(usedBytes <= totalBytes, `${n.name} used exceeds total`);
    assert.ok(loaded <= usedBytes + 0.2 * GiB, `${n.name}: ${(loaded / GiB).toFixed(1)} GiB loaded but ${(usedBytes / GiB).toFixed(1)} GiB used`);
  }
});

test('every model a node has loaded is in the fleet catalog', () => {
  const names = catalogNames();
  for (const n of m.mockGPUNodes) {
    for (const l of n.loadedModels ?? []) assert.ok(names.has(l.name), `${n.name} loads ${l.name}, missing from the catalog`);
  }
});

test('local requests, analytics rows and key allow-lists name fleet models', () => {
  const names = catalogNames();
  for (const r of m.mockRequests) {
    if (r.cloud || r.node === '' || r.node === 'none') continue;
    assert.ok(names.has(r.model), `local request for ${r.model} is not in the fleet`);
  }
  for (const row of m.mockAnalytics.by_model) assert.ok(names.has(row.model), `analytics lists ${row.model}, not in the fleet`);
  for (const key of m.mockAPIKeys) {
    for (const allowed of key.allowedModels ?? []) {
      if (allowed === 'all') continue;
      assert.ok(names.has(allowed), `${key.name} allows ${allowed}, not in the fleet`);
    }
  }
});
