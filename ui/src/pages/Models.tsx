import { useState, useEffect } from 'react';
import { Package, Download, Loader2 } from 'lucide-react';
import { StatusDot } from '../components/StatusDot';
import { Badge } from '../components/Badge';
import { SearchInput } from '../components/SearchInput';
import { mockModelCatalog } from '../lib/mockData';
import { fetchModels, pullModel, fetchNodes } from '../lib/api';
import { useDemoMode } from '../hooks/useDemoMode';
import type { ModelCatalog, ModelEntry, GPUNode } from '../types';
import { Modal } from '../components/Modal';

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

type PullStatus = 'idle' | 'pulling' | 'success' | 'error';

function ModelCard({ model, demoMode }: { model: ModelEntry; demoMode: boolean }) {
  const isWarm = model.warm_count > 0;
  const [pullInput, setPullInput] = useState('');
  const [pullStatus, setPullStatus] = useState<PullStatus>('idle');
  const [pullError, setPullError] = useState('');

  // Pick the first healthy node for pull target, fall back to any node
  const targetNode = model.nodes.find((n) => n.healthy) ?? model.nodes[0];

  const handlePull = async () => {
    const trimmed = pullInput.trim();
    if (!trimmed) return;
    setPullStatus('pulling');
    setPullError('');
    try {
      if (demoMode) {
        await new Promise<void>((resolve) => setTimeout(resolve, 1000));
      } else {
        await pullModel(targetNode.name, trimmed);
      }
      setPullStatus('success');
      setPullInput('');
    } catch (e: unknown) {
      setPullStatus('error');
      setPullError(e instanceof Error ? e.message : 'Pull failed');
    } finally {
      setTimeout(() => {
        setPullStatus('idle');
        setPullError('');
      }, 3000);
    }
  };

  const isPulling = pullStatus === 'pulling';
  const pullDisabled = isPulling || !pullInput.trim() || !targetNode;

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
        <Badge variant={isWarm ? 'success' : 'muted'} size="sm">
          {isWarm ? `${model.warm_count} warm` : 'cold'}
        </Badge>
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

        {/* Pull section */}
        {targetNode && (
          <div className="mt-3 pt-3 border-t border-border">
            <p className="text-xs font-medium text-muted-foreground mb-2">
              Pull to {targetNode.name}
            </p>
            <div className="flex gap-2">
              <input
                type="text"
                value={pullInput}
                onChange={(e) => setPullInput(e.target.value)}
                onKeyDown={(e) => e.key === 'Enter' && !pullDisabled && handlePull()}
                placeholder="model:tag"
                disabled={isPulling}
                className="flex-1 px-2 py-1 text-xs bg-secondary border border-border rounded-md text-foreground placeholder-muted-foreground focus:outline-none focus:ring-1 focus:ring-primary disabled:opacity-50"
              />
              <button
                onClick={handlePull}
                disabled={pullDisabled}
                className="px-3 py-1 text-xs font-medium bg-secondary border border-border rounded-md text-foreground hover:bg-primary/10 hover:border-primary/50 transition-colors disabled:opacity-50 disabled:cursor-not-allowed whitespace-nowrap"
              >
                {isPulling ? 'Pulling...' : 'Pull'}
              </button>
            </div>
            {pullStatus === 'success' && (
              <p className="mt-1.5 text-xs text-success font-medium">Pulled!</p>
            )}
            {pullStatus === 'error' && (
              <p className="mt-1.5 text-xs text-destructive font-medium">{pullError}</p>
            )}
          </div>
        )}
      </div>
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
  const [pullLoading, setPullLoading] = useState(false);
  const [pullSuccess, setPullSuccess] = useState(false);
  const [pullErrorMsg, setPullErrorMsg] = useState('');

  const openPullModal = async () => {
    if (demoMode) {
      setPullNodesList([
        {
          id: 'gpu-0',
          name: 'gpu-0',
          gpuModel: 'NVIDIA A100',
          port: 11434,
          vramTotalMB: 81920,
          vramUsedMB: 40960,
          vramSource: 'nvidia',
          runtime: 'ollama',
          powerDrawW: 250,
          cpuPercent: 12,
          temperature: 65,
          health: 'healthy',
          draining: false,
          uptime: '2d 4h',
          loadedModels: [],
          healthHistory: []
        }
      ]);
      setPullSelectedNode('gpu-0');
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

  const handleGeneralPull = async () => {
    const trimmedModel = pullModelName.trim();
    if (!trimmedModel || !pullSelectedNode) return;
    setPullLoading(true);
    setPullErrorMsg('');
    setPullSuccess(false);

    try {
      if (demoMode) {
        await new Promise<void>(resolve => setTimeout(resolve, 1500));
      } else {
        await pullModel(pullSelectedNode, trimmedModel);
      }
      setPullSuccess(true);
      setPullModelName('');
      loadModels();
    } catch (e: unknown) {
      setPullErrorMsg(e instanceof Error ? e.message : 'Pull failed');
    } finally {
      setPullLoading(false);
    }
  };

  const loadModels = async () => {
    if (demoMode) {
      setCatalog(mockModelCatalog);
      setIsLive(false);
      setError(null);
      setLoading(false);
      return;
    }
    try {
      const data = await fetchModels();
      setCatalog(data);
      setIsLive(true);
      setError(null);
    } catch (e: unknown) {
      setIsLive(false);
      setError(e instanceof Error ? e.message : 'Failed to connect to backend');
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    loadModels();
    if (demoMode) return;
    const interval = setInterval(loadModels, 5000);
    return () => clearInterval(interval);
  }, [demoMode]);

  const models = catalog?.models ?? [];
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
            All models across your nodes — warm (loaded in VRAM) and available on disk
          </p>
        </div>
        <div className="flex items-center gap-4">
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
            disabled={!isLive}
            title={!isLive ? 'Backend disconnected' : undefined}
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
            <ModelCard key={model.name} model={model} demoMode={demoMode} />
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
        onClose={() => {
          if (!pullLoading) {
            setIsPullModalOpen(false);
            setPullErrorMsg('');
            setPullSuccess(false);
          }
        }}
        title="Pull Model from Registry"
      >
        <div className="space-y-4">
          <p className="text-xs text-muted-foreground leading-normal">
            Download a model from the official Ollama library directly to one of your GPU nodes.
            Note: The node must have internet access to reach the Ollama registry.
          </p>

          <div className="space-y-1.5">
            <label className="text-xs font-semibold text-muted-foreground uppercase tracking-wider block">Target GPU Node</label>
            <select
              value={pullSelectedNode}
              onChange={(e) => setPullSelectedNode(e.target.value)}
              disabled={pullLoading || pullSuccess}
              className="w-full px-3 py-2 text-sm bg-secondary border border-border rounded-lg text-foreground focus:outline-none focus:ring-1 focus:ring-primary disabled:opacity-50"
            >
              {pullNodesList.map((n) => (
                <option key={n.name} value={n.name}>
                  {n.name} ({n.health})
                </option>
              ))}
            </select>
          </div>

          <div className="space-y-1.5">
            <label className="text-xs font-semibold text-muted-foreground uppercase tracking-wider block">Model Tag / Name</label>
            <input
              type="text"
              value={pullModelName}
              onChange={(e) => setPullModelName(e.target.value)}
              onKeyDown={(e) => e.key === 'Enter' && !pullLoading && pullModelName.trim() && pullSelectedNode && handleGeneralPull()}
              placeholder="e.g. llama3.2, gemma2, nomic-embed-text"
              disabled={pullLoading || pullSuccess}
              className="w-full px-3 py-2 text-sm bg-secondary border border-border rounded-lg text-foreground placeholder-muted-foreground focus:outline-none focus:ring-1 focus:ring-primary disabled:opacity-50"
            />
          </div>

          {pullErrorMsg && (
            <p className="text-xs text-destructive font-medium bg-destructive/10 border border-destructive/20 rounded-lg p-2.5">
              {pullErrorMsg}
            </p>
          )}

          {pullSuccess && (
            <p className="text-xs text-success font-medium bg-success/10 border border-success/20 rounded-lg p-2.5">
              Model pull request initiated successfully! The model will download in the background.
            </p>
          )}

          <div className="flex items-center justify-end gap-3 pt-2">
            <button
              onClick={() => {
                setIsPullModalOpen(false);
                setPullErrorMsg('');
                setPullSuccess(false);
              }}
              disabled={pullLoading}
              className="px-4 py-2 bg-secondary hover:bg-secondary/80 disabled:opacity-50 text-foreground text-sm font-semibold rounded-lg transition-colors cursor-pointer"
            >
              Close
            </button>
            <button
              onClick={handleGeneralPull}
              disabled={pullLoading || !pullModelName.trim() || !pullSelectedNode}
              className="inline-flex items-center gap-2 px-4 py-2 bg-primary hover:bg-primary/90 disabled:opacity-50 disabled:cursor-not-allowed text-primary-foreground font-semibold rounded-lg transition-colors shadow-sm cursor-pointer"
            >
              {pullLoading ? (
                <>
                  <Loader2 className="w-4 h-4 animate-spin" />
                  Pulling...
                </>
              ) : (
                <>
                  <Download className="w-4 h-4" />
                  Pull Model
                </>
              )}
            </button>
          </div>
        </div>
      </Modal>
    </div>
  );
}
