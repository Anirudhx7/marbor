// Run with: npm test. Covers the pure helpers behind the Backup & restore card
// (ui/src/lib/backupUi.ts) and the demo backup data in mockData.ts.
import test from 'node:test';
import assert from 'node:assert/strict';
import * as ui from '../src/lib/backupUi.ts';

const fmt = (iso) => `T(${iso})`;

test('formatBackupSize shows a dash for anything that is not a real size', () => {
  for (const bad of [undefined, null, NaN, -1, Infinity, '12']) assert.equal(ui.formatBackupSize(bad), '-');
  assert.equal(ui.formatBackupSize(0), '0 KB');
  assert.equal(ui.formatBackupSize(2048), '2 KB');
  assert.equal(ui.formatBackupSize(5 * 1024 * 1024), '5.0 MB');
  assert.equal(ui.formatBackupSize(3 * 1024 ** 3), '3.00 GB');
});

test('filenameFromDisposition reads quoted, bare and extended forms and rejects paths', () => {
  const ok = 'marbor-backup-20260101-000000.db.gz';
  assert.equal(ui.filenameFromDisposition(`attachment; filename="${ok}"`), ok);
  assert.equal(ui.filenameFromDisposition(`attachment; filename=${ok}`), ok);
  assert.equal(ui.filenameFromDisposition(`attachment; filename*=UTF-8''${ok}`), ok);
  assert.equal(ui.filenameFromDisposition(`attachment; filename="x.db"; filename*=UTF-8''${ok}`), ok);
  assert.equal(ui.filenameFromDisposition(`attachment; filename="marbor-backup-20260101-000000-upload.db"`), 'marbor-backup-20260101-000000-upload.db');
  for (const bad of ['attachment; filename="../../etc/x"', 'attachment; filename="notes.txt"', `attachment; filename*=UTF-8''..%2F${ok}`, '', null, undefined]) {
    assert.equal(ui.filenameFromDisposition(bad), ui.DEFAULT_BACKUP_FILENAME);
  }
  assert.equal(ui.filenameFromDisposition('', 'mine.db.gz'), 'mine.db.gz');
});

test('result kinds: unknown values are named as unknown, never blank', () => {
  assert.equal(ui.backupResultKind(''), 'none');
  assert.equal(ui.backupResultKind(undefined), 'none');
  assert.equal(ui.backupResultKind('cancelled'), 'cancelled');
  assert.equal(ui.backupResultKind('paused'), 'unknown');
});

test('status lines: no dangling colon, per-source wording, every condition shown', () => {
  const lines = ui.backupStatusLines(
    {
      last_result: 'failed',
      running: false,
      scheduled: { result: 'success', attempt_at: 'a1', success_at: 's1' },
      manual: { result: 'failed', attempt_at: 'a2', success_at: 's0' },
      dir_exists: false,
      dir_warning: 'mode 0755',
      status_not_saved: 'kept in memory',
      prune_error: 'not pruned',
      leftover_files: 2,
      last_restore: { result: 'failed', at: 'r1', error: 'boom' },
    },
    3,
    fmt,
  );
  const byKey = Object.fromEntries(lines.map((l) => [l.key, l]));
  assert.equal(byKey.scheduled.text, 'Last scheduled backup: T(s1)');
  assert.match(byKey.manual.text, /^Last on-demand backup failed at T\(a2\)\. The server gave no reason/);
  assert.match(byKey.manual.text, /Last success: T\(s0\)\.$/);
  assert.equal(byKey.manual.tone, 'error');
  assert.equal(byKey.restore.text, 'Last restore failed at T(r1): boom');
  for (const k of ['dir-missing', 'dir-warning', 'not-saved', 'prune', 'leftover', 'skipped']) assert.equal(byKey[k].tone, 'warn', k);
  assert.match(byKey.skipped.text, /^3 entries/);
  for (const l of lines) assert.doesNotMatch(l.text, /:\s*$/);
});

