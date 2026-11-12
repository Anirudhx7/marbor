// Pure helpers for the Routing page's cloud-only models section. Kept free of
// React and the API layer so the rules can be tested under plain node.
//
// The server is the authority on what a valid list is; the limits here only
// stop an obviously bad entry before a round trip, and the server's own
// message is always what the operator sees when a save is refused.

// Mirror of the server's limits (entry count and entry length in bytes).
export const MAX_ENTRIES = 200;
export const MAX_ENTRY_LEN = 256;

export type ProviderStatus = 'unknown' | 'usable' | 'none';

export interface ProviderStatusInput {
  // True once the cloud provider list has loaded successfully; false while it
  // loads, reloads or after a failure.
  providersReady: boolean;
  // LiteLLM counts as a usable route only with a non-empty URL. null while the
  // settings are loading or after a failed settings read.
  liteLLMUsable: boolean | null;
  providers: Array<{ enabled?: boolean }>;
}

// providerStatus says whether a cloud-only request could be sent anywhere.
// 'unknown' until both sources are known, so a warning is never raised from
// data that has not loaded or failed to load.
export function providerStatus(input: ProviderStatusInput): ProviderStatus {
  if (!input.providersReady || input.liteLLMUsable === null) return 'unknown';
  if (input.liteLLMUsable || input.providers.some(p => p.enabled)) return 'usable';
  return 'none';
}

export type AddResult = { list: string[] } | { error: string };

// addEntry returns the list with raw added, or the reason it cannot be. The
// original list is never changed.
export function addEntry(list: readonly string[], raw: string): AddResult {
  const entry = raw.trim();
  if (entry === '') return { error: 'Enter a model name.' };
  if (new TextEncoder().encode(entry).length > MAX_ENTRY_LEN) {
    return { error: `A model name can be at most ${MAX_ENTRY_LEN} characters.` };
  }
  if (/\s/.test(entry)) return { error: 'A model name cannot contain spaces.' };
  if (entry === '*') return { error: 'A bare * is not allowed. List model names or a prefix such as qwen*.' };
  const star = entry.indexOf('*');
  if (star >= 0 && star !== entry.length - 1) {
    return { error: 'A * is only allowed as the last character, as in qwen*.' };
  }
  const folded = entry.toLowerCase();
  if (list.some(e => e.toLowerCase() === folded)) {
    return { error: `"${entry}" is already on the list.` };
  }
  if (list.length >= MAX_ENTRIES) {
    return { error: `The list can hold at most ${MAX_ENTRIES} entries.` };
  }
  return { list: [...list, entry] };
}

// removeEntry returns the list without entry (case-folded compare).
export function removeEntry(list: readonly string[], entry: string): string[] {
  const folded = entry.toLowerCase();
  return list.filter(e => e.toLowerCase() !== folded);
}

// namesEquivalent mirrors the router's rule: equal ignoring case, or the same
// name with and without a trailing ":latest".
function namesEquivalent(a: string, b: string): boolean {
  const x = a.toLowerCase();
  const y = b.toLowerCase();
  if (x === y) return true;
  return x === `${y}:latest` || y === `${x}:latest`;
}

// matchesKnownModel reports whether a list entry covers any model the nodes
// currently serve: exact entries by name (with the :latest rule), prefix
// entries by startsWith.
export function matchesKnownModel(entry: string, known: readonly string[]): boolean {
  const e = entry.toLowerCase();
  if (e.endsWith('*')) {
    const prefix = e.slice(0, -1);
    return known.some(k => k.toLowerCase().startsWith(prefix));
  }
  return known.some(k => namesEquivalent(e, k));
}

// shouldWarnNoMatch decides whether to show the "matches no model your nodes
// serve" notice: only when the node model list is loaded and non-empty, so an
// empty or still-loading list never produces a false warning.
export function shouldWarnNoMatch(entry: string, known: readonly string[]): boolean {
  if (known.length === 0) return false;
  return !matchesKnownModel(entry, known);
}

// writeBlocked says whether adding or restoring an entry must wait. While the
// stored list is unreadable a save replaces it with exactly what is shown, so
// the operator has to tick the acknowledgement first.
export function writeBlocked(input: { unreadable: boolean; acknowledged: boolean; saving: boolean }): boolean {
  if (input.saving) return true;
  return input.unreadable && !input.acknowledged;
}
