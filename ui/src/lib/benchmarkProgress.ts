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
  state = { ...IDLE_STATE, node, model, n, phase: 'evicting', startedAtMs: Date.now() };
  notify();

  if (DEMO || simulate) {
    runDemoBenchmark(node, model, n);
    return;
  }

  runBenchmark(node, model, n)
    .then(({ job_id }) => {
      setState({ jobId: job_id });
      subscribeToProgress(job_id);
    })
    .catch((err) => {
      setState({ phase: 'error', error: err instanceof Error ? err.message : 'Failed to start benchmark' });
    });
}

function subscribeToProgress(jobId: string): void {
  const es = new EventSource(`${BASE}/benchmark/${encodeURIComponent(jobId)}/progress`, { withCredentials: true });
  eventSource = es;

  es.onmessage = (evt) => {
    let data: any;
    try {
      data = JSON.parse(evt.data);
    } catch {
      return;
    }
    setState({
      phase: data.phase || state.phase,
      coldSamplesMs: data.cold_samples_ms || [],
      warmSamplesMs: data.warm_samples_ms || [],
      error: data.error || '',
      result: data.result || null,
    });
    switch (data.phase) {
      case 'done':
      case 'error':
      case 'cancelled':
        closeStream();
        break;
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
  if (DEMO) {
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
  notify();
}
