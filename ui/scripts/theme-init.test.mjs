import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))
const indexHtml = readFileSync(resolve(here, '../index.html'), 'utf8')
const scriptSrc = readFileSync(resolve(here, '../public/assets/theme-init.js'), 'utf8')

// Runs the pre-paint theme script against fake browser globals and returns
// the classes it added to <html>.
function run({ stored, prefersDark, storageThrows = false }) {
  const added = []
  const fakeWindow = {
    matchMedia: () => ({ matches: prefersDark }),
  }
  const fakeDocument = { documentElement: { classList: { add: (c) => added.push(c) } } }
  const fakeStorage = {
    getItem: () => {
      if (storageThrows) throw new Error('blocked')
      return stored
    },
  }
  new Function('window', 'document', 'localStorage', scriptSrc)(fakeWindow, fakeDocument, fakeStorage)
  return added
}

test('index.html has no inline script, so script-src self never blocks it', () => {
  const inline = [...indexHtml.matchAll(/<script(?![^>]*\ssrc=)[^>]*>([\s\S]*?)<\/script>/g)]
  assert.equal(inline.length, 0)
})

test('index.html loads the theme script from the served /assets/ route, blocking, before the app', () => {
  const m = indexHtml.match(/<script\s+src="(\/assets\/theme-init\.js)"\s*><\/script>/)
  assert.ok(m, 'expected a plain <script src="/assets/theme-init.js"> (no async/defer/module)')
  assert.ok(indexHtml.indexOf(m[0]) < indexHtml.indexOf('/src/main.tsx'))
})

test('persisted theme wins over the media query', () => {
  assert.deepEqual(run({ stored: 'light', prefersDark: true }), ['light'])
  assert.deepEqual(run({ stored: 'dark', prefersDark: false }), ['dark'])
})

test('falls back to the media query when nothing valid is stored', () => {
  assert.deepEqual(run({ stored: null, prefersDark: true }), ['dark'])
  assert.deepEqual(run({ stored: 'bogus', prefersDark: false }), ['light'])
})

test('a throwing localStorage is swallowed', () => {
  assert.doesNotThrow(() => run({ stored: null, prefersDark: true, storageThrows: true }))
})
