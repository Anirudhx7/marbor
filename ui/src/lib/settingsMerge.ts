// Helpers shared by the settings API calls.

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

// deepMerge returns base with patch merged in: nested objects merge key by key,
// anything else (arrays, scalars, null) in patch replaces the base value. Neither
// input is changed. The demo build uses it so a partial settings save keeps the
// keys it did not mention, as the live server does.
export function deepMerge(base: unknown, patch: unknown): unknown {
  if (!isPlainObject(base) || !isPlainObject(patch)) return patch;
  const out: Record<string, unknown> = { ...base };
  for (const [k, v] of Object.entries(patch)) {
    if (k === '__proto__') continue;
    out[k] = k in out ? deepMerge(out[k], v) : v;
  }
  return out;
}

// serverErrorText returns the server's own error text from a JSON error body,
// or fallback when there is none.
export function serverErrorText(body: unknown, fallback: string): string {
  if (isPlainObject(body) && typeof body.error === 'string' && body.error.trim() !== '') {
    return body.error;
  }
  return fallback;
}
