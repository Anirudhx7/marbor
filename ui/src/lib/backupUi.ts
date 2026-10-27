// Pure helpers for the Backup & restore card: size and name formatting, status
// wording, and the checks done before a file leaves the browser. No React and
// no network, so they can be tested directly (ui/scripts/backup-ui.test.mjs).

import type { BackupStatus } from '../types';

// The only file names marbor itself ever gives a backup, optionally with the
// upload marker. Mirrors the server's own rule.
export const BACKUP_NAME_RE = /^marbor-backup-\d{8}-\d{6}(-upload)?\.db(\.gz)?$/;

export const DEFAULT_BACKUP_FILENAME = 'marbor-backup.db.gz';

// The server refuses uploads above this size; checking here saves sending
// gigabytes only to be turned away.
export const MAX_UPLOAD_BYTES = 2 * 1024 * 1024 * 1024;

// The most the in-memory save fallback will collect. A chosen safety limit, not
// a measured browser limit: about twice the compressed backup of the 1.6 GiB
// test database (242 MiB), so ordinary databases fit and a huge one is turned
// away with a pointer to the CLI instead of crashing the tab.
export const MAX_MEMORY_DOWNLOAD_BYTES = 512 * 1024 * 1024;

// tooBigForBrowser is what an on-demand backup, or a stored file that cannot go
// through the save dialog, says when it is larger than the in-memory limit. It
// points at the two ways around it that work for any size. size is the
// declared length, or null when the length was not known up front.
export function tooBigForBrowser(size: number | null): string {
  const what = size === null ? 'This backup is larger than' : `This backup is ${formatBackupSize(size)}, more than`;
  return `${what} this browser saves safely (${formatBackupSize(MAX_MEMORY_DOWNLOAD_BYTES)}), so nothing was saved. Run "marbor backup now" in a terminal on a machine that can reach marbor, or download one of the stored backups from the list below.`;
}

// The reference marbor gives each restore: 16 lowercase hex digits. Anything
// else is never shown or sent back.
const RESTORE_REF_RE = /^[0-9a-f]{16}$/;

export function isValidRestoreRef(ref: string | null | undefined): ref is string {
  return typeof ref === 'string' && RESTORE_REF_RE.test(ref);
}

// pollDelayMs is the wait before the next status refresh after `failures`
// refreshes in a row failed: 3 s, doubling up to 30 s. POLL_GIVE_UP_AFTER
// failures in a row stop the automatic refresh.
export function pollDelayMs(failures: number): number {
  const n = Math.max(0, Math.min(failures, 10));
  return Math.min(3000 * 2 ** n, 30000);
}
export const POLL_GIVE_UP_AFTER = 5;

const GZIP_MAGIC = [0x1f, 0x8b];
const SQLITE_MAGIC = 'SQLite format 3\0';

// looksLikeBackup checks the first bytes of a response: a gzip stream or a
// SQLite database. A login or captive-portal page fails this. Fewer than two
// bytes cannot be judged, so the caller keeps reading until it has two.
export function looksLikeBackup(first: Uint8Array): boolean {
  if (first.length >= 2 && first[0] === GZIP_MAGIC[0] && first[1] === GZIP_MAGIC[1]) return true;
  const n = Math.min(first.length, SQLITE_MAGIC.length);
  if (n < 2) return false;
  for (let i = 0; i < n; i++) {
    if (first[i] !== SQLITE_MAGIC.charCodeAt(i)) return false;
  }
  return true;
}

// isNewProcess says whether /health now describes a restarted marbor. With a
// start identifier on both sides they are compared; without one (an older
// marbor) the process counts as new only if it was seen down in between.
export function isNewProcess(before: string | null, now: string | null, sawDown: boolean): boolean {
  if (before && now) return before !== now;
  return sawDown;
}

