// Global, page-independent model-pull progress tracker - browser-download-
// style: a pull started from Models.tsx or ModelAdvisor.tsx keeps tracking
// even after navigating away, because it lives outside React's component
// tree (a module-level store, not page state). PullProgressWidget (rendered
// once at the app shell root) is the only consumer that needs to subscribe.
//
// Multiple pulls track concurrently, keyed by "node|model" - marbor itself
// only dedups by that same key (server-side, admin.go's pullJobs map), so a
// GPU shop pulling different models on different nodes (or several models on
// one node) sees every one of them, distinguished by node name, in the
// widget stack at once.

import { normalizePullTag, apiFetch } from './api';

const BASE = '/admin';

export type PullStatus = 'downloading' | 'verifying' | 'success' | 'failed' | 'load_failed' | 'cancelled';

// isPullActive mirrors admin.go's pullJobActive: "downloading" and
// "verifying" are the only non-terminal states - every stream-lifecycle
// decision (when to close the SSE connection, when to reconnect, when to
// offer Cancel vs Close) keys off this instead of a hardcoded
// status === 'downloading' check, so a verify_load pull's post-download
// probe phase doesn't get treated as already finished.
export function isPullActive(status: PullStatus): boolean {
  return status === 'downloading' || status === 'verifying';
}

export interface PullProgressState {
  key: string;
  node: string;
  model: string;
  method: 'direct' | 'agent' | '';
  status: PullStatus;
  bytesTotal: number;
  bytesCompleted: number;
  error: string;
  // verifyLoad records whether this pull opted into a post-download
  // load-verification probe (see admin.go's completePull/verifyModelLoads) -
  // carried on the job so retryPull can preserve the same choice rather than
  // silently reverting to unverified on a dropped-connection retry.
  verifyLoad: boolean;
  // simulating records whether THIS job is a simulated/demo pull (build-time
  // VITE_FORCE_DEMO, or the caller's runtime Demo Mode toggle at the moment
  // startPull was called) - cancelPull must branch on this per-job flag, not
  // on the build-time DEMO constant, or a runtime-Demo-Mode pull's Cancel
  // button fires a real authenticated DELETE for a job that doesn't exist
  // server-side.
  simulating: boolean;
  startedAtMs: number;
  // speedBps is real, derived from consecutive server-reported byte counts
  // (never fabricated - R1) - stays 0 until the direct-to-Ollama path
  // reports at least two progress samples with an actual delta, and decays
  // back to 0 if no new bytes arrive for a few ticks (a stalled transfer
  // should not keep showing its last-known rate as if still live). The
  // agent path never reports bytes at all, so this never populates there -
  // the widget shows elapsed time only in that case.
  speedBps: number;
  lastSampleMs: number;
  lastSampleBytes: number;
  staleTicks: number;
}

type Listener = () => void;

function jobKey(node: string, model: string): string {
  return `${node} ${model}`;
}

const jobs = new Map<string, PullProgressState>();
const eventSources = new Map<string, EventSource>();
const listeners = new Set<Listener>();
let snapshotArray: PullProgressState[] = [];

// reconnectAttempts: how many times subscribeToProgress has had to
// re-open the SSE connection for a given job after a drop. Not part of
// PullProgressState - it's connection-management bookkeeping, not
// something the widget displays.
const reconnectAttempts = new Map<string, number>();
const MAX_RECONNECT_ATTEMPTS = 5;
const RECONNECT_BASE_DELAY_MS = 1000;

// completionListeners: separate from `listeners` (which fires on every
// progress tick) - pages like Models.tsx only care about the moment a pull
// finishes successfully, so they can refetch the catalog immediately instead
// of waiting on their own poll interval.
type CompletionListener = (node: string, model: string) => void;
const completionListeners = new Set<CompletionListener>();

export function onPullSuccess(listener: CompletionListener): () => void {
  completionListeners.add(listener);
  return () => completionListeners.delete(listener);
}

function notifySuccess(node: string, model: string) {
  for (const l of completionListeners) l(node, model);
}

