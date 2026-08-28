import { useState, useEffect } from 'react';
import { Link, useLocation, useSearchParams } from 'react-router-dom';
import { Package, Download, Settings2, Trash2, AlertTriangle, Layers, Flame, Copy, CheckCircle2, ArrowUpRight } from 'lucide-react';
import { StatusDot } from '../components/StatusDot';
import { Badge } from '../components/Badge';
import { SearchInput } from '../components/SearchInput';
import { mockModelCatalog, mockGPUNodes } from '../lib/mockData';
import { fetchModels, fetchNodes, deleteNodeModel } from '../lib/api';
import { startPull, onPullSuccess } from '../lib/pullProgress';
import { useDemoMode, currentAppPath } from '../hooks/useDemoMode';
import type { ModelCatalog, ModelEntry, GPUNode } from '../types';
import { Modal } from '../components/Modal';
import { ModelConfigModal } from '../components/ModelConfigModal';
import { CustomSelect } from '../components/Select';

function formatVRAM(bytes: number): string {
  if (bytes === 0) return '0 B';
  const gb = bytes / (1024 * 1024 * 1024);
  if (gb >= 1) return `${gb.toFixed(1)} GB`;
  const mb = bytes / (1024 * 1024);
  return `${mb.toFixed(0)} MB`;
}

function shortDigest(digest?: string): string {
  if (!digest) return '-';
  let s = digest;
  if (s.startsWith('sha256:')) s = s.slice(7);
  if (s.length > 6) s = s.slice(0, 6);
  return s || '-';
}

function totalVRAMFor(model: ModelEntry): number {
  if (model.total_vram_bytes && model.total_vram_bytes > 0) return model.total_vram_bytes;
  if (model.warm_count > 0 && model.size_vram > 0) return model.warm_count * model.size_vram;
  return 0;
}

function SkeletonCard() {
  return (
    <div className="bg-card border border-border shadow-sm rounded-xl p-5 animate-pulse">
      <div className="h-5 bg-secondary rounded w-2/3 mb-4" />
      <div className="h-4 bg-secondary rounded w-1/3 mb-3" />
      <div className="flex gap-2">
        <div className="h-6 bg-secondary rounded w-20" />
        <div className="h-6 bg-secondary rounded w-20" />
      </div>
    </div>
  );
}

