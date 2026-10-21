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
  const switchAt = page.indexOf("{tab === 'favourites' ? (");
  assert.ok(noteAt > 0 && switchAt > 0, 'note or tab switch not found');
  assert.equal(page.split('{VRAM_UNKNOWN_NOTE}').length - 1, 1, 'note must render exactly once');
  assert.ok(noteAt < switchAt, 'note must render above the tab switch so both tabs show it');
  const gate = page.lastIndexOf('nodeVramUnknown(activeNode)', noteAt);
  assert.ok(gate > 0 && noteAt - gate < 250, 'note must be gated on nodeVramUnknown(activeNode)');
  assert.match(page.slice(noteAt, noteAt + 250), /<Link to="\/gpu-nodes"[^>]*>Go to GPU nodes<\/Link>/);
});

test('the curated panel heading and the Browse copy use the shared helpers', () => {
  assert.match(page, /curatedFitTitle\(node\.name, vramUnknown, incompatible\)/);
  assert.match(page, /const vramUnknown = nodeVramUnknown\(node\)/);
  assert.match(page, /\{browseHelpClaim\(browseRuntime\)\}/);
  assert.match(page, /copy=\{browseEmptyCopy\(browseRuntime\)\}/);
  assert.match(page, /const isGGUFRuntime = isGGUFSearch;/);
});

test('a failed search does not also show the empty-state copy', () => {
  assert.match(page, /models\.length === 0 \? \(\s*\/\/[^\n]*\n[^\n]*\n\s*searchError \? null : \(/);
});

test('the banner node count is derived, not hardcoded', () => {
  assert.match(page, /Demo mode - \{mockModelCatalogResponse\.nodes\.length\} demo inference nodes/);
  assert.ok(!/Demo mode - \d+ inference nodes/.test(page));
});

test('every advisor demo node is on the GPU nodes page unless allowlisted', () => {
  // Advisor-only demo nodes, each with the reason it has no GPU nodes record.
  const allowlist = {
    'gpu-node-12': 'demo of a node added by URL with no VRAM declared and no agent, so fit cannot be computed',
  };
  const start = mock.indexOf('const mockModelCatalogBase');
  assert.ok(start > 0, 'mockModelCatalogBase not found');
  const nodesAt = mock.indexOf('\n  nodes: [', start);
  const end = mock.indexOf('\n  ],\n};', nodesAt);
  const advisorNodes = [...mock.slice(nodesAt, end).matchAll(/\n {6}name: '([^']+)'/g)].map((m) => m[1]);
  assert.ok(advisorNodes.length > 0, 'no advisor catalog nodes found');
  const gpuNodes = new Set([...mock.matchAll(/\n {4}name: '(gpu-node-\d+)'/g)].map((m) => m[1]));
  for (const name of advisorNodes) {
    if (name in allowlist) {
      assert.ok(allowlist[name].length > 0);
      continue;
    }
    assert.ok(gpuNodes.has(name), `${name} is in the advisor catalog but not in mockGPUNodes`);
  }
});