test('status lines fall back to the single last result and word unknown results', () => {
  const failed = ui.backupStatusLines({ last_result: 'refused', last_source: 'manual', last_error: 'no space', last_attempt_at: 'a', running: false }, 0, fmt);
  assert.equal(failed.length, 1);
  assert.equal(failed[0].text, 'Last on-demand backup was refused at T(a): no space');
  const odd = ui.backupStatusLines({ last_result: 'paused', running: false }, 0, fmt);
  assert.match(odd[0].text, /^Last backup ended in an unknown state\./);
  assert.deepEqual(ui.backupStatusLines({ last_result: '', running: false }, 0, fmt), []);
});

test('error wording separates an expired session, a missing permission and a server message', () => {
  assert.match(ui.describeBackupError(401, '', 'The download'), /session has expired/);
  assert.equal(ui.describeBackupError(403, 'password change required first', 'The download'), 'password change required first');
  assert.match(ui.describeBackupError(403, '', 'The download'), /not allowed/);
  assert.match(ui.describeBackupError(401, 'ignored', 'The download'), /session has expired/);
  assert.equal(ui.describeBackupError(500, ' disk full ', 'The backup'), 'disk full');
  assert.match(ui.describeBackupError(502, '', 'The backup'), /answered 502/);
  assert.match(ui.errorText(new TypeError('Failed to fetch'), 'x'), /Could not reach marbor/);
  assert.equal(ui.errorText(new Error('boom'), 'x'), 'boom');
  assert.equal(ui.errorText('weird', 'fallback'), 'fallback');
});

test('upload checks turn away a wrong extension, an empty file and an oversized file', () => {
  assert.equal(ui.checkBackupFile({ name: 'a.db', size: 10 }), null);
  assert.equal(ui.checkBackupFile({ name: 'A.DB.GZ', size: 10 }), null);
  assert.match(ui.checkBackupFile({ name: 'a.zip', size: 10 }), /is an archive/);
  assert.match(ui.checkBackupFile({ name: 'a.tar.gz', size: 10 }), /extract the database/);
  assert.match(ui.checkBackupFile({ name: 'backup.gz', size: 10 }), /named \.db\.gz/);
  assert.match(ui.checkBackupFile({ name: 'a.txt', size: 10 }), /not a backup file/);
  assert.match(ui.checkBackupFile({ name: 'a.db', size: 0 }), /empty/);
  assert.match(ui.checkBackupFile({ name: 'a.db', size: ui.MAX_UPLOAD_BYTES + 1 }), /upload limit/);
});

test('backupFileNameFor matches the server naming rule', () => {
  const name = ui.backupFileNameFor(new Date(Date.UTC(2026, 0, 2, 3, 4, 5)));
  assert.equal(name, 'marbor-backup-20260102-030405.db.gz');
  assert.match(name, ui.BACKUP_NAME_RE);
});

test('backupFileNameFor takes a suffix for uploaded and plain files', () => {
  const d = new Date(Date.UTC(2026, 0, 2, 3, 4, 5));
  assert.equal(ui.backupFileNameFor(d, '-upload.db'), 'marbor-backup-20260102-030405-upload.db');
  assert.match(ui.backupFileNameFor(d, '-upload.db.gz'), ui.BACKUP_NAME_RE);
});

test('the in-memory save limit is a stated value and sizes render', () => {
  assert.equal(ui.MAX_MEMORY_DOWNLOAD_BYTES, 512 * 1024 * 1024);
  assert.equal(ui.formatBackupSize(ui.MAX_MEMORY_DOWNLOAD_BYTES), '512.0 MB');
  assert.equal(ui.isAbortError(new DOMException('x', 'AbortError')), true);
  assert.equal(ui.isAbortError(new Error('x')), false);
});

