// vLLM and TGI serve one launched model, so a pull there only downloads the
// weights. The helpers here are pure (no imports, no browser APIs) so they can be
// unit tested with `node --test` and shared by every pull entry point.

export const DOWNLOAD_ONLY_NOTE = 'Downloaded. Relaunch the runtime to serve it.';

export const DEFAULT_PULL_COMPLETE_TEXT = 'Pull complete.';

export function isDownloadOnlyRuntime(runtime: string | undefined | null): boolean {
  return runtime === 'vllm' || runtime === 'tgi';
}

// pullOptionsFor returns the verifyLoad flag and completion note a pull on this
// runtime must use: the load check is forced off (it would probe a model that is
// not being served) and the note says to relaunch. Other runtimes keep the
// caller's verify choice and the generic completion text.
export function pullOptionsFor(
  runtime: string | undefined | null,
  verifyLoad: boolean,
): { verifyLoad: boolean; completionNote: string } {
  if (isDownloadOnlyRuntime(runtime)) return { verifyLoad: false, completionNote: DOWNLOAD_ONLY_NOTE };
  return { verifyLoad, completionNote: '' };
}

// completionText is what the pull widget shows on success.
export function completionText(completionNote: string | undefined): string {
  return completionNote || DEFAULT_PULL_COMPLETE_TEXT;
}

// sizeNotCurated is true for a vLLM/TGI row that cannot be sized because its
// Hugging Face repo has a variant with no curated size (missing or zero size on
// the first variant). A row with no variants at all says nothing about sizing.
export function sizeNotCurated(
  downloadOnly: boolean,
  picked: boolean,
  reason: string | undefined,
  variants: { size_mb?: number }[] | undefined,
): boolean {
  return downloadOnly && !picked && reason === 'vram_unknown' && (variants?.length ?? 0) >= 1 && !variants?.[0]?.size_mb;
}

// retryArgs maps a finished pull job to the arguments of a fresh pull, so a retry
// keeps the original verify choice and completion note.
export function retryArgs(job: { node: string; model: string; verifyLoad: boolean; completionNote?: string; simulating?: boolean }): {
  node: string;
  model: string;
  verifyLoad: boolean;
  completionNote: string;
  simulate: boolean;
} {
  // A simulated (demo) pull retries as a simulated pull, never against the real API.
  return { node: job.node, model: job.model, verifyLoad: job.verifyLoad, completionNote: job.completionNote ?? '', simulate: job.simulating === true };
}
