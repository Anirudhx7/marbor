import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, writeFileSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { createRequire } from 'node:module';
import { parseKeyForm, parseNodeForm, parseUserForm } from '../src/lib/formValidation.ts';
import { formReducer, initialFormState, createSubmitGuard, nextFocusState, isFocusIdle } from '../src/lib/formState.ts';

const require = createRequire(import.meta.url);
const src = (p) => new URL(`../src/${p}`, import.meta.url);

// FormError is TSX, which node cannot import directly, so it (and the helper
// it imports) is compiled with vite's public transformWithOxc into a
// fresh temp folder and rendered on the server. The compiled files import
// react from the ui's node_modules by absolute URL, and the folder is removed
// when the test file finishes.
const scratch = [];
test.after(() => { for (const d of scratch) rmSync(d, { recursive: true, force: true }); });

async function loadFormError() {
  const { transformWithOxc } = await import('vite');
  const root = mkdtempSync(join(tmpdir(), 'form-errors-test-'));
  scratch.push(root);
  const dir = pathToFileURL(root + '/');
  const state = await transformWithOxc(readFileSync(src('lib/formState.ts'), 'utf8'), 'formState.ts');
  writeFileSync(new URL('formState.mjs', dir), state.code);
  const view = await transformWithOxc(readFileSync(src('components/FormError.tsx'), 'utf8'), 'FormError.tsx', {
    jsx: { runtime: 'automatic' },
  });
  const reactDir = new URL('../node_modules/', import.meta.url).href;
  writeFileSync(
    new URL('FormError.mjs', dir),
    view.code
      .replace('../lib/formState', './formState.mjs')
      .replace(/from ['"]react\/jsx-runtime['"]/, `from '${reactDir}react/jsx-runtime.js'`)
      .replace(/from ['"]react['"]/, `from '${reactDir}react/index.js'`),
  );
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
  assert.deepEqual(parseNodeForm(' gpu-1 ', ' 10.0.0.1 ', ' 8000 '), { errors: [], invalid: [], values: { name: 'gpu-1', host: '10.0.0.1', port: 8000 } });
  assert.deepEqual(parseNodeForm('', '').errors, ['Node name is required', 'Host is required']);
  assert.deepEqual(parseNodeForm('n', '  ').invalid, ['host']);
});

test('parseNodeForm accepts ports 1 to 65535 only', () => {
  for (const ok of ['1', '80', '11434', '65535']) assert.deepEqual(parseNodeForm('n', 'h', ok).invalid, [], ok);
  for (const bad of ['0', '65536', '99999', '-1', '1.5', '80a', '', ' ', '123456']) {
    assert.deepEqual(parseNodeForm('n', 'h', bad).invalid, ['port'], `"${bad}" should be rejected`);
  }
});

test('parseNodeForm rejects a host with a scheme, port or path', () => {
  for (const ok of ['10.0.0.1', 'gpu-1.local', 'localhost', '[::1]', '[fe80::1]']) {
    assert.deepEqual(parseNodeForm('n', ok).invalid, [], ok);
  }
  for (const bad of ['http://10.0.0.1', 'https://gpu', '10.0.0.1:11434', 'host/path', 'a b', 'user@host', '::1', '[]', 'host?x=1']) {
    assert.deepEqual(parseNodeForm('n', bad).invalid, ['host'], `"${bad}" should be rejected`);
  }
});

test('parseUserForm trims and requires a username', () => {
  assert.deepEqual(parseUserForm('  ann ').values, { username: 'ann', email: '' });
  assert.equal(parseUserForm('ann', '  a@b.co ').values.email, 'a@b.co');
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
    assert.doesNotMatch(block, /(?<!aria-)disabled=/, `${file}: submit must stay enabled`);
    assert.doesNotMatch(block, /-hint/, `${file}: no separate hint pattern`);
  }
});

test('a late create result only clears and closes the form that fired it', () => {
  const cases = [
    ['pages/APIKeys.tsx', 'keyModal', 'setNewKeyForm(EMPTY_KEY_FORM);'],
    ['pages/GPUNodes.tsx', 'addModal', 'setNewNode(EMPTY_NEW_NODE);'],
  ];
  for (const [file, modal, clear] of cases) {
    const text = readFileSync(src(file), 'utf8').replace(/\r\n/g, '\n');
    const at = text.indexOf(`const current = ${modal}.finish(token);`);
    assert.ok(at > 0, `${file}: finish result is kept`);
    const block = text.slice(at, at + 200);
    assert.match(block, /if \(current\) \{/, file);
    assert.ok(block.includes(clear), `${file}: cleared inside the current check`);
  }
});

test('the create key page queues one-time secrets instead of overwriting them', () => {
  const text = readFileSync(src('pages/APIKeys.tsx'), 'utf8');
  assert.match(text, /useState<string\[\]>\(\[\]\)/, 'a list of waiting secrets');
  assert.match(text, /setCreatedKeys\(\(q\) => \[\.\.\.q, result\.key\]\)/, 'a new secret joins the end');
  assert.match(text, /setCreatedKeys\(\(q\) => q\.slice\(1\)\)/, 'dismiss shows the next one');
  assert.doesNotMatch(text, /setNewlyCreatedKey|setNewKeyDismissTimer/, 'no single-slot state or shared timer');
  // The timer belongs to the secret on screen and is cleared when it changes.
  assert.match(text, /setTimeout\(\(\) => setCreatedKeys[\s\S]*?return \(\) => clearTimeout\(timer\);\s*\}, \[newlyCreatedKey\]\)/);
});

test('create user reports a failure only for a form that is still current', () => {
  const text = readFileSync(src('pages/Users.tsx'), 'utf8');
  assert.match(text, /if \(form\.finish\(token\)\) form\.fail\(/);
  assert.doesNotMatch(text, /form\.finish\(token\);\s*form\.fail\(/);
});

test('submit buttons are aria-disabled while busy and begin() runs before the demo branch', () => {
  for (const [file, modal] of [['pages/APIKeys.tsx', 'keyModal'], ['pages/Users.tsx', 'form'], ['pages/GPUNodes.tsx', 'addModal']]) {
    const text = readFileSync(src(file), 'utf8');
    assert.ok(text.includes(`aria-disabled={${modal}.state.busy || undefined}`), `${file}: aria-disabled while busy`);
  }
  for (const [file, begin, demo] of [
    ['pages/APIKeys.tsx', 'keyModal.begin()', 'if (demoMode) {\n      // Demo mode: fabricate'],
    ['pages/GPUNodes.tsx', 'addModal.begin()', 'if (demoMode) {\n      const added'],
  ]) {
    const text = readFileSync(src(file), 'utf8').replace(/\r\n/g, '\n');
    const b = text.indexOf(begin);
    assert.ok(b > 0 && b < text.indexOf(demo), `${file}: begin before demo branch`);
  }
});
