// Shares one in-flight request between concurrent callers. Every caller gets
// its own structured clone so one consumer mutating the result cannot leak into
// another. The slot clears when the request settles (no time-based caching);
// reset() drops it early so a read issued after a write is never served by a
// request that began before the write.
export function dedupeInflight<T>(fn: () => Promise<T>): { call: () => Promise<T>; reset: () => void } {
  let slot: Promise<T> | null = null;
  return {
    call: () => {
      if (!slot) {
        const p: Promise<T> = fn().finally(() => {
          if (slot === p) slot = null;
        });
        slot = p;
      }
      return slot.then((v) => structuredClone(v));
    },
    reset: () => {
      slot = null;
    },
  };
}
