import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, mkdirSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { parseKeyForm, parseNodeForm, parseUserForm } from '../src/lib/formValidation.ts';
import { formReducer, initialFormState, createSubmitGuard, nextFocusState, isFocusIdle } from '../src/lib/formState.ts';

const require = createRequire(import.meta.url);
const src = (p) => new URL(`../src/${p}`, import.meta.url);

// FormError is TSX, which node cannot import directly, so it (and the helper
// it imports) is compiled with vite's public transformWithOxc into a scratch
// folder under node_modules and rendered on the server.
async function loadFormError() {
  const { transformWithOxc } = await import('vite');
  const dir = new URL('../node_modules/.cache/form-errors-test/', import.meta.url);
  mkdirSync(dir, { recursive: true });
  const state = await transformWithOxc(readFileSync(src('lib/formState.ts'), 'utf8'), 'formState.ts');
  writeFileSync(new URL('formState.mjs', dir), state.code);
  const view = await transformWithOxc(readFileSync(src('components/FormError.tsx'), 'utf8'), 'FormError.tsx', {
    jsx: { runtime: 'automatic' },
  });
  writeFileSync(new URL('FormError.mjs', dir), view.code.replace('../lib/formState', './formState.mjs'));
  return (await import(new URL('FormError.mjs', dir).href)).FormError;
}

test('parseKeyForm trims the name and parses the rate limit once', () => {
  const ok = parseKeyForm('  prod  ', ' 1000 ');
  assert.deepEqual(ok, { errors: [], invalid: [], values: { name: 'prod', rateLimit: 1000 } });
  assert.equal(parseKeyForm('prod', '0').values.rateLimit, 0);
});

test('parseKeyForm reports every problem with the offending field', () => {
  const bad = parseKeyForm('   ', 'x');
  assert.equal(bad.errors.length, 2);
  assert.deepEqual(bad.invalid, ['name', 'rateLimit']);
  assert.deepEqual(parseKeyForm('a', '1000').invalid, []);
});

test('parseKeyForm rejects anything that is not a whole non-negative number', () => {
  for (const v of ['12abc', '1.5', '-1', '', ' ', '1e3', '+5', '99999999999999999999']) {
    const r = parseKeyForm('a', v);
    assert.deepEqual(r.invalid, ['rateLimit'], `"${v}" should be rejected`);
    assert.ok(Number.isNaN(r.values.rateLimit));
  }
});

test('parseNodeForm trims and names each missing field separately', () => {
  assert.deepEqual(parseNodeForm(' gpu-1 ', ' 10.0.0.1 '), { errors: [], invalid: [], values: { name: 'gpu-1', host: '10.0.0.1' } });
  assert.deepEqual(parseNodeForm('', '').errors, ['Node name is required', 'Host is required']);
  assert.deepEqual(parseNodeForm('n', '  ').invalid, ['host']);
});

test('parseUserForm trims and requires a username', () => {
  assert.deepEqual(parseUserForm('  ann ').values, { username: 'ann' });
  assert.deepEqual(parseUserForm(' ').errors, ['Username is required']);
  assert.deepEqual(parseUserForm(' ').invalid, ['username']);
});

test('formReducer: a failed submit shows errors and counts the attempt without touching busy', () => {
  let s = formReducer(initialFormState, { type: 'busy', value: true });
  s = formReducer(s, { type: 'fail', errors: ['Name is required'], invalid: ['name'] });
  assert.deepEqual(s, { errors: ['Name is required'], invalid: ['name'], attempt: 1, busy: true });
  s = formReducer(s, { type: 'busy', value: false });
  assert.equal(s.busy, false);
  s = formReducer(s, { type: 'fail', errors: ['Name is required'], invalid: ['name'] });
  assert.equal(s.attempt, 2, 'an identical error is a new attempt so it is announced again');
});

test('formReducer: any edit clears the error and keeps an in-flight submit busy', () => {
  let s = formReducer(initialFormState, { type: 'fail', errors: ['x'], invalid: ['name'] });
  s = formReducer(s, { type: 'busy', value: true });
  s = formReducer(s, { type: 'edit' });
  assert.deepEqual([s.errors, s.invalid, s.busy], [[], [], true]);
  assert.equal(formReducer(s, { type: 'edit' }), s, 'editing with no error changes nothing');
});

