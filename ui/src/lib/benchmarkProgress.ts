// Single-job hardware benchmark progress tracker for the hidden /benchmark
// page (Settings -> "Hardware Benchmark" card). Unlike pullProgress.ts, only
// one benchmark ever runs at a time - the page itself enforces this (no
// "start" while a job is already in flight) - so this is a plain
// module-level singleton rather than a keyed map. Lives outside React state
// so the SSE subscription survives whatever re-renders the page does while
// the job is running.

import { runBenchmark, cancelBenchmarkJob } from './api';
import type { BenchmarkRun } from '../types';

const BASE = '/admin';

export type BenchmarkPhase = 'idle' | 'evicting' | 'cold' | 'warm' | 'done' | 'error' | 'cancelled';

const KNOWN_PHASES = new Set<BenchmarkPhase>(['idle', 'evicting', 'cold', 'warm', 'done', 'error', 'cancelled']);

export interface BenchmarkProgressState {
  jobId: string;
  node: string;
  model: string;
  n: number;
  phase: BenchmarkPhase;
  coldSamplesMs: number[];
  warmSamplesMs: number[];
  error: string;
  result: BenchmarkRun | null;
  startedAtMs: number;
}

const IDLE_STATE: BenchmarkProgressState = {
  jobId: '', node: '', model: '', n: 0, phase: 'idle',
  coldSamplesMs: [], warmSamplesMs: [], error: '', result: null, startedAtMs: 0,
};

let state: BenchmarkProgressState = IDLE_STATE;
let eventSource: EventSource | null = null;
const listeners = new Set<() => void>();
// runId guards start()'s async POST chain against a subsequent start() call
// re-entering before the first one's response arrives - only the .then/
// .catch belonging to the current run is allowed to apply its result.
let runId = 0;
// Tracks whether the CURRENT job was started in simulated mode (build-time
// VITE_FORCE_DEMO or the runtime Demo Mode toggle), independent of DEMO -
// cancel() needs this because a run started with `simulate: true` never gets
// a jobId or EventSource, so `cancelBenchmarkJob` would have nothing to call.
let simulating = false;

function notify() {
  for (const l of listeners) l();
}

function setState(patch: Partial<BenchmarkProgressState>) {
  state = { ...state, ...patch };
  notify();
}

export function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

export function getSnapshot(): BenchmarkProgressState {
  return state;
}

function closeStream() {
  if (eventSource) {
    eventSource.close();
    eventSource = null;
  }
}

// DEMO mirrors pullProgress.ts's build-time flag for the public GitHub Pages
// demo (no backend to POST/stream against) - distinct from the in-app
// Demo Mode toggle, which callers pass in explicitly via `simulate`.
const DEMO = import.meta.env.VITE_FORCE_DEMO === 'true';

function runDemoBenchmark(node: string, model: string, n: number) {
  const coldBaseMs = 3200 + Math.random() * 900;
  const warmBaseMs = 90 + Math.random() * 40;
  let i = 0;
  setState({ phase: 'evicting' });

  const stepCold = () => {
    if (state.phase !== 'evicting' && state.phase !== 'cold') return;
    setState({ phase: 'cold' });
    if (i >= n) { i = 0; setTimeout(stepWarm, 300); return; }
    const ms = Math.round(coldBaseMs + (Math.random() - 0.5) * 300);
    setState({ coldSamplesMs: [...state.coldSamplesMs, ms] });
    i++;
    setTimeout(stepCold, 350);
  };

  const stepWarm = () => {
    if (state.phase !== 'cold' && state.phase !== 'warm') return;
    setState({ phase: 'warm' });
    if (i >= n) {
      const cold = [...state.coldSamplesMs].sort((a, b) => a - b);
      const warm = [...state.warmSamplesMs].sort((a, b) => a - b);
      const p50 = (arr: number[]) => arr.length % 2 ? arr[(arr.length - 1) / 2] : (arr[arr.length / 2 - 1] + arr[arr.length / 2]) / 2;
      const coldP50 = p50(cold), warmP50 = p50(warm);
      const result: BenchmarkRun = {
        id: 0, node, model, n,
        cold_p50_ms: coldP50, cold_min_ms: cold[0], cold_max_ms: cold[cold.length - 1],
        warm_p50_ms: warmP50, warm_min_ms: warm[0], warm_max_ms: warm[warm.length - 1],
        speedup_x: warmP50 > 0 ? coldP50 / warmP50 : 0,
        created_at: new Date().toISOString(),
      };
      setState({ phase: 'done', result });
      return;
    }
    const ms = Math.round(warmBaseMs + (Math.random() - 0.5) * 20);
    setState({ warmSamplesMs: [...state.warmSamplesMs, ms] });
    i++;
    setTimeout(stepWarm, 200);
  };

  setTimeout(stepCold, 600);
}

// start begins a new benchmark run. `simulate` should be the caller's
// runtime demo-mode flag (useDemoMode()), matching pullProgress.ts's
// startPull convention.
export function start(node: string, model: string, n: number, simulate: boolean = false): void {
  closeStream();
  const thisRun = ++runId;
  state = { ...IDLE_STATE, node, model, n, phase: 'evicting', startedAtMs: Date.now() };
  simulating = DEMO || simulate;
  notify();

  if (simulating) {
    runDemoBenchmark(node, model, n);
    return;
  }

  runBenchmark(node, model, n)
    .then(({ job_id }) => {
      if (thisRun !== runId) return; // a later start() has already superseded this one
      setState({ jobId: job_id });
      subscribeToProgress(job_id);
    })
    .catch((err) => {
      if (thisRun !== runId) return;
      setState({ phase: 'error', error: err instanceof Error ? err.message : 'Failed to start benchmark' });
    });
}

function subscribeToProgress(jobId: string): void {
  closeStream();
  const es = new EventSource(`${BASE}/benchmark/${encodeURIComponent(jobId)}/progress`, { withCredentials: true });
  eventSource = es;

  es.onmessage = (evt) => {
    let data: any;
    try {
      data = JSON.parse(evt.data);
    } catch {
      return;
    }
    // A phase this client build doesn't recognize (e.g. a newer server sent
    // something added after this UI shipped) must still resolve to a state
    // the page knows how to recover from, rather than silently rendering the
    // raw string forever with the stream left open and no reset button.
    const known = KNOWN_PHASES.has(data.phase);
    setState({
      phase: known ? data.phase : 'error',
      coldSamplesMs: data.cold_samples_ms || [],
      warmSamplesMs: data.warm_samples_ms || [],
      error: known ? (data.error || '') : `Unrecognized server phase "${data.phase}" - this UI may be out of date.`,
      result: data.result || null,
    });
    if (!known || data.phase === 'done' || data.phase === 'error' || data.phase === 'cancelled') {
      closeStream();
    }
  };

  es.onerror = () => {
    if (es.readyState === EventSource.CONNECTING) return; // native auto-retry in progress
    closeStream();
    if (state.phase !== 'done' && state.phase !== 'error' && state.phase !== 'cancelled') {
      setState({ phase: 'error', error: 'Lost connection to the progress stream.' });
    }
  };
}

export function cancel(): void {
  if (simulating) {
    setState({ phase: 'cancelled', error: 'Cancelled.' });
    return;
  }
  if (!state.jobId) return;
  cancelBenchmarkJob(state.jobId).catch(() => {
    /* the SSE stream's own terminal event (or onerror fallback) surfaces the outcome either way */
  });
}

export function reset(): void {
  closeStream();
  state = IDLE_STATE;
  simulating = false;
  notify();
}
