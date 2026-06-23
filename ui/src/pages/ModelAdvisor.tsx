import { useState, useEffect, useMemo, useCallback, useRef } from 'react';
import { Package, Download, Check, Server, Loader2, Cpu, HardDrive, Star, ArrowDown, ExternalLink, X } from 'lucide-react';
import { SearchInput } from '../components/SearchInput';
import { VramBar } from '../components/VramBar';
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
import { mockHFModels, mockHFRepoDetails, mockSystemInfo } from '../lib/mockData';

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

function ModelDetailOverlay({
  model,
  nodeName,
  isLive,
  demoMode,
  onClose,
  anchorRect,
}: {
  model: HFModel;
  nodeName: string | null;
  isLive: boolean;
  demoMode: boolean;
  onClose: () => void;
  anchorRect: DOMRect;
}) {
  const [visible, setVisible] = useState(false);
  const [loading, setLoading] = useState(false);
  const [details, setDetails] = useState<HFRepoDetails | null>(null);
  const [ctxLen, setCtxLen] = useState(8192);
  const [pullingTag, setPullingTag] = useState<string | null>(null);
  const [pulledTags, setPulledTags] = useState<Set<string>>(new Set());
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const id = requestAnimationFrame(() => setVisible(true));
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
    setTimeout(onClose, 180);
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
      const resp = await getHFRepoDetails(model.id, nodeName || undefined, len);
      setDetails(resp);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to load variants');
    } finally {
      setLoading(false);
    }
  }, [demoMode, model.id, model.downloads, model.likes, model.tags, model.lastModified, nodeName]);

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

  const panelWidth = Math.min(600, (typeof window !== 'undefined' ? window.innerWidth : 640) - 32);
  const panelLeft = Math.max(16, Math.min(anchorRect.left, (typeof window !== 'undefined' ? window.innerWidth : 640) - panelWidth - 16));
  const panelTop = anchorRect.bottom + 8;
  const maxPanelHeight = (typeof window !== 'undefined' ? window.innerHeight : 800) - panelTop - 16;

  return (
    <div
      className="fixed inset-0 z-50"
      style={{ opacity: visible ? 1 : 0, transition: 'opacity 180ms ease-out' }}
    >
      {/* Backdrop */}
      <div
        className="absolute inset-0 bg-black/40 backdrop-blur-sm"
        onClick={handleClose}
      />

      {/* Panel — anchored below clicked card */}
      <div
        className="absolute bg-card border border-border rounded-2xl overflow-hidden flex flex-col shadow-2xl"
        style={{
          top: panelTop,
          left: panelLeft,
          width: panelWidth,
          maxHeight: Math.max(300, maxPanelHeight),
          transition: 'transform 180ms ease-out, opacity 180ms ease-out',
          transform: visible ? 'translateY(0) scale(1)' : 'translateY(-8px) scale(0.98)',
          opacity: visible ? 1 : 0,
        }}
      >
        {/* Header */}
        <div className="flex items-start justify-between p-5 border-b border-border shrink-0">
          <div className="min-w-0 flex-1">
            <h2 className="font-semibold text-foreground text-base truncate" title={model.id}>
              {model.id.split('/').pop()}
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
              <span className="text-xs font-semibold text-muted-foreground uppercase tracking-wider block">GGUF File Quantizations</span>
              <div className="space-y-1.5">
                {details.variants.map((v) => {
                  const isPulled = v.downloaded || (v.tag && pulledTags.has(v.tag));
                  const isPulling = pullingTag === v.tag;
                  const vramGB = v.vram_est_mb >= 1024 ? `${(v.vram_est_mb / 1024).toFixed(1)} GB` : `${v.vram_est_mb} MB`;
                  const sizeGB = v.size_mb >= 1024 ? `${(v.size_mb / 1024).toFixed(1)} GB` : `${v.size_mb} MB`;
                  return (
                    <div
                      key={v.tag}
                      className="flex items-center justify-between text-xs rounded px-2.5 py-2 bg-secondary/50 border border-border/50 hover:border-border transition-colors"
                    >
                      <div className="min-w-0 flex-1 mr-2">
                        <div className="flex items-center gap-1.5">
                          <span className="font-mono font-semibold text-foreground">{v.quantization}</span>
                          <span className="text-[10px] text-muted-foreground">{sizeGB} size · {vramGB} VRAM</span>
                        </div>
                        <span className="text-[9px] text-muted-foreground font-mono block truncate" title={v.tag}>
                          {v.tag}
                        </span>
                      </div>
                      <div className="flex items-center gap-2 shrink-0">
                        <FitBadge fit={v.fit} />
                        {isPulled ? (
                          <span className="inline-flex items-center gap-1 text-[11px] font-medium text-green-600 dark:text-green-400">
                            <Check className="w-3.5 h-3.5" /> Ready
                          </span>
                        ) : (
                          <button
                            onClick={() => handlePull(v)}
                            disabled={!isLive || !nodeName || isPulling || v.fit === 'red'}
                            className="inline-flex items-center gap-1 px-2.5 py-1 bg-primary hover:bg-primary/90 disabled:opacity-40 disabled:hover:bg-primary text-[11px] font-medium text-primary-foreground rounded transition-colors cursor-pointer"
                            title={v.fit === 'red' ? 'Requires more VRAM than available' : ''}
                          >
                            {isPulling ? <Loader2 className="w-3 h-3 animate-spin" /> : <Download className="w-3 h-3" />}
                            Pull
                          </button>
                        )}
                      </div>
                    </div>
                  );
                })}
              </div>
            </div>
          ) : (
            <p className="text-xs text-muted-foreground py-4 text-center">No GGUF files found in this repository.</p>
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
      </div>
    </div>
  );
}

