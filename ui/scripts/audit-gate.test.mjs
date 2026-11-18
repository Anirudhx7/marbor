import test from 'node:test'
import assert from 'node:assert/strict'
import { evaluate, main, EXPIRES, LAST_AFFECTED, ALLOWED_URL } from './audit-gate.mjs'

const picomatchAudit = {
  vulnerabilities: {
    picomatch: { via: [{ source: 1, name: 'picomatch', url: ALLOWED_URL }] },
    vite: { via: ['picomatch', 'tinyglobby'] },
  },
}
const base = { audit: picomatchAudit, today: '2026-11-18', published: ['4.0.6', LAST_AFFECTED] }

test('allows only the picomatch advisory while no patched release exists', () => {
  assert.equal(evaluate(base).ok, true)
})

test('fails on any other advisory, even next to the allowed one', () => {
  const audit = {
    vulnerabilities: {
      ...picomatchAudit.vulnerabilities,
      other: { via: [{ source: 2, name: 'other', url: 'https://github.com/advisories/GHSA-aaaa-bbbb-cccc' }] },
    },
  }
  const v = evaluate({ ...base, audit })
  assert.equal(v.ok, false)
  assert.match(v.reason, /GHSA-aaaa-bbbb-cccc/)
})

test('does not accept the advisory id on another host or another package', () => {
  const host = { vulnerabilities: { p: { via: [{ name: 'picomatch', url: 'https://evil.example/GHSA-fc5q-39xc-vjpf' }] } } }
  const pkg = { vulnerabilities: { p: { via: [{ name: 'other', url: ALLOWED_URL }] } } }
  assert.equal(evaluate({ ...base, audit: host }).ok, false)
  assert.equal(evaluate({ ...base, audit: pkg }).ok, false)
})

test('fails closed on an audit error report, an empty report or a non-object', () => {
  assert.equal(evaluate({ ...base, audit: { error: { code: 'ENOAUDIT' } } }).ok, false)
  assert.equal(evaluate({ ...base, audit: {} }).ok, false)
  assert.equal(evaluate({ ...base, audit: { vulnerabilities: {} } }).ok, false)
  assert.equal(evaluate({ ...base, audit: null }).ok, false)
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

test('fails closed on a missing or malformed published version list', () => {
  for (const published of [undefined, null, [], [null], [{ error: 'x' }], ['4.0.6'], ['next', LAST_AFFECTED]]) {
    assert.equal(evaluate({ ...base, published }).ok, false, JSON.stringify(published))
  }
})

test('does not lapse on a prerelease alone', () => {
  assert.equal(evaluate({ ...base, published: ['4.1.0-beta.1', LAST_AFFECTED] }).ok, true)
})

const silent = { log() {}, error() {} }
const at = (iso) => () => new Date(iso)
const fakeRun = ({ audit, view }) => (cmd, args) => (args[0] === 'audit' ? audit : view)
const goodView = { out: JSON.stringify(['4.0.6', LAST_AFFECTED]), code: 0 }
const findings = { out: JSON.stringify(picomatchAudit), code: 1 }
const clean = { out: JSON.stringify({ vulnerabilities: {} }), code: 0 }

test('main passes a clean audit', () => {
  assert.equal(main({ run: fakeRun({ audit: clean }), log: silent }), 0)
})

test('main fails closed when a zero exit has no clean report', () => {
  assert.equal(main({ run: fakeRun({ audit: { out: '', code: 0 } }), log: silent }), 1)
  assert.equal(main({ run: fakeRun({ audit: { out: '{"error":{}}', code: 0 } }), log: silent }), 1)
})

test('main passes only the allowed advisory', () => {
  assert.equal(main({ run: fakeRun({ audit: findings, view: goodView }), now: at('2026-11-18T00:00:00Z'), log: silent }), 0)
})

test('main fails closed on audit error output, non-JSON output and a registry error', () => {
  const now = at('2026-11-18T00:00:00Z')
  assert.equal(main({ run: fakeRun({ audit: { out: '{"error":{"code":"ENOTFOUND"}}', code: 1 }, view: goodView }), now, log: silent }), 1)
  assert.equal(main({ run: fakeRun({ audit: { out: 'boom', code: 1 }, view: goodView }), now, log: silent }), 1)
  assert.equal(main({ run: fakeRun({ audit: findings, view: { out: '{"error":{"code":"E404"}}', code: 1 } }), now, log: silent }), 1)
  assert.equal(main({ run: fakeRun({ audit: findings, view: { out: 'null', code: 0 } }), now, log: silent }), 1)
})

test('main lapses after the expiry date', () => {
  assert.equal(main({ run: fakeRun({ audit: findings, view: goodView }), now: at('2027-02-19T00:00:00Z'), log: silent }), 1)
})
