// Which loaded Ollama models get a "partly on CPU" badge, and how the loaded
// context is written. Pure so the node:test suite can import it.
//
// gpuPercent and contextLength are reported by Ollama's /api/ps and are absent
// (null/undefined) for other runtimes and older Ollama builds. Absent never
// shows anything. All checks use != null, never truthiness, because 0 is a real
// reading.

export interface ModelShare {
  gpuPercent?: number | null;
  contextLength?: number | null;
}

export interface ShareNode {
  vramTotalMB?: number | null;
  loadedModels?: ModelShare[] | null;
}

export const PARTIAL_GPU_TOOLTIP =
  'Part of this model runs on CPU, so it is slower than a full GPU load. ' +
  'Lower num_ctx or free GPU memory. 0% can also mean this node has no GPU for Ollama.';

// showPartialGpuBadge is true for 0 < gpuPercent < 100 on any node. A reading of
// exactly 0 only warns where a GPU is known: the node reports VRAM, or another
// resident model on it is on the GPU. Otherwise it is likely a CPU-only node.
export function showPartialGpuBadge(model: ModelShare, node: ShareNode): boolean {
  const pct = model.gpuPercent;
  if (pct == null || !Number.isFinite(pct) || pct < 0 || pct >= 100) return false;
  if (pct > 0) return true;
  if ((node.vramTotalMB ?? 0) > 0) return true;
  return (node.loadedModels ?? []).some(m => m !== model && m.gpuPercent != null && Number.isFinite(m.gpuPercent) && m.gpuPercent > 0);
}

// formatContext writes a loaded context window compactly (4096 -> "4k",
// 32768 -> "32k", 131072 -> "128k"). Returns null when not reported.
export function formatContext(contextLength: number | null | undefined): string | null {
  if (contextLength == null || !Number.isFinite(contextLength) || !(contextLength > 0)) return null;
  if (contextLength >= 1024) {
    const k = contextLength / 1024;
    return `${k.toFixed(1).replace(/\.0$/, '')}k`;
  }
  return String(contextLength);
}
