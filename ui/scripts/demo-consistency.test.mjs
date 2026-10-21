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

test('demo shows an unlimited key, a skipped decision and UTC decision hours', () => {
  const keysStart = mock.indexOf('export const mockAPIKeys');
  assert.ok(keysStart >= 0, 'mockAPIKeys not found');
  const keysEnd = mock.indexOf('\n];', keysStart);
  assert.ok(keysEnd > keysStart, 'end of mockAPIKeys not found');
  const keys = mock.slice(keysStart, keysEnd);
  assert.ok(/rateLimit: 0,/.test(keys), 'no mock API key with rateLimit 0');
  const start = api.indexOf('export async function fetchPredictiveDecisions');
  assert.ok(start >= 0, 'fetchPredictiveDecisions not found');
  const apiFetchAt = api.indexOf('apiFetch', start);
  assert.ok(apiFetchAt > start, 'apiFetch call after fetchPredictiveDecisions not found');
  const body = api.slice(start, apiFetchAt);
  assert.ok(/was_already_warm: false,\s*warmup_triggered: false/.test(body), 'no skipped demo decision');
  assert.ok(!/\.getHours\(\)/.test(body), 'demo hour must use getUTCHours');
  assert.ok(/getUTCHours\(\)/.test(body), 'demo hour not UTC');
  const rows = body.split('\n').filter((l) => l.includes('predicted_model:'));
  assert.ok(rows.length > 0, 'no demo decision rows found');
  for (const row of rows) assert.ok(!/reason/i.test(row), 'demo decisions must not invent a skip reason');
});

test('the Soak Test demo key tells one story', () => {
  const m = mock.match(/name: 'Soak Test',\n\s*key: '([^']+)',[\s\S]*?rateLimit: (\d+),[\s\S]*?status: '(\w+)'/);
  assert.ok(m, 'Soak Test mock key not found');
  assert.ok(/soak/i.test(m[1]), 'Soak Test key string must look like a soak-test key');
  assert.equal(m[2], '0');
  assert.equal(m[3], 'active');
});

// Top-level exported api.ts functions as { name, text, wrapped }.
function apiExports() {
  const re = /^export (?:async function|function|const) (\w+)/gm;
  const marks = [...api.matchAll(re)];
  return marks.map((m, i) => {
    const chunk = api.slice(m.index, i + 1 < marks.length ? marks[i + 1].index : api.length);
    // Stop at the closing brace at column 0 so non-exported code between two
    // exports is not attributed to the earlier one.
    const close = chunk.search(/\n\}\)?;?\n/);
    const text = close >= 0 ? chunk.slice(0, close + 1) : chunk;
    return { name: m[1], text, wrapped: new RegExp(`^export const ${m[1]} = withSettingsReset\\(`).test(text) };
  });
}

test('every api.ts write that can change the settings payload resets the shared settings read', () => {
  const writes = /method: '(POST|PUT|PATCH|DELETE)'/;
  const touches = /\/(settings|model-aliases|cloud-providers|config\/reload|backup\/restore)\b/;
  const exports = apiExports();
  assert.ok(exports.length > 50, 'api.ts export scan found too few functions');
  const offenders = exports
    .filter((e) => writes.test(e.text) && touches.test(e.text) && !e.wrapped)
    .map((e) => e.name);
  assert.deepEqual(offenders, [], 'write functions missing withSettingsReset: ' + offenders.join(', '));
  for (const name of ['login', 'logout']) {
    const e = exports.find((x) => x.name === name);
    assert.ok(e, `${name} not found in api.ts`);
    assert.ok(e.text.includes('settingsInflight.reset()'), `${name} must reset the settings slot`);
  }
});

test('settingsInflight is declared before the functions that use it', () => {
  const decl = api.indexOf('const settingsInflight');
  assert.ok(decl >= 0, 'settingsInflight declaration not found');
  assert.ok(decl < api.indexOf('export async function login'), 'declare settingsInflight above login');
  assert.ok(decl < api.indexOf('function withSettingsReset'), 'declare settingsInflight above withSettingsReset');
});
