// Backends that store prices as float32 hand back values like
// 0.0020000000949949026 for 0.002. Round to 6 decimals (finer than any
// $/1k-token price an operator would enter) so inputs never show the artifact.
export function roundPrice(value: number): number {
  return Number.isFinite(value) ? Number(value.toFixed(6)) : 0;
}