test('formReducer: opening or closing the modal resets everything', () => {
  const dirty = { errors: ['x'], invalid: ['name'], attempt: 3, busy: true };
  assert.deepEqual(formReducer(dirty, { type: 'reset' }), initialFormState);
});

test('submit guard: re-entry is refused until the first submit finishes', () => {
  const g = createSubmitGuard();
  const t = g.begin();
  assert.equal(t, 0);
  assert.equal(g.begin(), null, 'a second click while in flight is ignored');
  assert.equal(g.finish(t), true);
  assert.equal(g.begin(), 0, 'free again after finish');
});

test('submit guard: reset makes the outstanding submit stale and clears busy', () => {
  const g = createSubmitGuard();
  const t = g.begin();
  g.reset(); // modal closed or reopened
  assert.equal(g.finish(t), false, 'a late result must not touch the form');
  const t2 = g.begin();
  assert.equal(t2, 1, 'epoch moved on');
  assert.equal(g.finish(t), false, 'the old token stays stale and does not release the new submit');
  assert.equal(g.begin(), null);
  assert.equal(g.finish(t2), true);
});

test('focus moves once per failed submit and never on a plain re-render', () => {
  let last = 0;
  const step = (attempt, hasErrors) => { const n = nextFocusState(last, attempt, hasErrors); last = n.last; return n.focus; };
  assert.equal(step(0, false), false, 'nothing showing');
  assert.equal(step(1, true), true, 'first failed submit focuses');
  assert.equal(step(1, true), false, 're-render (polling) must not steal focus');
  assert.equal(step(1, true), false);
  assert.equal(step(2, true), true, 'the same error on a second failed submit focuses again');
  assert.equal(step(2, false), false, 'edit clears the error');
  assert.equal(step(0, false), false, 'modal reopened, counter restarted');
  assert.equal(step(1, true), true, 'first failure after reopening focuses again');
});

// Manual QA only (the ui has no DOM test runner): the effect that applies these
// focus decisions in FormError, and the Users page's formStillOpen wiring.
test('a late server failure announces without stealing focus from another field', () => {
  assert.equal(isFocusIdle('INPUT'), false);
  assert.equal(isFocusIdle('TEXTAREA'), false);
  assert.equal(isFocusIdle('BUTTON', true), true, 'focus still on the submit button just clicked');
  assert.equal(isFocusIdle('BUTTON', false), false, 'a select trigger or checkbox elsewhere in the form');
  assert.equal(isFocusIdle('BODY'), true);
  assert.equal(isFocusIdle(undefined), true);
  // The attempt is consumed even when focus is not taken, so a later re-render cannot grab it.
  assert.deepEqual(nextFocusState(0, 1, true, false), { focus: false, last: 1 });
  assert.deepEqual(nextFocusState(1, 1, true, true), { focus: false, last: 1 });
  assert.deepEqual(nextFocusState(0, 1, true), { focus: true, last: 1 });
});

test('FormError renders an alert with id and tabIndex, and nothing without errors', async () => {
  const FormError = await loadFormError();
  const { createElement } = require('react');
  const { renderToStaticMarkup } = require('react-dom/server');
  assert.equal(renderToStaticMarkup(createElement(FormError, { errors: [], attempt: 0 })), '');
  const html = renderToStaticMarkup(createElement(FormError, { errors: ['Name is required', 'Host is required'], attempt: 1, id: 'my-error' }));
  assert.match(html, /role="alert"/);
  assert.match(html, /id="my-error"/);
  assert.match(html, /tabindex="-1"/);
  assert.match(html, /Name is required/);
  assert.match(html, /Host is required/);
});

test('the three create modals keep their submit buttons enabled and use FormError', () => {
  const pages = {
    'pages/APIKeys.tsx': /Create new API key[\s\S]*?\{\/\* Edit Key Modal/,
    'pages/Users.tsx': /function CreateUserModal[\s\S]*?function UserCreatedModal/,
    'pages/GPUNodes.tsx': /\{\/\* Add Node Modal \*\/\}[\s\S]*?\{\/\* Edit Node Modal \*\/\}/,
  };
  for (const [file, region] of Object.entries(pages)) {
    const text = readFileSync(src(file), 'utf8');
    const block = text.match(region)?.[0];
    assert.ok(block, `${file}: modal region found`);
    assert.match(block, /<FormError/, file);
    assert.doesNotMatch(block, /disabled=/, `${file}: submit must stay enabled`);
    assert.doesNotMatch(block, /-hint/, `${file}: no separate hint pattern`);
  }
});
