import type { SystemAuditEntry } from '../types';

// What the Activity feed holds: the loaded rows (newest first) and whether
// the server has older rows beyond them.
export interface ActivityFeed {
  entries: SystemAuditEntry[];
  hasMore: boolean;
}

// Identity of an audit row. Real rows carry a numeric id. Rows without one
// (fixtures, older servers) fall back to their fields, so two id-less rows
// with identical fields count as the same event; that only makes a refresh
// keep fewer duplicates, it never drops a row that has an id.
export function entryKey(e: SystemAuditEntry): string {
  if (e.id != null) return `id:${e.id}`;
  return `${e.time}|${e.username}|${e.action}|${e.target}|${e.source_ip}|${e.details}`;
}

export interface RefreshResult {
  feed: ActivityFeed;
  // True when the fresh page could not be joined to the loaded rows and the
  // list restarted from it (the caller invalidates in-flight Load more calls
  // and tells the operator).
  restarted: boolean;
}

export const EMPTY_FEED: ActivityFeed = { entries: [], hasMore: false };

// Fold a freshly fetched newest page into the feed without dropping the older
// pages the operator already paged in. The returned feed is the same object as
// current when nothing changed.
//
// - Overlap: the fresh page replaces the head. The anchor is the oldest fresh
//   row that is also loaded; everything older than it stays. Loaded rows newer
//   than the anchor but absent from the fresh page are dropped on purpose: the
//   fresh page is the server's current view of that window. A row the server
//   deleted inside the overlap disappears, while a deleted row in the older
//   tail stays until the next full load; that is intended, the tail is only
//   refetched on demand. hasMore is kept as is, the fresh page says nothing
//   about the older tail.
// - No overlap (more than a page of new events arrived, or the list was
//   empty): there is no way to join the two without a gap, so the list
//   restarts from the fresh page and hasMore comes from it.
// - Empty fresh page: returns current as is. With rows loaded it intentionally
//   never wipes what is on screen, since an empty page could be transient. A
//   log the server really emptied clears on the next full load (filter change
//   or manual Refresh).
export function applyRefresh(
  current: ActivityFeed,
  fresh: readonly SystemAuditEntry[],
  pageLimit: number,
): RefreshResult {
  if (fresh.length === 0) {
    return { feed: current, restarted: false };
  }
  if (current.entries.length > 0) {
    const freshIndex = new Map<string, number>();
    fresh.forEach((e, i) => freshIndex.set(entryKey(e), i));
    let bestFresh = -1;
    let anchor = -1;
    for (let i = 0; i < current.entries.length; i++) {
      const at = freshIndex.get(entryKey(current.entries[i]));
      if (at !== undefined && at > bestFresh) {
        bestFresh = at;
        anchor = i;
        if (at === fresh.length - 1) break;
      }
    }
    if (anchor >= 0) {
      const unchanged =
        anchor === fresh.length - 1 &&
        fresh.every((e, i) => entryKey(e) === entryKey(current.entries[i]));
      if (unchanged) return { feed: current, restarted: false };
      return {
        feed: { entries: [...fresh, ...current.entries.slice(anchor + 1)], hasMore: current.hasMore },
        restarted: false,
      };
    }
  }
  return {
    feed: { entries: [...fresh], hasMore: fresh.length === pageLimit },
    // An empty list starting from its first page is not a restart.
    restarted: current.entries.length > 0,
  };
}

// Append an older page, skipping any row already loaded. A full page means
// there may be more.
export function applyOlderPage(
  current: ActivityFeed,
  older: readonly SystemAuditEntry[],
  pageLimit: number,
): ActivityFeed {
  const seen = new Set(current.entries.map(entryKey));
  return {
    entries: [...current.entries, ...older.filter((e) => !seen.has(entryKey(e)))],
    hasMore: older.length === pageLimit,
  };
}

// A Load more result belongs to the list it was requested from. Once the list
// restarts or the filters change (the epoch moves on) the result would land on
// the wrong head, so it is discarded.
export function isStaleEpoch(captured: number, latest: number): boolean {
  return captured !== latest;
}

// A full load (filter change, manual Refresh) starts a new list: it moves the
// epoch on (a restart does too) and its own sequence, which only another full
// load changes, so a restart can never make a full load look superseded; so any Load more or background refresh still in flight is
// discarded, and it empties the feed so a failed load can never leave the
// previous filter's rows for a later refresh to merge against.
export function startFullLoad(epoch: number, fullSeq: number): { epoch: number; fullSeq: number; feed: ActivityFeed } {
  return { epoch: epoch + 1, fullSeq: fullSeq + 1, feed: EMPTY_FEED };
}

// Keys for a rendered list. Rows with an id are unique by id; id-less rows
// with identical fields get an occurrence suffix so React keys stay unique.
export function rowKeys(entries: readonly SystemAuditEntry[]): string[] {
  const seen = new Map<string, number>();
  return entries.map((e) => {
    const base = entryKey(e);
    const n = seen.get(base) ?? 0;
    seen.set(base, n + 1);
    return n === 0 ? base : `${base}\u0000#${n}`;
  });
}
