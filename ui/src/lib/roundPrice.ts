// Price rounding and a rate-limit label, shared by the API key forms and tables.
// Backends that store prices as float32 hand back values like
// 0.0020000000949949026 for 0.002. Round to 6 decimals (finer than any
// $/1k-token price an operator would enter) so inputs never show the artifact.
export function roundPrice(value: number): number {
  return Number.isFinite(value) ? Number(value.toFixed(6)) : 0;
}

// Rate limit label for an API key. 0 (or unset) means no cap, so show
// "Unlimited" instead of a misleading "0/hr". A value that is not a real number
// shows "-" rather than claiming the key is unlimited.
export function formatRateLimit(n?: number | null): string {
  if (n == null) return 'Unlimited';
  if (!Number.isFinite(n)) return '-';
  return n <= 0 ? 'Unlimited' : `${n.toLocaleString()}/hr`;
}
