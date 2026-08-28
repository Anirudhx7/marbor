// Hidden hardware benchmark page (no Sidebar entry - reached only via the
// "Hardware Benchmark" card on Settings). Lets an already-authenticated
// admin pick a node + model already known to marbor and watch a live
// cold-vs-warm TTFT run, with zero manual credential entry - the marbor
// auto-provisions and deletes an ephemeral API key server-side
// (internal/admin/benchmark.go). Not integral to marbor operation; this is a
// self-service validation/diagnostics tool, not a monitored dashboard
// surface, so it deliberately has no nav-bar presence.
import { useEffect, useMemo, useState, useSyncExternalStore } from 'react';
import { Gauge, Zap, Server, X, History, Copy, Check } from 'lucide-react';
import { fetchNodes, fetchModels, getNodeModels, fetchBenchmarkRuns } from '../lib/api';
import { subscribe, getSnapshot, start, cancel, reset } from '../lib/benchmarkProgress';
import { mockGPUNodes, mockBenchmarkRuns } from '../lib/mockData';
import type { GPUNode, BenchmarkRun } from '../types';
import { CustomSelect } from '../components/Select';
import { useDemoMode } from '../hooks/useDemoMode';
import { useTimezone } from '../hooks/useTimezone';
import { formatDateTimeInZone } from '../lib/time';
import { Modal } from '../components/Modal';

function fmtMs(ms: number): string {
  if (!Number.isFinite(ms)) return '-';
  return `${ms.toLocaleString(undefined, { maximumFractionDigits: 0 })} ms`;
}

// Ollama's own architecture classification (details.family from /api/tags)
// for known embedding/encoder-only families - these have no chat-completion
// endpoint, so this benchmark (always a /v1/chat/completions TTFT measurement
// - see internal/admin/benchmark.go) can never succeed against them. Family
// is only ever populated from Ollama sources today (Architecture Law 5:
// vLLM/TGI/llama.cpp/MLX have no equivalent metadata via their HF-cache scan)
// - a model with no known family is left in the picker rather than guessed
// at (R1), so this filter is best-effort, not exhaustive.
const EMBEDDING_FAMILIES = new Set(['bert', 'nomic-bert', 'clip']);

function isChatCapable(family: string | undefined): boolean {
  return !family || !EMBEDDING_FAMILIES.has(family);
}

function ResultCard({ result }: { result: BenchmarkRun }) {
  const [copied, setCopied] = useState(false);

  function copySummary() {
    const text = `${result.node} · ${result.model}\n${result.speedup_x.toFixed(1)}x faster warm vs. cold (p50)\nCold TTFT (p50): ${fmtMs(result.cold_p50_ms)} (min ${fmtMs(result.cold_min_ms)}, max ${fmtMs(result.cold_max_ms)})\nWarm TTFT (p50): ${fmtMs(result.warm_p50_ms)} (min ${fmtMs(result.warm_min_ms)}, max ${fmtMs(result.warm_max_ms)})\nn=${result.n} samples per phase, measured via Marbor's own proxy.`;
    navigator.clipboard.writeText(text).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    }).catch(() => {});
  }

  return (
    <div className="bg-card border border-primary/30 rounded-xl p-6 shadow-sm">
      <div className="flex items-center gap-2 mb-4 flex-wrap">
        <Zap className="w-5 h-5 text-primary shrink-0" />
        <h3 className="text-sm font-semibold text-foreground shrink-0">Result</h3>
        <span className="text-xs text-muted-foreground font-mono truncate min-w-0">{result.node} · {result.model}</span>
        <button onClick={copySummary}
          className="ml-auto flex items-center gap-1 px-2 py-1 text-[11px] text-muted-foreground border border-border rounded-md hover:bg-secondary transition-colors shrink-0">
          {copied ? <><Check className="w-3 h-3" /> Copied</> : <><Copy className="w-3 h-3" /> Copy</>}
        </button>
      </div>
      <div className="flex items-baseline gap-2 mb-5">
        <span className="text-4xl font-bold text-primary">{result.speedup_x.toFixed(1)}×</span>
        <span className="text-sm text-muted-foreground">faster warm vs. cold (p50)</span>
      </div>
      <div className="grid grid-cols-2 gap-4 text-sm">
        <div className="rounded-lg border border-border p-3">
          <p className="text-xs text-muted-foreground mb-1">Cold TTFT (p50)</p>
          <p className="font-mono text-foreground font-medium">{fmtMs(result.cold_p50_ms)}</p>
          <p className="text-[10px] text-muted-foreground/70 font-mono">min {fmtMs(result.cold_min_ms)} · max {fmtMs(result.cold_max_ms)}</p>
        </div>
        <div className="rounded-lg border border-border p-3">
          <p className="text-xs text-muted-foreground mb-1">Warm TTFT (p50)</p>
          <p className="font-mono text-foreground font-medium">{fmtMs(result.warm_p50_ms)}</p>
          <p className="text-[10px] text-muted-foreground/70 font-mono">min {fmtMs(result.warm_min_ms)} · max {fmtMs(result.warm_max_ms)}</p>
        </div>
      </div>
      <p className="text-[10px] text-muted-foreground/60 mt-4">
        n={result.n} samples per phase, measured through this marbor's own proxy. Copy the summary as text, or screenshot this card to share it.
      </p>
    </div>
  );
}