function ModelCard({
  model,
  onSelect,
}: {
  model: HFModel;
  onSelect: (rect: DOMRect) => void;
}) {
  const cardRef = useRef<HTMLDivElement>(null);
  const formattedDownloads = new Intl.NumberFormat().format(model.downloads);
  const formattedLikes = new Intl.NumberFormat().format(model.likes);

  const handleClick = () => {
    const rect = cardRef.current?.getBoundingClientRect() ?? new DOMRect();
    onSelect(rect);
  };

  return (
    <div ref={cardRef} className="bg-card border border-border shadow-sm rounded-xl p-5 flex flex-col hover:border-primary/50 transition-colors">
      <div className="flex items-start justify-between mb-2">
        <div className="min-w-0 flex-1">
          <h3 className="font-semibold text-foreground truncate" title={model.id}>
            {model.id.split('/').pop()}
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
        onClick={handleClick}
        className="mt-auto w-full py-2 bg-secondary hover:bg-secondary/80 text-foreground text-xs font-medium rounded-lg transition-colors cursor-pointer"
      >
        View Quantizations & Pull
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
  const [selectedModel, setSelectedModel] = useState<HFModel | null>(null);
  const [anchorRect, setAnchorRect] = useState<DOMRect | null>(null);

  useEffect(() => {
    const handler = setTimeout(() => setDebouncedSearch(search), 300);
    return () => clearTimeout(handler);
  }, [search]);

  const loadSystemInfo = async () => {
    if (demoMode) {
      setSysInfo(mockSystemInfo);
      setNodes([{
        name: 'gpu-0',
        url: 'http://localhost:11435',
        vram_free_bytes: 10 * 1024 * 1024 * 1024,
        vram_total_bytes: 24 * 1024 * 1024 * 1024,
        vram_source: 'nvidia-smi',
        models: [],
      }]);
      setSelectedNode('gpu-0');
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
      if (demoMode) {
        setSearchError(null);
        if (debouncedSearch.trim() === '') {
          setModels(mockHFModels);
        } else {
          const q = debouncedSearch.toLowerCase();
          setModels(mockHFModels.filter(m => m.id.toLowerCase().includes(q)));
        }
        return;
      }
      setSearching(true);
      setSearchError(null);
      try {
        const resp = await searchHFModels(debouncedSearch);
        setModels(resp || []);
      } catch (e: unknown) {
        setSearchError(e instanceof Error ? e.message : 'Failed to search Hugging Face models. Make sure the backend has internet access.');
        setModels([]);
      } finally {
        setSearching(false);
      }
    };
    doSearch();
  }, [debouncedSearch, demoMode]);

  useEffect(() => { setSelectedModel(null); }, [models]);

  const uniqueModels = useMemo(() => {
    const seen = new Set<string>();
    return models.filter(m => {
      if (!m.id || seen.has(m.id)) return false;
      seen.add(m.id);
      return true;
    });
  }, [models]);

  const activeNode = useMemo(
    () => (!selectedNode || nodes.length === 0) ? null : nodes.find(n => n.name === selectedNode) || null,
    [nodes, selectedNode]
  );

  return (
    <div className="space-y-6 animate-fade-in max-w-7xl mx-auto">
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold text-foreground flex items-center gap-2">
            <Package className="w-6 h-6 text-primary" /> Model Advisor
          </h1>
          <p className="text-sm text-muted-foreground mt-1 font-medium">Search and pull Hugging Face GGUF models directly to Ollama</p>
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
          Demo mode - showing simulated NVIDIA RTX 4090 (24 GB VRAM, 10 GB free). Connect a real node to pull models.
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
            <div className="flex items-center justify-between mb-3">
              <div>
                <span className="text-xs font-bold text-muted-foreground uppercase tracking-wider">GPU Node</span>
                <h3 className="font-semibold text-foreground mt-0.5">{activeNode.name}</h3>
              </div>
              <span className="text-xs text-muted-foreground">
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
            2. Click <strong className="text-foreground">View Quantizations & Pull</strong> on a card to open the detail panel.
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
              {uniqueModels.map((m) => (
                <ModelCard
                  key={m.id}
                  model={m}
                  onSelect={(rect) => { setSelectedModel(m); setAnchorRect(rect); }}
                />
              ))}
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

      {selectedModel && anchorRect && (
        <ModelDetailOverlay
          model={selectedModel}
          nodeName={selectedNode}
          isLive={isLive}
          demoMode={demoMode}
          onClose={() => { setSelectedModel(null); setAnchorRect(null); }}
          anchorRect={anchorRect}
        />
      )}
    </div>
  );
}

function bytesToGB(bytes: number): string {
  const gb = bytes / (1024 * 1024 * 1024);
  return gb >= 1 ? `${gb.toFixed(1)} GB` : `${(bytes / (1024 * 1024)).toFixed(0)} MB`;
}
