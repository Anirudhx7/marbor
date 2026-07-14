import { useState, useEffect } from 'react';
import { Plus, Trash2, Server, Thermometer, Cpu, Clock, Activity, Pencil, X, Pin, Flame, Settings2 } from 'lucide-react';
import { StatusDot } from '../components/StatusDot';
import { VramBar } from '../components/VramBar';
import { Badge } from '../components/Badge';
import { Sparkline } from '../components/Sparkline';
import { SearchInput } from '../components/SearchInput';
import { Modal } from '../components/Modal';
import { ModelConfigModal } from '../components/ModelConfigModal';
import { mockGPUNodes } from '../lib/mockData';
import { fetchNodes, addNode, removeNode, drainNode, undrainNode, setNodePrewarm, patchNode, fetchModelFit, unloadModel, getPinned } from '../lib/api';
import type { GPUNode, ModelFitResponse, NodeFit, FitStatus } from '../types';

function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B';
  const gb = bytes / (1024 * 1024 * 1024);
  if (gb >= 1) return `${gb.toFixed(1)} GB`;
  const mb = bytes / (1024 * 1024);
  return `${mb.toFixed(0)} MB`;
}

function FitBadge({ fit }: { fit: FitStatus }) {
  const styles: Record<FitStatus, string> = {
    green:   'bg-green-500/15 text-green-600 dark:text-green-400 border border-green-500/30',
    yellow:  'bg-amber-500/15 text-amber-600 dark:text-amber-400 border border-amber-500/30',
    red:     'bg-red-500/15 text-red-600 dark:text-red-400 border border-red-500/30',
    unknown: 'bg-secondary text-muted-foreground border border-border',
  };
  const labels: Record<FitStatus, string> = {
    green:   'Fits',
    yellow:  'Tight',
    red:     "Won't Fit",
    unknown: 'Unknown',
  };
  return (
    <span className={`inline-flex items-center px-2 py-0.5 rounded text-xs font-medium ${styles[fit]}`}>
      {labels[fit]}
    </span>
  );
}