test('the demo backup data is self-consistent, derived from now, and shows every state', async () => {
  const { mockBackupResponse } = await import('../src/lib/backupMock.ts');
  const hour = 3600 * 1000;
  const now = Date.UTC(2030, 5, 15, 12, 0, 0);
  const r = mockBackupResponse(now);
  assert.ok(r.backups.length >= 5);
  for (const b of r.backups) {
    assert.match(b.name, ui.BACKUP_NAME_RE);
    const m = /^marbor-backup-(\d{4})(\d{2})(\d{2})-(\d{2})(\d{2})(\d{2})/.exec(b.name);
    const fromName = Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], +m[6]);
    assert.ok(Math.abs(fromName - Date.parse(b.modified_at)) < 1000, b.name);
    assert.equal(b.compressed, b.name.endsWith('.gz'));
  }
  // Ages move with the clock instead of staying fixed at module load.
  assert.equal(Date.parse(r.backups[0].modified_at), now - 3 * hour);
  assert.ok(r.backups.some((b) => b.uploaded && b.integrity === 'unverified'));
  assert.ok(r.backups.some((b) => b.integrity === 'invalid'));
  assert.equal(r.status.scheduled.result, 'success');
  assert.equal(r.status.manual.result, 'refused');
  for (const k of ['dir_warning', 'status_not_saved', 'prune_error', 'leftover_files']) assert.ok(r.status[k], k);
  assert.equal(r.status.last_restore.result, 'success');
  assert.ok(r.skipped > 0);
  assert.equal(r.backups[0].modified_at, r.status.scheduled.success_at);
  const lines = ui.backupStatusLines(r.status, r.skipped, (x) => x);
  for (const k of ['scheduled', 'manual', 'restore', 'dir-warning', 'not-saved', 'prune', 'leftover', 'skipped']) {
    assert.ok(lines.some((l) => l.key === k), k);
  }
});

test('status lines show warnings, notes, the directory notice, the uploads retention problem and restore notes', () => {
  const lines = ui.backupStatusLines(
    {
      last_result: 'success',
      running: false,
      dir_exists: true,
      scheduled: { result: 'success', success_at: '2026-01-01T00:00:00Z', warning: 'the directory could not be flushed to disk' },
      manual: { result: 'failed', error: 'boom', note: 'The download was cut short.' },
      upload_prune_error: 'retention: 1 damaged backup(s) are kept',
      notice: 'the backup directory was changed',
      last_restore: { result: 'success', at: '2026-01-02T00:00:00Z', note: 'the directory could not be flushed', ref: 'abc' },
    },
    0,
    fmt,
  );
  const text = Object.fromEntries(lines.map((l) => [l.key, l]));
  assert.match(text.scheduled.text, /could not be flushed to disk/);
  assert.equal(text.scheduled.tone, 'warn');
  assert.match(text.manual.text, /cut short/);
  assert.equal(text.manual.tone, 'error');
  assert.match(text['upload-prune'].text, /Uploaded files: retention/);
  assert.match(text.notice.text, /^The backup directory was changed/);
  assert.match(text.restore.text, /Last restore succeeded.*could not be flushed/);
  assert.equal(text.restore.tone, 'warn');
});

test('a clean success has no warning tone and adds no extra text', () => {
  const [line] = ui.backupStatusLines(
    { last_result: 'success', running: false, dir_exists: true, scheduled: { result: 'success', success_at: '2026-01-01T00:00:00Z' } },
    0,
    fmt,
  );
  assert.equal(line.tone, 'info');
  assert.equal(line.text, 'Last scheduled backup: T(2026-01-01T00:00:00Z)');
});

const okStatus = { last_result: 'success', running: false, dir_exists: true, scheduled: { result: 'success', success_at: '2026-01-01T00:00:00Z' } };

test('restore references are shown only in their exact format', () => {
  assert.equal(ui.isValidRestoreRef('3f9a1c2b7d4e5a60'), true);
  for (const bad of ['3F9A1C2B7D4E5A60', '3f9a1c2b', '3f9a1c2b7d4e5a60x', '<b>', '', null, undefined]) {
    assert.equal(ui.isValidRestoreRef(bad), false, String(bad));
  }
  const line = (ref) => ui.backupStatusLines({ ...okStatus, last_restore: { result: 'success', at: '2026-01-02T00:00:00Z', ref } }, 0, fmt).find((l) => l.key === 'restore').text;
  assert.match(line('3f9a1c2b7d4e5a60'), /reference 3f9a1c2b7d4e5a60/);
  assert.match(line('<script>'), /reference not shown/);
  assert.doesNotMatch(line('<script>'), /script/);
});

