// Run with: npm test. Guards the Activity page kind taxonomy: sign-ins, key
// changes and user changes get their own kinds, the server-side kind they map
// to stays "config", and the demo fixtures carry the events that show it.
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import {
  toActivityKind,
  serverKindFor,
  isFailedLogin,
  matchesKindFilter,
  distinctOperators,
  isClientNarrowed,
  getActivityKindLabel,
} from '../src/lib/activityKind.ts';

const here = path.dirname(fileURLToPath(import.meta.url));
const read = (rel) => fs.readFileSync(path.join(here, '..', rel), 'utf8').replace(/\r\n/g, '\n');

const ACCESS = ['login_success', 'login_failure'];
const KEYS = ['add_key', 'patch_key', 'revoke_key', 'rotate_key', 'api_key_expiry_malformed'];
const USERS = ['create_user', 'patch_user', 'delete_user', 'suspend_user', 'approve_user', 'reset_user_password'];

test('sign-in, key and user actions map to their own kinds', () => {
  for (const a of ACCESS) assert.equal(toActivityKind(a), 'access', a);
  for (const a of KEYS) assert.equal(toActivityKind(a), 'keys', a);
  for (const a of USERS) assert.equal(toActivityKind(a), 'users', a);
});

test('existing kinds and the settings fallback are unchanged', () => {
  assert.equal(toActivityKind('drain_node'), 'drain');
  assert.equal(toActivityKind('enable_marbor_agent'), 'agent');
  assert.equal(toActivityKind('runtime_restart'), 'runtime');
  assert.equal(toActivityKind('add_node'), 'node');
  assert.equal(toActivityKind('pull_model'), 'warmup');
  assert.equal(toActivityKind('scheduled_warmup'), 'schedule');
  assert.equal(toActivityKind('update_settings'), 'config');
  assert.equal(toActivityKind('some_future_action'), 'config');
  assert.equal(getActivityKindLabel('config'), 'Settings');
});

test('there is no logout mapping', () => {
  assert.equal(toActivityKind('logout'), 'config');
  assert.equal(toActivityKind('login_something_new'), 'config');
});

test('exact access/keys/users matches are checked before the catch-all fallbacks', () => {
  const src = read('src/lib/activityKind.ts');
  const exact = src.indexOf("return 'access'");
  const fallback = src.indexOf('Fallback prefix checks');
  assert.ok(exact > 0 && fallback > 0, 'markers not found');
  assert.ok(exact < fallback, 'exact matches must come before the fallbacks');
  assert.ok(src.indexOf("return 'keys'") < fallback);
  assert.ok(src.indexOf("return 'users'") < fallback);
});

test('serverKindFor sends config-derived kinds to the server as config', () => {
  for (const k of ['access', 'keys', 'users', 'config']) assert.equal(serverKindFor(k), 'config', k);
  for (const k of ['drain', 'agent', 'runtime', 'node', 'warmup', 'schedule', 'predictive']) {
    assert.equal(serverKindFor(k), k, k);
  }
});

test('every sub-kind action stays in the server config bucket', () => {
  for (const a of [...ACCESS, ...KEYS, ...USERS]) {
    assert.equal(serverKindFor(toActivityKind(a)), 'config', a);
  }
});

test('isFailedLogin flags only login_failure', () => {
  assert.equal(isFailedLogin('login_failure'), true);
  assert.equal(isFailedLogin('login_success'), false);
  assert.equal(isFailedLogin('add_key'), false);
});

test('matchesKindFilter narrows config-derived kinds on the client', () => {
  assert.equal(matchesKindFilter('login_failure', 'all'), true);
  assert.equal(matchesKindFilter('drain_node', 'all'), true);
  assert.equal(matchesKindFilter('login_success', 'access'), true);
  assert.equal(matchesKindFilter('add_key', 'access'), false);
  assert.equal(matchesKindFilter('add_key', 'keys'), true);
  assert.equal(matchesKindFilter('create_user', 'users'), true);
  assert.equal(matchesKindFilter('create_user', 'keys'), false);
});

test('Settings keeps everything in the server config bucket that is not access/keys/users', () => {
  assert.equal(matchesKindFilter('update_settings', 'config'), true);
  assert.equal(matchesKindFilter('login_failure', 'config'), false);
  assert.equal(matchesKindFilter('add_key', 'config'), false);
  assert.equal(matchesKindFilter('create_user', 'config'), false);
  // The server files this under config although the UI badges it Warmup.
  assert.equal(matchesKindFilter('delete_model_replica', 'config'), true);
});

test('other kinds pass through because the server already filtered them', () => {
  assert.equal(matchesKindFilter('drain_node', 'drain'), true);
  assert.equal(matchesKindFilter('anything', 'warmup'), true);
});

test('distinctOperators ignores case and keeps first-seen casing', () => {
  assert.equal(distinctOperators(['admin', 'Admin', 'dana.rao']).length, 2);
  assert.deepEqual(distinctOperators(['Admin', 'admin', 'dana.rao']), ['Admin', 'dana.rao']);
  assert.deepEqual(distinctOperators([]), []);
});

test('isClientNarrowed is true for sub-kinds or local search text only', () => {
  for (const k of ['access', 'keys', 'users', 'config']) assert.equal(isClientNarrowed(k, ''), true, k);
  assert.equal(isClientNarrowed('all', 'dana'), true);
  assert.equal(isClientNarrowed('all', '   '), false);
  assert.equal(isClientNarrowed('drain', ''), false);
  assert.equal(isClientNarrowed('all', ''), false);
});

test('demo audit fixtures show sign-ins, a failed sign-in, key changes and case variants', () => {
  const api = read('src/lib/api.ts');
  const mock = read('src/lib/mockData.ts');
  for (const [name, src] of [['api.ts', api], ['mockData.ts', mock]]) {
    assert.match(src, /action: 'login_success'/, `${name} login_success`);
    assert.match(src, /action: 'login_failure'/, `${name} login_failure`);
    assert.match(src, /action: 'add_key'/, `${name} add_key`);
    assert.match(src, /action: '(?:revoke_key|patch_key)'/, `${name} revoke/patch key`);
    assert.match(src, /username: 'Admin'/, `${name} Admin variant`);
    assert.match(src, /username: 'admin'/, `${name} admin`);
  }
});