function rebuildSnapshot() {
  snapshotArray = Array.from(jobs.values()).sort((a, b) => b.startedAtMs - a.startedAtMs);
}

function notify() {
  rebuildSnapshot();
  for (const l of listeners) l();
}

function setJob(key: string, patch: Partial<PullProgressState>) {
  const job = jobs.get(key);
  if (!job) return;
  jobs.set(key, { ...job, ...patch });
  notify();
}

export function subscribe(listener: Listener): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

export function getSnapshot(): PullProgressState[] {
  return snapshotArray;
}

function closeStream(key: string) {
  const es = eventSources.get(key);
  if (es) {
    es.close();
    eventSources.delete(key);
  }
}

// DEMO mirrors the VITE_FORCE_DEMO check every other api.ts call site uses
// for the public GitHub Pages demo (no backend to POST/stream against).
// Distinct from the in-app "Demo Mode" toggle (useDemoMode(), backed by
// localStorage) - callers pass that runtime value in via `simulate` so a
// backend-connected build never fires a real download while an operator has
// merely switched the dashboard into its own demo view.
const DEMO = import.meta.env.VITE_FORCE_DEMO === 'true';

function runDemoPull(key: string, node: string, model: string, verifyLoad: boolean) {
  const total = 4_200_000_000; // plausible fixed size, clearly a demo fixture
  let completed = 0;
  const startedAtMs = Date.now();
  jobs.set(key, {
    key, node, model, method: 'direct', status: 'downloading', verifyLoad, simulating: true,
    bytesTotal: total, bytesCompleted: 0, error: '',
    startedAtMs, speedBps: 0, lastSampleMs: startedAtMs, lastSampleBytes: 0, staleTicks: 0,
  });
  notify();

  const finishSuccess = () => {
    setJob(key, { status: 'success' });
    notifySuccess(node, model);
  };

  const step = () => {
    const job = jobs.get(key);
    if (!job || job.status !== 'downloading') return;
    completed = Math.min(total, completed + total * 0.08);
    const now = Date.now();
    const deltaBytes = completed - job.lastSampleBytes;
    const deltaSec = (now - job.lastSampleMs) / 1000;
    setJob(key, {
      bytesCompleted: completed,
      speedBps: deltaSec > 0 ? deltaBytes / deltaSec : job.speedBps,
      lastSampleMs: now,
      lastSampleBytes: completed,
    });
    if (completed >= total) {
      if (!verifyLoad) {
        finishSuccess();
        return;
      }
      // Demo illustrates the real flow's extra phase without a backend to
      // actually probe - always resolves to success, matching every other
      // demo fixture's "idealized but plausible" convention (never a
      // fabricated failure - R1 applies to demo data too).
      setJob(key, { status: 'verifying' });
      setTimeout(finishSuccess, 900);
      return;
    }
    setTimeout(step, 400);
  };
  setTimeout(step, 400);
}

// startPull begins tracking a new pull. `simulate` should be the caller's
// runtime demo-mode flag (useDemoMode()) - passed in explicitly rather than
// read from here, since this module has no React context of its own.
// `verifyLoad` opts into a post-download load-verification probe (see
// admin.go's completePull) before the job reports "success" - catches a
// model whose architecture downloads fine but can't actually be loaded by
// this node's installed runtime.
export function startPull(node: string, model: string, simulate: boolean = false, verifyLoad: boolean = false): void {
  const tag = normalizePullTag(model);
  const key = jobKey(node, tag);
  closeStream(key);
  const startedAtMs = Date.now();
  jobs.set(key, {
    key, node, model: tag, method: '', status: 'downloading', verifyLoad, simulating: DEMO || simulate,
    bytesTotal: 0, bytesCompleted: 0, error: '',
    startedAtMs, speedBps: 0, lastSampleMs: startedAtMs, lastSampleBytes: 0, staleTicks: 0,
  });
  notify();

  if (DEMO || simulate) {
    runDemoPull(key, node, tag, verifyLoad);
    return;
  }

  apiFetch(`${BASE}/v1/nodes/${encodeURIComponent(node)}/pull`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ model: tag, verify_load: verifyLoad }),
  })
    .then(async (res) => {
      if (res.status === 409) {
        // The marbor confirms a pull for this key is still running server-side
        // - this is exactly the state a retry-after-a-dropped-stream lands
        // in (see subscribeToProgress's onerror), not a real conflict from
        // the admin's perspective. Resubscribe to its progress instead of
        // reporting a failure for a download that's still (or already)
        // succeeding.
        subscribeToProgress(key, node, tag);
        return;
      }
      if (!res.ok) {
        const body = await res.json().catch(() => ({}));
        setJob(key, { status: 'failed', error: body?.error || `Pull failed: ${res.statusText}` });
        return;
      }
      subscribeToProgress(key, node, tag);
    })
    .catch((err) => {
      setJob(key, { status: 'failed', error: err instanceof Error ? err.message : 'Pull failed to start' });
    });
}

