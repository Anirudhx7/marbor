// CI gate for `npm audit --omit=dev`.
//
// Runs the audit and fails on any finding, with one time-boxed exception: the
// picomatch ReDoS advisory below. It is a build-time dependency of vite (not in
// the shipped bundle) and the advisory had no patched release when the allowance
// was added. The allowance lapses by itself, so it cannot be forgotten:
//   - after EXPIRES, or
//   - as soon as the registry publishes a picomatch version newer than
//     LAST_AFFECTED (a patched release may exist: update picomatch, then delete
//     this allowance and go back to a plain `npm audit --omit=dev`).
import { execFileSync } from 'node:child_process'
import { pathToFileURL } from 'node:url'

export const ALLOWED_ADVISORY = 'GHSA-fc5q-39xc-vjpf'
export const PACKAGE = 'picomatch'
export const LAST_AFFECTED = '4.0.7'
export const EXPIRES = '2027-02-18'

function parseVersion(v) {
  const m = /^(\d+)\.(\d+)\.(\d+)$/.exec(v)
  return m ? [Number(m[1]), Number(m[2]), Number(m[3])] : null
}

function isNewer(a, b) {
  const x = parseVersion(a)
  const y = parseVersion(b)
  if (!x || !y) return false
  for (let i = 0; i < 3; i++) {
    if (x[i] !== y[i]) return x[i] > y[i]
  }
  return false
}

// evaluate decides whether the audit result passes. audit is the parsed
// `npm audit --json` object, today is an ISO date (YYYY-MM-DD), published is the
// list of versions the registry has for PACKAGE.
export function evaluate({ audit, today, published }) {
  const vulns = Object.values(audit?.vulnerabilities ?? {})
  if (vulns.length === 0) return { ok: true, reason: 'no findings' }

  const advisories = vulns.flatMap((v) => (v.via ?? []).filter((x) => typeof x === 'object'))
  if (advisories.length === 0) {
    return { ok: false, reason: 'audit reported findings but no advisory details; failing closed' }
  }
  const other = advisories.filter((a) => !String(a.url ?? '').endsWith(`/${ALLOWED_ADVISORY}`))
  if (other.length > 0) {
    return { ok: false, reason: `unallowed advisory: ${other.map((a) => a.url ?? a.title).join(', ')}` }
  }
  if (today > EXPIRES) {
    return { ok: false, reason: `the ${ALLOWED_ADVISORY} allowance expired on ${EXPIRES}: update ${PACKAGE} or renew it deliberately` }
  }
  const newer = published.filter((v) => isNewer(v, LAST_AFFECTED))
  if (newer.length > 0) {
    return {
      ok: false,
      reason: `${PACKAGE} ${newer.join(', ')} is newer than ${LAST_AFFECTED}, so a patched release may exist: run "npm update ${PACKAGE}" in ui/ and delete the ${ALLOWED_ADVISORY} allowance (ui/scripts/audit-gate.mjs and the CI step that calls it)`,
    }
  }
  return { ok: true, reason: `only ${ALLOWED_ADVISORY} (${PACKAGE} <= ${LAST_AFFECTED}, no patched release, allowed until ${EXPIRES})` }
}

function run(cmd, args) {
  try {
    return { out: execFileSync(cmd, args, { encoding: 'utf8', maxBuffer: 64 * 1024 * 1024 }), code: 0 }
  } catch (err) {
    // npm audit exits non-zero when it has findings; its JSON is still on stdout.
    return { out: String(err.stdout ?? ''), code: err.status ?? 1 }
  }
}

function main() {
  const audit = run('npm', ['audit', '--omit=dev', '--json'])
  if (audit.code === 0) {
    console.log('npm audit: no findings')
    return 0
  }
  let parsed
  try {
    parsed = JSON.parse(audit.out)
  } catch {
    console.error('npm audit failed and its output was not JSON (registry or network problem); failing closed')
    return 1
  }
  const view = run('npm', ['view', PACKAGE, 'versions', '--json'])
  let published
  try {
    published = JSON.parse(view.out)
  } catch {
    console.error(`could not read the published ${PACKAGE} versions; failing closed`)
    return 1
  }
  if (!Array.isArray(published)) published = [published]
  const today = new Date().toISOString().slice(0, 10)
  const verdict = evaluate({ audit: parsed, today, published })
  if (verdict.ok) {
    console.log(`npm audit: ${verdict.reason}`)
    return 0
  }
  console.error(`npm audit gate failed: ${verdict.reason}`)
  console.error(audit.out.length < 4000 ? audit.out : audit.out.slice(0, 4000))
  return 1
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exit(main())
}