function ModelFleetCard({ model, demoMode, onConfigure, onDeleted }: { model: ModelEntry; demoMode: boolean; onConfigure: () => void; onDeleted: (modelName: string, nodeName: string) => void }) {
  const isWarm = model.warm_count > 0;
  const totalVRAM = totalVRAMFor(model);
  const isDrifted = !!model.digest_mismatch;
  const driftDetails = model.drift_details || '';
  const wasteCopies = model.warm_count > 1 ? model.warm_count - 1 : 0;
  const wasteVRAM = wasteCopies > 0 && model.size_vram ? wasteCopies * model.size_vram : 0;

  const [selectedDeleteNode, setSelectedDeleteNode] = useState('');
  const [deleteConfirmOpen, setDeleteConfirmOpen] = useState(false);
  const [deleteBusy, setDeleteBusy] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const deleteNode = model.nodes.some((n) => n.name === selectedDeleteNode)
    ? selectedDeleteNode
    : (model.nodes[0]?.name ?? '');

  const handleDeleteModel = async () => {
    if (!deleteNode) return;
    if (demoMode) {
      setDeleteError(null);
      setDeleteConfirmOpen(false);
      onDeleted(model.name, deleteNode);
      return;
    }
    setDeleteBusy(true);
    try {
      await deleteNodeModel(deleteNode, model.name);
      setDeleteError(null);
      setDeleteConfirmOpen(false);
      onDeleted(model.name, deleteNode);
    } catch (e: unknown) {
      setDeleteError(e instanceof Error ? e.message : `Failed to delete ${model.name} from ${deleteNode}`);
    } finally {
      setDeleteBusy(false);
    }
  };

  return (
    <div className={`bg-card border shadow-sm rounded-xl p-5 hover:border-primary/50 transition-all duration-300 ease-out hover:shadow-md hover:scale-[1.01] active:scale-[0.99] ${isWarm ? 'border-border' : 'border-border opacity-80'}`}>
      {/* Header */}
      <div className="flex items-start justify-between mb-3">
        <div className="flex items-start gap-3 min-w-0">
          <div className="p-2 bg-secondary rounded-lg shrink-0">
            <Package className="w-4 h-4 text-muted-foreground" />
          </div>
          <div className="min-w-0">
            <h3 className="font-mono font-semibold text-foreground truncate text-sm">
              {model.name}
            </h3>
            <p className="text-xs text-muted-foreground mt-0.5">
              {formatVRAM(model.size_vram)} per copy
              {totalVRAM ? ` · ${formatVRAM(totalVRAM)} total warm` : ''}
              {model.size_disk ? ` · ${formatVRAM(model.size_disk)} on disk` : ''}
            </p>
            {model.family && (
              <p className="text-[11px] text-muted-foreground/70 font-mono mt-0.5">{model.family}</p>
            )}
          </div>
        </div>
        <div className="flex items-center gap-1.5 shrink-0">
          <button
            onClick={onConfigure}
            title={`Advanced settings for ${model.name}`}
            className="p-1 text-muted-foreground hover:text-primary transition-colors"
          >
            <Settings2 className="w-3.5 h-3.5" />
          </button>
          {isDrifted ? (
            <span title={driftDetails ? `Drift: ${driftDetails}` : 'Nodes disagree on this model\'s content - different digests reported for the same name'}>
              <Badge variant="warning" size="sm">
                drift {driftDetails ? driftDetails : ''}
              </Badge>
            </span>
          ) : (
            model.warm_count > 1 && (
              <span title="All warm copies report the same digest">
                <Badge variant="success" size="sm">
                  consistent
                </Badge>
              </span>
            )
          )}
          <Badge variant={isWarm ? 'success' : 'muted'} size="sm">
            {isWarm ? `${model.warm_count} warm` : 'cold'}
          </Badge>
        </div>
      </div>

      {/* Fleet residency summary */}
      <div className="flex flex-wrap items-center gap-2 text-xs mb-3">
        <span className="inline-flex items-center gap-1.5 px-2 py-1 bg-secondary rounded-md font-medium text-foreground">
          <Layers className="w-3 h-3 text-muted-foreground" />
          {model.warm_count} / {model.total_nodes} nodes warm
        </span>
        {totalVRAM > 0 && (
          <span className="inline-flex items-center gap-1.5 px-2 py-1 bg-secondary rounded-md font-medium text-foreground" title="Sum of VRAM across all warm copies (live, not estimated)">
            <Flame className="w-3 h-3 text-muted-foreground" />
            {formatVRAM(totalVRAM)} warm total
          </span>
        )}
        {wasteCopies > 0 && (
          <span className="inline-flex items-center gap-1.5 px-2 py-1 bg-amber-500/10 border border-amber-500/20 rounded-md font-medium text-amber-700 dark:text-amber-400" title="Duplicated warm copies beyond 1 - VRAM that could hold a different model">
            <Copy className="w-3 h-3" />
            +{wasteCopies} dup · {wasteVRAM ? formatVRAM(wasteVRAM) : '-'} waste
          </span>
        )}
        {isDrifted && driftDetails && (
          <span className="inline-flex items-center gap-1.5 px-2 py-1 bg-warning/10 border border-warning/20 rounded-md font-mono text-[11px] text-warning" title="Distinct digests (short hex) seen across nodes for this model">
            <AlertTriangle className="w-3 h-3" />
            {driftDetails}
          </span>
        )}
      </div>

      {/* Node chips - read-first, link to GPU Nodes for mutations */}
      <div className="border-t border-border pt-3">
        <div className="flex items-center justify-between mb-2 gap-2">
          <p className="text-xs font-medium text-muted-foreground">Resident on</p>
          <Link to={`/gpu-nodes?highlight=${encodeURIComponent(model.nodes.map((n) => n.name).join(','))}&from=models`} className="text-xs text-primary hover:text-primary/80 inline-flex items-center gap-1 font-medium min-h-[32px] px-2 py-1 rounded-md hover:bg-secondary transition-colors shrink-0">
            View nodes <ArrowUpRight className="w-3 h-3 shrink-0" />
          </Link>
        </div>
        <div className="flex flex-wrap gap-1.5">
          {model.nodes.map((node) => (
            <Link
              key={node.name}
              to={`/gpu-nodes?highlight=${encodeURIComponent(node.name)}&from=models`}
              title={`${node.name} · ${node.runtime || 'runtime unknown'} · ${node.warm ? 'warm' : 'cold'} · digest ${node.digest || '-'}${node.vram_bytes ? ` · ${formatVRAM(node.vram_bytes)}` : ''}`}
              className="inline-flex items-center gap-1.5 px-2 py-1 bg-secondary hover:bg-secondary/80 border border-transparent hover:border-border rounded-md text-xs font-medium text-foreground transition-all duration-200 ease-out hover:scale-[1.02] active:scale-[0.98]"
            >
              <StatusDot status={node.healthy ? 'healthy' : 'down'} />
              <span className="font-mono">{node.name}</span>
              {node.runtime && (
                <span className="text-[10px] px-1 py-0.5 bg-card border border-border rounded font-mono text-muted-foreground">
                  {node.runtime}
                </span>
              )}
              <span className={`text-[10px] font-mono px-1 py-0.5 rounded ${node.warm ? 'bg-success/10 text-success border border-success/20' : 'bg-muted text-muted-foreground border border-border'}`}>
                {node.warm ? 'warm' : 'cold'}
              </span>
              <span className="text-[10px] font-mono text-muted-foreground" title={node.digest ? `Digest: ${node.digest}` : 'Digest unknown'}>
                {shortDigest(node.digest)}
              </span>
              {node.vram_bytes ? (
                <span className="text-[10px] font-mono text-muted-foreground">{formatVRAM(node.vram_bytes)}</span>
              ) : null}
            </Link>
          ))}
          {model.nodes.length === 0 && (
            <span className="text-xs text-muted-foreground">No node reports this model</span>
          )}
        </div>

        {/* Delete section - secondary, link-first mutations stay on GPU Nodes */}
        {model.nodes.length > 0 && (
          <div className="mt-3 pt-3 border-t border-border">
            <p className="text-xs font-medium text-muted-foreground mb-2">Delete from node <span className="font-normal">(secondary - prefer GPU Nodes for fleet changes)</span></p>
            <div className="flex gap-2 items-center">
              <div className="flex-1 min-w-0">
                <CustomSelect
                  value={deleteNode}
                  onChange={setSelectedDeleteNode}
                  options={model.nodes.map((n) => ({ value: n.name, label: n.name }))}
                />
              </div>
              <button
                onClick={() => { setDeleteError(null); setDeleteConfirmOpen(true); }}
                disabled={!deleteNode}
                title={`Delete ${model.name} from ${deleteNode}`}
                className="px-3 py-2 min-h-[44px] min-w-[44px] flex items-center justify-center text-xs font-medium bg-secondary border border-border rounded-md text-destructive hover:bg-destructive/10 hover:border-destructive/50 transition-all duration-200 ease-out disabled:opacity-50 disabled:cursor-not-allowed whitespace-nowrap shrink-0"
              >
                <Trash2 className="w-3.5 h-3.5" />
              </button>
            </div>
          </div>
        )}
      </div>

      <Modal
        isOpen={deleteConfirmOpen}
        onClose={() => { if (!deleteBusy) setDeleteConfirmOpen(false); }}
        title="Delete Local Model"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Delete <span className="text-foreground font-semibold break-all">{model.name}</span> from{' '}
            <span className="text-foreground font-semibold">{deleteNode}</span>'s local storage?
          </p>
          <p className="text-xs text-muted-foreground">
            This removes the downloaded model files from disk - not just from VRAM. Re-pulling it later will re-download the full model.
          </p>
          {deleteBusy && (
            <p className="text-xs text-muted-foreground">
              Deleting can take a minute or more for large models over slow or network storage - please wait, this dialog will close automatically.
            </p>
          )}
          {deleteError && (
            <p className="text-sm text-destructive">{deleteError}</p>
          )}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setDeleteConfirmOpen(false)}
              disabled={deleteBusy}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={handleDeleteModel}
              disabled={deleteBusy}
              className="px-4 py-2 bg-destructive hover:bg-destructive/90 disabled:opacity-50 disabled:cursor-not-allowed text-destructive-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              {deleteBusy ? 'Deleting...' : 'Delete Model'}
            </button>
          </div>
        </div>
      </Modal>
    </div>
  );
}