// formatBackupSize renders a byte count, or "-" when the server did not give a
// usable number (never a made-up "NaN KB" or "0 KB").
export function formatBackupSize(bytes: number | null | undefined): string {
  if (typeof bytes !== 'number' || !Number.isFinite(bytes) || bytes < 0) return '-';
  if (bytes < 1024 * 1024) return `${Math.max(0, Math.round(bytes / 1024))} KB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(2)} GB`;
}

// filenameFromDisposition reads the file name out of a Content-Disposition
// header: the RFC 5987 form (filename*=UTF-8''...) wins over the plain form,
// quotes are stripped, and only a bare marbor backup name is accepted - any
// other value (a path, an unexpected extension) falls back so a response can
// never choose an arbitrary local file name.
export function filenameFromDisposition(header: string | null | undefined, fallback: string = DEFAULT_BACKUP_FILENAME): string {
  if (!header) return fallback;
  const candidates: string[] = [];
  const ext = /filename\*\s*=\s*[^';]*'[^';]*'([^;]+)/i.exec(header);
  if (ext) {
    try {
      candidates.push(decodeURIComponent(ext[1].trim().replace(/^"|"$/g, '')));
    } catch {
      // an undecodable value is skipped, the plain form may still be usable
    }
  }
  const plain = /filename\s*=\s*(?:"([^"]*)"|([^;]+))/i.exec(header.replace(/filename\*\s*=\s*[^;]*/gi, ''));
  if (plain) candidates.push((plain[1] ?? plain[2] ?? '').trim());
  for (const name of candidates) {
    if (BACKUP_NAME_RE.test(name)) return name;
  }
  return fallback;
}

export type BackupResultKind = 'none' | 'success' | 'failed' | 'refused' | 'cancelled' | 'unknown';

// backupResultKind maps whatever the server sent to a kind the card knows how
// to word, so a result added later shows as "unknown" and not as a blank.
export function backupResultKind(result: string | null | undefined): BackupResultKind {
  switch (result) {
    case undefined:
    case null:
    case '':
      return 'none';
    case 'success':
    case 'failed':
    case 'refused':
    case 'cancelled':
      return result;
    default:
      return 'unknown';
  }
}

// backupFailureText words a failed, refused, cancelled or unknown outcome. The
// reason is shown only when the server gave one; the text never ends in a bare
// colon. label is "backup", "scheduled backup" or "on-demand backup".
export function backupFailureText(label: string, kind: BackupResultKind, error: string | null | undefined, when: string | null): string {
  const verb =
    kind === 'refused' ? 'was refused' : kind === 'cancelled' ? 'was cancelled' : kind === 'failed' ? 'failed' : 'ended in an unknown state';
  let text = `Last ${label} ${verb}`;
  if (when) text += ` at ${when}`;
  const reason = (error ?? '').trim();
  text += reason ? `: ${reason}` : '. The server gave no reason - check its log.';
  return text;
}

export interface BackupLine {
  key: string;
  // error: shown in red and announced at once; warn: amber, announced politely;
  // info: plain muted text.
  tone: 'error' | 'warn' | 'info';
  text: string;
}

interface OutcomeLike {
  result?: string;
  attempt_at?: string;
  success_at?: string;
  error?: string;
  warning?: string;
  note?: string;
}

// outcomeLine words one source's last outcome, or null when it never ran.
function outcomeLine(key: string, label: string, o: OutcomeLike | null | undefined, fmt: (iso: string) => string): BackupLine | null {
  if (!o) return null;
  const kind = backupResultKind(o.result);
  if (kind === 'none') return null;
  if (kind === 'success') {
    const at = o.success_at || o.attempt_at;
    let ok = at ? `Last ${label}: ${fmt(at)}` : `Last ${label} succeeded`;
    const extra = [o.warning, o.note].filter((x): x is string => !!x && x.trim() !== '');
    if (extra.length > 0) ok += `. ${extra.map((x) => { const t = x.replace(/[.\s]+$/, ''); return t.charAt(0).toUpperCase() + t.slice(1); }).join('. ')}.`;
    return { key, tone: extra.length > 0 ? 'warn' : 'info', text: ok };
  }
  let text = backupFailureText(label, kind, o.error, o.attempt_at ? fmt(o.attempt_at) : null);
  if (o.success_at) text += ` Last success: ${fmt(o.success_at)}.`;
  if (o.note) text += ` ${o.note}`;
  return { key, tone: 'error', text };
}

// backupStatusLines turns the server's status into the lines the card shows:
// the last scheduled and on-demand outcome (falling back to the single last
// result when the server sends no per-source objects) and every condition the
// operator should know about. Nothing is invented: a field that is absent adds
// no line.
export interface BackupLineOptions {
  // The on-demand backup was made on the server but its download was cancelled
  // or failed in this browser, so no copy reached this computer.
  manualNotSaved?: boolean;
}

export function backupStatusLines(status: BackupStatus, skipped: number, fmt: (iso: string) => string, opts: BackupLineOptions = {}): BackupLine[] {
  const lines: BackupLine[] = [];
  const push = (l: BackupLine | null) => {
    if (l) lines.push(l);
  };
  if (status.scheduled || status.manual) {
    push(outcomeLine('scheduled', 'scheduled backup', status.scheduled, fmt));
    const manual = outcomeLine('manual', 'on-demand backup', status.manual, fmt);
    if (manual && opts.manualNotSaved && backupResultKind(status.manual?.result) === 'success') {
      manual.text += ' It was not saved to this computer: the download was cancelled or did not finish.';
      manual.tone = 'warn';
    }
    push(manual);
  } else {
    const label = status.last_source === 'manual' ? 'on-demand backup' : status.last_source === 'scheduled' ? 'scheduled backup' : 'backup';
    push(
      outcomeLine(
        'last',
        label,
        { result: status.last_result, attempt_at: status.last_attempt_at, success_at: status.last_success_at, error: status.last_error },
        fmt,
      ),
    );
  }
  if (status.last_restore && status.last_restore.result) {
    const r = status.last_restore;
    const when = r.at ? ` at ${fmt(r.at)}` : '';
    const note = (r.note ?? '').trim();
    // The reference is shown only in its exact format; anything else becomes a
    // fixed phrase, never the server's text.
    const ref = !r.ref ? '' : isValidRestoreRef(r.ref) ? ` (reference ${r.ref})` : ' (reference not shown)';
    if (r.result === 'success') {
      lines.push({ key: 'restore', tone: note ? 'warn' : 'info', text: `Last restore succeeded${when}.${note ? ' ' + note : ''}${ref}` });
    } else {
      const reason = (r.error ?? '').trim();
      lines.push({ key: 'restore', tone: 'error', text: `Last restore ${r.result === 'failed' ? 'failed' : 'ended as ' + r.result}${when}${reason ? ': ' + reason : '.'}${ref}` });
    }
  }
  if (status.dir_exists === false) {
    lines.push({ key: 'dir-missing', tone: 'warn', text: 'The backup directory does not exist yet; it is created when the first backup runs.' });
  }
  const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);
  if (status.dir_warning) lines.push({ key: 'dir-warning', tone: 'warn', text: cap(status.dir_warning) });
  if (status.status_not_saved) lines.push({ key: 'not-saved', tone: 'warn', text: cap(status.status_not_saved) });
  if (status.prune_error) lines.push({ key: 'prune', tone: 'warn', text: cap(status.prune_error) });
  if (status.upload_prune_error) lines.push({ key: 'upload-prune', tone: 'warn', text: `Uploaded files: ${status.upload_prune_error}` });
  if (status.notice) lines.push({ key: 'notice', tone: 'info', text: cap(status.notice) });
  if (status.dir_scan_error) lines.push({ key: 'dir-scan', tone: 'warn', text: cap(status.dir_scan_error) });
  if (status.leftover_files && status.leftover_files > 0) {
    lines.push({ key: 'leftover', tone: 'warn', text: `${status.leftover_files} leftover file(s) from an earlier run could not be removed from the backup directory.` });
  }
  if (skipped > 0) {
    lines.push({ key: 'skipped', tone: 'warn', text: `${skipped} entr${skipped === 1 ? 'y' : 'ies'} in the backup directory could not be listed.` });
  }
  return lines;
}

