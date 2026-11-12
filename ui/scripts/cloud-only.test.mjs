import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import {
  MAX_ENTRIES, MAX_ENTRY_LEN, providerStatus, addEntry, removeEntry,
  matchesKnownModel, shouldWarnNoMatch, writeBlocked,
} from '../src/lib/cloudOnlyView.ts';
import { deepMerge, serverErrorText } from '../src/lib/settingsMerge.ts';

test('limits mirror the server', () => {
  assert.equal(MAX_ENTRIES, 200);
  assert.equal(MAX_ENTRY_LEN, 256);
});

test('providerStatus is unknown until provider and LiteLLM data are ready', () => {
  const on = [{ enabled: true }];
  assert.equal(providerStatus({ providersReady: false, liteLLMUsable: true, providers: on }), 'unknown');
  assert.equal(providerStatus({ providersReady: true, liteLLMUsable: null, providers: on }), 'unknown');
  assert.equal(providerStatus({ providersReady: false, liteLLMUsable: null, providers: [] }), 'unknown');
});

test('providerStatus: usable via LiteLLM or an enabled provider, none otherwise', () => {
  assert.equal(providerStatus({ providersReady: true, liteLLMUsable: true, providers: [] }), 'usable');
  assert.equal(providerStatus({ providersReady: true, liteLLMUsable: false, providers: [{ enabled: true }] }), 'usable');
  assert.equal(providerStatus({ providersReady: true, liteLLMUsable: false, providers: [{ enabled: false }, {}] }), 'none');
  assert.equal(providerStatus({ providersReady: true, liteLLMUsable: false, providers: [] }), 'none');
});

test('providerStatus goes back to unknown on a refetch or a failure', () => {
  const p = [{ enabled: true }];
  assert.equal(providerStatus({ providersReady: true, liteLLMUsable: false, providers: p }), 'usable');
  assert.equal(providerStatus({ providersReady: false, liteLLMUsable: false, providers: p }), 'unknown');
});

test('addEntry trims and appends without changing the input', () => {
  const list = ['a'];
  const r = addEntry(list, '  qwen*  ');
  assert.deepEqual(r, { list: ['a', 'qwen*'] });
  assert.deepEqual(list, ['a']);
});

test('addEntry rules', () => {
  assert.ok('error' in addEntry([], '   '));
  assert.ok('error' in addEntry([], 'has space'));
  assert.ok('error' in addEntry([], '*'));
  assert.ok('error' in addEntry([], 'a*b'));
  assert.ok('error' in addEntry([], '**'));
  assert.ok('list' in addEntry([], 'ab*'));
  assert.ok('error' in addEntry([], 'x'.repeat(MAX_ENTRY_LEN + 1)));
  assert.ok('list' in addEntry([], 'x'.repeat(MAX_ENTRY_LEN)));
});

test('addEntry refuses a case-folded duplicate and a full list', () => {
  assert.ok('error' in addEntry(['Llama3'], 'llama3'));
  const full = Array.from({ length: MAX_ENTRIES }, (_, i) => `m${i}`);
  assert.ok('error' in addEntry(full, 'extra'));
  assert.ok('list' in addEntry(full.slice(1), 'extra'));
});

test('removeEntry removes ignoring case and keeps order', () => {
  assert.deepEqual(removeEntry(['a', 'B', 'c'], 'b'), ['a', 'c']);
  assert.deepEqual(removeEntry(['a'], 'zzz'), ['a']);
});

test('matchesKnownModel: exact with the latest rule, prefix by startsWith, case-folded', () => {
  const known = ['llama3.3:70b', 'qwen2.5:14b', 'mistral:latest'];
  assert.equal(matchesKnownModel('LLAMA3.3:70B', known), true);
  assert.equal(matchesKnownModel('mistral', known), true);
  assert.equal(matchesKnownModel('llama3.3', known), false);
  assert.equal(matchesKnownModel('qwen*', known), true);
  assert.equal(matchesKnownModel('gemma*', known), false);
  assert.equal(matchesKnownModel('nope', known), false);
});

test('shouldWarnNoMatch is silent while the model list is empty or loading', () => {
  assert.equal(shouldWarnNoMatch('anything', []), false);
  assert.equal(shouldWarnNoMatch('anything', ['x']), true);
  assert.equal(shouldWarnNoMatch('x', ['x']), false);
});

test('writeBlocked: saving blocks; unreadable needs the acknowledgement', () => {
  assert.equal(writeBlocked({ unreadable: false, acknowledged: false, saving: false }), false);
  assert.equal(writeBlocked({ unreadable: false, acknowledged: false, saving: true }), true);
  assert.equal(writeBlocked({ unreadable: true, acknowledged: false, saving: false }), true);
  assert.equal(writeBlocked({ unreadable: true, acknowledged: true, saving: false }), false);
  assert.equal(writeBlocked({ unreadable: true, acknowledged: true, saving: true }), true);
});

test('deepMerge keeps sibling keys of a nested partial and replaces arrays', () => {
  const base = { routing: { a: 1, local_degradation_chains: { x: ['y'] }, cloud_only_models: ['p'] }, timezone: 'UTC' };
  const out = deepMerge(base, { routing: { local_degradation_chains: { z: ['w'] }, cloud_only_models: ['q'] } });
  assert.deepEqual(out, { routing: { a: 1, local_degradation_chains: { x: ['y'], z: ['w'] }, cloud_only_models: ['q'] }, timezone: 'UTC' });
  assert.deepEqual(base.routing.cloud_only_models, ['p']);
});

test('serverErrorText shows the server message verbatim, with a fallback', () => {
  assert.equal(serverErrorText({ error: 'the stored list could not be read' }, 'x'), 'the stored list could not be read');
  assert.equal(serverErrorText({}, 'Failed to update settings'), 'Failed to update settings');
  assert.equal(serverErrorText(null, 'f'), 'f');
  assert.equal(serverErrorText({ error: '  ' }, 'f'), 'f');
});

test('demo cloud-only api paths never touch demo_settings or the live API', () => {
  const api = fs.readFileSync(new URL('../src/lib/api.ts', import.meta.url), 'utf8').replace(/\r\n/g, '\n');
  for (const fn of ['fetchCloudOnlySettings', 'addCloudOnlyModel', 'removeCloudOnlyModel']) {
    const start = api.indexOf(`function ${fn}`);
    assert.ok(start > 0, `${fn} missing`);
    const demoIdx = api.indexOf('if (DEMO)', start);
    assert.ok(demoIdx > 0 && demoIdx - start < 400, `${fn} must branch to demo first`);
    const body = api.slice(demoIdx, api.indexOf('\n  }\n', demoIdx));
    assert.ok(!body.includes('demo_settings') && !body.includes('apiFetch') && !body.includes('updateSettings'), `${fn} demo path must use the mock store only`);
  }
});

test('request reason labels carry both cloud-only reasons', () => {
  const src = fs.readFileSync(new URL('../src/components/RequestLogParts.tsx', import.meta.url), 'utf8');
  assert.ok(src.includes("cloud_only_policy: 'Cloud-only model'"));
  assert.ok(src.includes("cloud_only_list_unreadable: 'Cloud list unreadable'"));
});
