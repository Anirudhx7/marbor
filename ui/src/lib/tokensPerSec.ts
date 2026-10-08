// Pure derivation for the Metrics tokens per second chart. An hour counts as
// measured only when the server recorded generation time for it
// (gen_duration_ms > 0); every other hour is null so the chart shows a gap.
// A rate of 0 is never produced for an unmeasured hour.

export interface TpsInput {
  hour: string;
  tokens_per_sec?: number;
  gen_duration_ms?: number;
}

export interface TpsPoint {
  hour: string;
  tps: number | null;
}

export function toTpsSeries(hourly: readonly TpsInput[]): TpsPoint[] {
  return hourly.map(b => {
    const measured =
      typeof b.gen_duration_ms === 'number' &&
      b.gen_duration_ms > 0 &&
      typeof b.tokens_per_sec === 'number' &&
      Number.isFinite(b.tokens_per_sec);
    return { hour: b.hour, tps: measured ? (b.tokens_per_sec as number) : null };
  });
}
