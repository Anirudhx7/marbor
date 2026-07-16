import { useState, useEffect, useMemo, useCallback, useRef } from 'react';
import { Package, Download, Check, Server, Loader2, Cpu, HardDrive, Star, ArrowDown, ExternalLink, X, Settings2 } from 'lucide-react';
import { SearchInput } from '../components/SearchInput';
import { VramBar } from '../components/VramBar';
import { ModelConfigModal } from '../components/ModelConfigModal';
import { CustomSelect } from '../components/Select';
import {
  pullModel,
  fetchSystemInfo,
  SystemInfo,
  fetchModelCatalog,
  searchHFModels,
  getHFRepoDetails,
  HFModel,
  HFRepoDetails,
  ModelVariantFit,
} from '../lib/api';
import { useDemoMode } from '../hooks/useDemoMode';
import { mockHFModels, mockHFRepoDetails, mockSystemInfo, mockModelCatalogResponse } from '../lib/mockData';
import { CustomDatePicker } from '../components/DateTimePicker';

function FitBadge({ fit }: { fit: 'green' | 'yellow' | 'red' | 'unknown' }) {
  const styles = {
    green: 'bg-green-500/15 text-green-600 dark:text-green-400 border border-green-500/30',
    yellow: 'bg-amber-500/15 text-amber-600 dark:text-amber-400 border border-amber-500/30',
    red: 'bg-red-500/15 text-red-600 dark:text-red-400 border border-red-500/30',
    unknown: 'bg-secondary text-muted-foreground border border-border',
  };
  const labels = { green: 'Fits', yellow: 'Tight', red: 'Too Large', unknown: 'Unknown' };
  return (
    <span className={`inline-flex items-center px-2 py-0.5 rounded text-xs font-medium ${styles[fit]}`}>
      {labels[fit]}
    </span>
  );
}

