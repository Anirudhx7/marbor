import { useState, useEffect } from 'react';
import { useLocation } from 'react-router-dom';
import { Package, Download, Settings2, Trash2 } from 'lucide-react';
import { StatusDot } from '../components/StatusDot';
import { Badge } from '../components/Badge';
import { SearchInput } from '../components/SearchInput';
import { mockModelCatalog, mockGPUNodes } from '../lib/mockData';
import { fetchModels, fetchNodes, deleteNodeModel } from '../lib/api';
import { startPull } from '../lib/pullProgress';
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

function ModelCard({ model, demoMode, onConfigure, onDeleted }: { model: ModelEntry; demoMode: boolean; onConfigure: () => void; onDeleted: (modelName: string, nodeName: string) => void }) {
  const isWarm = model.warm_count > 0;
  // Stores the user's explicit dropdown pick; derived below into deleteNode
  // so a stale pick (a poll refresh reorders model.nodes or drops the
  // previously-selected node) always falls back to the current first node
  // instead of silently pointing at a node that's no longer in the list.
  const [selectedDeleteNode, setSelectedDeleteNode] = useState('');
  const [deleteConfirmOpen, setDeleteConfirmOpen] = useState(false);
  const [deleteBusy, setDeleteBusy] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const deleteNode = model.nodes.some((n) => n.name === selectedDeleteNode)
    ? selectedDeleteNode
    : (model.nodes[0]?.name ?? '');

  // No pre-flight capability check here - ModelNode
  // doesn't carry agentCapabilities (only GPUNode does), so this attempts the
  // delete and surfaces the backend's own 501 "not supported" error if the
  // target node's agent lacks models.delete, rather than threading capability
  // info through the /admin/models aggregation just for a pre-check.
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
    <div className={`bg-card border shadow-sm rounded-xl p-5 hover:border-primary/50 transition-colors ${
      isWarm ? 'border-border' : 'border-border opacity-75'
    }`}>
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
              {formatVRAM(model.size_vram)} VRAM
            </p>
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
          <Badge variant={isWarm ? 'success' : 'muted'} size="sm">
            {isWarm ? `${model.warm_count} warm` : 'cold'}
          </Badge>
        </div>
      </div>

      {/* Node count indicator */}
      <div className="text-xs text-muted-foreground mb-3 font-medium">
        {model.warm_count} / {model.total_nodes} nodes
      </div>

      {/* Node chips */}
      <div className="border-t border-border pt-3">
        <p className="text-xs font-medium text-muted-foreground mb-2">Loaded on</p>
        <div className="flex flex-wrap gap-1.5">
          {model.nodes.map((node) => (
            <span
              key={node.name}
              className="inline-flex items-center gap-1.5 px-2 py-1 bg-secondary rounded-md text-xs font-medium text-foreground"
            >
              <StatusDot status={node.healthy ? 'healthy' : 'down'} />
              {node.name}
            </span>
          ))}
        </div>

        {/* Delete section */}
        {model.nodes.length > 0 && (
          <div className="mt-3 pt-3 border-t border-border">
            <p className="text-xs font-medium text-muted-foreground mb-2">Delete from node</p>
            <div className="flex gap-2">
              <CustomSelect
                value={deleteNode}
                onChange={setSelectedDeleteNode}
                options={model.nodes.map((n) => ({ value: n.name, label: n.name }))}
              />
              <button
                onClick={() => { setDeleteError(null); setDeleteConfirmOpen(true); }}
                disabled={!deleteNode}
                title={`Delete ${model.name} from ${deleteNode}`}
                className="px-3 py-1 text-xs font-medium bg-secondary border border-border rounded-md text-destructive hover:bg-destructive/10 hover:border-destructive/50 transition-colors disabled:opacity-50 disabled:cursor-not-allowed whitespace-nowrap shrink-0"
              >
                <Trash2 className="w-3.5 h-3.5" />
              </button>
            </div>
          </div>
        )}
      </div>

      {/* Delete Confirmation Modal */}
      <Modal
        isOpen={deleteConfirmOpen}
        onClose={() => setDeleteConfirmOpen(false)}
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
          {deleteError && (
            <p className="text-sm text-destructive">{deleteError}</p>
          )}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setDeleteConfirmOpen(false)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
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
  const [searchQuery, setSearchQuery] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(!demoMode);

  const [isPullModalOpen, setIsPullModalOpen] = useState(false);
  const [pullNodesList, setPullNodesList] = useState<GPUNode[]>([]);
  const [pullSelectedNode, setPullSelectedNode] = useState('');
  const [pullModelName, setPullModelName] = useState('');
  const [runtimeByNode, setRuntimeByNode] = useState<Record<string, string>>({});
  const [configModel, setConfigModel] = useState<string | null>(null);

  const location = useLocation();

  // Cross-reference each model's resident node names against node runtimes so
  // the Advanced Settings modal can gate load-time/engine params for
  // non-Ollama (or mixed) runtimes. Failure just leaves gating info absent
  // (modal defaults to enabled), never blocks the page.
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
    startPull(pullSelectedNode, trimmedModel, demoMode);
    setPullModelName('');
    setIsPullModalOpen(false);
  };

  // In demo mode there's no backend to re-poll (see the 5s interval below,
  // which only runs when !demoMode), so reflect the deletion directly in the
  // static catalog - mirrors GPUNodes.tsx's handleDeleteModel demo branch.
  // In live mode, just let the next poll (or an immediate reload) pick up
  // the real post-delete state rather than guessing at warm_count/total_nodes.
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

  const models = catalog?.models ?? [];
  const configModelEntry = configModel ? models.find((m) => m.name === configModel) ?? null : null;
  // Every node this model is resident on, paired with its runtime - the
  // Advanced Settings modal scopes configuration to one (model, node) pair
  // and needs the full list to drive its node selector.
  const configNodes = configModelEntry
    ? configModelEntry.nodes.map((n) => ({ name: n.name, runtime: runtimeByNode[n.name] || 'ollama' }))
    : [];
  // total_models is the full catalog (warm + on-disk); "warm" means loaded in VRAM somewhere.
  const warmModelCount = models.filter((m) => m.warm_count > 0).length;
  const filteredModels = models.filter((m) =>
    m.name.toLowerCase().includes(searchQuery.toLowerCase())
  );

  return (
    <div className="space-y-6 animate-fade-in max-w-7xl mx-auto">
      {/* Header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold text-foreground">Model Catalog</h1>
          <p className="text-sm text-muted-foreground mt-1">
            All models across your nodes - warm (loaded in VRAM) and available on disk
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-3">
          {catalog && (
            <span className="inline-flex items-center gap-1.5 px-3 py-1.5 bg-secondary rounded-lg text-xs font-medium text-foreground">
              <span className="text-primary font-semibold">{warmModelCount}</span> of
              <span className="text-primary font-semibold">{catalog.total_models}</span> models warm across
              <span className="text-primary font-semibold">{catalog.healthy_nodes}</span> nodes
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

      {error && !demoMode && (
        <div className="p-4 bg-destructive/10 border border-destructive/20 rounded-xl text-destructive text-sm font-medium">
          {error}
        </div>
      )}

      {/* Search */}
      <div className="max-w-md">
        <SearchInput
          value={searchQuery}
          onChange={setSearchQuery}
          placeholder="Search models by name..."
        />
      </div>

      {/* Grid */}
      {loading ? (
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-6">
          <SkeletonCard />
          <SkeletonCard />
          <SkeletonCard />
        </div>
      ) : filteredModels.length > 0 ? (
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-6">
          {filteredModels.map((model) => (
            <ModelCard key={model.name} model={model} demoMode={demoMode} onConfigure={() => setConfigModel(model.name)} onDeleted={handleModelDeleted} />
          ))}
        </div>
      ) : (
        <div className="text-center py-16 bg-card border border-border rounded-xl shadow-sm">
          <Package className="w-12 h-12 text-muted-foreground/30 mx-auto mb-4" />
          {catalog && catalog.total_nodes === 0 ? (
            <div className="space-y-1">
              <h3 className="text-lg font-semibold text-foreground">No GPU Nodes Connected</h3>
              <p className="text-muted-foreground max-w-md mx-auto text-sm leading-normal">
                Connect your first Ollama node in the <strong>GPU Nodes</strong> page to view loaded models and monitor warm VRAM status.
              </p>
            </div>
          ) : (
            <p className="text-muted-foreground text-sm font-medium">
              {searchQuery
                ? 'No models matching your search.'
                : 'No models loaded across any nodes. Start an Ollama request to load a model into VRAM.'}
            </p>
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
