import { useEffect, useState, useSyncExternalStore } from 'react';
import { Download, CheckCircle2, XCircle, X, Loader2, ChevronUp, ChevronDown, Trash2, AlertTriangle } from 'lucide-react';
import { subscribe, getSnapshot, retryPull, cancelPull, closeJob, restoreActivePulls, isPullActive, PullProgressState } from '../lib/pullProgress';
import { deleteNodeModel } from '../lib/api';

function formatBytes(n: number): string {
  if (n <= 0) return '0 B';
  const gb = n / (1024 * 1024 * 1024);
  if (gb >= 1) return `${gb.toFixed(2)} GB`;
  const mb = n / (1024 * 1024);
  return `${mb.toFixed(0)} MB`;
}

function formatSpeed(bps: number): string {
  const mbps = bps / (1024 * 1024);
  return `${mbps.toFixed(1)} MB/s`;
}

function formatDuration(seconds: number): string {
  if (!isFinite(seconds) || seconds < 0) return '';
  const m = Math.floor(seconds / 60);
  const s = Math.floor(seconds % 60);
  return m > 0 ? `${m}m ${s}s` : `${s}s`;
}

// verifyModelLoads (admin.go) surfaces the runtime's real error verbatim as
// "HTTP <code>: <raw response body>" - usually JSON like
// {"error":{"message":"..."}}. Pulling the human-readable message out of
// that wrapper (when it parses) is presentation only - the raw text is still
// shown as a fallback, never silently dropped, so this can't hide a real
// error the JSON shape doesn't match.
function parseLoadError(raw: string): { status: string | null; message: string } {
  const match = raw.match(/^HTTP (\d+):\s*([\s\S]*)$/);
  if (!match) return { status: null, message: raw };
  const [, status, body] = match;
  const trimmedBody = body.trim();
  try {
    const parsed = JSON.parse(trimmedBody);
    const message = parsed?.error?.message ?? parsed?.message;
    if (typeof message === 'string' && message.trim()) {
      return { status, message };
    }
  } catch {
    // Not JSON (or not the expected shape) - fall through to the raw body.
  }
  return { status, message: trimmedBody || raw };
}

// classifyLoadFailure interprets the real error text into a specific,
// accurate explanation - never a blanket guess. Different causes need
// genuinely different guidance (an embedding model needs a different model
// entirely; a VRAM shortage needs freeing space, not a different model), and
// always attributing failure to "architecture isn't supported" would be
// false for those cases (R1: never present an unverified guess as fact).
// Falls back to an honest "cause not identified" message rather than
// defaulting to architecture when the error text doesn't match a
// recognized pattern.
function classifyLoadFailure(message: string): string {
  const lower = message.toLowerCase();
  if (lower.includes('does not support chat')) {
    return "This model doesn't support chat/text-generation requests - it's likely an embedding-only model. Pick a text-generation model instead.";
  }
  if (/\bout of memory\b|insufficient (system )?memory|requires more (system )?memory|\boom\b/.test(lower)) {
    return 'This node likely ran out of free VRAM/memory to load this model. Free up VRAM (unload another model) or try a smaller quantization.';
  }
  if (lower.includes('unable to load model') || lower.includes('failed to load')) {
    return "Ollama couldn't load this model but didn't report a specific reason. Common causes: an unsupported architecture, a corrupted download, or insufficient VRAM - check this node's Ollama logs for the exact cause.";
  }
  return "The exact cause isn't automatically identified from this error - check this node's Ollama logs for more detail.";
}

