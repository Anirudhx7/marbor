import { useState, useEffect, useMemo } from 'react';
import { Package, Download, Check, Server, Loader2, Cpu, HardDrive } from 'lucide-react';
import { SearchInput } from '../components/SearchInput';
import { VramBar } from '../components/VramBar';
import { fetchModelCatalog, pullModel, fetchSystemInfo, SystemInfo } from '../lib/api';
import { useDemoMode } from '../hooks/useDemoMode';
import { mockModelCatalogResponse, mockSystemInfo } from '../lib/mockData';
import type {
  ModelCatalogResponse,
  CatalogNodeEntry,
  CatalogModelFit,
  CatalogVariantFit,
  FitStatus,
} from '../types';

type CategoryFilter = 'all' | 'chat' | 'coding' | 'reasoning' | 'embedding' | 'vision';
type FitFilter = 'all' | 'fits' | 'too-large';

const CATEGORY_OPTIONS: { value: CategoryFilter; label: string }[] = [
  { value: 'all', label: 'All' },
  { value: 'chat', label: 'Chat' },
  { value: 'coding', label: 'Coding' },
  { value: 'reasoning', label: 'Reasoning' },
  { value: 'embedding', label: 'Embedding' },
  { value: 'vision', label: 'Vision' },
];

const FIT_OPTIONS: { value: FitFilter; label: string }[] = [
  { value: 'all', label: 'All' },
  { value: 'fits', label: 'Fits' },
  { value: 'too-large', label: 'Too Large' },
];

function bytesToGB(bytes: number): string {
  const gb = bytes / (1024 * 1024 * 1024);
  return gb >= 1 ? `${gb.toFixed(1)} GB` : `${(bytes / (1024 * 1024)).toFixed(0)} MB`;
}

function mbToGB(mb: number): string {
  return mb >= 1024 ? `${(mb / 1024).toFixed(1)} GB` : `${mb} MB`;
}

function FitBadge({ fit }: { fit: FitStatus }) {
  const styles: Record<FitStatus, string> = {
    green: 'bg-green-500/15 text-green-600 dark:text-green-400 border border-green-500/30',
    yellow: 'bg-amber-500/15 text-amber-600 dark:text-amber-400 border border-amber-500/30',
    red: 'bg-red-500/15 text-red-600 dark:text-red-400 border border-red-500/30',
    unknown: 'bg-secondary text-muted-foreground border border-border',
  };
  const labels: Record<FitStatus, string> = {
    green: 'Fits',
    yellow: 'Tight',
    red: 'Too Large',
    unknown: 'Unknown',
  };
  return (
    <span className={`inline-flex items-center px-2 py-0.5 rounded text-xs font-medium ${styles[fit]}`}>
      {labels[fit]}
    </span>
  );
}

// Pick the highest-quality variant that fits the node's actual VRAM.
// Priority: green (recommended first) > green > yellow (recommended first) > yellow > catalog recommended > first.
function recommendedVariant(model: CatalogModelFit): CatalogVariantFit {
  const greens = model.variants.filter((v) => v.fit === 'green');
  if (greens.length > 0) return greens.find((v) => v.recommended) ?? greens[0];
  const yellows = model.variants.filter((v) => v.fit === 'yellow');
  if (yellows.length > 0) return yellows.find((v) => v.recommended) ?? yellows[0];
  return model.variants.find((v) => v.recommended) ?? model.variants[0];
}