function ModelDetailPanel({
  model,
  nodeName,
  nodeRuntime,
  isLive,
  demoMode,
  onClose,
}: {
  model: HFModel;
  nodeName: string | null;
  nodeRuntime: string | null;
  isLive: boolean;
  demoMode: boolean;
  onClose: () => void;
}) {
  const panelRef = useRef<HTMLDivElement>(null);
  const [visible, setVisible] = useState(false);
  const [loading, setLoading] = useState(false);
  const [details, setDetails] = useState<HFRepoDetails | null>(null);
  const [ctxLen, setCtxLen] = useState(8192);
  const [pullingTag, setPullingTag] = useState<string | null>(null);
  const [pulledTags, setPulledTags] = useState<Set<string>>(new Set());
  const [error, setError] = useState<string | null>(null);
  const [configTag, setConfigTag] = useState<string | null>(null);

  useEffect(() => {
    const id = requestAnimationFrame(() => {
      setVisible(true);
      panelRef.current?.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
    });
    return () => cancelAnimationFrame(id);
  }, []);

  useEffect(() => {
    const handler = (e: KeyboardEvent) => { if (e.key === 'Escape') handleClose(); };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const handleClose = useCallback(() => {
    setVisible(false);
    setTimeout(onClose, 150);
  }, [onClose]);

  const fetchDetails = useCallback(async (len: number) => {
    if (demoMode) {
      const mock = mockHFRepoDetails[model.id];
      if (mock) {
        const adjustedVariants = mock.variants.map((v: any) => {
          const estVram = v.size_mb * 1.10 + len * 0.15;
          let fit: 'green' | 'yellow' | 'red' = 'green';
          if (estVram > 24576) fit = 'red';
          else if (estVram > 10240) fit = 'yellow';
          return { ...v, vram_est_mb: Math.round(estVram), fit };
        });
        setDetails({ ...mock, variants: adjustedVariants });
      } else {
        setDetails({
          id: model.id,
          downloads: model.downloads,
          likes: model.likes,
          tags: model.tags,
          last_modified: model.lastModified,
          variants: [{ tag: `hf.co/${model.id}:Q4_K_M`, quantization: 'Q4_K_M', vram_est_mb: 4000, size_mb: 3500, fit: 'green', downloaded: false }],
        });
      }
      return;
    }
    setLoading(true);
    setError(null);
    try {
      const resp = await getHFRepoDetails(model.id, nodeName || undefined, len, nodeRuntime || undefined);
      setDetails(resp);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to load variants');
    } finally {
      setLoading(false);
    }
  }, [demoMode, model.id, model.downloads, model.likes, model.tags, model.lastModified, nodeName, nodeRuntime]);

  useEffect(() => { fetchDetails(ctxLen); }, [ctxLen, fetchDetails]);

  const handlePull = async (variant: ModelVariantFit) => {
    if (!nodeName) return;
    setPullingTag(variant.tag);
    setError(null);
    try {
      await pullModel(nodeName, variant.tag);
      setPulledTags(prev => new Set([...prev, variant.tag]));
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Pull failed');
    } finally {
      setPullingTag(null);
    }
  };

  const formattedDownloads = new Intl.NumberFormat().format(model.downloads);
  const formattedLikes = new Intl.NumberFormat().format(model.likes);

  return (
    <div
      ref={panelRef}
      className="col-span-full bg-card border border-primary/30 rounded-2xl overflow-hidden flex flex-col shadow-xl"
      style={{
        opacity: visible ? 1 : 0,
        transform: visible ? 'translateY(0)' : 'translateY(-6px)',
        transition: 'opacity 150ms ease-out, transform 150ms ease-out',
        maxHeight: '70vh',
      }}
    >
      {/* Header */}
      <div className="flex items-start justify-between p-5 border-b border-border shrink-0">
        <div className="min-w-0 flex-1">
          <h2 className="font-semibold text-foreground text-base truncate" title={model.id}>
            {(model.id ?? '').split('/').pop()}
          </h2>
          <span className="text-xs text-muted-foreground block truncate">{model.id}</span>
          <div className="flex items-center gap-3 text-xs text-muted-foreground mt-1.5">
            <span className="flex items-center gap-1">
              <Star className="w-3 h-3 text-amber-500 fill-amber-500" /> {formattedLikes}
            </span>
            <span className="flex items-center gap-1">
              <ArrowDown className="w-3.5 h-3.5" /> {formattedDownloads} downloads
            </span>
            {model.pipeline_tag && (
              <span className="px-1.5 py-0.5 rounded bg-primary/10 text-primary capitalize text-[10px]">
                {model.pipeline_tag.replace('-', ' ')}
              </span>
            )}
          </div>
        </div>
        <button
          onClick={handleClose}
          className="ml-4 shrink-0 p-1.5 rounded-lg text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors cursor-pointer"
          aria-label="Close"
        >
          <X className="w-4 h-4" />
        </button>
      </div>

      {/* Body */}
      <div className="overflow-y-auto flex-1 p-5 space-y-4">
        {/* Context slider */}
        <div className="space-y-1.5">
          <div className="flex justify-between items-center text-xs">
            <span className="text-muted-foreground">Target Context Length:</span>
            <span className="font-mono font-medium text-foreground">
              {new Intl.NumberFormat().format(ctxLen)} tokens
            </span>
          </div>
          <input
            type="range"
            min="2048"
            max="32768"
            step="2048"
            value={ctxLen}
            onChange={(e) => setCtxLen(parseInt(e.target.value))}
            disabled={loading}
            className="w-full h-1.5 bg-secondary rounded-lg appearance-none cursor-pointer accent-primary"
          />
          <p className="text-[10px] text-muted-foreground leading-normal">
            Larger context windows increase KV-cache allocation and total VRAM requirement.
          </p>
        </div>

        {/* Variants */}
        {loading ? (
          <div className="flex items-center justify-center py-10 gap-2 text-xs text-muted-foreground">
            <Loader2 className="w-4 h-4 animate-spin text-primary" /> Loading variants...
          </div>
        ) : error ? (
          <div className="text-xs text-destructive bg-destructive/10 border border-destructive/20 rounded-lg p-2.5">
            {error}
          </div>
        ) : details && details.variants && details.variants.length > 0 ? (
          <div className="space-y-2">
            <span className="text-xs font-semibold text-muted-foreground uppercase tracking-wider block">
              {(!nodeRuntime || nodeRuntime === 'ollama' || nodeRuntime === 'llamacpp') ? 'GGUF File Quantizations' : 'Safetensors Repository'}
            </span>
            <div className="space-y-1.5">
              {details.variants.map((v) => {
                const isPulled = v.downloaded || (v.tag && pulledTags.has(v.tag));
                const isPulling = pullingTag === v.tag;
                const vramGB = v.vram_est_mb >= 1024 ? `${(v.vram_est_mb / 1024).toFixed(1)} GB` : `${v.vram_est_mb} MB`;
                const sizeGB = v.size_mb >= 1024 ? `${(v.size_mb / 1024).toFixed(1)} GB` : `${v.size_mb} MB`;
                return (
                  <div
                    key={v.tag}
                    className="flex flex-col sm:flex-row sm:items-center justify-between gap-2.5 text-xs rounded px-2.5 py-2.5 bg-secondary/50 border border-border/50 hover:border-border transition-colors"
                  >
                    <div className="min-w-0 flex-1 mr-2">
                      <div className="flex flex-wrap items-center gap-1.5">
                        <span className="font-mono font-semibold text-foreground">{v.quantization}</span>
                        <span className="text-[10px] text-muted-foreground whitespace-nowrap">{sizeGB} size · {vramGB} VRAM</span>
                      </div>
                      <span className="text-[9px] text-muted-foreground font-mono block truncate" title={v.tag}>
                        {v.tag}
                      </span>
                    </div>
                    <div className="flex items-center gap-2 shrink-0 self-start sm:self-auto">
                      <FitBadge fit={v.fit} />
                      <button
                        onClick={() => setConfigTag(v.tag)}
                        title={`Advanced settings for ${v.tag}`}
                        className="p-1 text-muted-foreground hover:text-primary transition-colors"
                      >
                        <Settings2 className="w-3.5 h-3.5" />
                      </button>
                      {isPulled ? (
                        <span className="inline-flex items-center gap-1 text-[11px] font-medium text-green-600 dark:text-green-400">
                          <Check className="w-3.5 h-3.5" /> Ready
                        </span>
                      ) : (!nodeRuntime || nodeRuntime === 'ollama') ? (
                        <button
                          onClick={() => handlePull(v)}
                          disabled={!isLive || !nodeName || isPulling || v.fit === 'red'}
                          className="inline-flex items-center gap-1 px-2.5 py-1 bg-primary hover:bg-primary/90 disabled:opacity-40 disabled:hover:bg-primary text-[11px] font-medium text-primary-foreground rounded transition-colors cursor-pointer"
                          title={v.fit === 'red' ? 'Requires more VRAM than available' : ''}
                        >
                          {isPulling ? <Loader2 className="w-3 h-3 animate-spin" /> : <Download className="w-3 h-3" />}
                          Pull
                        </button>
                      ) : (
                        <span className="text-[11px] text-muted-foreground italic">
                          Model loaded at startup
                        </span>
                      )}
                    </div>
                  </div>
                );
              })}
            </div>
          </div>
        ) : (
          <p className="text-xs text-muted-foreground py-4 text-center">
            {(!nodeRuntime || nodeRuntime === 'ollama' || nodeRuntime === 'llamacpp') ? 'No GGUF files found in this repository.' : 'No safetensors weights found in this repository.'}
          </p>
        )}
      </div>

      {/* Footer */}
      <div className="flex items-center justify-between px-5 py-3 border-t border-border shrink-0 bg-secondary/20">
        <a
          href={`https://huggingface.co/${model.id}`}
          target="_blank"
          rel="noreferrer"
          className="text-[11px] text-primary hover:underline inline-flex items-center gap-1"
        >
          <ExternalLink className="w-3 h-3" /> View on Hugging Face
        </a>
        <button
          onClick={handleClose}
          className="text-[11px] text-muted-foreground hover:text-foreground cursor-pointer transition-colors"
        >
          Close
        </button>
      </div>

      <ModelConfigModal
        model={configTag}
        demoMode={demoMode}
        nodes={nodeName ? [{ name: nodeName, runtime: nodeRuntime ?? 'ollama' }] : []}
        presetNumCtx={ctxLen}
        onClose={() => setConfigTag(null)}
      />
    </div>
  );
}

function ModelCard({
  model,
  selected,
  onSelect,
}: {
  model: HFModel;
  selected: boolean;
  onSelect: () => void;
}) {
  const formattedDownloads = new Intl.NumberFormat().format(model.downloads);
  const formattedLikes = new Intl.NumberFormat().format(model.likes);

  return (
    <div className={`bg-card border shadow-sm rounded-xl p-5 flex flex-col transition-colors ${selected ? 'border-primary' : 'border-border hover:border-primary/50'}`}>
      <div className="flex items-start justify-between mb-2">
        <div className="min-w-0 flex-1">
          <h3 className="font-semibold text-foreground truncate" title={model.id}>
            {(model.id ?? '').split('/').pop()}
          </h3>
          <span className="text-xs text-muted-foreground block truncate">{model.id}</span>
        </div>
        <span className="inline-flex items-center gap-1 px-2 py-0.5 rounded bg-secondary text-[11px] font-medium text-foreground shrink-0 ml-2">
          <Star className="w-3 h-3 text-amber-500 fill-amber-500" /> {formattedLikes}
        </span>
      </div>

      <div className="flex items-center gap-3 text-xs text-muted-foreground mb-3">
        <span className="flex items-center gap-1">
          <ArrowDown className="w-3.5 h-3.5" /> {formattedDownloads} downloads
        </span>
        {model.pipeline_tag && (
          <span className="px-1.5 py-0.5 rounded bg-primary/10 text-primary capitalize text-[10px]">
            {model.pipeline_tag.replace('-', ' ')}
          </span>
        )}
      </div>

      <button
        onClick={onSelect}
        className={`mt-auto w-full py-2 text-foreground text-xs font-medium rounded-lg transition-colors cursor-pointer ${selected ? 'bg-primary/10 text-primary hover:bg-primary/20' : 'bg-secondary hover:bg-secondary/80'}`}
      >
        {selected ? 'Hide Details' : 'View Quantizations & Pull'}
      </button>
    </div>
  );
}

export function ModelAdvisor() {
  const { demoMode } = useDemoMode();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [isLive, setIsLive] = useState(false);
  const [selectedNode, setSelectedNode] = useState<string | null>(null);
  const [sysInfo, setSysInfo] = useState<SystemInfo | null>(null);
  const [nodes, setNodes] = useState<any[]>([]);

  const [search, setSearch] = useState('');
  const [debouncedSearch, setDebouncedSearch] = useState('');
  const [models, setModels] = useState<HFModel[]>([]);
  const [searching, setSearching] = useState(false);
  const [searchError, setSearchError] = useState<string | null>(null);
  const [selectedModelId, setSelectedModelId] = useState<string | null>(null);
  const [columnCount, setColumnCount] = useState(3);

  const [sortBy, setSortBy] = useState<'downloads' | 'likes' | 'newest' | 'oldest'>('downloads');
  const [minDownloads, setMinDownloads] = useState('');
  const [minLikes, setMinLikes] = useState('');
  const [createdAfter, setCreatedAfter] = useState('');

  const activeNode = useMemo(
    () => (!selectedNode || nodes.length === 0) ? null : nodes.find(n => n.name === selectedNode) || null,
    [nodes, selectedNode]
  );

  // Track grid column count to insert panel at end of the correct row
  useEffect(() => {
    const update = () => {
      const w = window.innerWidth;
      setColumnCount(w >= 1280 ? 3 : w >= 1024 ? 2 : 1);
    };
    update();
    window.addEventListener('resize', update);
    return () => window.removeEventListener('resize', update);
  }, []);

  useEffect(() => {
    const handler = setTimeout(() => setDebouncedSearch(search), 300);
    return () => clearTimeout(handler);
  }, [search]);

  const loadSystemInfo = async () => {
    if (demoMode) {
      setSysInfo(mockSystemInfo);
      const catalogNodes = mockModelCatalogResponse.nodes;
      setNodes(catalogNodes);
      setSelectedNode(catalogNodes[0]?.name ?? '');
      setIsLive(false);
      setError(null);
      setLoading(false);
      return;
    }
    setLoading(true);
    try {
      const [sys, catalogResp] = await Promise.all([
        fetchSystemInfo().catch(() => null),
        fetchModelCatalog().catch(() => null),
      ]);
      setSysInfo(sys);
      setIsLive(true);
      setError(null);
      if (catalogResp && catalogResp.nodes) {
        setNodes(catalogResp.nodes);
        if (catalogResp.nodes.length > 0) {
          setSelectedNode(prev => {
            const exists = catalogResp.nodes.some((n: any) => n.name === prev);
            return exists ? prev : catalogResp.nodes[0].name;
          });
        }
      }
    } catch (e: unknown) {
      setIsLive(false);
      setError(e instanceof Error ? e.message : 'Failed to connect to backend');
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => { loadSystemInfo(); }, [demoMode]);

  useEffect(() => {
    const doSearch = async () => {
      const minDl = minDownloads ? Number(minDownloads) : undefined;
      const minLk = minLikes ? Number(minLikes) : undefined;

      if (demoMode) {
        setSearchError(null);
        const q = debouncedSearch.trim().toLowerCase();
        let filtered = q === '' ? mockHFModels : mockHFModels.filter(m => m.id.toLowerCase().includes(q));
        if (minDl) filtered = filtered.filter(m => m.downloads >= minDl);
        if (minLk) filtered = filtered.filter(m => m.likes >= minLk);
        if (createdAfter) filtered = filtered.filter(m => m.lastModified >= createdAfter);
        const sorted = [...filtered].sort((a, b) => {
          if (sortBy === 'likes') return b.likes - a.likes;
          if (sortBy === 'newest') return b.lastModified.localeCompare(a.lastModified);
          if (sortBy === 'oldest') return a.lastModified.localeCompare(b.lastModified);
          return b.downloads - a.downloads;
        });
        setModels(sorted);
        return;
      }
      setSearching(true);
      setSearchError(null);
      try {
        const resp = await searchHFModels(debouncedSearch, {
          runtime: activeNode?.runtime,
          sort: sortBy,
          minDownloads: minDl,
          minLikes: minLk,
          createdAfter: createdAfter || undefined,
        });
        setModels(resp || []);
      } catch (e: unknown) {
        setSearchError(e instanceof Error ? e.message : 'Failed to search Hugging Face models. Make sure the backend has internet access.');
        setModels([]);
      } finally {
        setSearching(false);
      }
    };
    doSearch();
  }, [debouncedSearch, demoMode, sortBy, minDownloads, minLikes, createdAfter, activeNode?.runtime]);

  // Close panel when search results change
  useEffect(() => { setSelectedModelId(null); }, [models]);

  const uniqueModels = useMemo(() => {
    const seen = new Set<string>();
    return models.filter(m => {
      if (!m.id || seen.has(m.id)) return false;
      seen.add(m.id);
      return true;
    });
  }, [models]);

  const selectedModel = useMemo(
    () => uniqueModels.find(m => m.id === selectedModelId) ?? null,
    [uniqueModels, selectedModelId]
  );

  // Build grid items: cards + panel inserted after end of the clicked card's row
  type GridItem = { kind: 'card'; model: HFModel } | { kind: 'panel'; model: HFModel };
  const gridItems = useMemo((): GridItem[] => {
    if (!selectedModelId || !selectedModel) {
      return uniqueModels.map(m => ({ kind: 'card' as const, model: m }));
    }
    const idx = uniqueModels.findIndex(m => m.id === selectedModelId);
    if (idx < 0) return uniqueModels.map(m => ({ kind: 'card' as const, model: m }));

    // Insert panel after the last card in the row containing the selected card
    const rowEnd = Math.ceil((idx + 1) / columnCount) * columnCount - 1;
    const insertAfter = Math.min(rowEnd, uniqueModels.length - 1);

    const items: GridItem[] = [];
    uniqueModels.forEach((m, i) => {
      items.push({ kind: 'card', model: m });
      if (i === insertAfter) {
        items.push({ kind: 'panel', model: selectedModel });
      }
    });
    return items;
  }, [uniqueModels, selectedModelId, selectedModel, columnCount]);

  return (
    <div className="space-y-6 animate-fade-in max-w-7xl mx-auto">
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold text-foreground flex items-center gap-2">
            <Package className="w-6 h-6 text-primary" /> Model Advisor
          </h1>
          <p className="text-sm text-muted-foreground mt-1 font-medium">Search Hugging Face for models that fit your node&apos;s runtime and VRAM</p>
        </div>
        <div className="flex items-center gap-2">
          <div className={`w-2 h-2 rounded-full ${demoMode ? 'bg-success' : (loading && !isLive) ? 'bg-blue-500 animate-pulse' : isLive ? 'bg-success' : 'bg-amber-500'}`} />
          <span className={`text-xs font-semibold ${demoMode ? 'text-success' : (loading && !isLive) ? 'text-blue-500 animate-pulse' : isLive ? 'text-success' : 'text-amber-600 dark:text-amber-400'}`}>
            {demoMode ? 'Demo Mode' : (loading && !isLive) ? 'Connecting...' : isLive ? 'Live Data' : 'Disconnected'}
          </span>
        </div>
      </div>

      {demoMode && (
        <div className="p-4 bg-amber-500/10 border border-amber-500/20 rounded-xl text-amber-700 dark:text-amber-400 text-sm font-medium">
          Demo mode - 4 inference nodes (Ollama, vLLM, TGI, llama.cpp). Connect real nodes to see live data.
        </div>
      )}

      {error && !demoMode && (
        <div className="p-4 bg-destructive/10 border border-destructive/20 rounded-xl text-destructive text-sm font-semibold">
          {error}
        </div>
      )}

      {sysInfo && (
        <div className="bg-card border border-border rounded-xl px-5 py-3 flex flex-wrap gap-5 items-center text-xs shadow-sm">
          <span className="text-xs font-bold text-muted-foreground uppercase tracking-wider shrink-0">Mesh Host</span>
          <span className="flex items-center gap-1.5">
            <Cpu className="w-3.5 h-3.5 text-muted-foreground" />
            <span className="text-foreground font-semibold">{sysInfo.cpu_cores} cores</span>
            <span className="text-muted-foreground font-medium">{sysInfo.arch} · {sysInfo.os}</span>
          </span>
          {sysInfo.ram_total_mb > 0 && (
            <span className="flex items-center gap-1.5">
              <HardDrive className="w-3.5 h-3.5 text-muted-foreground" />
              <span className="text-foreground font-semibold">{(sysInfo.ram_free_mb / 1024).toFixed(1)} GB free</span>
              <span className="text-muted-foreground font-medium">of {(sysInfo.ram_total_mb / 1024).toFixed(0)} GB RAM</span>
            </span>
          )}
        </div>
      )}

      {activeNode && (
        <>
          {nodes.length > 1 && (
            <div className="flex flex-wrap gap-2">
              {nodes.map((n) => (
                <button
                  key={n.name}
                  onClick={() => setSelectedNode(n.name)}
                  className={`px-3 py-1.5 rounded-lg text-sm font-semibold transition-colors cursor-pointer ${
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

          <div className="bg-card border border-border rounded-xl p-5 shadow-sm">
            <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2 mb-3">
              <div>
                <span className="text-xs font-bold text-muted-foreground uppercase tracking-wider">GPU Node</span>
                <h3 className="font-semibold text-foreground mt-0.5">{activeNode.name}</h3>
              </div>
              <span className="text-xs text-muted-foreground self-start sm:self-auto">
                VRAM source:{' '}
                <span className={`px-1.5 py-0.5 rounded font-semibold ${
                  activeNode.vram_source === 'nvidia-smi'
                    ? 'bg-green-500/15 text-green-600 dark:text-green-400'
                    : activeNode.vram_source === 'inferred'
                    ? 'bg-amber-500/15 text-amber-600 dark:text-amber-400'
                    : activeNode.vram_source === 'declared'
                    ? 'bg-blue-500/15 text-blue-600 dark:text-blue-400'
                    : 'bg-secondary text-muted-foreground'
                }`}>
                  {activeNode.vram_source === 'declared' ? 'declared' : activeNode.vram_source}
                </span>
              </span>
            </div>
            {activeNode.vram_total_bytes > 0 ? (
              <>
                <VramBar
                  used={(activeNode.vram_total_bytes - activeNode.vram_free_bytes) / (1024 * 1024 * 1024)}
                  total={activeNode.vram_total_bytes / (1024 * 1024 * 1024)}
                />
                <p className="text-xs text-muted-foreground font-medium mt-2">
                  {bytesToGB(activeNode.vram_free_bytes)} free of {bytesToGB(activeNode.vram_total_bytes)} VRAM
                </p>
              </>
            ) : (activeNode.vram_used_bytes ?? 0) > 0 ? (
              <>
                <div className="w-full bg-secondary rounded-full h-2 mt-1">
                  <div className="bg-amber-500 h-2 rounded-full w-full opacity-40" />
                </div>
                <p className="text-xs text-muted-foreground font-medium mt-2">
                  {bytesToGB(activeNode.vram_used_bytes!)} in use &middot; total unknown
                </p>
              </>
            ) : (
              <p className="text-xs text-muted-foreground font-medium">
                VRAM totals unavailable - nvidia-smi reads the mesh host only.
              </p>
            )}
          </div>

          <div className="bg-secondary/30 border border-border rounded-xl p-4 text-xs text-muted-foreground leading-relaxed shadow-sm">
            <span className="font-semibold text-foreground block mb-1">How to add models to nodes:</span>
            1. Search for any GGUF model (e.g. <code className="font-mono text-primary font-semibold">llama-3.2</code> or <code className="font-mono text-primary font-semibold">qwen2.5</code>) using the search bar below.
            <br />
            2. Click <strong className="text-foreground">View Quantizations & Pull</strong> on a card to expand the detail panel inline.
            <br />
            3. Select a quantization and click <strong className="text-foreground">Pull</strong> to download it to the active node.
          </div>

          <div className="flex flex-col md:flex-row gap-4 md:items-center">
            <div className="max-w-md flex-1">
              <SearchInput value={search} onChange={setSearch} placeholder="Search Hugging Face (e.g. llama-3.2, deepseek-r1)..." />
            </div>
            {searching && (
              <span className="flex items-center gap-1.5 text-xs text-muted-foreground font-medium animate-pulse">
                <Loader2 className="w-3.5 h-3.5 animate-spin text-primary" /> Searching Hugging Face...
              </span>
            )}
          </div>

          <div className="flex flex-wrap gap-3 items-center text-xs">
            <label className="flex items-center gap-1.5">
              <span className="text-muted-foreground font-medium shrink-0">Sort:</span>
              <CustomSelect
                value={sortBy}
                onChange={(val) => setSortBy(val as typeof sortBy)}
                size="sm"
                className="w-36"
                options={[
                  { value: 'downloads', label: 'Most Downloads' },
                  { value: 'likes', label: 'Most Likes' },
                  { value: 'newest', label: 'Newest' },
                  { value: 'oldest', label: 'Oldest' },
                ]}
              />
            </label>
            <label className="flex items-center gap-1.5">
              <span className="text-muted-foreground font-medium">Min Downloads:</span>
              <input
                type="number"
                min="0"
                value={minDownloads}
                onChange={(e) => setMinDownloads(e.target.value)}
                placeholder="0"
                className="w-24 bg-secondary border border-border rounded-lg px-2 py-1.5 text-foreground font-medium"
              />
            </label>
            <label className="flex items-center gap-1.5">
              <span className="text-muted-foreground font-medium">Min Likes:</span>
              <input
                type="number"
                min="0"
                value={minLikes}
                onChange={(e) => setMinLikes(e.target.value)}
                placeholder="0"
                className="w-20 bg-secondary border border-border rounded-lg px-2 py-1.5 text-foreground font-medium"
              />
            </label>
            <label className="flex items-center gap-1.5">
              <span className="text-muted-foreground font-medium">Created After:</span>
              <CustomDatePicker
                value={createdAfter}
                onChange={setCreatedAfter}
                className="w-36"
              />
            </label>
            {(minDownloads || minLikes || createdAfter || sortBy !== 'downloads') && (
              <button
                onClick={() => { setSortBy('downloads'); setMinDownloads(''); setMinLikes(''); setCreatedAfter(''); }}
                className="text-primary hover:underline font-medium cursor-pointer"
              >
                Reset filters
              </button>
            )}
          </div>

          {searchError && (
            <div className="p-4 bg-destructive/10 border border-destructive/20 rounded-xl text-destructive text-xs font-semibold">
              {searchError}
            </div>
          )}

          {loading && !demoMode ? (
            <div className="flex justify-center items-center py-16 gap-2 text-muted-foreground">
              <Loader2 className="w-6 h-6 animate-spin text-primary" /> Loading models...
            </div>
          ) : models.length === 0 ? (
            <div className="text-center py-16 bg-card border border-border rounded-xl shadow-sm">
              <Package className="w-12 h-12 text-muted-foreground/30 mx-auto mb-4" />
              <p className="text-muted-foreground font-medium">No repositories found. Try searching for "llama" or "gemma".</p>
            </div>
          ) : (
            <div className="grid grid-cols-1 lg:grid-cols-2 xl:grid-cols-3 gap-5">
              {gridItems.map((item) =>
                item.kind === 'card' ? (
                  <ModelCard
                    key={item.model.id}
                    model={item.model}
                    selected={item.model.id === selectedModelId}
                    onSelect={() => setSelectedModelId(
                      item.model.id === selectedModelId ? null : item.model.id
                    )}
                  />
                ) : (
                  <ModelDetailPanel
                    key="__panel__"
                    model={item.model}
                    nodeName={selectedNode}
                    nodeRuntime={activeNode?.runtime ?? null}
                    isLive={isLive}
                    demoMode={demoMode}
                    onClose={() => setSelectedModelId(null)}
                  />
                )
              )}
            </div>
          )}
        </>
      )}

      {!activeNode && !loading && !error && nodes.length === 0 && (
        <div className="text-center py-16 bg-card border border-border rounded-xl shadow-sm">
          <Server className="w-12 h-12 text-muted-foreground/30 mx-auto mb-4" />
          <h3 className="text-lg font-semibold text-foreground">No GPU Nodes Connected</h3>
          <p className="text-muted-foreground max-w-md mx-auto text-sm leading-normal mt-1">
            Ollama-Mesh requires at least one active Ollama node to calculate VRAM capacity and check model compatibility.
            Connect your first node in the <strong>GPU Nodes</strong> page.
          </p>
        </div>
      )}
    </div>
  );
}

function bytesToGB(bytes: number): string {
  const gb = bytes / (1024 * 1024 * 1024);
  return gb >= 1 ? `${gb.toFixed(1)} GB` : `${(bytes / (1024 * 1024)).toFixed(0)} MB`;
}
