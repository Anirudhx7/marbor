import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))
const read = (p) =>
  readFileSync(resolve(here, '..', p), 'utf8')
    .replace(/\/\*[\s\S]*?\*\//g, (c) => ' '.repeat(c.length))
    .replace(/^\s*\/\/.*$/gm, '')

const settings = read('src/pages/Settings.tsx')
const routing = read('src/pages/Routing.tsx')
const gpuNodes = read('src/pages/GPUNodes.tsx')
const api = read('src/lib/api.ts')
const css = read('src/index.css')

// The body of a two-space-indented `const name = ... => {` handler, found by brace matching.
function handler(src, name) {
  const start = src.indexOf(`  const ${name} = `)
  assert.notEqual(start, -1, `${name} not found`)
  const open = src.indexOf('=> {', start)
  assert.notEqual(open, -1, `${name} has no block body`)
  let depth = 0
  for (let i = open + 3; i < src.length; i++) {
    if (src[i] === '{') depth++
    else if (src[i] === '}' && --depth === 0) return src.slice(open, i + 1)
  }
  assert.fail(`${name}: unbalanced braces`)
}

// A runtime demo toggle must be checked in an `if (demoMode` before the call that would reach the real backend.
function guardedBefore(body, call, label) {
  const callAt = body.indexOf(call)
  assert.notEqual(callAt, -1, `${label}: ${call} not found`)
  const guardAt = body.search(/if [(]!?demoMode[^A-Za-z]/)
  assert.ok(guardAt !== -1 && guardAt < callAt, `${label}: an if (demoMode) / if (!demoMode) check must come before ${call}`)
}

test('Settings save never reaches the server in demo mode', () => {
  guardedBefore(handler(settings, 'handleSave'), 'await updateSettings(', 'handleSave')
})

test('Settings credential change and restore acknowledgement are local-only in demo mode', () => {
  guardedBefore(handler(settings, 'handleChangeCredentials'), 'await changePassword(', 'handleChangeCredentials')
  guardedBefore(handler(settings, 'handleAckRestore'), 'await acknowledgeRestoreResult(', 'handleAckRestore')
})

test('leaving demo mode clears the dirty flag so the live reload applies', () => {
  assert.ok(settings.includes("wasDemoRef.current && !demoMode"))
  assert.ok(settings.includes("dirtyRef.current = false"))
})

test('api.ts reads the demo flag through the shared storage key, guarded against storage errors', () => {
  assert.ok(!api.includes("'marbor-demo-mode'"), 'no hardcoded storage key in api.ts')
  const at = api.indexOf('const demoUsersActive')
  const w = api.slice(at, at + 400)
  assert.ok(w.includes("try {") && w.includes("localStorage.getItem(DEMO_STORAGE_KEY)") && w.includes("catch"))
  const cp = api.indexOf('export async function changePassword(')
  assert.ok(api.slice(cp, cp + 300).includes('demoUsersActive()'), 'changePassword must self-guard')
})

test('Settings load never fetches from the server in demo mode', () => {
  const at = settings.indexOf('fetchSettings()')
  assert.notEqual(at, -1)
  assert.ok(settings.slice(Math.max(0, at - 250), at).includes('demoMode'), 'fetchSettings() needs a demoMode guard before it')
})

test('Routing cloud provider and chain mutations are local-only in demo mode', () => {
  guardedBefore(handler(routing, 'moveCloudProvider'), 'await reorderCloudProviders(', 'moveCloudProvider')
  guardedBefore(handler(routing, 'handleSaveCloudProvider'), 'await testCloudProvider(', 'handleSaveCloudProvider test')
  guardedBefore(handler(routing, 'handleSaveCloudProvider'), 'await addCloudProvider(', 'handleSaveCloudProvider add')
  guardedBefore(handler(routing, 'handleSaveCloudProvider'), 'await updateCloudProvider(', 'handleSaveCloudProvider update')
  guardedBefore(handler(routing, 'handleDeleteCloudProvider'), 'await deleteCloudProvider(', 'handleDeleteCloudProvider')
  guardedBefore(handler(routing, 'persistChains'), 'await updateSettings(', 'persistChains')
})

test('GPUNodes unload checks demo mode before calling the backend', () => {
  guardedBefore(handler(gpuNodes, 'handleUnloadModel'), 'await unloadModel(', 'handleUnloadModel')
})

test('Users API functions follow the runtime demo toggle, not only the build flag', () => {
  for (const fn of ['listUsers', 'createUser', 'approveUser', 'suspendUser', 'deleteUser', 'patchUser', 'resetUserPassword']) {
    const start = api.indexOf(`export async function ${fn}(`)
    assert.notEqual(start, -1, fn)
    const body = api.slice(start, api.indexOf('\n}\n', start))
    assert.ok(body.includes('demoUsersActive()'), `${fn} must use demoUsersActive()`)
    assert.ok(!/if \(DEMO\)/.test(body), `${fn} must not check only the build flag`)
  }
})

test('cloud provider modal mode is stored, not derived from the typed name', () => {
  assert.ok(/cloudModalMode/.test(routing), 'cloudModalMode state missing')
  const modal = routing.slice(routing.indexOf('Add/edit cloud provider modal'))
  assert.ok(!modal.includes('cloudProviders.some('), 'modal must not derive Add/Edit from the live provider list')
  assert.ok(routing.includes('already exists'), 'Add mode must block a duplicate name with an inline error')
})

test('dark: utilities follow the .dark class, not the OS preference', () => {
  assert.match(css, /@custom-variant dark \(&:where\(\.dark, \.dark \*\)\);/)
})
