import { useState, useEffect } from 'react';
import { Package } from 'lucide-react';
import { StatusDot } from '../components/StatusDot';
import { Badge } from '../components/Badge';
import { SearchInput } from '../components/SearchInput';
import { mockModelCatalog } from '../lib/mockData';
import { fetchModels, pullModel } from '../lib/api';
import { useDemoMode } from '../hooks/useDemoMode';
import type { ModelCatalog, ModelEntry } from '../types';

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
    const interval = setInterval(loadModels, 5000);
    return () => clearInterval(interval);
  }, [demoMode]);

  const models = catalog?.models ?? [];
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
            Models currently loaded in VRAM across all nodes
          </p>
        </div>
        <div className="flex items-center gap-4">
          {catalog && (
            <span className="inline-flex items-center gap-1.5 px-3 py-1.5 bg-secondary rounded-lg text-xs font-medium text-foreground">
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
        <div className="text-center py-12">
          <Package className="w-12 h-12 text-muted-foreground/30 mx-auto mb-4" />
          <p className="text-muted-foreground">
            {searchQuery
              ? 'No models matching your search.'
              : 'No models loaded across any nodes. Start an Ollama request to load a model into VRAM.'}
          </p>
        </div>
      )}
    </div>
  );
}
