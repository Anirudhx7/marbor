// Follows a restore that was asked for until marbor is back and the outcome is
// known. It lives at module level, not in the Settings page: leaving the page
// and coming back (or reloading the tab) must not lose track of a restore, and
// the page reload that ends a successful restore has to happen whichever page
// the operator is on. A record in localStorage lets a reloaded tab pick the
// watch up again; storage failing only means that resume is not available.

import { fetchBackupList, fetchStartedAt, waitForRestart } from './api';
import { isValidRestoreRef } from './backupUi';

export type RestoreWatchPhase =
  | 'restarting'
  | 'checking'
  | 'restarted'
  | 'still-running'
  | 'timeout'
  | 'succeeded'
  | 'failed'
  | 'signin'
  | 'unknown';

export interface RestoreWatchState {
  phase: RestoreWatchPhase;
  detail: string;
}

interface Pending {
  at: number;
  ref: string | null;
  before: string | null;
}

const STORAGE_KEY = 'marbor.restoreWatch';
const RELOAD_DELAY_MS = 1500;
// A pending record older than this is stale (the tab was closed long ago).
const PENDING_MAX_AGE_MS = 10 * 60 * 1000;
const SLACK_MS = 60 * 1000;

let state: RestoreWatchState | null = null;
let running: AbortController | null = null;
const listeners = new Set<() => void>();

function emit(next: RestoreWatchState | null): void {
  state = next;
  listeners.forEach((l) => l());
}

export function getRestoreWatch(): RestoreWatchState | null {
  return state;
}

export function subscribeRestoreWatch(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function savePending(p: Pending): void {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(p));
  } catch {
    // storage unavailable: the watch still runs, it just cannot resume after a reload
  }
}

function clearPending(): void {
  try {
    localStorage.removeItem(STORAGE_KEY);
  } catch {
    // nothing stored, nothing to clear
  }
}

function readPending(): Pending | null {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return null;
    const p = JSON.parse(raw) as Pending;
    if (typeof p.at !== 'number' || Date.now() - p.at > PENDING_MAX_AGE_MS) {
      clearPending();
      return null;
    }
    return { at: p.at, ref: isValidRestoreRef(p.ref) ? p.ref : null, before: typeof p.before === 'string' ? p.before : null };
  } catch {
    return null;
  }
}

// readBefore captures the identifier of the running process; call it before
// asking for the restore.
export async function readProcessStart(): Promise<string | null> {
  return fetchStartedAt();
}

async function conclude(p: Pending): Promise<void> {
  let result: { result: string; at?: string; error?: string; ref?: string } | undefined;
  try {
    const r = await fetchBackupList();
    result = r.status?.last_restore;
  } catch (err) {
    const message = err instanceof Error ? err.message : '';
    if (/session has expired/i.test(message)) {
      clearPending();
      emit({ phase: 'signin', detail: '' });
      setTimeout(() => window.location.reload(), RELOAD_DELAY_MS);
      return;
    }
    clearPending();
    emit({ phase: 'unknown', detail: '' });
    return;
  }
  const mine = !!result && (p.ref ? result.ref === p.ref : !!result.at && Date.parse(result.at) >= p.at - SLACK_MS);
  clearPending();
  if (mine && result && result.result === 'success') {
    emit({ phase: 'succeeded', detail: '' });
    setTimeout(() => window.location.reload(), RELOAD_DELAY_MS);
  } else if (mine && result) {
    // A failed restore keeps the old database; nothing to reload, the reason is shown.
    emit({ phase: 'failed', detail: (result.error ?? '').trim() });
  } else {
    // marbor is back but no result matches this restore: do not claim success.
    emit({ phase: 'unknown', detail: '' });
  }
}

async function follow(p: Pending, first: 'restarting' | 'checking'): Promise<void> {
  running?.abort();
  const ctl = new AbortController();
  running = ctl;
  emit({ phase: first, detail: '' });
  const seen = await waitForRestart(ctl.signal, p.before);
  if (ctl.signal.aborted || running !== ctl) return;
  running = null;
  if (seen === 'cancelled') return;
  if (seen === 'restarted') {
    emit({ phase: 'restarted', detail: '' });
    await conclude(p);
    return;
  }
  clearPending();
  emit({ phase: seen, detail: '' });
}

// startRestoreWatch begins following a restore that was accepted (or whose
// answer was lost: first = 'checking').
export function startRestoreWatch(before: string | null, ref: string | null, first: 'restarting' | 'checking'): void {
  const p: Pending = { at: Date.now(), ref: isValidRestoreRef(ref) ? ref : null, before };
  savePending(p);
  void follow(p, first);
}

// resumeRestoreWatch picks up a watch recorded by an earlier page load.
export function resumeRestoreWatch(): void {
  if (state || running) return;
  const p = readPending();
  if (!p) return;
  void follow(p, 'checking');
}

export function dismissRestoreWatch(): void {
  if (state && (state.phase === 'restarting' || state.phase === 'checking' || state.phase === 'restarted')) return;
  emit(null);
}
