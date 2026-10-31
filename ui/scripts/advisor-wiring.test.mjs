// Run with: npm test. Source-text checks that ModelAdvisor.tsx wires the copy
// helpers to the right arguments and renders the unknown-VRAM note once above
// the Browse/Favourites switch. Works on source text because the page is a .tsx
// file with extensionless imports (same approach as demo-consistency.test.mjs).
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const read = (rel) => fs.readFileSync(path.join(here, '..', rel), 'utf8').replace(/\r\n/g, '\n');

const page = read('src/pages/ModelAdvisor.tsx');
const mock = read('src/lib/mockData.ts');

test('the unknown-VRAM note is gated on nodeVramUnknown and sits above the tab switch', () => {
  const noteAt = page.indexOf('{VRAM_UNKNOWN_NOTE}');
  const switchAt = page.search(/\{tab === 'favourites' \?/);
  assert.ok(noteAt > 0 && switchAt > 0, 'note or tab switch not found');
  assert.equal(page.split('{VRAM_UNKNOWN_NOTE}').length - 1, 1, 'note must render exactly once');
  assert.ok(noteAt < switchAt, 'note must render above the tab switch so both tabs show it');
  const gate = page.lastIndexOf('nodeVramUnknown(activeNode)', noteAt);
  assert.ok(gate > 0 && noteAt - gate < 400, 'note must be gated on nodeVramUnknown(activeNode)');
  assert.match(page.slice(noteAt, noteAt + 400), /<Link to="\/gpu-nodes"[^>]*>Go to GPU Nodes<\/Link>/);
});

test('the curated panel heading and the Browse copy use the shared helpers', () => {
  assert.match(page, /curatedFitTitle\(node\.name, vramUnknown, incompatible\)/);
  assert.match(page, /const vramUnknown = nodeVramUnknown\(node\)/);
  assert.match(page, /\{browseHelpClaim\(browseRuntime\)\}/);
  assert.match(page, /copy=\{browseEmptyCopy\(browseRuntime\)\}/);
  assert.match(page, /import \{[^}]*\bisGGUFSearch\b[^}]*\} from '\.\.\/lib\/advisorCopy'/);
  assert.ok(page.split('isGGUFSearch').length - 1 >= 2, 'isGGUFSearch must be imported and used, not just imported');
  assert.ok(!/function isGGUFRuntime|const isGGUFRuntime = \(/.test(page), 'no second GGUF rule may be defined in the page');
});

test('a failed search does not also show the empty-state copy', () => {
  const emptyAt = page.search(/models\.length === 0 \?/);
  assert.ok(emptyAt > 0, 'empty-results branch not found');
  assert.match(page.slice(emptyAt, emptyAt + 400), /searchError \? null :/);
});

test('unknown VRAM on a compatible runtime renders no curated header', () => {
  const fnAt = page.indexOf('function NodeFitList(');
  assert.ok(fnAt > 0, 'NodeFitList not found');
  const body = page.slice(fnAt);
  const guard = body.indexOf('if (vramUnknown && !incompatible) return null;');
  assert.ok(guard > 0, 'unknown-VRAM compatible case must return null');
  const headerAt = body.indexOf('aria-expanded');
  assert.ok(headerAt > guard, 'null return must come before the header button');
  assert.ok(!/disabled=\{!expandable\}|const expandable\b/.test(body), 'no disabled dead-end header');
  assert.equal((page.match(/<NodeFitList\s+key=\{activeNode\.name\}/g) ?? []).length, 2, 'both NodeFitList call sites must be keyed by node');
});

test('the banner node count is derived, not hardcoded', () => {
  assert.match(page, /Demo mode - \{mockModelCatalogResponse\.nodes\.length\} demo inference nodes/);
  assert.ok(!/Demo mode - \d+ inference nodes/.test(page));
});

test('every advisor demo node is on the GPU nodes page unless allowlisted', () => {
  // Advisor-only demo nodes, each with the reason it has no GPU nodes record.
  const allowlist = {};
  const start = mock.indexOf('const mockModelCatalogBase');
  assert.ok(start > 0, 'mockModelCatalogBase not found');
  const nodesAt = mock.indexOf('\n  nodes: [', start);
  const end = mock.indexOf('\n  ],\n};', nodesAt);
  const advisorNodes = [...mock.slice(nodesAt, end).matchAll(/\n\s+name: '([^']+)'/g)].map((m) => m[1]);
  assert.ok(advisorNodes.length > 0, 'no advisor catalog nodes found');
  const gpuStart = mock.indexOf('export const mockGPUNodes');
  assert.ok(gpuStart > 0, 'mockGPUNodes not found');
  const gpuEnd = mock.indexOf('\nexport ', gpuStart + 1);
  const gpuNodes = new Set([...mock.slice(gpuStart, gpuEnd).matchAll(/\bname: '(gpu-node-\d+)'/g)].map((m) => m[1]));
  assert.ok(gpuNodes.size > 0, 'no GPU nodes found');
  for (const [name, reason] of Object.entries(allowlist)) {
    assert.ok(reason.length > 0, `${name} needs a reason`);
  }
  for (const name of advisorNodes) {
    if (name in allowlist) continue;
    assert.ok(gpuNodes.has(name), `${name} is in the advisor catalog but not in mockGPUNodes`);
  }
});

test("the advisor lists exactly the fleet nodes", () => {
  assert.ok(mock.includes("nodes: mockGPUNodes.map(rec =>"));
});