// One card per tracked pull. Node name is always shown up front - with
// multiple GPU nodes (or multiple models pulling on the same node) in the
// stack at once, the node is the thing that tells them apart.
function PullJobCard({ job }: { job: PullProgressState }) {
  const [expanded, setExpanded] = useState(true);
  const [confirmingCancel, setConfirmingCancel] = useState(false);
  const [elapsedMs, setElapsedMs] = useState(0);
  const [deleting, setDeleting] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [deleted, setDeleted] = useState(false);

  useEffect(() => {
    if (!isPullActive(job.status)) return;
    const id = setInterval(() => setElapsedMs(Date.now() - job.startedAtMs), 500);
    return () => clearInterval(id);
  }, [job.startedAtMs, job.status]);

  // A finished job (success/failed/load_failed/cancelled) should drop the
  // cancel-confirm dialog if it was open - e.g. the pull completed naturally
  // right as the admin clicked Cancel.
  useEffect(() => {
    if (!isPullActive(job.status)) setConfirmingCancel(false);
  }, [job.status]);

  const hasBytes = job.bytesTotal > 0;
  const pct = hasBytes ? Math.min(100, (job.bytesCompleted / job.bytesTotal) * 100) : null;
  const eta = hasBytes && job.speedBps > 0 ? (job.bytesTotal - job.bytesCompleted) / job.speedBps : null;

  const handleClose = () => {
    if (isPullActive(job.status)) {
      setConfirmingCancel(true);
      return;
    }
    closeJob(job.key);
  };

  const handleDelete = async () => {
    setDeleting(true);
    setDeleteError(null);
    try {
      await deleteNodeModel(job.node, job.model);
      setDeleted(true);
    } catch (e: unknown) {
      setDeleteError(e instanceof Error ? e.message : 'Failed to delete model');
    } finally {
      setDeleting(false);
    }
  };

  const icon = isPullActive(job.status) ? (
    <Loader2 className="w-4 h-4 text-primary animate-spin shrink-0" />
  ) : job.status === 'success' ? (
    <CheckCircle2 className="w-4 h-4 text-success shrink-0" />
  ) : (
    <XCircle className="w-4 h-4 text-destructive shrink-0" />
  );

  return (
    <div className="w-auto min-w-80 max-w-[min(28rem,calc(100vw-2rem))] bg-card border border-border shadow-lg rounded-xl overflow-hidden">
      {/* Collapsed header - always visible, click toggles expand/collapse */}
      <button
        onClick={() => setExpanded((v) => !v)}
        className="w-full flex items-center gap-2 px-3 py-2.5 text-left cursor-pointer hover:bg-secondary/50 transition-colors"
      >
        {icon}
        <span className="flex-1 min-w-0 text-sm font-medium text-foreground truncate" title={`${job.node} · ${job.model}`}>
          {job.model}
        </span>
        {pct !== null && job.status === 'downloading' && (
          <span className="text-xs text-muted-foreground shrink-0">{pct.toFixed(0)}%</span>
        )}
        {job.status === 'verifying' && (
          <span className="text-xs text-muted-foreground shrink-0">verifying…</span>
        )}
        {expanded ? (
          <ChevronDown className="w-3.5 h-3.5 text-muted-foreground shrink-0" />
        ) : (
          <ChevronUp className="w-3.5 h-3.5 text-muted-foreground shrink-0" />
        )}
      </button>

      {expanded && (
        <div className="px-3 pb-3 border-t border-border pt-3">
          <p className="text-xs text-muted-foreground mb-2">
            <span className="font-semibold text-foreground">{job.node}</span>
            {' · '}
            {job.method === 'agent' ? 'via Node Agent' : 'direct'}
          </p>

          {job.status === 'downloading' && (
            <>
              {hasBytes ? (
                <>
                  <div className="w-full h-1.5 bg-secondary rounded-full overflow-hidden mb-2">
                    <div
                      className="h-full bg-primary transition-all duration-300"
                      style={{ width: `${pct}%` }}
                    />
                  </div>
                  <div className="flex justify-between text-xs text-muted-foreground">
                    <span>{formatBytes(job.bytesCompleted)} / {formatBytes(job.bytesTotal)}</span>
                    <span>
                      {job.speedBps > 0 ? formatSpeed(job.speedBps) : '-'}
                      {eta !== null && ` · ${formatDuration(eta)} left`}
                    </span>
                  </div>
                </>
              ) : (
                <>
                  <div className="w-full h-1.5 bg-secondary rounded-full overflow-hidden mb-2">
                    <div className="h-full w-1/3 bg-primary rounded-full animate-pulse" />
                  </div>
                  <p className="text-xs text-muted-foreground">
                    Downloading… {formatDuration(elapsedMs / 1000)} elapsed
                  </p>
                </>
              )}
            </>
          )}

          {job.status === 'verifying' && (
            <>
              <div className="w-full h-1.5 bg-secondary rounded-full overflow-hidden mb-2">
                <div className="h-full w-1/3 bg-primary rounded-full animate-pulse" />
              </div>
              <p className="text-xs text-muted-foreground">
                Verifying it actually loads… {formatDuration(elapsedMs / 1000)} elapsed
              </p>
            </>
          )}

          {job.status === 'success' && (
            <p className="text-xs text-success font-medium">Pull complete.</p>
          )}

          {(job.status === 'failed' || job.status === 'cancelled') && (
            <p className="text-xs text-destructive font-medium">
              {job.status === 'cancelled' ? 'Cancelled.' : job.error || 'Pull failed.'}
            </p>
          )}

          {job.status === 'load_failed' && (() => {
            const { status, message } = parseLoadError(job.error || '');
            return (
              <div className="rounded-lg border border-destructive/25 bg-destructive/5 p-2.5">
                <div className="flex items-center gap-1.5 mb-1.5">
                  <AlertTriangle className="w-3.5 h-3.5 text-destructive shrink-0" />
                  <span className="text-xs font-semibold text-destructive">
                    Downloaded, but failed to load{status ? ` · HTTP ${status}` : ''}
                  </span>
                </div>
                <p className="text-[11px] text-foreground/80 font-mono leading-normal break-words">
                  {message || 'Unknown load error.'}
                </p>
                <p className="text-[11px] text-muted-foreground mt-2 leading-normal">
                  {classifyLoadFailure(message)}
                </p>
                {deleted ? (
                  <p className="text-[11px] text-success mt-1.5">Deleted from {job.node}.</p>
                ) : deleteError ? (
                  <p className="text-[11px] text-destructive mt-1.5">{deleteError}</p>
                ) : null}
              </div>
            );
          })()}

          {confirmingCancel ? (
            <div className="mt-3 flex items-center gap-2">
              <span className="text-xs text-foreground flex-1">Discard this download?</span>
              <button
                onClick={() => setConfirmingCancel(false)}
                className="px-2.5 py-1 text-xs bg-secondary hover:bg-secondary/80 rounded-lg text-foreground cursor-pointer"
              >
                Never mind
              </button>
              <button
                onClick={() => {
                  cancelPull(job.key);
                  setConfirmingCancel(false);
                }}
                className="px-2.5 py-1 text-xs bg-destructive/10 hover:bg-destructive/20 text-destructive rounded-lg cursor-pointer"
              >
                Discard
              </button>
            </div>
          ) : (
            <div className="mt-3 flex items-center gap-2 justify-end">
              {job.status === 'failed' && (
                <button
                  onClick={() => retryPull(job.key)}
                  className="flex items-center gap-1 px-2.5 py-1 text-xs bg-primary/10 hover:bg-primary/20 text-primary rounded-lg cursor-pointer"
                >
                  <Download className="w-3 h-3" /> Retry
                </button>
              )}
              {job.status === 'load_failed' && !deleted && (
                <button
                  onClick={handleDelete}
                  disabled={deleting}
                  className="flex items-center gap-1 px-2.5 py-1 text-xs bg-destructive/10 hover:bg-destructive/20 disabled:opacity-50 text-destructive rounded-lg cursor-pointer"
                >
                  {deleting ? <Loader2 className="w-3 h-3 animate-spin" /> : <Trash2 className="w-3 h-3" />}
                  Delete model
                </button>
              )}
              <button
                onClick={handleClose}
                className="flex items-center gap-1 px-2.5 py-1 text-xs bg-secondary hover:bg-secondary/80 text-foreground rounded-lg cursor-pointer"
              >
                <X className="w-3 h-3" /> {isPullActive(job.status) ? 'Cancel' : 'Close'}
              </button>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

// Browser-download-style stack: one card per tracked pull, newest on top.
// Global - rendered once at the app shell root (App.tsx) so it survives page
// navigation; a pull started on ModelAdvisor keeps tracking if the admin
// navigates to Models mid-download, and pulling several models across
// several GPU nodes at once shows every one of them here simultaneously.
export function PullProgressWidget() {
  const jobs = useSyncExternalStore(subscribe, getSnapshot);

  // Reload wipes pullProgress.ts's in-memory job map, even for pulls still
  // running server-side - ask the mesh what's still in flight and resubscribe,
  // once, the first time the app shell mounts.
  useEffect(() => {
    restoreActivePulls();
  }, []);

  if (jobs.length === 0) return null;

  return (
    <div className="fixed bottom-4 right-4 z-50 flex flex-col-reverse gap-2 items-end">
      {jobs.map((job) => (
        <PullJobCard key={job.key} job={job} />
      ))}
    </div>
  );
}