function ModelCard({
  model,
  nodeName,
  isLive,
  onPulled,
}: {
  model: CatalogModelFit;
  nodeName: string | null;
  isLive: boolean;
  onPulled: () => void;
}) {
  const rec = recommendedVariant(model);
  const [pulling, setPulling] = useState(false);
  const [pullError, setPullError] = useState<string | null>(null);
  const [pulled, setPulled] = useState(false);

  const handlePull = async () => {
    if (!nodeName) return;
    setPulling(true);
    setPullError(null);
    try {
      await pullModel(nodeName, rec.tag);
      setPulled(true);
      onPulled();
    } catch (e: unknown) {
      setPullError(e instanceof Error ? e.message : 'Pull failed');
    } finally {
      setPulling(false);
    }
  };

  const alreadyDownloaded = model.downloaded || pulled;

  return (
    <div className="bg-card border border-border shadow-sm rounded-xl p-5 flex flex-col hover:border-primary/50 transition-colors">
      <div className="flex items-start justify-between mb-2">
        <div>
          <h3 className="font-semibold text-foreground">{model.display_name}</h3>
          <code className="text-xs text-muted-foreground font-mono">{model.name}</code>
        </div>
        <span className="inline-flex items-center px-2 py-0.5 rounded bg-secondary text-xs font-medium text-foreground shrink-0">
          {model.param_count}
        </span>
      </div>

      <p className="text-sm text-muted-foreground mb-3 flex-1">{model.description}</p>

      <div className="flex flex-wrap gap-1.5 mb-3">
        {model.categories.map((c) => (
          <span key={c} className="px-1.5 py-0.5 rounded text-[10px] font-medium bg-primary/10 text-primary capitalize">
            {c}
          </span>
        ))}
      </div>

      {/* All variants with fit badges */}
      <div className="border-t border-border pt-3 mb-3 space-y-1.5">
        {model.variants.map((v) => (
          <div key={v.tag} className={`flex items-center justify-between text-xs rounded px-2 py-1 ${v.tag === rec.tag ? 'bg-primary/8 border border-primary/20' : 'bg-secondary/50'}`}>
            <div className="flex items-center gap-1.5 min-w-0">
              {v.tag === rec.tag && <span className="text-[9px] font-bold text-primary uppercase tracking-wide shrink-0">Best</span>}
              <span className="font-mono text-foreground truncate">{v.quantization}</span>
              <span className="text-muted-foreground shrink-0">{mbToGB(v.vram_est_mb)} VRAM</span>
            </div>
            <FitBadge fit={v.fit} />
          </div>
        ))}
      </div>

      {rec.fit === 'red' && (
        <p className="text-xs text-destructive mb-2 font-medium">Requires more VRAM than available - no variant fits your hardware.</p>
      )}
      {pullError && <p className="text-xs text-destructive mb-2">{pullError}</p>}

      <div className="flex items-center justify-between gap-2">
        {alreadyDownloaded ? (
          <span className="inline-flex items-center gap-1.5 text-xs font-medium text-green-600 dark:text-green-400">
            <Check className="w-3.5 h-3.5" /> Already Downloaded
          </span>
        ) : (
          <span className="text-xs text-muted-foreground font-mono">{mbToGB(rec.size_mb)} download</span>
        )}
        <button
          onClick={handlePull}
          disabled={!isLive || !nodeName || pulling || alreadyDownloaded || rec.fit === 'red'}
          className="inline-flex items-center gap-1.5 px-3 py-1.5 bg-primary hover:bg-primary/90 disabled:opacity-50 disabled:cursor-not-allowed text-primary-foreground text-xs font-medium rounded-lg transition-colors"
          title={rec.fit === 'red' ? 'No variant fits your available VRAM' : !isLive ? 'Connect to backend to pull models' : !nodeName ? 'No node selected' : ''}
        >
          {pulling ? (
            <>
              <Loader2 className="w-3.5 h-3.5 animate-spin" /> Pulling...
            </>
          ) : (
            <>
              <Download className="w-3.5 h-3.5" /> Pull
            </>
          )}
        </button>
      </div>
    </div>
  );
}

