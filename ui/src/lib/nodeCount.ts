// Last-known collection sizes for first-poll skeletons (Dashboard, GPU Nodes,
// Model Advisor). Live mode mounts with empty collections, which would flash
// false empty states ("No nodes connected", zero metrics, spinners) before
// /admin/v1 answers. Skeleton grids render as many placeholders as the last
// successful poll saw, so each deployment shimmers in its own shape instead
// of a one-size count. First-ever visit (nothing stored yet) falls back to a
// documented per-use default; stored counts clamp so a stale giant number
// can't flood the page. Demo data is synchronous and never reads this.
const NODE_KEY = 'marbor-last-node-count';
const MODEL_KEY = 'marbor-last-model-count';

function readKey(key: string, fallback: number, max: number): number {
  try {
    const n = parseInt(localStorage.getItem(key) ?? '', 10);
    if (Number.isFinite(n)) return Math.min(Math.max(n, 1), max);
  } catch {
    /* storage unavailable - fall through to the fallback */
  }
  return fallback;
}

function writeKey(key: string, n: number): void {
  try {
    localStorage.setItem(key, String(Math.max(0, Math.floor(n))));
  } catch {
    /* storage unavailable - skeletons just use the fallback next visit */
  }
}

// Fleet size - written by the Dashboard/GPU Nodes polls, read by anything
// rendering node-shaped placeholders.
export function readLastNodeCount(): number {
  return readKey(NODE_KEY, 3, 12);
}

export function writeLastNodeCount(n: number): void {
  writeKey(NODE_KEY, n);
}

// Browse-grid size - written on every successful Hugging Face search, read
// for the initial-load placeholders (result count is query-dependent, so
// this approximates the default result set, not any single query).
export function readLastModelCount(): number {
  return readKey(MODEL_KEY, 6, 9);
}

export function writeLastModelCount(n: number): void {
  writeKey(MODEL_KEY, n);
}
