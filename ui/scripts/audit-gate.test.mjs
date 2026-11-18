import test from 'node:test'
import assert from 'node:assert/strict'
import { evaluate, EXPIRES, LAST_AFFECTED, ALLOWED_ADVISORY } from './audit-gate.mjs'

const url = (id) => `https://github.com/advisories/${id}`
const picomatchAudit = {
  vulnerabilities: {
    picomatch: { via: [{ source: 1, url: url(ALLOWED_ADVISORY) }] },
    vite: { via: ['picomatch', 'tinyglobby'] },
  },
}
const base = { audit: picomatchAudit, today: '2026-11-18', published: ['4.0.6', LAST_AFFECTED] }

test('passes when there are no findings', () => {
  assert.equal(evaluate({ ...base, audit: { vulnerabilities: {} } }).ok, true)
})

test('allows only the picomatch advisory while no patched release exists', () => {
  assert.equal(evaluate(base).ok, true)
})

test('fails on any other advisory, even next to the allowed one', () => {
  const audit = {
    vulnerabilities: {
      ...picomatchAudit.vulnerabilities,
      other: { via: [{ source: 2, url: url('GHSA-aaaa-bbbb-cccc') }] },
    },
  }
  const v = evaluate({ ...base, audit })
  assert.equal(v.ok, false)
  assert.match(v.reason, /GHSA-aaaa-bbbb-cccc/)
})

test('fails closed when findings carry no advisory details', () => {
  assert.equal(evaluate({ ...base, audit: { vulnerabilities: { x: { via: ['y'] } } } }).ok, false)
})

test('lapses the day after the expiry date', () => {
  assert.equal(evaluate({ ...base, today: EXPIRES }).ok, true)
  assert.equal(evaluate({ ...base, today: '2027-02-19' }).ok, false)
})

test('lapses as soon as a newer picomatch is published', () => {
  const v = evaluate({ ...base, published: ['4.0.7', '4.0.8'] })
  assert.equal(v.ok, false)
  assert.match(v.reason, /patched release may exist/)
})

test('ignores prerelease and malformed version strings', () => {
  assert.equal(evaluate({ ...base, published: ['4.1.0-beta.1', 'next'] }).ok, true)
})
