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
const userPortal = read('src/pages/UserPortal.tsx')
const demoBanner = read('src/components/DemoBanner.tsx')
const budgetBanner = read('src/components/BudgetBanner.tsx')
const timezone = read('src/hooks/useTimezone.tsx')
const api = read('src/lib/api.ts')
const css = read('src/index.css')

// The body of a two-space-indented `const name = ... => {` handler, found by brace matching.
function handler(src, name) {
  const start = src.indexOf(`const ${name} = `)
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

// A runtime demo toggle must be handled before the call that would reach the real backend: either an
// `if (demoMode) { ... return }` / `if (!demoMode) return` branch ahead of the call, or the call itself
// sits inside an `if (!demoMode) {` block / `!demoMode &&` expression.
function guardedBefore(body, call, label) {
  const callAt = body.indexOf(call)
  assert.notEqual(callAt, -1, `${label}: ${call} not found`)
  const before = body.slice(0, callAt)
  const re = /if [(](!?)demoMode[^A-Za-z]/g
  let ok = /!demoMode\s*&&[^;\n]*$/.test(before.slice(before.lastIndexOf('\n') + 1))
  for (let m; !ok && (m = re.exec(before)); ) {
    const open = body.indexOf('{', m.index)
    const bare = !/^[^;{]*\{/.test(body.slice(m.index, m.index + 200)) // `if (x) return` without braces
    if (bare) {
      const rest = body.slice(body.indexOf(')', m.index) + 1)
      ok = m[1] === '!' ? callAt - body.indexOf(')', m.index) - 1 < rest.indexOf(';') && !before.slice(m.index).includes(';') : /^\s*return\b/.test(rest)
      continue
    }
    let depth = 0, end = -1
    for (let i = open; i < body.length; i++) {
      if (body[i] === '{') depth++
      else if (body[i] === '}' && --depth === 0) { end = i; break }
    }
    const block = body.slice(open, end + 1)
    if (m[1] === '!') ok = end >= callAt
    else ok = end < callAt && /\breturn\b/.test(block)
  }
  assert.ok(ok, `${label}: ${call} must be guarded by a demoMode branch that returns (or an !demoMode block)`)
}

test('Settings save never reaches the server in demo mode', () => {
  guardedBefore(handler(settings, 'handleSave'), 'await updateSettings(', 'handleSave')
})

test('Settings restore acknowledgement never reaches the server in demo mode', () => {
  guardedBefore(handler(settings, 'handleAckRestore'), 'await acknowledgeRestoreResult(', 'handleAckRestore')
})

test('Settings save does not wake live readers in demo mode', () => {
  const body = handler(settings, 'handleSave')
  for (const call of ["dispatchEvent(new Event('marbor-settings-change'))", 'notifyTimezoneChanged()']) {
    guardedBefore(body, call, `handleSave ${call}`)
  }
})

test('UserPortal always changes the real password and has no demo guard', () => {
  assert.ok(!/demoMode|useDemoMode/.test(userPortal), 'UserPortal must not branch on demo mode')
  assert.ok(userPortal.includes('await changePassword('))
})

test('DemoBanner never reads or writes live settings in demo mode', () => {
  assert.ok(!/fetchSettings|updateSettings/.test(demoBanner), 'demo banner dismissal is session-only and never touches live settings')
  assert.ok(!/if \(forcedDemo\)/.test(demoBanner), 'dismiss/load must follow demoMode, not only the forced build flag')
})

test('BudgetBanner never reads or writes live data in demo mode', () => {
  const dismiss = handler(budgetBanner, 'handleDismiss')
  guardedBefore(dismiss, 'fetchSettings()', 'budget dismiss fetch')
  guardedBefore(dismiss, 'updateSettings(', 'budget dismiss update')
  const eff = budgetBanner.slice(budgetBanner.indexOf('useEffect('))
  guardedBefore(eff, 'fetchSettings()', 'budget settings effect')
  guardedBefore(eff, 'fetchCloudBudgetStatus()', 'budget status effect')
})

test('DemoBanner re-derives hidden when demo mode flips', () => {
  const eff = demoBanner.slice(demoBanner.indexOf('useEffect('))
  assert.ok(eff.indexOf('sessionStorage.getItem') !== -1 && eff.indexOf('sessionStorage.getItem') < eff.indexOf('return;'), 'effect must read the session flag in demo mode')
  assert.ok(!demoBanner.includes('FORCED_DEMO_HIDDEN_KEY'), 'key constant name must fit every demo mode')
  assert.ok(demoBanner.includes("'marbor_demo_banner_hidden_session'"), 'stored key string must not change')
})

test('TimezoneProvider never polls live settings in demo mode', () => {
  assert.ok(timezone.includes('useDemoMode'), 'provider must follow the runtime demo toggle')
  guardedBefore(handler(timezone, 'load'), 'await fetchSettings()', 'timezone load')
})

test('Settings restore notice can be dismissed locally in demo mode', () => {
  const body = handler(settings, 'handleAckRestore')
  assert.ok(/if \(demoMode\) \{[^}]*setBackupStatus/.test(body), 'demo branch must clear the notice locally')
  assert.ok(!settings.includes('disabled={ackingRestore || demoMode}'), 'dismiss button must be enabled in demo mode')
})

test('leaving demo mode clears the dirty flag so the live reload applies', () => {
  const eff = settings.indexOf('const wasDemoRef')
  assert.notEqual(eff, -1)
  const win = settings.slice(eff, eff + 900)
  const reset = win.indexOf('wasDemoRef.current && !demoMode')
  const clear = win.indexOf('dirtyRef.current = false')
  const pathGuard = win.indexOf("currentAppPath() !== '/settings'")
  const branch = win.indexOf('if (demoMode)')
  assert.ok(reset !== -1 && clear > reset, 'reset must clear the dirty flag')
  assert.ok(pathGuard !== -1 && reset < pathGuard, 'reset must run before the route early return')
  assert.ok(branch !== -1 && clear < branch, 'reset must run before the demoMode branch')
})

test('api.ts reads the demo flag through the shared storage key, guarded against storage errors', () => {
  assert.ok(!api.includes("'marbor-demo-mode'"), 'no hardcoded storage key in api.ts')
  assert.ok(api.includes("from './demoFlag'") && !api.includes('hooks/useDemoMode'))
  const flag = read('src/lib/demoFlag.ts')
  assert.ok(flag.includes('try {') && flag.includes('localStorage.getItem(DEMO_STORAGE_KEY)') && flag.includes('catch'))
  const cp = api.indexOf('export async function changePassword(')
  assert.notEqual(cp, -1)
  const body = api.slice(cp, api.indexOf('\n}\n', cp))
  assert.ok(!/demoUsersActive|DEMO\b|demoDelay/.test(body), 'changePassword must always call the real API (first-login change)')
})

test('Settings load never fetches from the server in demo mode', () => {
  const at = settings.indexOf('fetchSettings()')
  assert.notEqual(at, -1)
  const before = settings.slice(0, at)
  const branch = before.lastIndexOf('if (demoMode) {')
  assert.ok(branch !== -1 && at - branch < 600, 'an if (demoMode) block must precede fetchSettings()')
  const open = branch + 'if (demoMode) '.length
  let depth = 0, end = -1
  for (let i = open; i < before.length; i++) {
    if (before[i] === '{') depth++
    else if (before[i] === '}' && --depth === 0) { end = i; break }
  }
  assert.ok(end !== -1, 'the demoMode block must close before fetchSettings()')
  assert.ok(/\breturn;/.test(before.slice(open, end)), 'the demoMode block itself must return before fetchSettings()')
})

test('Routing cloud provider and chain mutations are local-only in demo mode', () => {
  guardedBefore(handler(routing, 'moveCloudProvider'), 'await reorderCloudProviders(', 'moveCloudProvider')
  guardedBefore(handler(routing, 'handleSaveCloudProvider'), 'await testCloudProvider(', 'handleSaveCloudProvider test')
  guardedBefore(handler(routing, 'handleSaveCloudProvider'), 'await addCloudProvider(', 'handleSaveCloudProvider add')
  guardedBefore(handler(routing, 'handleSaveCloudProvider'), 'await updateCloudProvider(', 'handleSaveCloudProvider update')
  guardedBefore(handler(routing, 'handleDeleteCloudProvider'), 'await deleteCloudProvider(', 'handleDeleteCloudProvider')
  guardedBefore(handler(routing, 'persistChains'), 'await updateSettings(', 'persistChains')
})

test('Routing demo add computes duplicate and priority from the latest list', () => {
  const body = handler(routing, 'handleSaveCloudProvider')
  const at = body.indexOf('setCloudProviders(prev =>')
  assert.notEqual(at, -1)
  const upd = body.slice(at, body.indexOf('setCloudModalOpen(false)', at))
  assert.ok(upd.includes('prev.reduce('), 'priority must derive from prev')
  assert.ok(!upd.includes('prev.some('), 'no silent duplicate branch: the inline duplicate error runs first')
  assert.ok(!body.slice(0, at).includes('cloudProviders.reduce('), 'priority must not come from the render closure')
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
  const at = routing.indexOf('title={cloudModalMode')
  assert.notEqual(at, -1, 'modal title anchor missing')
  const modal = routing.slice(at)
  assert.ok(modal.length > 500, 'modal slice must be substantial')
  assert.ok(!modal.includes('cloudProviders.some('), 'modal must not derive Add/Edit from the live provider list')
  assert.ok(routing.includes('already exists'), 'Add mode must block a duplicate name with an inline error')
})

test('dark: utilities follow the .dark class, not the OS preference', () => {
  assert.match(css, /@custom-variant dark \(&:where\(\.dark, \.dark \*\)\);/)
})

test('a node added in the demo carries no invented temperature or health history', () => {
  const body = handler(gpuNodes, 'handleAddNode')
  const start = body.indexOf('if (demoMode) {')
  const end = body.indexOf('if (!isLive)')
  assert.ok(start >= 0 && end > start, 'demo add-node branch not found')
  const demoBranch = body.slice(start, end)
  assert.ok(demoBranch.includes('withDemoSignals('), 'demo add-node branch not found')
  // Unknown stays unknown: null and an empty history, rendered as N/A and "no data yet".
  assert.match(demoBranch, /temperature:\s*null,/, 'demo node temperature must be null')
  assert.match(demoBranch, /healthHistory:\s*\[\],/, 'demo node health history must be empty')
})