export function Models() {
  const { demoMode } = useDemoMode();
  const [catalog, setCatalog] = useState<ModelCatalog | null>(demoMode ? mockModelCatalog : null);
  const [isLive, setIsLive] = useState(!demoMode);
  const [searchParams, setSearchParams] = useSearchParams();
  const searchQuery = searchParams.get('q') || '';
  const driftedOnly = searchParams.get('drifted') === '1';
  const warmOnly = searchParams.get('warm') === '1';
  const activeTab = (searchParams.get('view') === 'catalog' ? 'catalog' : 'fleet') as 'fleet' | 'catalog';
  const setSearchQuery = (v: string) => {
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev);
      if (v) next.set('q', v); else next.delete('q');
      return next;
    }, { replace: true });
  };
  const setDriftedOnly = (v: boolean) => {
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev);
      if (v) next.set('drifted', '1'); else next.delete('drifted');
      return next;
    }, { replace: true });
  };
  const setWarmOnly = (v: boolean) => {
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev);
      if (v) next.set('warm', '1'); else next.delete('warm');
      return next;
    }, { replace: true });
  };
  const setActiveTab = (v: 'fleet' | 'catalog') => {
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev);
      if (v === 'catalog') next.set('view', 'catalog'); else next.delete('view');
      return next;
    }, { replace: true });
  };
  const clearAllFilters = () => {
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev);
      next.delete('q');
      next.delete('drifted');
      next.delete('warm');
      return next;
    }, { replace: true });
  };
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(!demoMode);

  const [isPullModalOpen, setIsPullModalOpen] = useState(false);
  const [pullNodesList, setPullNodesList] = useState<GPUNode[]>([]);
  const [pullSelectedNode, setPullSelectedNode] = useState('');
  const [pullModelName, setPullModelName] = useState('');
  const [pullVerifyLoad, setPullVerifyLoad] = useState(true);
  const [runtimeByNode, setRuntimeByNode] = useState<Record<string, string>>({});
  const [configModel, setConfigModel] = useState<string | null>(null);

  const location = useLocation();

  useEffect(() => {
    if (currentAppPath() !== '/models') return;
    let active = true;
    (async () => {
      try {
        const list = demoMode ? mockGPUNodes : await fetchNodes();
        if (!active || currentAppPath() !== '/models') return;
        setRuntimeByNode(Object.fromEntries((list || []).map((n) => [n.name, n.runtime])));
      } catch {
        if (!active || currentAppPath() !== '/models') return;
        setRuntimeByNode({});
      }
    })();
    return () => { active = false; };
  }, [demoMode, location.pathname]);

  const openPullModal = async () => {
    if (demoMode) {
      setPullNodesList(mockGPUNodes);
      const healthyNode = mockGPUNodes.find((n) => n.health === 'healthy') || mockGPUNodes[0];
      setPullSelectedNode(healthyNode?.name ?? '');
      setIsPullModalOpen(true);
      return;
    }

    try {
      const nodeList = await fetchNodes();
      setPullNodesList(nodeList || []);
      if (nodeList && nodeList.length > 0) {
        const healthyNode = nodeList.find(n => n.health === 'healthy') || nodeList[0];
        setPullSelectedNode(healthyNode.name);
      }
      setIsPullModalOpen(true);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to load nodes for pulling');
    }
  };

  const handleGeneralPull = () => {
    const trimmedModel = pullModelName.trim();
    if (!trimmedModel || !pullSelectedNode) return;
    startPull(pullSelectedNode, trimmedModel, demoMode, pullVerifyLoad);
    setPullModelName('');
    setIsPullModalOpen(false);
  };

  const handleModelDeleted = (modelName: string, nodeName: string) => {
    if (demoMode) {
      setCatalog((prev) => prev ? {
        ...prev,
        models: prev.models
          .map((m) => m.name === modelName ? { ...m, nodes: m.nodes.filter((n) => n.name !== nodeName) } : m)
          .filter((m) => m.nodes.length > 0),
      } : prev);
      return;
    }
    loadModels();
  };

  const loadModels = async (active: boolean = true) => {
    if (demoMode) {
      if (!active || currentAppPath() !== '/models') return;
      setCatalog(mockModelCatalog);
      setIsLive(false);
      setError(null);
      setLoading(false);
      return;
    }
    try {
      const data = await fetchModels();
      if (!active || currentAppPath() !== '/models') return;
      setCatalog(data);
      setIsLive(true);
      setError(null);
    } catch (e: unknown) {
      if (!active || currentAppPath() !== '/models') return;
      setIsLive(false);
      setError(e instanceof Error ? e.message : 'Failed to connect to backend');
    } finally {
      if (active && currentAppPath() === '/models') {
        setLoading(false);
      }
    }
  };

  useEffect(() => {
    if (currentAppPath() !== '/models') return;
    let active = true;
    loadModels(active);
    if (demoMode) return () => { active = false; };
    const interval = setInterval(() => loadModels(active), 5000);
    return () => {
      active = false;
      clearInterval(interval);
    };
  }, [demoMode, location.pathname]);

  useEffect(() => {
    return onPullSuccess(() => {
      if (currentAppPath() !== '/models') return;
      loadModels();
    });
  }, [demoMode]);

  const models = catalog?.models ?? [];
  const configModelEntry = configModel ? models.find((m) => m.name === configModel) ?? null : null;
  const configNodes = configModelEntry
    ? configModelEntry.nodes.map((n) => ({ name: n.name, runtime: runtimeByNode[n.name] || n.runtime || 'ollama' }))
    : [];
  // Fleet summary - live, never estimated
  const warmModels = models.filter((m) => m.warm_count > 0);
  const warmModelCount = warmModels.length;
  const totalWarmCopies = models.reduce((a, m) => a + m.warm_count, 0);
  const driftedModels = models.filter((m) => m.digest_mismatch);
  const driftedCount = driftedModels.length;
  const duplicatedCopies = models.reduce((a, m) => a + (m.warm_count > 1 ? m.warm_count - 1 : 0), 0);
  const duplicatedVRAM = models.reduce((a, m) => {
    if (m.warm_count <= 1) return a;
    const per = m.size_vram || 0;
    if (per === 0) return a;
    return a + per * (m.warm_count - 1);
  }, 0);
  const totalWarmVRAM = models.reduce((a, m) => a + totalVRAMFor(m), 0);

  const filteredModels = models.filter((m) => {
    if (searchQuery && !m.name.toLowerCase().includes(searchQuery.toLowerCase())) return false;
    if (driftedOnly && !m.digest_mismatch) return false;
    if (warmOnly && m.warm_count === 0) return false;
    return true;
  });

  return (
    <div className="space-y-6 animate-fade-in max-w-7xl mx-auto">
      {/* Header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold text-foreground">Models</h1>
          <p className="text-sm text-muted-foreground mt-1">
            Fleet residency · where warm, how many copies, total VRAM, drift. Catalog is secondary below.
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-3">
          {catalog && (
            <span className="inline-flex items-center gap-1.5 px-3 py-1.5 bg-secondary rounded-lg text-xs font-medium text-foreground">
              <span className="text-primary font-semibold">{warmModelCount}</span> of
              <span className="text-primary font-semibold">{catalog.total_models}</span> models warm ·
              <span className="text-primary font-semibold">{catalog.healthy_nodes}</span>/{catalog.total_nodes} nodes healthy
            </span>
          )}
          <div className="flex items-center gap-2">
            <div className={`w-2 h-2 rounded-full ${isLive ? 'bg-success' : 'bg-amber-500'}`} />
            <span className={`text-xs font-medium ${isLive ? 'text-success' : 'text-amber-600 dark:text-amber-400'}`}>
              {demoMode ? 'Demo Mode' : isLive ? 'Live Data' : 'Disconnected'}
            </span>
          </div>
          <button
            onClick={openPullModal}
            disabled={!demoMode && !isLive}
            title={!demoMode && !isLive ? 'Backend disconnected' : undefined}
            className="flex items-center gap-2 px-3 py-1.5 bg-primary hover:bg-primary/90 disabled:opacity-50 disabled:cursor-not-allowed text-primary-foreground text-xs font-semibold rounded-lg transition-colors shadow-sm cursor-pointer"
          >
            <Download className="w-3.5 h-3.5" />
            Pull Model
          </button>
        </div>
      </div>

      {/* Tabs - Fleet first, Catalog secondary - smooth like sidenav 200ms ease */}
      <div className="flex items-center gap-1 p-1 bg-secondary rounded-lg w-fit">
        <button
          onClick={() => setActiveTab('fleet')}
          className={`px-4 py-1.5 text-sm font-medium rounded-md transition-all duration-200 ease-out hover:scale-[1.02] active:scale-[0.97] ${activeTab === 'fleet' ? 'bg-card shadow-sm text-foreground border border-border' : 'text-muted-foreground hover:text-foreground'}`}
        >
          Fleet
        </button>
        <button
          onClick={() => setActiveTab('catalog')}
          className={`px-4 py-1.5 text-sm font-medium rounded-md transition-all duration-200 ease-out hover:scale-[1.02] active:scale-[0.97] ${activeTab === 'catalog' ? 'bg-card shadow-sm text-foreground border border-border' : 'text-muted-foreground hover:text-foreground'}`}
        >
          Catalog
        </button>
      </div>

      {activeTab === 'fleet' ? (
        <div className="space-y-6 animate-fade-in">
          {error && !demoMode && (
            <div className="p-4 bg-destructive/10 border border-destructive/20 rounded-xl text-destructive text-sm font-medium">
              {error}
            </div>
          )}

          {/* Fleet summary cards */}
          <div className="grid grid-cols-2 lg:grid-cols-4 gap-4">
            <div className="bg-card border border-border rounded-xl p-4">
              <p className="text-xs font-semibold text-muted-foreground uppercase tracking-wider">Distinct warm</p>
              <p className="text-2xl font-bold text-foreground mt-1">{warmModelCount}</p>
              <p className="text-xs text-muted-foreground mt-1">{models.length} total models in fleet</p>
            </div>
            <div className="bg-card border border-border rounded-xl p-4">
              <p className="text-xs font-semibold text-muted-foreground uppercase tracking-wider">Warm copies</p>
              <p className="text-2xl font-bold text-foreground mt-1">{totalWarmCopies}</p>
              <p className="text-xs text-muted-foreground mt-1">{totalWarmVRAM ? formatVRAM(totalWarmVRAM) + ' total warm' : '- total'}</p>
            </div>
            <div className="bg-card border border-border rounded-xl p-4">
              <p className="text-xs font-semibold text-muted-foreground uppercase tracking-wider flex items-center gap-1">
                <AlertTriangle className="w-3.5 h-3.5" /> Drifted
              </p>
              <p className={`text-2xl font-bold mt-1 ${driftedCount > 0 ? 'text-amber-700 dark:text-amber-400' : 'text-foreground'}`}>{driftedCount}</p>
              <p className="text-xs text-muted-foreground mt-1">{driftedCount > 0 ? 'digest mismatch' : 'all consistent'}</p>
            </div>
            <div className="bg-card border border-border rounded-xl p-4">
              <p className="text-xs font-semibold text-muted-foreground uppercase tracking-wider flex items-center gap-1">
                <Copy className="w-3.5 h-3.5" /> Duplication waste
              </p>
              <p className="text-2xl font-bold text-foreground mt-1">{duplicatedCopies}</p>
              <p className="text-xs text-muted-foreground mt-1">{duplicatedVRAM ? `${formatVRAM(duplicatedVRAM)} duplicated VRAM` : 'no waste'}</p>
            </div>
          </div>

          {/* Filters - search + toggles, smooth picking/clearing like sidenav 200ms ease */}
          <div className="flex flex-col sm:flex-row gap-3 items-start sm:items-center">
            <div className="w-full sm:max-w-md transition-all duration-200 ease-out">
              <SearchInput
                value={searchQuery}
                onChange={setSearchQuery}
                placeholder="Search models by name..."
              />
            </div>
            <div className="flex flex-wrap items-center gap-2">
              <label className={`inline-flex items-center gap-2 px-3 py-1.5 border rounded-lg text-xs font-medium cursor-pointer transition-all duration-200 ease-out hover:scale-[1.02] active:scale-[0.97] ${driftedOnly ? 'bg-primary/10 border-primary/30 text-primary shadow-sm' : 'bg-secondary border-border hover:bg-secondary/80'}`}>
                <input
                  type="checkbox"
                  checked={driftedOnly}
                  onChange={(e) => setDriftedOnly(e.target.checked)}
                  className="accent-primary cursor-pointer transition-all duration-200"
                />
                Drifted only
              </label>
              <label className={`inline-flex items-center gap-2 px-3 py-1.5 border rounded-lg text-xs font-medium cursor-pointer transition-all duration-200 ease-out hover:scale-[1.02] active:scale-[0.97] ${warmOnly ? 'bg-primary/10 border-primary/30 text-primary shadow-sm' : 'bg-secondary border-border hover:bg-secondary/80'}`}>
                <input
                  type="checkbox"
                  checked={warmOnly}
                  onChange={(e) => setWarmOnly(e.target.checked)}
                  className="accent-primary cursor-pointer transition-all duration-200"
                />
                Warm only
              </label>
              <div className={`transition-all duration-200 ease-out ${driftedOnly || warmOnly || searchQuery ? 'opacity-100 translate-y-0 scale-100' : 'opacity-0 -translate-y-1 scale-95 pointer-events-none w-0 overflow-hidden'}`}>
                <button
                  onClick={clearAllFilters}
                  className="text-xs text-muted-foreground hover:text-foreground px-2 py-1 rounded-md hover:bg-secondary transition-all duration-200 ease-out hover:scale-105 active:scale-95 whitespace-nowrap"
                >
                  Clear filters
                </button>
              </div>
            </div>
          </div>

          {/* Fleet content */}
          {loading ? (
            <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-6 animate-fade-in">
              <SkeletonCard />
              <SkeletonCard />
              <SkeletonCard />
            </div>
          ) : filteredModels.length > 0 ? (
            <div className="space-y-6 animate-fade-in">
              {/* Desktop table - hidden on mobile, no horizontal scroll at 375px because hidden */}
              <div className="hidden md:block bg-card border border-border rounded-xl overflow-hidden animate-fade-in transition-all duration-300 ease-out">
                <div className="overflow-x-auto">
                  <table className="w-full text-sm">
                    <thead>
                      <tr className="border-b border-border bg-secondary/30 text-xs font-semibold text-muted-foreground uppercase tracking-wider">
                        <th className="text-left px-4 py-3 font-semibold">Model</th>
                        <th className="text-left px-4 py-3 font-semibold">Warm</th>
                        <th className="text-left px-4 py-3 font-semibold">Total VRAM</th>
                        <th className="text-left px-4 py-3 font-semibold">Drift</th>
                        <th className="text-left px-4 py-3 font-semibold">Resident on</th>
                        <th className="text-left px-4 py-3 font-semibold">Actions</th>
                      </tr>
                    </thead>
                    <tbody>
                      {filteredModels.map((model) => {
                        const totalVRAM = totalVRAMFor(model);
                        const isDrifted = !!model.digest_mismatch;
                        return (
                          <tr key={model.name} className="border-b border-border last:border-0 hover:bg-secondary/30 transition-colors">
                            <td className="px-4 py-3 align-top">
                              <div className="font-mono font-semibold text-foreground text-sm truncate max-w-[200px]" title={model.name}>{model.name}</div>
                              <div className="text-xs text-muted-foreground font-mono">{formatVRAM(model.size_vram)} per copy{model.size_disk ? ` · ${formatVRAM(model.size_disk)} disk` : ''}</div>
                              {model.family && <div className="text-[11px] text-muted-foreground/70 font-mono">{model.family}</div>}
                            </td>
                            <td className="px-4 py-3 align-top">
                              <Badge variant={model.warm_count > 0 ? 'success' : 'muted'} size="sm">
                                {model.warm_count}/{model.total_nodes}
                              </Badge>
                              {model.warm_count > 1 && model.size_vram > 0 && (
                                <div className="text-[11px] text-amber-700 dark:text-amber-400 mt-1 font-medium">+{model.warm_count - 1} dup</div>
                              )}
                            </td>
                            <td className="px-4 py-3 align-top font-mono text-xs text-foreground">
                              {totalVRAM ? formatVRAM(totalVRAM) : '-'}
                            </td>
                            <td className="px-4 py-3 align-top">
                              {isDrifted ? (
                                <span title={model.drift_details || 'digest mismatch'}>
                                  <Badge variant="warning" size="sm">
                                    {model.drift_details || 'drift'}
                                  </Badge>
                                </span>
                              ) : (
                                <span className="inline-flex items-center gap-1 text-xs text-success">
                                  <CheckCircle2 className="w-3.5 h-3.5" /> ok
                                </span>
                              )}
                            </td>
                            <td className="px-4 py-3 align-top">
                              <div className="flex flex-wrap gap-1.5 max-w-[320px]">
                                {model.nodes.map((node) => (
                                  <Link
                                    key={node.name}
                                    to={`/gpu-nodes?highlight=${encodeURIComponent(node.name)}&from=models`}
                                    title={`${node.name} ${node.runtime || ''} ${node.warm ? 'warm' : 'cold'} ${node.digest || ''}`}
                                    className="inline-flex items-center gap-1 px-1.5 py-0.5 bg-secondary rounded text-xs font-medium hover:bg-secondary/80 transition-all duration-200 ease-out hover:scale-[1.02] active:scale-[0.98]"
                                  >
                                    <StatusDot status={node.healthy ? 'healthy' : 'down'} size="sm" />
                                    <span className="font-mono">{node.name}</span>
                                    <span className="text-[10px] font-mono text-muted-foreground">{shortDigest(node.digest)}</span>
                                  </Link>
                                ))}
                              </div>
                            </td>
                            <td className="px-4 py-3 align-top">
                              <div className="flex items-center gap-1.5">
                                <button
                                  onClick={() => setConfigModel(model.name)}
                                  title={`Settings for ${model.name}`}
                                  className="p-1.5 text-muted-foreground hover:text-primary hover:bg-secondary rounded transition-all duration-200 ease-out hover:scale-110 active:scale-95"
                                >
                                  <Settings2 className="w-3.5 h-3.5" />
                                </button>
                                <Link
                                  to={`/gpu-nodes?highlight=${encodeURIComponent(model.nodes.map((n) => n.name).join(','))}&from=models`}
                                  title="Manage on GPU Nodes (mutations live there)"
                                  className="p-1.5 text-muted-foreground hover:text-primary hover:bg-secondary rounded transition-all duration-200 ease-out hover:scale-110 active:scale-95"
                                >
                                  <ArrowUpRight className="w-3.5 h-3.5" />
                                </Link>
                              </div>
                            </td>
                          </tr>
                        );
                      })}
                    </tbody>
                  </table>
                </div>
              </div>

              {/* Mobile cards - visible only below md, stacked, no horizontal scroll */}
              <div className="grid grid-cols-1 md:hidden gap-4 animate-fade-in">
                {filteredModels.map((model) => (
                  <ModelFleetCard key={model.name} model={model} demoMode={demoMode} onConfigure={() => setConfigModel(model.name)} onDeleted={handleModelDeleted} />
                ))}
              </div>

              {/* Desktop also show cards as secondary grid? Keep table as primary on desktop, but also provide card grid for quick scan on large screens - hidden, table is enough */}
              <div className="hidden lg:hidden md:grid grid-cols-2 gap-6">
                {/* This block intentionally empty - table covers md+ */}
              </div>
            </div>
          ) : (
            <div className="text-center py-16 bg-card border border-border rounded-xl shadow-sm animate-fade-in">
              <Package className="w-12 h-12 text-muted-foreground/30 mx-auto mb-4" />
              {catalog && catalog.total_nodes === 0 ? (
                <div className="space-y-1">
                  <h3 className="text-lg font-semibold text-foreground">No GPU Nodes Connected</h3>
                  <p className="text-muted-foreground max-w-md mx-auto text-sm leading-normal">
                    Connect your first node in the <strong>GPU Nodes</strong> page to view fleet residency and monitor warm VRAM.
                  </p>
                </div>
              ) : (
                <p className="text-muted-foreground text-sm font-medium">
                  {searchQuery || driftedOnly || warmOnly
                    ? 'No models matching your filters.'
                    : 'No models reported across any nodes. Start a request or pull a model to populate the fleet.'}
                </p>
              )}
            </div>
          )}
        </div>
      ) : (
        /* Catalog secondary - read-only browse, fleet remains headline */
        <div className="space-y-6 animate-fade-in">
          <div className="bg-card border border-border rounded-xl p-6">
            <h3 className="text-lg font-semibold text-foreground flex items-center gap-2">
              <Layers className="w-5 h-5 text-primary" />
              Model Catalog
            </h3>
            <p className="text-sm text-muted-foreground mt-2 leading-normal">
              Catalog is secondary to fleet intelligence. Browse curated popular models and check per-node fit in{' '}
              <Link to="/model-advisor" className="text-primary hover:underline font-medium">
                Model Advisor
              </Link>
              , or pull any model directly to a node below. The fleet view above is the source of truth for what is actually warm and where.
            </p>
            <div className="flex flex-wrap gap-3 mt-4">
              <Link
                to="/model-advisor"
                className="inline-flex items-center gap-2 px-4 py-2 bg-secondary hover:bg-secondary/80 border border-border rounded-lg text-sm font-semibold text-foreground transition-colors"
              >
                <Layers className="w-4 h-4" />
                Open Model Advisor
              </Link>
              <button
                onClick={openPullModal}
                className="inline-flex items-center gap-2 px-4 py-2 bg-primary hover:bg-primary/90 text-primary-foreground font-semibold rounded-lg text-sm transition-colors shadow-sm"
              >
                <Download className="w-4 h-4" />
                Pull Model
              </button>
            </div>
          </div>

          {/* Reuse fleet grid in catalog tab as "currently available models" - not duplicated catalog.go logic */}
          {loading ? (
            <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-6">
              <SkeletonCard />
              <SkeletonCard />
              <SkeletonCard />
            </div>
          ) : filteredModels.length > 0 ? (
            <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-6">
              {filteredModels.map((model) => (
                <ModelFleetCard key={model.name} model={model} demoMode={demoMode} onConfigure={() => setConfigModel(model.name)} onDeleted={handleModelDeleted} />
              ))}
            </div>
          ) : (
            <div className="text-center py-12 bg-card border border-border rounded-xl">
              <Package className="w-10 h-10 text-muted-foreground/30 mx-auto mb-3" />
              <p className="text-sm text-muted-foreground">No models to show in catalog view. Pull a model or adjust filters on the Fleet tab.</p>
            </div>
          )}
        </div>
      )}

      {/* General Pull Model Modal */}
      <Modal
        isOpen={isPullModalOpen}
        onClose={() => setIsPullModalOpen(false)}
        title="Pull Model from Registry"
      >
        <div className="space-y-4">
          <p className="text-xs text-muted-foreground leading-normal">
            Download a model from the official Ollama library directly to one of your GPU nodes.
            Note: The node must have internet access to reach the Ollama registry.
          </p>

          <div className="space-y-1.5">
            <label className="text-xs font-semibold text-muted-foreground uppercase tracking-wider block">Target GPU Node</label>
            <CustomSelect
              value={pullSelectedNode}
              onChange={setPullSelectedNode}
              options={pullNodesList.map((n) => ({
                value: n.name,
                label: `${n.name} (${n.health})`
              }))}
            />
          </div>

          <div className="space-y-1.5">
            <label className="text-xs font-semibold text-muted-foreground uppercase tracking-wider block">Model Tag / Name</label>
            <input
              type="text"
              value={pullModelName}
              onChange={(e) => setPullModelName(e.target.value)}
              onKeyDown={(e) => e.key === 'Enter' && pullModelName.trim() && pullSelectedNode && handleGeneralPull()}
              placeholder="e.g. llama3.2, gemma2, nomic-embed-text"
              className="w-full px-3 py-2 text-sm bg-secondary border border-border rounded-lg text-foreground placeholder-muted-foreground focus:outline-none focus:ring-1 focus:ring-primary disabled:opacity-50"
            />
          </div>

          <label className="flex items-start gap-2 cursor-pointer">
            <input
              type="checkbox"
              checked={pullVerifyLoad}
              onChange={(e) => setPullVerifyLoad(e.target.checked)}
              className="mt-0.5 accent-primary cursor-pointer"
            />
            <span className="text-xs text-muted-foreground leading-normal">
              Verify it loads before reporting success. Recommended for community/Hugging Face
              models - some architectures download fine but fail to load; this catches that at
              pull time instead of the first time something tries to use the model.
            </span>
          </label>

          <div className="flex items-center justify-end gap-3 pt-2">
            <button
              onClick={() => setIsPullModalOpen(false)}
              className="px-4 py-2 bg-secondary hover:bg-secondary/80 disabled:opacity-50 text-foreground text-sm font-semibold rounded-lg transition-colors cursor-pointer"
            >
              Close
            </button>
            <button
              onClick={handleGeneralPull}
              disabled={!pullModelName.trim() || !pullSelectedNode}
              className="inline-flex items-center gap-2 px-4 py-2 bg-primary hover:bg-primary/90 disabled:opacity-50 disabled:cursor-not-allowed text-primary-foreground font-semibold rounded-lg transition-colors shadow-sm cursor-pointer"
            >
              <Download className="w-4 h-4" />
              Pull Model
            </button>
          </div>
        </div>
      </Modal>

      {/* Model Advanced Settings Modal */}
      <ModelConfigModal
        model={configModel}
        demoMode={demoMode}
        nodes={configNodes}
        onClose={() => setConfigModel(null)}
      />
    </div>
  );
}
