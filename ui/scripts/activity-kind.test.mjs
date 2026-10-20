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
  olderEventsMayMatch,
  getActivityKindLabel,
  classifyAction,
  EXACT_ACTION_KINDS,
  ALL_ACTIVITY_KINDS,
  LOGIN_FAILURE,
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

test('the exact table is consulted before any prefix or substring fallback', () => {
  // These would hit a fallback rule (_agent, scheduled_, warmup) if the table were not first.
  const crafted = {
    custom_agent_thing: 'keys',
    scheduled_special: 'users',
    my_warmup_audit: 'access',
  };
  assert.equal(classifyAction('custom_agent_thing', crafted), 'keys');
  assert.equal(classifyAction('scheduled_special', crafted), 'users');
  assert.equal(classifyAction('my_warmup_audit', crafted), 'access');
  // Without a table entry the same actions fall back as before.
  assert.equal(classifyAction('custom_agent_thing', {}), 'agent');
  assert.equal(classifyAction('scheduled_special', {}), 'schedule');
  assert.equal(classifyAction('my_warmup_audit', {}), 'warmup');
  // The real table drives toActivityKind.
  for (const [action, kind] of Object.entries(EXACT_ACTION_KINDS)) assert.equal(toActivityKind(action), kind, action);
});

test('toActivityKind tolerates a missing action and inherited object keys', () => {
  assert.equal(toActivityKind(''), 'config');
  assert.equal(toActivityKind(undefined), 'config');
  assert.equal(toActivityKind('constructor'), 'config');
  assert.equal(toActivityKind('toString'), 'config');
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

// mockData.ts and api.ts cannot be imported under plain node (extensionless
// imports), so the demo rows are read from the source and run through the real helpers.
function rowsIn(src) {
  const rows = [];
  for (const line of src.split('\n')) {
    const m = line.match(/username: '([^']*)', action: '([^']*)', target: '[^']*', details: '([^']*)'/);
    if (m) rows.push({ username: m[1], action: m[2], details: m[3] });
  }
  return rows;
}
const mockSrc = read('src/lib/mockData.ts');
const apiSrc = read('src/lib/api.ts');
const mockRows = rowsIn(mockSrc.slice(mockSrc.indexOf('export const mockActivityEntries')));
const apiRows = rowsIn(apiSrc.slice(apiSrc.indexOf('export async function fetchSystemAuditFiltered')));

for (const [name, rows] of [['mockActivityEntries', mockRows], ['api.ts DEMO', apiRows]]) {
  test(`${name} shows sign-ins, a failed sign-in, key changes and case variants`, () => {
    assert.ok(rows.length > 0, 'no rows parsed');
    const kinds = new Set(rows.map((r) => toActivityKind(r.action)));
    for (const k of ['access', 'keys']) assert.ok(kinds.has(k), k);
    const failed = rows.filter((r) => isFailedLogin(r.action));
    assert.ok(failed.length >= 2, 'a failed pair');
    for (const r of failed) assert.equal(r.details, '', 'real failed logins have empty details');
    assert.ok(rows.some((r) => r.action === 'add_key') && rows.some((r) => r.action === 'revoke_key' || r.action === 'patch_key'));
    assert.ok(rows.some((r) => r.username === 'Admin') && rows.some((r) => r.username === 'admin'));
    const ops = distinctOperators(rows.map((r) => r.username));
    assert.equal(ops.filter((o) => o.toLowerCase() === 'admin').length, 1, 'case variants collapse');
  });
}

test('the add_key demo rows match between api.ts and mockActivityEntries', () => {
  const keys = (rows) => rows.filter((r) => r.action === 'add_key').map((r) => `${r.username}|${r.details}`).sort();
  assert.deepEqual(keys(mockRows), keys(apiRows));
});

test('distinctOperators skips blank and missing names', () => {
  assert.deepEqual(distinctOperators(['', '  ', null, undefined, 'dana.rao']), ['dana.rao']);
});

test('kind labels for the sub-kinds', () => {
  assert.equal(getActivityKindLabel('access'), 'Access');
  assert.equal(getActivityKindLabel('keys'), 'Keys');
  assert.equal(getActivityKindLabel('users'), 'Users');
});

test('the login failure constant is shared', () => {
  assert.equal(LOGIN_FAILURE, 'login_failure');
  assert.equal(isFailedLogin(LOGIN_FAILURE), true);
  assert.equal(toActivityKind(LOGIN_FAILURE), 'access');
  const src = read('src/lib/activityKind.ts').replace("export const LOGIN_FAILURE = 'login_failure';", '');
  assert.doesNotMatch(src, /'login_failure'/);
});

test('all kinds are listed, so the Kind filter cannot miss one', () => {
  const expected = ['access', 'keys', 'users', 'config', 'drain', 'agent', 'runtime', 'node', 'warmup', 'schedule', 'predictive'];
  assert.deepEqual([...ALL_ACTIVITY_KINDS].sort(), expected.sort());
  assert.match(read('src/pages/Activity.tsx'), /KIND_OPTIONS[^=]*= \['all', \.\.\.ALL_ACTIVITY_KINDS\]/);
});

test('config rows are not hidden by default', () => {
  for (const a of ['update_settings', 'some_future_action', 'delete_model_replica']) {
    assert.equal(matchesKindFilter(a, 'all'), true, a);
  }
  const page = read('src/pages/Activity.tsx');
  assert.doesNotMatch(page, /kindFilter\s*===\s*'all'[\s\S]{0,200}?'config'/);
  assert.doesNotMatch(page, /'config'[\s\S]{0,200}?kindFilter\s*===\s*'all'/);
  assert.doesNotMatch(page, /kind\s*[!=]==\s*'config'/);
});

test('predictive passes through matchesKindFilter and never needs older events', () => {
  assert.equal(matchesKindFilter('anything', 'predictive'), true);
  assert.equal(olderEventsMayMatch('predictive', '', true), false);
  assert.equal(olderEventsMayMatch('predictive', '', false), false);
});

test('olderEventsMayMatch needs a client narrowing and unloaded events', () => {
  for (const k of ['access', 'keys', 'users', 'config']) {
    assert.equal(olderEventsMayMatch(k, '', true), true, k);
    assert.equal(olderEventsMayMatch(k, '', false), false, k);
  }
  assert.equal(olderEventsMayMatch('all', 'dana', true), true);
  assert.equal(olderEventsMayMatch('all', 'dana', false), false);
  // Server-side filters alone never need it.
  for (const k of ['all', 'drain', 'agent', 'runtime', 'node', 'warmup', 'schedule']) {
    assert.equal(olderEventsMayMatch(k, '  ', true), false, k);
  }
});

test('distinctOperators trims names before comparing', () => {
  assert.deepEqual(distinctOperators(['admin ', 'admin', ' Admin']), ['admin']);
});

test('the selected operator listed first wins casing', () => {
  assert.deepEqual(distinctOperators(['Admin', 'admin', 'dana']), ['Admin', 'dana']);
});

test('Activity page footer and filters behave as specified', () => {
  const page = read('src/pages/Activity.tsx');
  // One Load more when the empty state already offers it.
  assert.match(page, /hasMore && !\(filteredEntries\.length === 0 && olderMayMatch\)/);
  assert.doesNotMatch(page, /load more\./i);
  // Live regions for the changing messages.
  assert.match(page, /role=\{filteredEntries\.length > 0 \? 'status' : undefined\}/);
  assert.match(page, /\blive\n\s*copy=/);
  // Clear filters sits outside the live region.
  assert.match(page, /<\/span>\s*\{hasActiveFilters && \(\s*<button[^>]*onClick=\{clearAllFilters\}/);
  // The selected operator is listed first so its casing wins.
  assert.match(page, /distinctOperators\(\[userFilter === 'all' \? null : userFilter, \.\.\.entries/);
  // The selected operator stays an option; the list is sorted with localeCompare.
  assert.match(page, /localeCompare/);
  // Failed sign-ins are not operators.
  assert.match(page, /filter\(\(e\) => !isFailedLogin\(e\.action\)\)/);
  assert.match(page, /\{decisions\.length\} predictive decisions \(all loaded\)/);
  assert.match(page, /ignoring case, excluding failed sign-ins/);
  assert.match(page, /olderEventsMayMatch\(kindFilter, searchQuery, hasMore\)/);
});
