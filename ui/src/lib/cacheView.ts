// Display rules for prompt-cache figures. Kept free of runtime imports so they
// can be tested directly with node. An unreported value is always a dash: a
// missing count is unknown, and unknown is never shown as 0.
import type { Analytics } from '../types';

function groupDigits(n: number): string {
  return n.toLocaleString('en-US');
}

export interface CacheCell {
  text: string;
  title: string;
  // False when the cell is the dash for an unknown split; the dash needs
  // spoken text beside it because a title alone is not reachable by keyboard
  // or touch.
  known: boolean;
}

const UNKNOWN_TITLE = 'No cache split recorded for this request (not reported, or not readable)';

// A share of the prompt, kept honest at both ends: a small real share is never
// rounded down to look like zero, and a share short of everything is never
// rounded up to 100%.
function sharePct(cached: number, input: number): string {
  if (cached === 0) return '0%';
  const raw = (cached * 100) / input;
  if (raw < 1) return '<1%';
  if (cached < input && raw >= 99.5) return '>99%';
  return `${Math.round(raw)}%`;
}

// A usable split (input first, as the server's ValidCacheSplit): both values are finite whole numbers, input > 0 and
// 0 <= cached <= input. It mirrors the server's rule for a stored split.
export function isValidSplit(input: unknown, cached: unknown): cached is number {
  return (
    typeof cached === 'number' && typeof input === 'number' &&
    Number.isInteger(cached) && Number.isInteger(input) &&
    input > 0 && cached >= 0 && cached <= input
  );
}

// One request's cache split as "cached / input (pct)".
export function cacheCell(cached?: number | null, input?: number | null): CacheCell {
  if (!isValidSplit(input, cached)) {
    return { text: '-', title: UNKNOWN_TITLE, known: false };
  }
  return {
    text: `${groupDigits(cached)} / ${groupDigits(input as number)} (${sharePct(cached, input as number)})`,
    title: `${groupDigits(cached)} of ${groupDigits(input as number)} prompt tokens were served from the engine's prefix cache`,
    known: true,
  };
}

// The audit page's cache_split_error: a non-empty string only. Anything else
// (absent, null, empty, wrong type) means the split was read fine.
export function parseCacheSplitError(v: unknown): string | undefined {
  return typeof v === 'string' && v ? v : undefined;
}

// A group's hit rate; null (no request in the group reported) is a dash.
export function hitPctLabel(pct?: number | null): string {
  if (pct == null) return '-';
  return `${Number(pct.toFixed(1))}%`;
}

// The window caption shown beside every cache figure.
export function cacheWindowLabel(windowRequests: number, reporting: number): string {
  const noun = windowRequests === 1 ? 'request' : 'requests';
  return `last ${groupDigits(windowRequests)} ${noun}, ${groupDigits(reporting)} reported`;
}

// Which runtimes report a cache split, said only as far as it is verified.
export const CACHE_EMPTY_HINT =
  'No request in this window reported a cache split. vLLM reports it when started with --enable-prompt-tokens-details, and llama.cpp reports it in non-streaming responses. Ollama and MLX do not report it.';

export type CacheWindowState = 'loading' | 'unavailable' | 'unsupported' | 'empty' | 'unreported' | 'ready';

// Which of the card's states applies. Only a successful response with a
// genuinely empty window is "empty"; a failed read is "unavailable" and a
// server without the field is "unsupported", so neither reads as no traffic.
export function cacheWindowState(
  data: Analytics | null | undefined,
  loading: boolean,
  loadError: boolean,
): CacheWindowState {
  if (loading) return 'loading';
  // A failed request wins over any figures still held from an earlier poll, so
  // old numbers are never shown as current.
  if (loadError) return 'unavailable';
  if (!data) return 'unavailable';
  if (data.cache_window_error) return 'unavailable';
  const cw = data.cache_window;
  if (cw === undefined) return 'unsupported';
  if (cw === null) return 'unavailable';
  if (cw.window_requests === 0) return 'empty';
  if (cw.reporting_requests === 0) return 'unreported';
  return 'ready';
}

// The key that identifies one row of a by-model or by-key list. The group of
// requests with no model or key name is flagged by the server and gets its own
// prefix, so it never shares a key with a real name that happens to be "-".
export function cacheRowKey(kind: 'model' | 'key', name: string, unknown?: boolean): string {
  return `${unknown ? 'none' : 'name'}:${kind}:${name}`;
}