function ModelFitTable({ nodeFit }: { nodeFit: NodeFit }) {
  if (nodeFit.models.length === 0) {
    return (
      <p className="text-xs text-muted-foreground py-2">No downloaded models found on this node.</p>
    );
  }
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b border-border">
            <th className="text-left text-xs font-medium text-muted-foreground pb-2 pr-4">Model</th>
            <th className="text-left text-xs font-medium text-muted-foreground pb-2 pr-4">Size</th>
            <th className="text-left text-xs font-medium text-muted-foreground pb-2 pr-4">Est. VRAM</th>
            <th className="text-left text-xs font-medium text-muted-foreground pb-2 pr-4">Fit</th>
            <th className="text-left text-xs font-medium text-muted-foreground pb-2">Status</th>
          </tr>
        </thead>
        <tbody>
          {nodeFit.models.map((m) => (
            <tr key={m.name} className="border-b border-border/50 last:border-0">
              <td className="py-2 pr-4 font-mono text-xs text-foreground">{m.name}</td>
              <td className="py-2 pr-4 text-xs text-muted-foreground">{formatBytes(m.size_bytes)}</td>
              <td className="py-2 pr-4 text-xs text-muted-foreground">{formatBytes(m.vram_estimate_bytes)}</td>
              <td className="py-2 pr-4"><FitBadge fit={m.fit} /></td>
              <td className="py-2">
                {m.loaded && (
                  <span className="inline-flex items-center gap-1 text-xs text-primary font-medium">
                    <span className="w-1.5 h-1.5 rounded-full bg-primary inline-block" />
                    In VRAM
                  </span>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function RuntimeBadge({ runtime }: { runtime: string }) {
  const runtimeStyles: Record<string, string> = {
    ollama:   'bg-blue-500/20 text-blue-400 border border-blue-500/30',
    vllm:     'bg-purple-500/20 text-purple-400 border border-purple-500/30',
    tgi:      'bg-orange-500/20 text-orange-400 border border-orange-500/30',
    llamacpp: 'bg-green-500/20 text-green-400 border border-green-500/30',
  };
  const runtimeLabels: Record<string, string> = {
    ollama:   'Ollama',
    vllm:     'vLLM',
    tgi:      'TGI',
    llamacpp: 'llama.cpp',
  };
  const key = (runtime || '').toLowerCase();
  const style = runtimeStyles[key] ?? 'bg-gray-500/20 text-gray-400 border border-gray-500/30';
  const label = runtimeLabels[key] ?? (runtime || 'unknown');
  return (
    <span className={`inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium ${style}`}>
      {label}
    </span>
  );
}

function NodeCard({ node, pinnedModels, onRemove, onDrain, onUndrain, onTogglePrewarm, onEdit, onUnload, onConfigureModel }: {
  node: GPUNode;
  pinnedModels: string[];
  onRemove: (name: string) => void;
  onDrain: (name: string) => void;
  onUndrain: (name: string) => void;
  onTogglePrewarm: (name: string, disabled: boolean) => void;
  onEdit: (node: GPUNode) => void;
  onUnload: (nodeName: string, model: string) => void;
  onConfigureModel: (modelName: string, nodeName: string, runtime: string) => void;
}) {
  const healthColor = {
    healthy: 'text-primary',
    degraded: 'text-amber-500',
    down: 'text-destructive',
  }[node.health];

  return (
    <div className={`bg-card border shadow-sm rounded-xl p-5 hover:border-primary/50 transition-colors ${node.draining ? 'border-amber-500/60' : 'border-border'}`}>
      {/* Header */}
      <div className="flex flex-col sm:flex-row sm:items-start justify-between gap-4 mb-4">
        <div className="flex items-start gap-3 min-w-0">
          <div className="p-2 bg-secondary rounded-lg shrink-0">
            <Server className="w-5 h-5 text-muted-foreground" />
          </div>
          <div className="min-w-0">
            <div className="flex items-center gap-2 flex-wrap">
              <StatusDot status={node.health} />
              <h3 className="font-semibold text-foreground truncate">{node.name}</h3>
              {node.draining && (
                <span
                  title={node.drainedReason ? `Drained: ${node.drainedReason}` : undefined}
                  className="text-xs font-medium px-1.5 py-0.5 rounded bg-amber-500/15 text-amber-400 border border-amber-500/30 whitespace-nowrap"
                >
                  DRAINING{node.drainedReason ? ` (${node.drainedReason})` : ''}
                </span>
              )}
              {node.prewarmDisabled && (
                <span
                  title="Predictive engine will not warm new models onto this node until re-enabled or the mesh restarts"
                  className="text-xs font-medium px-1.5 py-0.5 rounded bg-secondary text-muted-foreground border border-border whitespace-nowrap"
                >
                  PREWARM OFF
                </span>
              )}
            </div>
            <div className="flex flex-wrap items-center gap-2 mt-1">
              <p className="text-sm text-muted-foreground">{node.gpuModel}</p>
              <RuntimeBadge runtime={node.runtime} />
            </div>
          </div>
        </div>
        <div className="flex items-center gap-1 self-end sm:self-auto shrink-0">
          <button
            onClick={() => onEdit(node)}
            title="Edit node metadata"
            className="p-1.5 text-muted-foreground hover:text-primary transition-colors"
          >
            <Pencil className="w-4 h-4" />
          </button>
          <button
            onClick={() => node.draining ? onUndrain(node.name) : onDrain(node.name)}
            title={node.draining ? 'Undrain node' : 'Drain node (stop new requests)'}
            className={`p-1.5 transition-colors ${node.draining ? 'text-amber-400 hover:text-primary' : 'text-muted-foreground hover:text-amber-400'}`}
          >
            <Activity className="w-4 h-4" />
          </button>
          <button
            onClick={() => onTogglePrewarm(node.name, !node.prewarmDisabled)}
            title={node.prewarmDisabled ? 'Re-enable predictive prewarm on this node' : 'Disable predictive prewarm on this node (does not stop live traffic)'}
            className={`p-1.5 transition-colors ${node.prewarmDisabled ? 'text-muted-foreground/40 hover:text-primary' : 'text-primary hover:text-muted-foreground'}`}
          >
            <Flame className="w-4 h-4" />
          </button>
          <button
            onClick={() => onRemove(node.name)}
            className="p-1.5 text-muted-foreground hover:text-destructive transition-colors"
          >
            <Trash2 className="w-4 h-4" />
          </button>
        </div>
      </div>

      {/* Stats Grid */}
      <div className="grid grid-cols-1 sm:grid-cols-2 gap-4 mb-4">
        <div className="bg-secondary rounded-lg p-3">
          <div className="flex items-center gap-2 text-muted-foreground mb-1">
            <Cpu className="w-3.5 h-3.5" />
            <span className="text-xs font-medium">CPU</span>
          </div>
          <span className={`font-mono text-lg font-medium ${healthColor}`}>
            {node.cpuPercent}%
          </span>
        </div>
        <div className="bg-secondary rounded-lg p-3">
          <div className="flex items-center gap-2 text-muted-foreground mb-1">
            <Thermometer className="w-3.5 h-3.5" />
            <span className="text-xs font-medium">Temp</span>
          </div>
          <span className={`font-mono text-lg font-medium ${
            node.temperature && node.temperature > 80 ? 'text-destructive' : 
            node.temperature && node.temperature > 70 ? 'text-amber-500' : 'text-primary'
          }`}>
            {node.temperature ? `${node.temperature}°C` : 'N/A'}
          </span>
        </div>
      </div>

      {/* VRAM */}
      <div className="mb-4">
        <VramBar
          used={node.vramUsedMB / 1024}
          total={node.vramTotalMB / 1024}
          source={node.vramSource}
          pending={(node.pendingPrewarmMB ?? 0) / 1024}
        />
      </div>

      {/* Health History */}
      <div className="flex items-center justify-between mb-3">
        <div className="flex items-center gap-2 text-muted-foreground">
          <Activity className="w-3.5 h-3.5" />
          <span className="text-xs font-medium">Health (60m)</span>
        </div>
        <div className="flex items-center gap-2">
          {(node.healthHistory || []).length > 1 ? (
            <>
              <Sparkline data={node.healthHistory} width={100} height={24} />
              <span className={`text-xs font-mono font-medium ${healthColor}`}>
                {Math.round((node.healthHistory || [])[(node.healthHistory || []).length - 1])}%
              </span>
            </>
          ) : (
            <span className="text-xs text-muted-foreground">no data yet</span>
          )}
        </div>
      </div>

      {/* Port & Uptime */}
      <div className="flex items-center justify-between text-xs text-muted-foreground mb-3">
        <div className="flex items-center gap-1.5">
          <Clock className="w-3 h-3" />
          <span className="font-medium">Uptime: {node.uptime}</span>
        </div>
        <code className="font-mono">:{node.port}</code>
      </div>

      {/* Loaded Models */}
      <div className="border-t border-border pt-3">
        <p className="text-xs font-medium text-muted-foreground mb-2">Loaded Models</p>
        <div className="flex flex-wrap gap-1.5">
          {(node.loadedModels || []).length === 0 ? (
            <span className="text-xs text-muted-foreground/60">None resident</span>
          ) : (
            (node.loadedModels || []).map((model) => {
              const pinned = pinnedModels.includes(model.name);
              return (
                <Badge
                  key={model.name}
                  variant="success"
                  size="sm"
                >
                  {model.name}
                  <span className="ml-1.5 opacity-70 font-mono">
                    {(model.sizeVram / 1024).toFixed(1)}GB
                  </span>
                  <button
                    onClick={() => onConfigureModel(model.name, node.name, node.runtime)}
                    title={`Advanced settings for ${model.name}`}
                    className="ml-1.5 opacity-50 hover:opacity-100 hover:text-primary transition-opacity"
                  >
                    <Settings2 className="w-3 h-3" />
                  </button>
                  {pinned ? (
                    <span
                      title="Pinned  --  never evicted or unloaded. Unpin on the Warmup page first."
                      className="ml-1.5 -mr-0.5 opacity-60 cursor-not-allowed"
                    >
                      <Pin className="w-3 h-3" />
                    </span>
                  ) : (
                    <button
                      onClick={() => onUnload(node.name, model.name)}
                      title={`Unload ${model.name} from VRAM`}
                      className="ml-1.5 -mr-0.5 opacity-50 hover:opacity-100 hover:text-destructive transition-opacity"
                    >
                      <X className="w-3 h-3" />
                    </button>
                  )}
                </Badge>
              );
            })
          )}
        </div>
      </div>
    </div>
  );
}

import { useDemoMode } from '../hooks/useDemoMode';

export function GPUNodes() {
  const { demoMode } = useDemoMode();
  const [nodes, setNodes] = useState<GPUNode[]>(demoMode ? mockGPUNodes : []);
  const [isLive, setIsLive] = useState(!demoMode);
  const [searchQuery, setSearchQuery] = useState('');
  const [isAddModalOpen, setIsAddModalOpen] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [newNode, setNewNode] = useState({
    name: '',
    host: '',
    port: '11434',
    gpuModel: '',
  });
  const [modelFit, setModelFit] = useState<ModelFitResponse | null>(null);
  const [modelFitError, setModelFitError] = useState<string | null>(null);
  const [modelFitLoading, setModelFitLoading] = useState(false);
  const [pinnedByNode, setPinnedByNode] = useState<Record<string, string[]>>({});
  const [actionError, setActionError] = useState<string | null>(null);
  const [nodeToDelete, setNodeToDelete] = useState<string | null>(null);
  const [nodeToDrain, setNodeToDrain] = useState<string | null>(null);
  const [prewarmToToggle, setPrewarmToToggle] = useState<{ name: string; disabled: boolean } | null>(null);
  const [modelToUnload, setModelToUnload] = useState<{ nodeName: string; model: string } | null>(null);
  const [configTarget, setConfigTarget] = useState<{ model: string; node: string; runtime: string } | null>(null);

  const loadPinned = async (nodeList: GPUNode[]) => {
    if (demoMode || nodeList.length === 0) return;
    const entries = await Promise.all(nodeList.map(async (n) => {
      try {
        return [n.name, await getPinned(n.name)] as const;
      } catch {
        return [n.name, []] as const; // pinned-fetch failure just means no badges for this node
      }
    }));
    setPinnedByNode(Object.fromEntries(entries));
  };

  const loadNodes = async () => {
    if (demoMode) {
      setNodes(mockGPUNodes);
      setIsLive(false);
      setError(null);
      return;
    }
    try {
      const data = await fetchNodes();
      setNodes(data || []);
      setIsLive(true);
      setError(null);
      await loadPinned(data || []);
    } catch (e: any) {
      setIsLive(false);
      setNodes([]);
      setError(e.message || 'Failed to connect to backend');
    }
  };

  const loadModelFit = async () => {
    if (demoMode) return;
    setModelFitLoading(true);
    try {
      const data = await fetchModelFit();
      setModelFit(data);
      setModelFitError(null);
    } catch (e: unknown) {
      const msg = e instanceof Error ? e.message : 'Failed to fetch model fit data';
      setModelFitError(msg);
    } finally {
      setModelFitLoading(false);
    }
  };

  useEffect(() => {
    loadNodes();
    if (demoMode) return;
    const interval = setInterval(loadNodes, 10000);
    return () => clearInterval(interval);
  }, [demoMode]);

  useEffect(() => {
    if (!demoMode) {
      loadModelFit();
      const interval = setInterval(loadModelFit, 30000);
      return () => clearInterval(interval);
    }
  }, [demoMode]);

  const filteredNodes = nodes.filter(node =>
    (node.name || '').toLowerCase().includes(searchQuery.toLowerCase()) ||
    (node.gpuModel || '').toLowerCase().includes(searchQuery.toLowerCase())
  );

  const handleAddNode = async () => {
    if (!newNode.name || !newNode.host || !isLive) return;

    const nodeData = {
      name: newNode.name,
      url: `http://${newNode.host}:${newNode.port}`,
      gpu_model: newNode.gpuModel || 'Unknown GPU',
    };

    try {
      await addNode(nodeData);
      await loadNodes();
      setActionError(null);
      setIsAddModalOpen(false);
      setNewNode({ name: '', host: '', port: '11434', gpuModel: '' });
    } catch (err: any) {
      setActionError(err?.message || 'Failed to add node');
    }
  };

  const handleRemoveNode = async (name: string): Promise<boolean> => {
    if (!isLive) return false;
    try {
      await removeNode(name);
      await loadNodes();
      setActionError(null);
      return true;
    } catch (err: any) {
      setActionError(err?.message || `Failed to remove node ${name}`);
      return false;
    }
  };

  const handleDrainNode = async (name: string): Promise<boolean> => {
    if (!isLive) return false;
    try {
      await drainNode(name);
      await loadNodes();
      setActionError(null);
      return true;
    } catch (err: any) {
      setActionError(err?.message || `Failed to drain node ${name}`);
      return false;
    }
  };

  const handleUndrainNode = async (name: string) => {
    if (!isLive) return;
    try {
      await undrainNode(name);
      await loadNodes();
      setActionError(null);
    } catch (err: any) {
      setActionError(err?.message || `Failed to undrain node ${name}`);
    }
  };

  const handleTogglePrewarm = async (name: string, disabled: boolean): Promise<boolean> => {
    if (!isLive) return false;
    try {
      await setNodePrewarm(name, disabled);
      await loadNodes();
      setActionError(null);
      return true;
    } catch (err: any) {
      setActionError(err?.message || `Failed to toggle prewarm for node ${name}`);
      return false;
    }
  };

  const handleUnloadModel = async (nodeName: string, model: string): Promise<boolean> => {
    try {
      await unloadModel(nodeName, model);
      setActionError(null);
      if (demoMode) {
        // Reflect the unload immediately in the static demo (no backend to re-poll).
        setNodes(prev => prev.map(n => n.name === nodeName
          ? { ...n, loadedModels: (n.loadedModels || []).filter(m => m.name !== model) }
          : n));
      } else {
        await loadNodes();
      }
      return true;
    } catch (e: any) {
      // The unload button is already hidden for models we know are pinned, but
      // pinned state can go stale between polls (e.g. pinned from another tab
      // right after this page loaded)  --  the backend still enforces it (409),
      // so surface that clearly instead of silently dropping the click.
      setActionError(e?.message || `Failed to unload ${model} from ${nodeName}`);
      return false;
    }
  };

  const [editNode, setEditNode] = useState<GPUNode | null>(null);
  const [editVRAM, setEditVRAM] = useState('');
  const [editGPUModel, setEditGPUModel] = useState('');
  const [editSaving, setEditSaving] = useState(false);
  const [editError, setEditError] = useState('');

  const openEditModal = (node: GPUNode) => {
    setEditNode(node);
    setEditVRAM(node.vramTotalMB > 0 ? String(node.vramTotalMB) : '');
    setEditGPUModel(node.gpuModel ?? '');
    setEditError('');
  };

  const handleSavePatch = async () => {
    if (!editNode || !isLive) return;
    const patch: { vram_total_mb?: number; gpu_model?: string } = {};
    if (editVRAM.trim() !== '') {
      const v = parseInt(editVRAM, 10);
      if (isNaN(v) || v < 0) { setEditError('VRAM must be a non-negative integer (MB)'); return; }
      patch.vram_total_mb = v;
    }
    if (editGPUModel.trim() !== '') patch.gpu_model = editGPUModel.trim();
    if (Object.keys(patch).length === 0) { setEditNode(null); return; }
    setEditSaving(true);
    setEditError('');
    try {
      await patchNode(editNode.name, patch);
      await loadNodes();
      setEditNode(null);
    } catch (e) {
      setEditError('Failed to save changes');
    } finally {
      setEditSaving(false);
    }
  };

  return (
    <div className="space-y-6 animate-fade-in max-w-7xl mx-auto">
      {/* Header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold text-foreground">GPU Nodes</h1>
          <p className="text-sm text-muted-foreground mt-1">
            Manage {nodes.length} inference nodes across your infrastructure
          </p>
        </div>
        <div className="flex items-center gap-4">
          <div className="flex items-center gap-2">
            <div className={`w-2 h-2 rounded-full ${isLive ? 'bg-success' : 'bg-amber-500'}`} />
            <span className={`text-xs font-medium ${isLive ? 'text-success' : 'text-amber-600 dark:text-amber-400'}`}>
              {demoMode ? 'Demo Mode' : (isLive ? 'Live Data' : 'Disconnected')}
            </span>
          </div>
          <button
            onClick={() => { setActionError(null); setIsAddModalOpen(true); }}
            disabled={!isLive}
            title={!isLive ? 'Backend disconnected' : undefined}
            className="flex items-center gap-2 px-4 py-2 bg-primary hover:bg-primary/90 disabled:opacity-50 disabled:cursor-not-allowed text-primary-foreground font-medium rounded-lg transition-colors shadow-sm"
          >
            <Plus className="w-4 h-4" />
            Add Node
          </button>
        </div>
      </div>

      {error && !demoMode && (
        <div className="p-4 bg-destructive/10 border border-destructive/20 rounded-xl text-destructive text-sm font-medium">
          {error}
        </div>
      )}

      {actionError && (
        <div className="p-4 bg-destructive/10 border border-destructive/20 rounded-xl text-destructive text-sm font-medium flex items-center justify-between gap-3">
          <span>{actionError}</span>
          <button
            onClick={() => setActionError(null)}
            className="text-destructive/70 hover:text-destructive shrink-0"
            title="Dismiss"
          >
            <X className="w-4 h-4" />
          </button>
        </div>
      )}

      {/* Search */}
      <div className="max-w-md">
        <SearchInput
          value={searchQuery}
          onChange={setSearchQuery}
          placeholder="Search nodes by name or GPU model..."
        />
      </div>

      {/* Nodes Grid */}
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
        {filteredNodes.map((node) => (
          <NodeCard key={node.id} node={node} pinnedModels={pinnedByNode[node.name] ?? []} onRemove={(name) => { setActionError(null); setNodeToDelete(name); }} onDrain={(name) => { setActionError(null); setNodeToDrain(name); }} onUndrain={handleUndrainNode} onTogglePrewarm={(name, disabled) => { setActionError(null); setPrewarmToToggle({ name, disabled }); }} onEdit={openEditModal} onUnload={(nodeName, model) => { setActionError(null); setModelToUnload({ nodeName, model }); }} onConfigureModel={(modelName, nodeName, runtime) => setConfigTarget({ model: modelName, node: nodeName, runtime })} />
        ))}
      </div>

      {filteredNodes.length === 0 && (
        <div className="text-center py-12">
          <Server className="w-12 h-12 text-muted-foreground/30 mx-auto mb-4" />
          <p className="text-muted-foreground">No inference nodes found matching your search.</p>
        </div>
      )}

      {/* Model Fit Section */}
      {!demoMode && (
        <div className="space-y-4">
          <div className="flex items-center justify-between">
            <div>
              <h2 className="text-lg font-semibold text-foreground">Model Fit</h2>
              <p className="text-sm text-muted-foreground mt-0.5">Will each downloaded model fit in available VRAM?</p>
            </div>
            {modelFitLoading && (
              <span className="text-xs text-muted-foreground animate-pulse">Checking...</span>
            )}
          </div>

          {modelFitError && (
            <div className="p-3 bg-destructive/10 border border-destructive/20 rounded-lg text-destructive text-sm">
              {modelFitError}
            </div>
          )}

          {modelFit && (modelFit.nodes ?? []).map((nodeFit) => (
            <div key={nodeFit.name} className="bg-card border border-border rounded-xl p-5">
              <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2 mb-3">
                <div className="flex items-baseline flex-wrap gap-2">
                  <span className="font-semibold text-foreground">{nodeFit.name}</span>
                  <span className="text-xs text-muted-foreground font-mono">{nodeFit.url}</span>
                </div>
                <div className="flex flex-wrap items-center gap-3 text-xs text-muted-foreground self-start sm:self-auto">
                  {nodeFit.vram_source !== 'unknown' && nodeFit.vram_total_bytes > 0 && (
                    <span>
                      Free: <span className="font-mono text-foreground">{formatBytes(nodeFit.vram_free_bytes)}</span>
                      {' / '}
                      <span className="font-mono text-foreground">{formatBytes(nodeFit.vram_total_bytes)}</span>
                    </span>
                  )}
                  <span className={`px-1.5 py-0.5 rounded text-xs ${
                    nodeFit.vram_source === 'nvidia-smi'
                      ? 'bg-green-500/15 text-green-600 dark:text-green-400'
                      : nodeFit.vram_source === 'inferred'
                      ? 'bg-amber-500/15 text-amber-600 dark:text-amber-400'
                      : nodeFit.vram_source === 'declared'
                      ? 'bg-blue-500/15 text-blue-600 dark:text-blue-400'
                      : 'bg-secondary text-muted-foreground'
                  }`}>
                    {nodeFit.vram_source === 'declared' ? 'declared' : nodeFit.vram_source}
                  </span>
                </div>
              </div>
              <ModelFitTable nodeFit={nodeFit} />
            </div>
          ))}

          {modelFit && modelFit.nodes.length === 0 && !modelFitLoading && (
            <div className="text-center py-8 text-muted-foreground text-sm">No nodes available for model fit analysis.</div>
          )}
        </div>
      )}

      {/* Add Node Modal */}
      <Modal
        isOpen={isAddModalOpen}
        onClose={() => setIsAddModalOpen(false)}
        title="Add GPU Node"
      >
        <div className="space-y-4">
          <div>
            <label className="block text-sm font-medium text-muted-foreground mb-1.5">
              Node Name
            </label>
            <input
              type="text"
              value={newNode.name}
              onChange={(e) => setNewNode({ ...newNode, name: e.target.value })}
              placeholder="e.g., gpu-node-05"
              className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
            />
          </div>
          <div>
            <label className="block text-sm font-medium text-muted-foreground mb-1.5">
              Host
            </label>
            <input
              type="text"
              value={newNode.host}
              onChange={(e) => setNewNode({ ...newNode, host: e.target.value })}
              placeholder="e.g., 10.0.1.15"
              className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
            />
          </div>
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Port
              </label>
              <input
                type="text"
                value={newNode.port}
                onChange={(e) => setNewNode({ ...newNode, port: e.target.value })}
                className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
              />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                GPU Model
              </label>
              <input
                type="text"
                value={newNode.gpuModel}
                onChange={(e) => setNewNode({ ...newNode, gpuModel: e.target.value })}
                placeholder="e.g., NVIDIA A100"
                className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
              />
            </div>
          </div>
          {actionError && (
            <p className="text-sm text-destructive">{actionError}</p>
          )}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setIsAddModalOpen(false)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={handleAddNode}
              disabled={!newNode.name || !newNode.host}
              className="px-4 py-2 bg-primary hover:bg-primary/90 disabled:opacity-50 disabled:cursor-not-allowed text-primary-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              Add Node
            </button>
          </div>
        </div>
      </Modal>

      {/* Edit Node Modal */}
      <Modal
        isOpen={editNode !== null}
        onClose={() => setEditNode(null)}
        title={`Edit Node: ${editNode?.name ?? ''}`}
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Override runtime metadata. Changes apply immediately without restart.
          </p>
          <div>
            <label className="block text-sm font-medium text-muted-foreground mb-1.5">
              GPU Model Label
            </label>
            <input
              type="text"
              value={editGPUModel}
              onChange={(e) => setEditGPUModel(e.target.value)}
              placeholder="e.g., NVIDIA RTX 4090"
              className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
            />
          </div>
          <div>
            <label className="block text-sm font-medium text-muted-foreground mb-1.5">
              VRAM Total Override (MB)
            </label>
            <input
              type="number"
              min="0"
              value={editVRAM}
              onChange={(e) => setEditVRAM(e.target.value)}
              placeholder="e.g., 24576 for 24 GB"
              className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
            />
            <p className="text-xs text-muted-foreground mt-1">
              Only applied when nvidia-smi has no measurement (remote nodes). Ignored if source is nvidia.
            </p>
          </div>
          {editError && (
            <p className="text-sm text-destructive">{editError}</p>
          )}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setEditNode(null)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={handleSavePatch}
              disabled={editSaving}
              className="px-4 py-2 bg-primary hover:bg-primary/90 disabled:opacity-50 disabled:cursor-not-allowed text-primary-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              {editSaving ? 'Saving...' : 'Save Changes'}
            </button>
          </div>
        </div>
      </Modal>

      {/* Remove Node Confirmation Modal */}
      <Modal
        isOpen={nodeToDelete !== null}
        onClose={() => setNodeToDelete(null)}
        title="Remove GPU Node"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Are you sure you want to remove the node <span className="text-foreground font-semibold">{nodeToDelete}</span> from the mesh?
          </p>
          <p className="text-xs text-muted-foreground">
            This deletes the node's configuration. In-flight requests to it will fail over, and any warm models on it will need a cold start elsewhere.
          </p>
          {actionError && (
            <p className="text-sm text-destructive">{actionError}</p>
          )}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setNodeToDelete(null)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={async () => {
                if (!nodeToDelete) return;
                const ok = await handleRemoveNode(nodeToDelete);
                if (ok) setNodeToDelete(null);
              }}
              className="px-4 py-2 bg-destructive hover:bg-destructive/90 text-destructive-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              Remove Node
            </button>
          </div>
        </div>
      </Modal>

      {/* Unload Model Confirmation Modal */}
      <Modal
        isOpen={modelToUnload !== null}
        onClose={() => setModelToUnload(null)}
        title="Unload Model from VRAM"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Unload <span className="text-foreground font-semibold">{modelToUnload?.model}</span> from{' '}
            <span className="text-foreground font-semibold">{modelToUnload?.nodeName}</span>?
          </p>
          <p className="text-xs text-muted-foreground">
            The next request for this model on this node will trigger a cold start.
          </p>
          {actionError && (
            <p className="text-sm text-destructive">{actionError}</p>
          )}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setModelToUnload(null)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={async () => {
                if (!modelToUnload) return;
                const ok = await handleUnloadModel(modelToUnload.nodeName, modelToUnload.model);
                if (ok) setModelToUnload(null);
              }}
              className="px-4 py-2 bg-destructive hover:bg-destructive/90 text-destructive-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              Unload Model
            </button>
          </div>
        </div>
      </Modal>

      {/* Drain Node Confirmation Modal */}
      <Modal
        isOpen={nodeToDrain !== null}
        onClose={() => setNodeToDrain(null)}
        title="Drain Node"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Are you sure you want to drain <span className="text-foreground font-semibold">{nodeToDrain}</span>?
          </p>
          <p className="text-xs text-muted-foreground">
            This stops new requests from being routed to this node. In-flight requests are unaffected. You can undrain it again at any time.
          </p>
          {actionError && (
            <p className="text-sm text-destructive">{actionError}</p>
          )}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setNodeToDrain(null)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={async () => {
                if (!nodeToDrain) return;
                const ok = await handleDrainNode(nodeToDrain);
                if (ok) setNodeToDrain(null);
              }}
              className="px-4 py-2 bg-amber-600 hover:bg-amber-600/90 text-white font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              Drain Node
            </button>
          </div>
        </div>
      </Modal>

      {/* Toggle Predictive Prewarm Confirmation Modal */}
      <Modal
        isOpen={prewarmToToggle !== null}
        onClose={() => setPrewarmToToggle(null)}
        title={prewarmToToggle?.disabled ? 'Disable Predictive Prewarm' : 'Re-enable Predictive Prewarm'}
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Are you sure you want to {prewarmToToggle?.disabled ? 'disable' : 're-enable'} predictive prewarm on{' '}
            <span className="text-foreground font-semibold">{prewarmToToggle?.name}</span>?
          </p>
          <p className="text-xs text-muted-foreground">
            {prewarmToToggle?.disabled
              ? 'The predictive engine will not warm new models onto this node until re-enabled or the mesh restarts. Live traffic and warm-state routing are unaffected.'
              : 'The predictive engine will resume warming models onto this node based on usage patterns.'}
          </p>
          {actionError && (
            <p className="text-sm text-destructive">{actionError}</p>
          )}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setPrewarmToToggle(null)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={async () => {
                if (!prewarmToToggle) return;
                const ok = await handleTogglePrewarm(prewarmToToggle.name, prewarmToToggle.disabled);
                if (ok) setPrewarmToToggle(null);
              }}
              className={`px-4 py-2 font-medium rounded-lg text-sm transition-colors shadow-sm ${
                prewarmToToggle?.disabled
                  ? 'bg-amber-600 hover:bg-amber-600/90 text-white'
                  : 'bg-primary hover:bg-primary/90 text-primary-foreground'
              }`}
            >
              {prewarmToToggle?.disabled ? 'Disable Prewarm' : 'Re-enable Prewarm'}
            </button>
          </div>
        </div>
      </Modal>

      {/* Model Advanced Settings Modal */}
      <ModelConfigModal
        model={configTarget?.model ?? null}
        demoMode={demoMode}
        nodes={configTarget ? [{ name: configTarget.node, runtime: configTarget.runtime }] : []}
        onClose={() => setConfigTarget(null)}
      />
    </div>
  );
}
