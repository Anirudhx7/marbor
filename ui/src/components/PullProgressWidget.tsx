import { useEffect, useState, useSyncExternalStore } from 'react';
import { Download, CheckCircle2, XCircle, X, Loader2, ChevronUp, ChevronDown } from 'lucide-react';
import { subscribe, getSnapshot, retryPull, cancelPull, closeJob, restoreActivePulls, PullProgressState } from '../lib/pullProgress';

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

// One card per tracked pull. Node name is always shown up front - with
// multiple GPU nodes (or multiple models pulling on the same node) in the
// stack at once, the node is the thing that tells them apart.
function PullJobCard({ job }: { job: PullProgressState }) {
  const [expanded, setExpanded] = useState(true);
  const [confirmingCancel, setConfirmingCancel] = useState(false);
  const [elapsedMs, setElapsedMs] = useState(0);

  useEffect(() => {
    if (job.status !== 'downloading') return;
    const id = setInterval(() => setElapsedMs(Date.now() - job.startedAtMs), 500);
    return () => clearInterval(id);
  }, [job.startedAtMs, job.status]);

  // A finished job (success/failed/cancelled) should drop the cancel-confirm
  // dialog if it was open - e.g. the pull completed naturally right as the
  // admin clicked Cancel.
  useEffect(() => {
    if (job.status !== 'downloading') setConfirmingCancel(false);
  }, [job.status]);

  const hasBytes = job.bytesTotal > 0;
  const pct = hasBytes ? Math.min(100, (job.bytesCompleted / job.bytesTotal) * 100) : null;
  const eta = hasBytes && job.speedBps > 0 ? (job.bytesTotal - job.bytesCompleted) / job.speedBps : null;

  const handleClose = () => {
    if (job.status === 'downloading') {
      setConfirmingCancel(true);
      return;
    }
    closeJob(job.key);
  };

  const icon =
    job.status === 'downloading' ? (
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

          {job.status === 'success' && (
            <p className="text-xs text-success font-medium">Pull complete.</p>
          )}

          {(job.status === 'failed' || job.status === 'cancelled') && (
            <p className="text-xs text-destructive font-medium">
              {job.status === 'cancelled' ? 'Cancelled.' : job.error || 'Pull failed.'}
            </p>
          )}

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
              <button
                onClick={handleClose}
                className="flex items-center gap-1 px-2.5 py-1 text-xs bg-secondary hover:bg-secondary/80 text-foreground rounded-lg cursor-pointer"
              >
                <X className="w-3 h-3" /> {job.status === 'downloading' ? 'Cancel' : 'Close'}
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
