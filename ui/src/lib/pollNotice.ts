// Pure rules for a polled panel that must stay honest when a refresh fails:
// keep the last good data, say it may be out of date and since when, and never
// let a slow earlier response overwrite a newer one.

export const MAX_REASON_LENGTH = 160;

// errorReason turns anything thrown into one short, single-line sentence
// fragment safe to render: whitespace collapsed, trailing full stops dropped,
// length capped.
export function errorReason(e: unknown, fallback: string): string {
  const raw = e instanceof Error ? e.message : '';
  const text = raw.replace(/\s+/g, ' ').trim().replace(/\.+$/, '');
  if (!text) return fallback;
  return text.length > MAX_REASON_LENGTH ? `${text.slice(0, MAX_REASON_LENGTH - 1)}…` : text;
}

export interface PollState {
  hasData: boolean; // some earlier refresh succeeded
  lastOkAt: string | null; // ISO time the browser last received good data
  error: string | null; // reason the latest refresh failed, if it did
}

export type PollNotice =
  | { kind: 'none'; message: '' }
  | { kind: 'failed'; message: string } // first load failed, nothing to show
  | { kind: 'stale'; message: string }; // refresh failed, older data still shown

// pollNotice decides what to tell the operator. what is the plural noun for
// the data ("figures", "rows"); formatTime renders lastOkAt in the viewer's zone.
export function pollNotice(
  state: PollState,
  what: string,
  formatTime: (iso: string) => string,
): PollNotice {
  if (!state.error) return { kind: 'none', message: '' };
  if (!state.hasData || !state.lastOkAt) {
    return { kind: 'failed', message: `${state.error}.` };
  }
  return {
    kind: 'stale',
    message: `${state.error}. Showing ${what} last updated ${formatTime(state.lastOkAt)}; they may be out of date.`,
  };
}

// createPollSequence numbers each request as it is sent. accept(n) is true only
// for a response newer than the last one applied, so a slow earlier request can
// neither overwrite newer data nor record a stale error or last-good time.
export function createPollSequence(): { next: () => number; accept: (n: number) => boolean } {
  let issued = 0;
  let applied = 0;
  return {
    next: () => ++issued,
    accept: (n: number) => {
      if (n <= applied) return false;
      applied = n;
      return true;
    },
  };
}
