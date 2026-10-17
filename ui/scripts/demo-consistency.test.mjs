// Run with: npm test. Cross-view checks on the demo fixtures and the homepage
// so a fix that lands in one place cannot silently miss its sibling. Works on
// source text because the fixtures are .ts files with extensionless imports.
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const read = (rel) => fs.readFileSync(path.join(here, '..', rel), 'utf8').replace(/\r\n/g, '\n');

const mock = read('src/lib/mockData.ts');
const api = read('src/lib/api.ts');
const home = read('../website/index.html');

// Model blocks of the demo catalog: { name, digestMismatch }.
function catalogModels() {
  const start = mock.indexOf('export const mockModelCatalog');
  const end = mock.indexOf('].map(m =>', start);
  const body = mock.slice(start, end);
  return body.split(/\n    \{\n      name: '/).slice(1).map((blk) => ({
    name: blk.slice(0, blk.indexOf("'")),
    digestMismatch: /digest_mismatch: true/.test(blk),
  }));
}

test('homepage checklist names the demo dashboard warm model', () => {
  const m = home.match(/data-done="([^"]+) is warm on your fleet\./);
  assert.ok(m, 'homepage checklist line not found');
  assert.equal(m[1], catalogModels()[0].name);
  // The visible text is a separate copy of the data-done attribute.
  assert.ok(!/>llama3\.2 is warm/.test(home), 'stale visible warm-model text');
  assert.ok(home.includes(`>${m[1]} is warm on your fleet.<`), 'visible text differs from data-done');
});

test('keep-warm notices name models that really have a digest mismatch', () => {
  const drifted = new Set(catalogModels().filter((m) => m.digestMismatch).map((m) => m.name));
  const notices = [...mock.matchAll(/warmupWarnings: \{ '([^']+)': "digest drift/g)].map((x) => x[1]);
  assert.ok(notices.length > 0);
  for (const model of notices) assert.ok(drifted.has(model), `${model} is not drifted in the catalog`);
});

test('demo users reference API keys from the shared key list', () => {
  const lits = [...api.matchAll(/api_key_name: '([^']*)'/g)].map((x) => x[1]).filter(Boolean);
  assert.deepEqual(lits, [], 'hardcoded key names: ' + lits.join(', '));
});

test('no test-scaffolding names or raw runtime ids in demo copy', () => {
  assert.ok(!/fake-/.test(mock.replace(/no-fake-data/g, '')), 'fake- name in mockData');
  assert.ok(!/auto-detected as llamacpp/.test(mock), 'raw llamacpp in user-facing hint');
});
