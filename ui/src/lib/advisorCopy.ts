// Copy and small predicates for the Model Advisor that must stay true to what
// the server actually does. Pure (no imports, no browser APIs) so they can be
// unit tested with `node --test`.

// Mirrors the runtimes hfSearchFilters covers in internal/admin/catalog.go:
// Ollama and llama.cpp search the gguf format (an empty runtime defaults to
// Ollama), vLLM, TGI and MLX search one format with a text/vision tag filter.
// Any other runtime gets no filtering at all, so no claim is made for it.
const GGUF_SEARCH_RUNTIMES = ['', 'ollama', 'llamacpp'];
const TAG_FILTERED_SEARCH_RUNTIMES = ['vllm', 'tgi', 'mlx'];

const isGGUFSearch = (runtime: string | null | undefined) =>
  GGUF_SEARCH_RUNTIMES.includes(runtime ?? '');

// browseHelpClaim is the honest sentence about how Browse results are narrowed
// for the browsed runtime. The GGUF filter is best-effort: it hides repos whose
// Hugging Face tag is speech, image, video or detection, and repos without a
// tag are still listed.
export function browseHelpClaim(runtime: string | null | undefined): string {
  if (isGGUFSearch(runtime)) {
    return 'Results leave out models tagged as speech, image, video or detection; repos without a tag are shown.';
  }
  if (TAG_FILTERED_SEARCH_RUNTIMES.includes(runtime ?? '')) {
    return 'Results are limited to text and vision-language models.';
  }
  return '';
}

// browseEmptyCopy is the empty-state hint for a Browse search with no rows. For
// GGUF runtimes it says media models are hidden so an empty list does not read
// as a broken search.
export function browseEmptyCopy(runtime: string | null | undefined): string {
  const hint = 'Try searching for "llama" or "gemma".';
  if (isGGUFSearch(runtime)) {
    return `${hint} Speech, image and video models are hidden for this runtime.`;
  }
  return hint;
}

// nodeVramUnknown is true when the Fit column cannot be computed for a node:
// no VRAM total, or a total that is only inferred rather than declared or
// detected. The server's fit classifier returns Unknown for exactly these.
export function nodeVramUnknown(node: { vram_total_bytes: number; vram_source: string }): boolean {
  return !(node.vram_total_bytes > 0) || node.vram_source === 'unknown' || node.vram_source === 'inferred';
}

// curatedFitTitle names the curated-model panel. "What fits" is only honest
// when VRAM is known and curated models are offered for the runtime.
export function curatedFitTitle(nodeName: string, vramUnknown: boolean, incompatible: boolean): string {
  return vramUnknown || incompatible ? `Curated models for ${nodeName}` : `What fits ${nodeName}`;
}

// Shown once per page (Browse and Favourites) when the selected node's VRAM is
// unknown, because every Fit badge on the page reads Unknown for that reason.
export const VRAM_UNKNOWN_NOTE =
  "Fit needs this node's VRAM, so Fit shows Unknown. Declare it in Edit node > Capacity overrides, or enable the agent.";