// STALE_TICK_LIMIT: after this many consecutive SSE ticks with no new bytes,
// the displayed speed decays to 0 rather than showing a stale last-known
// rate as if the transfer were still moving at that pace.
const STALE_TICK_LIMIT = 3;

function subscribeToProgress(key: string, node: string, model: string): void {
  // Re-entrant callers (409-resubscribe, reconnect timer racing a fresh
  // start, restoreActivePulls) must never leave a prior EventSource for this
  // same key alive - two live streams under one key would both mutate
  // shared state and double-fire completion notifications.
  closeStream(key);
  const url = `${BASE}/v1/nodes/${encodeURIComponent(node)}/pull/progress?model=${encodeURIComponent(model)}`;
  const es = new EventSource(url, { withCredentials: true });
  eventSources.set(key, es);

  es.onmessage = (evt) => {
    const job = jobs.get(key);
    if (!job) return;
    let data: any;
    try {
      data = JSON.parse(evt.data);
    } catch {
      return;
    }
    const now = Date.now();
    const bytesTotal: number = data.bytes_total || 0;
    const bytesCompleted: number = data.bytes_completed || 0;
    const deltaBytes = bytesCompleted - job.lastSampleBytes;
    const deltaSec = (now - job.lastSampleMs) / 1000;
    const gotSample = bytesTotal > 0 && deltaSec > 0.2;
    const movedBytes = gotSample && deltaBytes > 0;
    const staleTicks = movedBytes ? 0 : job.staleTicks + (gotSample ? 1 : 0);

    setJob(key, {
      method: data.method || job.method,
      status: data.status,
      bytesTotal,
      bytesCompleted,
      error: data.error || '',
      speedBps: movedBytes ? deltaBytes / deltaSec : staleTicks >= STALE_TICK_LIMIT ? 0 : job.speedBps,
      lastSampleMs: gotSample ? now : job.lastSampleMs,
      lastSampleBytes: gotSample ? bytesCompleted : job.lastSampleBytes,
      staleTicks,
    });

    // A message came through cleanly - this connection is healthy, so any
    // prior drop's retry count no longer applies.
    reconnectAttempts.delete(key);
    if (!isPullActive(data.status)) {
      closeStream(key);
    }
    if (data.status === 'success') {
      notifySuccess(node, model);
    }
  };

  es.onerror = () => {
    const job = jobs.get(key);
    if (!job || !isPullActive(job.status)) {
      closeStream(key);
      return;
    }
    // Native EventSource auto-reconnects on its own for a transient drop -
    // readyState is CONNECTING while it retries. Only readyState CLOSED
    // means the browser has actually given up on this connection.
    if (es.readyState === EventSource.CONNECTING) {
      return;
    }
    closeStream(key);

    // The pull job itself runs server-side (runDirectPull/pullModelViaAgent
    // goroutines), entirely independent of this SSE connection - a dropped
    // stream is almost always a network blip, a sleeping tab, or an
    // intermediate proxy's idle timeout, not the download failing. Re-open
    // the stream (a stateless GET against the current job snapshot - safe
    // and idempotent) rather than declaring the pull failed while it may
    // still be downloading, or may have already finished successfully.
    const attempts = (reconnectAttempts.get(key) || 0) + 1;
    reconnectAttempts.set(key, attempts);
    if (attempts > MAX_RECONNECT_ATTEMPTS) {
      reconnectAttempts.delete(key);
      setJob(key, {
        status: 'failed',
        error: 'Lost connection to the progress stream. The download may still be running on the node - check the Models page before retrying.',
      });
      return;
    }
    setTimeout(() => {
      const stillTracked = jobs.get(key);
      if (stillTracked && isPullActive(stillTracked.status)) {
        subscribeToProgress(key, job.node, job.model);
      }
    }, RECONNECT_BASE_DELAY_MS * attempts);
  };
}

