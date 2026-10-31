// Demo data for the Backup & restore card. Ages are computed when the data is
// asked for, so the demo never shows a file that is hours older than "now".

import type { BackupFileInfo, BackupListResponse } from '../types/index.ts';
import { backupFileNameFor } from './backupUi.ts';

const HOUR_MS = 60 * 60 * 1000;

// mockBackupResponse is the demo answer of GET /admin/backup/list: scheduled
// gzip backups a day apart, an older plain file from before compression, an
// uploaded file that was not read in full, one that failed the server's check,
// and a status with the notices an operator plausibly sees together: a refused
// on-demand backup, a directory permission warning and a finished restore. The
// other warnings the card can show are covered by unit tests, not stacked here.
export function mockBackupResponse(now: number = Date.now()): BackupListResponse {
  const newest = now - 3 * HOUR_MS;
  const file = (ms: number, suffix: string, size: number, extra: Partial<BackupFileInfo> = {}): BackupFileInfo => ({
    name: backupFileNameFor(new Date(ms), suffix),
    size_bytes: size,
    modified_at: new Date(ms).toISOString(),
    compressed: suffix.endsWith('.gz'),
    uploaded: false,
    integrity: 'ok',
    ...extra,
  });
  const refusedAt = newest + 90 * 60 * 1000;
  const space = 'not enough free disk space for a backup: 38 MB free, about 52 MB needed';
  return {
    backups: [
      file(newest, '.db.gz', 1_204_224),
      file(newest - 24 * HOUR_MS, '.db.gz', 1_198_080),
      file(newest - 30 * HOUR_MS, '-upload.db.gz', 1_150_976, { uploaded: true, integrity: 'unverified' }),
      file(newest - 36 * HOUR_MS, '-upload.db.gz', 612_352, { uploaded: true, integrity: 'invalid' }),
      file(newest - 48 * HOUR_MS, '.db', 4_780_032),
    ],
    status: {
      last_result: 'refused',
      last_attempt_at: new Date(refusedAt).toISOString(),
      last_success_at: new Date(newest).toISOString(),
      last_error: space,
      last_source: 'manual',
      scheduled: {
        result: 'success',
        attempt_at: new Date(newest).toISOString(),
        success_at: new Date(newest).toISOString(),
      },
      manual: { result: 'refused', attempt_at: new Date(refusedAt).toISOString(), error: space },
      running: false,
      dir_exists: true,
      dir_warning: 'the backup directory has mode 0755; backups contain API keys and session data, so restrict it to the service user (chmod 700)',
      last_restore: {
        result: 'success',
        at: new Date(now - 26 * HOUR_MS).toISOString(),
        ref: '3f9a1c2b7d4e5a60',
      },
    },
    skipped: 0,
  };
}