export function ModelAdvisor() {
  const { demoMode } = useDemoMode();
  const [data, setData] = useState<ModelCatalogResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [isLive, setIsLive] = useState(false);
  const [selectedNode, setSelectedNode] = useState<string | null>(null);
  const [sysInfo, setSysInfo] = useState<SystemInfo | null>(null);

  const [search, setSearch] = useState('');
  const [category, setCategory] = useState<CategoryFilter>('all');
  const [fitFilter, setFitFilter] = useState<FitFilter>('all');

  const loadCatalog = async () => {
    if (demoMode) {
      setData(mockModelCatalogResponse);
      setSysInfo(mockSystemInfo);
      setSelectedNode(mockModelCatalogResponse.nodes[0]?.name ?? null);
      setLoading(false);
      setIsLive(false);
      setError(null);
      return;
    }
    setLoading(true);
    try {
      const [resp, sys] = await Promise.all([fetchModelCatalog(), fetchSystemInfo().catch(() => null)]);
      setSysInfo(sys);
      setData(resp);
      setIsLive(true);
      setError(null);
      setFitFilter('fits'); // default to showing only runnable models when hardware is known
      if (resp.nodes.length > 0) {
        setSelectedNode((prev) =>
          prev && resp.nodes.some((n) => n.name === prev) ? prev : resp.nodes[0].name
        );
      } else {
        setSelectedNode(null);
      }
    } catch (e: unknown) {
      setIsLive(false);
      setData(null);
      setError(e instanceof Error ? e.message : 'Failed to load catalog');
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    loadCatalog();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [demoMode]);

  const activeNode: CatalogNodeEntry | null = useMemo(() => {
    if (!data || !selectedNode) return null;
    return data.nodes.find((n) => n.name === selectedNode) ?? null;
  }, [data, selectedNode]);

  const models: CatalogModelFit[] = useMemo(() => {
    if (!activeNode) return [];
    let list = [...activeNode.models].sort((a, b) => a.rank - b.rank);

    if (search.trim()) {
      const q = search.toLowerCase();
      list = list.filter(
        (m) => m.name.toLowerCase().includes(q) || m.display_name.toLowerCase().includes(q)
      );
    }
    if (category !== 'all') {
      list = list.filter((m) => m.categories.includes(category));
    }
    if (fitFilter !== 'all') {
      list = list.filter((m) => {
        const fit = recommendedVariant(m).fit;
        return fitFilter === 'fits' ? fit === 'green' || fit === 'yellow' : fit === 'red';
      });
    }
    return list;
  }, [activeNode, search, category, fitFilter]);

  return (
    <div className="space-y-6 animate-fade-in max-w-7xl mx-auto">
      {/* Header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold text-foreground flex items-center gap-2">
            <Package className="w-6 h-6 text-primary" /> Model Advisor
          </h1>
          <p className="text-sm text-muted-foreground mt-1">Find models that fit your hardware</p>
        </div>
        <div className="flex items-center gap-2">
          <div className={`w-2 h-2 rounded-full ${isLive ? 'bg-success' : 'bg-amber-500'}`} />
          <span className={`text-xs font-medium ${isLive ? 'text-success' : 'text-amber-600 dark:text-amber-400'}`}>
            {demoMode ? 'Demo Mode' : isLive ? 'Live Data' : 'Disconnected'}
          </span>
        </div>
      </div>

      {demoMode && (
        <div className="p-4 bg-amber-500/10 border border-amber-500/20 rounded-xl text-amber-700 dark:text-amber-400 text-sm">
          Demo mode - showing simulated NVIDIA RTX 4090 (24 GB VRAM, 10 GB free). Connect a real node to see live fit results.
        </div>
      )}

      {error && !demoMode && (
        <div className="p-4 bg-destructive/10 border border-destructive/20 rounded-xl text-destructive text-sm font-medium">
          {error}
        </div>
      )}

      {loading && !demoMode && (
        <div className="flex items-center gap-2 text-muted-foreground text-sm">
          <Loader2 className="w-4 h-4 animate-spin" /> Loading catalog...
        </div>
      )}

      {/* No nodes */}
      {!loading && !error && data && data.nodes.length === 0 && (
        <div className="text-center py-16">
          <Server className="w-12 h-12 text-muted-foreground/30 mx-auto mb-4" />
          <p className="text-muted-foreground">No nodes available. Add a GPU node to see model fit.</p>
        </div>
      )}

      {/* Mesh host system info — shown once, not per node */}
      {sysInfo && (
        <div className="bg-card border border-border rounded-xl px-5 py-3 flex flex-wrap gap-5 items-center text-xs">
          <span className="text-xs font-semibold text-muted-foreground uppercase tracking-wider shrink-0">Mesh Host</span>
          <span className="flex items-center gap-1.5">
            <Cpu className="w-3.5 h-3.5 text-muted-foreground" />
            <span className="text-foreground font-medium">{sysInfo.cpu_cores} cores</span>
            <span className="text-muted-foreground">{sysInfo.arch} · {sysInfo.os}</span>
          </span>
          {sysInfo.ram_total_mb > 0 && (
            <span className="flex items-center gap-1.5">
              <HardDrive className="w-3.5 h-3.5 text-muted-foreground" />
              <span className="text-foreground font-medium">{(sysInfo.ram_free_mb / 1024).toFixed(1)} GB free</span>
              <span className="text-muted-foreground">of {(sysInfo.ram_total_mb / 1024).toFixed(0)} GB RAM</span>
            </span>
          )}
        </div>
      )}

      {/* Node tabs + VRAM */}
      {activeNode && (
        <>
          {data && data.nodes.length > 1 && (
            <div className="flex flex-wrap gap-2">
              {data.nodes.map((n) => (
                <button
                  key={n.name}
                  onClick={() => setSelectedNode(n.name)}
                  className={`px-3 py-1.5 rounded-lg text-sm font-medium transition-colors ${
                    n.name === selectedNode
                      ? 'bg-primary text-primary-foreground'
                      : 'bg-secondary text-muted-foreground hover:text-foreground'
                  }`}
                >
                  {n.name}
                </button>
              ))}
            </div>
          )}

          <div className="bg-card border border-border rounded-xl p-5">
            <div className="flex items-center justify-between mb-3">
              <div>
                <span className="text-xs font-semibold text-muted-foreground uppercase tracking-wider">GPU Node</span>
                <h3 className="font-semibold text-foreground mt-0.5">{activeNode.name}</h3>
              </div>
              <span className="text-xs text-muted-foreground">
                VRAM source:{' '}
                <span
                  className={`px-1.5 py-0.5 rounded font-medium ${
                    activeNode.vram_source === 'nvidia-smi'
                      ? 'bg-green-500/15 text-green-600 dark:text-green-400'
                      : activeNode.vram_source === 'inferred'
                      ? 'bg-amber-500/15 text-amber-600 dark:text-amber-400'
                      : 'bg-secondary text-muted-foreground'
                  }`}
                >
                  {activeNode.vram_source}
                </span>
              </span>
            </div>
            {activeNode.vram_total_bytes > 0 ? (
              <>
                <VramBar
                  used={(activeNode.vram_total_bytes - activeNode.vram_free_bytes) / (1024 * 1024 * 1024)}
                  total={activeNode.vram_total_bytes / (1024 * 1024 * 1024)}
                />
                <div className="flex items-center justify-between mt-2">
                  <p className="text-xs text-muted-foreground">
                    {bytesToGB(activeNode.vram_free_bytes)} free of {bytesToGB(activeNode.vram_total_bytes)} VRAM
                  </p>
                  <div className="flex items-center gap-3 text-xs">
                    <span className="text-green-600 dark:text-green-400 font-medium">
                      {activeNode.models.filter((m) => recommendedVariant(m).fit === 'green' || recommendedVariant(m).fit === 'yellow').length} models fit
                    </span>
                    <span className="text-destructive font-medium">
                      {activeNode.models.filter((m) => recommendedVariant(m).fit === 'red').length} too large
                    </span>
                  </div>
                </div>
              </>
            ) : (
              <p className="text-xs text-muted-foreground">
                VRAM totals unavailable - nvidia-smi reads the mesh host only. Fit shown as "unknown".
              </p>
            )}
          </div>

          {/* Filter bar */}
          <div className="flex flex-col lg:flex-row gap-4 lg:items-center">
            <div className="max-w-md flex-1">
              <SearchInput value={search} onChange={setSearch} placeholder="Search models by name..." />
            </div>
            <div className="flex items-center gap-2">
              <span className="text-xs text-muted-foreground">Category:</span>
              <div className="flex flex-wrap gap-1">
                {CATEGORY_OPTIONS.map((opt) => (
                  <button
                    key={opt.value}
                    onClick={() => setCategory(opt.value)}
                    className={`px-2.5 py-1 rounded-md text-xs font-medium transition-colors ${
                      category === opt.value
                        ? 'bg-primary text-primary-foreground'
                        : 'bg-secondary text-muted-foreground hover:text-foreground'
                    }`}
                  >
                    {opt.label}
                  </button>
                ))}
              </div>
            </div>
            <div className="flex items-center gap-2">
              <span className="text-xs text-muted-foreground">Fit:</span>
              <div className="flex gap-1">
                {FIT_OPTIONS.map((opt) => (
                  <button
                    key={opt.value}
                    onClick={() => setFitFilter(opt.value)}
                    className={`px-2.5 py-1 rounded-md text-xs font-medium transition-colors ${
                      fitFilter === opt.value
                        ? 'bg-primary text-primary-foreground'
                        : 'bg-secondary text-muted-foreground hover:text-foreground'
                    }`}
                  >
                    {opt.label}
                  </button>
                ))}
              </div>
            </div>
          </div>

          {/* Model grid */}
          {models.length === 0 ? (
            <div className="text-center py-12">
              <Package className="w-12 h-12 text-muted-foreground/30 mx-auto mb-4" />
              <p className="text-muted-foreground">No models match your filters.</p>
            </div>
          ) : (
            <div className="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-5">
              {models.map((m) => (
                <ModelCard
                  key={m.name}
                  model={m}
                  nodeName={selectedNode}
                  isLive={isLive}
                  onPulled={loadCatalog}
                />
              ))}
            </div>
          )}
        </>
      )}
    </div>
  );
}