test('only fetch network failures read as "could not reach marbor"', () => {
  assert.equal(ui.isNetworkFailure(new TypeError('Failed to fetch')), true);
  assert.equal(ui.isNetworkFailure(new TypeError('NetworkError when attempting to fetch resource.')), true);
  assert.equal(ui.isNetworkFailure(new TypeError('Load failed')), true);
  assert.equal(ui.isNetworkFailure(new TypeError("Cannot read properties of undefined (reading 'x')")), false);
  assert.equal(ui.isNetworkFailure(new Error('Failed to fetch')), false);
  assert.match(ui.errorText(new TypeError('Failed to fetch'), 'x'), /Could not reach marbor/);
  assert.equal(ui.errorText(new TypeError('boom is not a function'), 'fallback'), 'boom is not a function');
});

test('polling backs off from 3 s to 30 s and gives up after five failures', () => {
  assert.deepEqual([0, 1, 2, 3, 4, 5, 50].map(ui.pollDelayMs), [3000, 6000, 12000, 24000, 30000, 30000, 30000]);
  assert.equal(ui.POLL_GIVE_UP_AFTER, 5);
});

test('limit, conflict and housekeeping answers have their own wording', () => {
  assert.match(ui.describeBackupError(429, '', 'The backup'), /most it allows/);
  assert.match(ui.describeBackupError(503, '', 'The backup'), /housekeeping/);
  assert.match(ui.describeBackupError(409, '', 'Deleting the backup'), /was not done/);
  assert.equal(ui.describeBackupError(409, 'file is in use', 'x'), 'file is in use');
});

test('status lines show the directory scan problem and an unsaved on-demand download', () => {
  const lines = ui.backupStatusLines({ ...okStatus, dir_scan_error: 'could not read the backup directory', manual: { result: 'success', success_at: '2026-01-01T00:00:00Z' } }, 0, fmt, { manualNotSaved: true });
  const by = Object.fromEntries(lines.map((l) => [l.key, l]));
  assert.match(by['dir-scan'].text, /^Could not read the backup directory/);
  assert.match(by.manual.text, /not saved to this computer/);
  assert.equal(by.manual.tone, 'warn');
  const plain = ui.backupStatusLines({ ...okStatus, manual: { result: 'success', success_at: '2026-01-01T00:00:00Z' } }, 0, fmt).find((l) => l.key === 'manual');
  assert.doesNotMatch(plain.text, /not saved/);
});

test('looksLikeBackup needs two bytes and accepts gzip and SQLite', () => {
  assert.equal(ui.looksLikeBackup(new Uint8Array([0x1f])), false);
  assert.equal(ui.looksLikeBackup(new Uint8Array([0x1f, 0x8b, 8])), true);
  assert.equal(ui.looksLikeBackup(new TextEncoder().encode('SQLite format 3\0')), true);
  assert.equal(ui.looksLikeBackup(new TextEncoder().encode('<html>')), false);
});

test('a restart is recognised by the process identifier, or by seeing it down', () => {
  assert.equal(ui.isNewProcess('a', 'a', true), false);
  assert.equal(ui.isNewProcess('a', 'b', false), true);
  assert.equal(ui.isNewProcess(null, 'b', false), false);
  assert.equal(ui.isNewProcess(null, 'b', true), true);
  assert.equal(ui.isNewProcess('a', null, true), true);
});

test('the too-big message names the way around the limit', () => {
  assert.match(ui.tooBigForBrowser(1 << 30), /marbor backup now/);
  assert.match(ui.tooBigForBrowser(null), /larger than this browser saves safely/);
});