export function retryPull(key: string): void {
  const job = jobs.get(key);
  if (!job) return;
  startPull(job.node, job.model, false, job.verifyLoad);
}

export function cancelPull(key: string): void {
  const job = jobs.get(key);
  if (!job) return;
  if (job.simulating) {
    setJob(key, { status: 'cancelled', error: 'Cancelled.' });
    return;
  }
  apiFetch(`${BASE}/v1/nodes/${encodeURIComponent(job.node)}/pull?model=${encodeURIComponent(job.model)}`, {
    method: 'DELETE',
  }).catch(() => {
    /* the SSE stream (or its onerror fallback) surfaces the outcome either way */
  });
}

// restoreActivePulls re-populates the (in-memory, reload-wiped) job map from
// the marbor's own bookkeeping (s.pullJobs, admin.go) - called once from
// PullProgressWidget on mount so a browser refresh mid-download doesn't make
// the progress bar vanish for a pull that's still running server-side. The
// widget itself renders purely off `jobs`, so as soon as an entry exists
// here (and its SSE stream is (re)subscribed) the existing rendering path
// just works - no separate "restored" UI state needed.
export function restoreActivePulls(): void {
  if (DEMO) return;
  apiFetch(`${BASE}/pulls`)
    .then((res) => (res.ok ? res.json() : []))
    .then((active: Array<{ node: string; model: string; method?: string; status?: string; bytes_total?: number; bytes_completed?: number; verify_load?: boolean }>) => {
      for (const j of active) {
        const key = jobKey(j.node, j.model);
        if (jobs.has(key)) continue; // already tracked (e.g. started in this same tab)
        const startedAtMs = Date.now();
        // status defaults to 'downloading' only when the server didn't send
        // one (shouldn't happen) - GET /admin/pulls now also returns
        // "verifying" jobs (opt-in load-verification, still in progress),
        // and the first real SSE tick corrects this immediately either way.
        jobs.set(key, {
          key, node: j.node, model: j.model, method: (j.method as 'direct' | 'agent') || '',
          // verify_load comes straight from the job's stored server-side
          // flag now, not inferred from status - a job still 'downloading'
          // that was started with verify_load:true no longer loses the flag
          // on a page refresh.
          status: (j.status as PullStatus) || 'downloading', verifyLoad: !!j.verify_load,
          // A restored job always came from the marbor's own server-side
          // bookkeeping (GET /admin/pulls), never a simulated one.
          simulating: false,
          bytesTotal: j.bytes_total || 0, bytesCompleted: j.bytes_completed || 0, error: '',
          startedAtMs, speedBps: 0, lastSampleMs: startedAtMs, lastSampleBytes: j.bytes_completed || 0, staleTicks: 0,
        });
        subscribeToProgress(key, j.node, j.model);
      }
      if (active.length > 0) notify();
    })
    .catch(() => {
      /* best-effort restore - a failed fetch here just means no widget until the next pull starts */
    });
}

export function closeJob(key: string): void {
  closeStream(key);
  reconnectAttempts.delete(key);
  jobs.delete(key);
  notify();
}