// backupFileNameFor builds the name the server gives a backup made at date (UTC,
// second precision). Used only as the save dialog's suggested name.
export function backupFileNameFor(date: Date, suffix: string = '.db.gz'): string {
  const p = (n: number) => String(n).padStart(2, '0');
  const stamp = `${date.getUTCFullYear()}${p(date.getUTCMonth() + 1)}${p(date.getUTCDate())}-${p(date.getUTCHours())}${p(date.getUTCMinutes())}${p(date.getUTCSeconds())}`;
  return `marbor-backup-${stamp}${suffix}`;
}

// isAbortError tells a cancelled request or save from a real failure.
export function isAbortError(err: unknown): boolean {
  return err instanceof DOMException && err.name === 'AbortError';
}

// isNetworkFailure tells fetch's own "could not connect" failure from any other
// TypeError (a bug in this page must not read as "marbor is unreachable"). The
// message is browser-specific: Chromium, Firefox and Safari each word it their own way.
export function isNetworkFailure(err: unknown): boolean {
  return err instanceof TypeError && /failed to fetch|networkerror|load failed|network request failed|fetch failed/i.test(err.message);
}

// errorText is one sentence for a failure caught in a handler: a network
// failure gets the "cannot reach marbor" wording, anything else its own message.
export function errorText(err: unknown, fallback: string): string {
  if (isNetworkFailure(err)) return 'Could not reach marbor. Check that it is running and reachable from this browser.';
  if (err instanceof Error && err.message) return err.message;
  return fallback;
}

