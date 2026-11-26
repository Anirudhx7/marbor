// wholeNumber returns a filter value only when the server will accept it: a
// whole number above zero that is exactly representable. Anything else means
// no filter.
export function wholeNumber(raw: string): number | undefined {
  const n = raw ? Number(raw) : NaN;
  return Number.isInteger(n) && n > 0 && n <= Number.MAX_SAFE_INTEGER ? n : undefined;
}

// realDate returns the value only when it is a real calendar date written as
// YYYY-MM-DD (rejects 2024-02-30 and similar), so the server never 400s on it.
export function realDate(raw: string): string | undefined {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(raw)) return undefined;
  const d = new Date(`${raw}T00:00:00Z`);
  if (Number.isNaN(d.getTime())) return undefined;
  return d.toISOString().slice(0, 10) === raw ? raw : undefined;
}
