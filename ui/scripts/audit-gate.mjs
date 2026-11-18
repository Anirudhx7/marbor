// CI gate for `npm audit --omit=dev`.
//
// Runs the audit and fails on any finding, with one time-boxed exception: the
// picomatch ReDoS advisory below. picomatch is reached through vite (installed as
// a peer of the @tailwindcss/vite build plugin), a build-time tool that is not in
// the shipped bundle, and the advisory had no patched release when the allowance
// was added. The advisory covers every picomatch up to LAST_AFFECTED, so a fix can
// only arrive as a higher version. The allowance lapses by itself, so it cannot be
// forgotten:
//   - after EXPIRES, or
//   - as soon as the registry publishes a picomatch version newer than
//     LAST_AFFECTED (a patched release may exist: update picomatch, then delete
//     this allowance and go back to a plain `npm audit --omit=dev`).
import { execFileSync } from 'node:child_process'
import { pathToFileURL } from 'node:url'

export const ALLOWED_ADVISORY = 'GHSA-fc5q-39xc-vjpf'
export const ALLOWED_URL = `https://github.com/advisories/${ALLOWED_ADVISORY}`
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

const VERSION_LIST = /^\d+\.\d+\.\d+/

// evaluate decides whether a FAILED audit result may pass. audit is the parsed
// `npm audit --json` object, today is an ISO date (YYYY-MM-DD), published is the
// list of versions the registry has for PACKAGE. Anything it cannot positively
// account for fails.
export function evaluate({ audit, today, published }) {
  if (!audit || typeof audit !== 'object' || audit.error) {
    return { ok: false, reason: 'npm audit returned an error, not a findings report; failing closed' }
  }
  const vulns = audit.vulnerabilities
  if (!vulns || typeof vulns !== 'object' || Array.isArray(vulns) || Object.keys(vulns).length === 0) {
    return { ok: false, reason: 'npm audit failed but reported no findings; failing closed' }
  }
  const advisories = Object.values(vulns).flatMap((v) => (v.via ?? []).filter((x) => typeof x === 'object' && x !== null))
  if (advisories.length === 0) {
    return { ok: false, reason: 'audit reported findings but no advisory details; failing closed' }
  }
  const other = advisories.filter((a) => a.name !== PACKAGE || a.url !== ALLOWED_URL)
  if (other.length > 0) {
    return { ok: false, reason: `unallowed advisory: ${other.map((a) => `${a.name}: ${a.url ?? a.title}`).join(', ')}` }
  }
  if (today > EXPIRES) {
    return { ok: false, reason: `the ${ALLOWED_ADVISORY} allowance expired on ${EXPIRES}: update ${PACKAGE} or renew it deliberately` }
  }
  if (!Array.isArray(published) || published.length === 0 || !published.every((v) => typeof v === 'string' && VERSION_LIST.test(v)) || !published.includes(LAST_AFFECTED)) {
    return { ok: false, reason: `the published ${PACKAGE} version list is missing or malformed; failing closed` }
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

const COMMAND_TIMEOUT_MS = 120_000

export function defaultRun(cmd, args) {
  try {
    const out = execFileSync(cmd, args, { encoding: 'utf8', maxBuffer: 64 * 1024 * 1024, timeout: COMMAND_TIMEOUT_MS })
    return { out, code: 0 }
  } catch (err) {
    // npm audit exits non-zero when it has findings; its JSON is still on stdout.
    return { out: String(err.stdout ?? ''), code: typeof err.status === 'number' ? err.status : 1 }
  }
}

function parseJSON(text) {
  try {
    return JSON.parse(text)
  } catch {
    return undefined
  }
}

// main returns the process exit code. run and now are injectable for tests.
export function main({ run = defaultRun, now = () => new Date(), log = console } = {}) {
  const audit = run('npm', ['audit', '--omit=dev', '--audit-level=low', '--json'])
  if (audit.code === 0) {
    const parsed = parseJSON(audit.out)
    if (parsed && !parsed.error && parsed.vulnerabilities && Object.keys(parsed.vulnerabilities).length === 0) {
      log.log('npm audit: no findings')
      return 0
    }
    log.error('npm audit exited 0 without a clean findings report; failing closed')
    return 1
  }
  const parsed = parseJSON(audit.out)
  if (parsed === undefined) {
    log.error('npm audit failed and its output was not JSON (registry or network problem); failing closed')
    return 1
  }
  const view = run('npm', ['view', PACKAGE, 'versions', '--json'])
  const viewed = view.code === 0 ? parseJSON(view.out) : undefined
  const published = typeof viewed === 'string' ? [viewed] : viewed
  const today = now().toISOString().slice(0, 10)
  const verdict = evaluate({ audit: parsed, today, published })
  if (verdict.ok) {
    log.log(`npm audit: ${verdict.reason}`)
    return 0
  }
  log.error(`npm audit gate failed: ${verdict.reason}`)
  log.error(audit.out.slice(0, 4000))
  return 1
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exit(main())
}