// describeBackupError turns a failed response into one sentence for the
// operator: the server's own message when it sent one (also for 403, which
// carries the reason), a clear wording for an expired session, and a plain
// fallback when the server said nothing.
export function describeBackupError(status: number, serverMessage: string | null | undefined, action: string): string {
  if (status === 401) return `${action} failed: your session has expired. Sign in again.`;
  const message = (serverMessage ?? '').trim();
  if (message) return message;
  if (status === 403) return `${action} failed: your account is not allowed to do this.`;
  if (status === 429) return `${action} was not started: marbor is already handling the most it allows at once. Try again in a moment.`;
  if (status === 503) return `${action} was not started: marbor is busy with housekeeping. Try again in a moment.`;
  if (status === 409) return `${action} was not done: another backup, upload or restore is using the disk, or the file is in use. Try again when it finishes.`;
  return `${action} failed (the server answered ${status || 'with an error'}).`;
}

// checkBackupFile vets a file picked for upload before it is sent. Returns an
// error sentence, or null when the file looks acceptable (the server still
// checks it in full).
export function checkBackupFile(file: { name: string; size: number }): string | null {
  const lower = file.name.toLowerCase();
  if (/\.(tar\.gz|tgz|tar|zip)$/.test(lower)) {
    return `"${file.name}" is an archive. A marbor backup is a single .db or .db.gz file - extract the database from the archive first.`;
  }
  if (!lower.endsWith('.db') && !lower.endsWith('.db.gz')) {
    return `"${file.name}" is not a backup file - choose a .db file, or a gzip-compressed one named .db.gz.`;
  }
  if (!(file.size > 0)) return `"${file.name}" is empty.`;
  if (file.size > MAX_UPLOAD_BYTES) {
    return `"${file.name}" is ${formatBackupSize(file.size)}, larger than the ${formatBackupSize(MAX_UPLOAD_BYTES)} upload limit.`;
  }
  return null;
}
