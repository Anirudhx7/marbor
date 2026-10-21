import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))
const indexHtml = readFileSync(resolve(here, '../index.html'), 'utf8').replace(/<!--[\s\S]*?-->/g, (c) => ' '.repeat(c.length))
const scriptSrc = readFileSync(resolve(here, '../public/assets/theme-init.js'), 'utf8')
// Comments blanked, so a commented-out call cannot satisfy the cross-check.
const useThemeSrc = readFileSync(resolve(here, '../src/hooks/useTheme.tsx'), 'utf8')
  .replace(/\/\*[\s\S]*?\*\//g, (c) => ' '.repeat(c.length))
  .replace(/^\s*\/\/.*$/gm, '')

const STORAGE_KEY = 'marbor-theme'
const DARK_QUERY = '(prefers-color-scheme: dark)'

// Runs the pre-paint theme script against fake browser globals and reports
// what it asked for and which classes it put on <html>.
function run({ stored, prefersDark, storageThrows = false }) {
  const classes = new Set()
  const calls = { storageKeys: [], mediaQueries: [] }
  const fakeWindow = {
    matchMedia: (q) => {
      calls.mediaQueries.push(q)
      return { matches: q === DARK_QUERY && prefersDark }
    },
  }
  const fakeDocument = { documentElement: { classList: { add: (c) => classes.add(c) } } }
  const fakeStorage = {
    getItem: (k) => {
      calls.storageKeys.push(k)
      if (storageThrows) throw new Error('blocked')
      return k === STORAGE_KEY ? stored : null
    },
  }
  new Function('window', 'document', 'localStorage', scriptSrc)(fakeWindow, fakeDocument, fakeStorage)
  return { classes: [...classes], ...calls }
}

// Every <script> element in index.html, as { attrs, body }.
function scriptTags(html) {
  return [...html.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script>/g)].map((m) => ({
    attrs: m[1].trim(),
    body: m[2].trim(),
    index: m.index,
  }))
}

test('index.html has no inline script, so script-src self never blocks it', () => {
  const inline = scriptTags(indexHtml).filter((s) => !/(^|\s)src\s*=/.test(s.attrs) || s.body !== '')
  assert.deepEqual(inline, [])
})

test('index.html loads the theme script once, blocking, in <head>, before the app bundle', () => {
  const tags = scriptTags(indexHtml)
  const theme = tags.filter((s) => s.attrs.includes('theme-init.js'))
  assert.equal(theme.length, 1, 'expected exactly one theme-init script tag')
  // An absolute /assets/ path is the one form Vite prefixes with the build base
  // (/ in the dashboard, ./ in the static demo); a relative path is left alone and
  // breaks on nested routes. No async, defer or module: those would paint first.
  assert.equal(theme[0].attrs, 'src="/assets/theme-init.js"')
  const headEnd = indexHtml.indexOf('</head>')
  assert.ok(theme[0].index < headEnd, 'theme script must be in <head>')
  const app = tags.find((s) => s.attrs.includes('/src/main.tsx'))
  assert.ok(app, 'expected the app bundle script')
  assert.ok(theme[0].index < app.index, 'theme script must come before the app bundle')
})

test('theme script reads the same storage key and media query as useTheme.tsx', () => {
  assert.ok(
    useThemeSrc.includes(`localStorage.getItem('${STORAGE_KEY}')`),
    'useTheme.tsx no longer reads the storage key the pre-paint script reads',
  )
  assert.ok(useThemeSrc.includes(DARK_QUERY), 'useTheme.tsx no longer uses the dark media query')
  const r = run({ stored: 'dark', prefersDark: false })
  assert.deepEqual(r.storageKeys, [STORAGE_KEY])
})

test('persisted theme wins over the media query and adds exactly one class', () => {
  const light = run({ stored: 'light', prefersDark: true })
  assert.deepEqual(light.classes, ['light'])
  assert.deepEqual(light.mediaQueries, [])
  assert.deepEqual(run({ stored: 'dark', prefersDark: false }).classes, ['dark'])
})

test('falls back to the dark-scheme media query when nothing valid is stored', () => {
  const dark = run({ stored: null, prefersDark: true })
  assert.deepEqual(dark.classes, ['dark'])
  assert.deepEqual(dark.mediaQueries, [DARK_QUERY])
  assert.deepEqual(run({ stored: 'bogus', prefersDark: false }).classes, ['light'])
})

test('a throwing localStorage is swallowed and leaves the default theme untouched', () => {
  const r = run({ stored: null, prefersDark: true, storageThrows: true })
  assert.deepEqual(r.classes, [])
  assert.deepEqual(r.storageKeys, [STORAGE_KEY])
})