export function Benchmark() {
  const tz = useTimezone();
  const { demoMode } = useDemoMode();
  const progress = useSyncExternalStore(subscribe, getSnapshot);

  const [nodes, setNodes] = useState<GPUNode[]>([]);
  const [selectedNode, setSelectedNode] = useState('');
  const [models, setModels] = useState<string[]>([]);
  const [modelsError, setModelsError] = useState<string | null>(null);
  const [selectedModel, setSelectedModel] = useState('');
  const [n, setN] = useState(10);
  const [loading, setLoading] = useState(true);
  const [runError, setRunError] = useState<string | null>(null);
  const [runConfirmOpen, setRunConfirmOpen] = useState(false);
  const [history, setHistory] = useState<BenchmarkRun[]>([]);
  const [historyError, setHistoryError] = useState<string | null>(null);

  const healthyNodes = useMemo(() => nodes.filter(n => n.health !== 'down'), [nodes]);

  useEffect(() => {
    let active = true;
    if (demoMode) {
      setNodes(mockGPUNodes);
      const healthy = mockGPUNodes.filter(n => n.health !== 'down');
      if (healthy.length === 1) setSelectedNode(healthy[0].name);
      setHistory(mockBenchmarkRuns);
      setLoading(false);
      return () => { active = false; };
    }
    fetchNodes()
      .then(ns => {
        if (!active) return;
        setNodes(ns);
        const healthy = ns.filter(n => n.health !== 'down');
        if (healthy.length === 1) setSelectedNode(healthy[0].name);
      })
      .catch(() => { if (active) setRunError('Failed to load nodes - refresh the page to retry.'); })
      .finally(() => { if (active) setLoading(false); });
    refreshHistory();
    return () => { active = false; };
  }, [demoMode]);

  function refreshHistory() {
    if (demoMode) { setHistory(mockBenchmarkRuns); setHistoryError(null); return; }
    fetchBenchmarkRuns()
      .then(runs => { setHistory(runs); setHistoryError(null); })
      .catch(() => setHistoryError('Failed to load benchmark history.'));
  }

  // Refresh history the moment a run finishes, so a completed benchmark shows
  // up immediately instead of only after the user clicks "Run another".
  useEffect(() => {
    if (progress.phase === 'done') refreshHistory();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [progress.phase]);

  useEffect(() => {
    if (!selectedNode) { setModels([]); return; }
    setModelsError(null);
    const node = nodes.find(n => n.name === selectedNode);
    // Prefer the marbor agent's models.list (runtime-agnostic, on-disk models).
    // Without that capability, fall back to the same /admin/models
    // aggregation the Model Catalog page uses (queries the node's /api/tags
    // directly, so it sees all on-disk models, not just currently-warm ones)
    // rather than just the router's live loadedModels view, which is empty
    // whenever nothing happens to be loaded in VRAM right now.
    if (demoMode) {
      setModels((node?.loadedModels || []).map(m => m.name));
    } else if (node?.agentCapabilities?.includes('models.list')) {
      getNodeModels(selectedNode)
        .then(list => setModels(list.filter(m => isChatCapable(m.family)).map(m => m.name)))
        .catch(() => {
          setModels((node.loadedModels || []).map(m => m.name));
          setModelsError('marbor agent model list unavailable - showing currently-loaded models only.');
        });
    } else {
      fetchModels()
        .then(catalog => {
          const names = catalog.models
            .filter(m => m.nodes.some(n => n.name === selectedNode) && isChatCapable(m.family))
            .map(m => m.name);
          setModels(names);
        })
        .catch(() => setModels((node?.loadedModels || []).map(m => m.name)));
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selectedNode, nodes]);

  useEffect(() => {
    setSelectedModel(prev => (models.includes(prev) ? prev : models[0] || ''));
  }, [models]);

  const running = progress.phase === 'evicting' || progress.phase === 'cold' || progress.phase === 'warm';

  async function handleRun() {
    setRunError(null);
    if (!selectedNode || !selectedModel) { setRunError('Pick a node and a model.'); return; }
    setRunConfirmOpen(true);
  }

  function confirmRun() {
    setRunConfirmOpen(false);
    start(selectedNode, selectedModel, n, demoMode);
  }

  function handleReset() {
    reset();
    refreshHistory();
  }

  const phaseLabel: Record<string, string> = {
    evicting: 'Evicting model from VRAM…',
    cold: `Cold sample ${progress.coldSamplesMs.length}/${progress.n}`,
    warm: `Warm sample ${progress.warmSamplesMs.length}/${progress.n}`,
    done: 'Done',
    error: 'Failed',
    cancelled: 'Cancelled',
  };

  return (
    <div className="space-y-4 animate-fade-in max-w-3xl mx-auto">
      <div className="flex items-center gap-2.5">
        <Gauge className="w-5 h-5 text-primary" />
        <h1 className="text-lg font-bold text-foreground">Hardware Benchmark</h1>
      </div>
      <p className="text-sm text-muted-foreground -mt-2">
        Measures real cold-vs-warm Time-To-First-Token on this marbor's own hardware, through its own proxy.
        Not required for normal operation - a self-service diagnostic, off the main navigation on purpose.
      </p>

      {/* Setup panel */}
      <div className="bg-card border border-border rounded-xl p-5 space-y-4">
        {loading ? (
          <p className="text-sm text-muted-foreground">Loading nodes…</p>
        ) : healthyNodes.length === 0 ? (
          <p className="text-sm text-muted-foreground">No healthy nodes available to benchmark.</p>
        ) : (
          <>
            <div className="grid grid-cols-1 sm:grid-cols-3 gap-3">
              <div>
                <label className="block text-xs font-medium text-muted-foreground mb-1">Node</label>
                <CustomSelect
                  value={selectedNode}
                  onChange={setSelectedNode}
                  options={healthyNodes.map(n => ({ value: n.name, label: n.name }))}
                  disabled={running}
                />
              </div>
              <div>
                <label className="block text-xs font-medium text-muted-foreground mb-1">Model</label>
                <CustomSelect
                  value={selectedModel}
                  onChange={setSelectedModel}
                  options={models.map(m => ({ value: m, label: m }))}
                  disabled={running || models.length === 0}
                />
                {models.length === 0 && selectedNode && (
                  <p className="text-[10px] text-destructive mt-1">No chat-capable models found on this node - pull one first (embedding models are excluded, since this benchmark measures chat TTFT).</p>
                )}
              </div>
              <div>
                <label className="block text-xs font-medium text-muted-foreground mb-1">Samples per phase</label>
                <input type="number" min={1} max={50} value={n}
                  onChange={e => setN(Math.max(1, Math.min(50, parseInt(e.target.value, 10) || 1)))}
                  disabled={running}
                  className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground disabled:opacity-50" />
              </div>
            </div>
            {modelsError && <p className="text-xs text-amber-600 dark:text-amber-400">{modelsError}</p>}
            {runError && <p className="text-sm text-destructive">{runError}</p>}
            <div className="flex gap-2">
              {!running ? (
                <button onClick={handleRun} disabled={!selectedNode || !selectedModel}
                  className="flex items-center gap-2 px-4 py-2 bg-primary text-primary-foreground text-sm font-medium rounded-lg hover:bg-primary/90 disabled:opacity-50 transition-colors shadow-sm">
                  <Zap className="w-4 h-4" /> Run Benchmark
                </button>
              ) : (
                <button onClick={cancel}
                  className="flex items-center gap-2 px-4 py-2 bg-destructive text-destructive-foreground text-sm font-medium rounded-lg hover:bg-destructive/90 transition-colors shadow-sm">
                  <X className="w-4 h-4" /> Cancel
                </button>
              )}
              {(progress.phase === 'done' || progress.phase === 'error' || progress.phase === 'cancelled') && (
                <button onClick={handleReset}
                  className="px-4 py-2 border border-border text-sm text-muted-foreground rounded-lg hover:bg-secondary transition-colors">
                  Run another
                </button>
              )}
            </div>
          </>
        )}
      </div>

      {/* Progress panel */}
      {progress.phase !== 'idle' && (
        <div className="bg-card border border-border rounded-xl p-5">
          <div className="flex items-center gap-2 mb-3">
            <Server className="w-4 h-4 text-muted-foreground" />
            <span className="text-sm font-medium text-foreground">{phaseLabel[progress.phase] || progress.phase}</span>
          </div>
          {running && (
            <div className="w-full h-1.5 bg-secondary rounded-full overflow-hidden">
              <div className="h-full bg-primary transition-all duration-300" style={{
                width: `${Math.min(100, ((progress.coldSamplesMs.length + progress.warmSamplesMs.length) / Math.max(1, progress.n * 2)) * 100)}%`,
              }} />
            </div>
          )}
          {progress.phase === 'error' && (
            <p className="text-sm text-destructive mt-2">{progress.error || 'Benchmark failed.'}</p>
          )}
          {progress.phase === 'cancelled' && (
            <p className="text-sm text-muted-foreground mt-2">Cancelled by admin.</p>
          )}
        </div>
      )}

      {/* Result */}
      {progress.phase === 'done' && progress.result && <ResultCard result={progress.result} />}

      {/* History */}
      <div className="bg-card border border-border rounded-xl overflow-hidden">
        <div className="flex items-center gap-2 px-5 py-3 border-b border-border">
          <History className="w-4 h-4 text-muted-foreground" />
          <h3 className="text-sm font-semibold text-foreground">History</h3>
        </div>
        {historyError ? (
          <p className="text-sm text-destructive px-5 py-4">{historyError}</p>
        ) : history.length === 0 ? (
          <p className="text-sm text-muted-foreground px-5 py-4">No benchmark runs yet.</p>
        ) : (
          <div className="divide-y divide-border">
            {history.map(r => (
              <div key={r.id} className="flex flex-col sm:flex-row sm:items-center justify-between gap-1 px-5 py-3 text-sm">
                <div className="min-w-0">
                  <span className="font-medium text-foreground">{r.node}</span>
                  <span className="text-muted-foreground"> · </span>
                  <span className="font-mono text-xs text-muted-foreground">{r.model}</span>
                </div>
                <div className="flex items-center gap-3 text-xs text-muted-foreground font-mono shrink-0">
                  <span>{fmtMs(r.cold_p50_ms)} → {fmtMs(r.warm_p50_ms)}</span>
                  <span className="text-primary font-semibold">{r.speedup_x.toFixed(1)}×</span>
                  <span className="text-muted-foreground/60">{formatDateTimeInZone(r.created_at, tz)}</span>
                </div>
              </div>
            ))}
          </div>
        )}
      </div>

      <Modal
        isOpen={runConfirmOpen}
        onClose={() => setRunConfirmOpen(false)}
        title="Run Hardware Benchmark"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Benchmark <span className="text-foreground font-semibold break-all">{selectedModel}</span> on{' '}
            <span className="text-foreground font-semibold">{selectedNode}</span>?
          </p>
          <p className="text-xs text-muted-foreground">
            This evicts the model from VRAM if it's currently warm on this node, to measure a genuine
            cold-start TTFT - any other traffic to this node relying on that model being warm will see
            a cold load until the benchmark finishes.
          </p>
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setRunConfirmOpen(false)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={confirmRun}
              className="px-4 py-2 bg-primary hover:bg-primary/90 text-primary-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              Run Benchmark
            </button>
          </div>
        </div>
      </Modal>
    </div>
  );
}
