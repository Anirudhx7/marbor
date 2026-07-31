import { useState, useEffect, useRef } from 'react';
import { useLocation } from 'react-router-dom';
import { Plus, Trash2, Server, Thermometer, Cpu, Clock, Activity, Pencil, X, Pin, Flame, Settings2, Radio, Copy, Fan, MemoryStick, HardDrive } from 'lucide-react';
import { StatusDot } from '../components/StatusDot';
import { VramBar } from '../components/VramBar';
import { Badge } from '../components/Badge';
import { Sparkline } from '../components/Sparkline';
import { SearchInput } from '../components/SearchInput';
import { Modal } from '../components/Modal';
import { ModelConfigModal } from '../components/ModelConfigModal';
import { CustomSelect } from '../components/Select';
import { mockGPUNodes, mockRuntimeLogLines } from '../lib/mockData';
import { fetchNodes, addNode, removeNode, drainNode, undrainNode, setNodePrewarm, patchNode, fetchModelFit, unloadModel, getPinned, getNodeAgent, enableNodeAgent, regenerateNodeAgentToken, disableNodeAgent, checkNodeHealth, getNodeControl, acceptNodeControl, clearNodeControl, startNodeRuntime, stopNodeRuntime, restartNodeRuntime, getNodeRuntimeLogs } from '../lib/api';
import type { NodeAgentStatus, NodeHealthCheckResult, NodeControlStatus } from '../lib/api';
import type { GPUNode, ModelFitResponse, NodeFit, FitStatus } from '../types';
import { formatDurationLong } from '../lib/time';

function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B';
  const gb = bytes / (1024 * 1024 * 1024);
  if (gb >= 1) return `${gb.toFixed(1)} GB`;
  const mb = bytes / (1024 * 1024);
  return `${mb.toFixed(0)} MB`;
}

// LIVE_VRAM_TOOL_SOURCES are every `vram_source` string handleModelFit
// (admin.go) can report for a value read straight from a vendor tool -
// "nvidia-smi" for the mesh's own local card, or whatever tool the Node
// Agent detected ("rocm-smi"/"xpu-smi"/"system_profiler"), plus its generic
// "agent" fallback when the vendor itself somehow wasn't reported. All of
// these get the same "live reading" badge color; only "declared"/"inferred"/
// "unknown" are visually distinct.
const LIVE_VRAM_TOOL_SOURCES = new Set(['nvidia-smi', 'rocm-smi', 'xpu-smi', 'system_profiler', 'agent']);

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
    <>
      <div className="hidden md:block overflow-x-auto">
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

      <div className="md:hidden space-y-3">
        {nodeFit.models.map((m) => (
          <div key={m.name} className="bg-card/50 backdrop-blur-sm border border-border/60 rounded-xl p-4">
            <div className="flex items-start justify-between gap-2 mb-3">
              <span className="font-mono text-xs text-foreground break-all">{m.name}</span>
              <FitBadge fit={m.fit} />
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div>
                <span className="block text-[10px] uppercase tracking-wider text-muted-foreground">Size</span>
                <span className="text-sm text-foreground">{formatBytes(m.size_bytes)}</span>
              </div>
              <div>
                <span className="block text-[10px] uppercase tracking-wider text-muted-foreground">Est. VRAM</span>
                <span className="text-sm text-foreground">{formatBytes(m.vram_estimate_bytes)}</span>
              </div>
              <div>
                <span className="block text-[10px] uppercase tracking-wider text-muted-foreground">Status</span>
                {m.loaded ? (
                  <span className="inline-flex items-center gap-1 text-sm text-primary font-medium">
                    <span className="w-1.5 h-1.5 rounded-full bg-primary inline-block" />
                    In VRAM
                  </span>
                ) : (
                  <span className="text-sm text-muted-foreground">-</span>
                )}
              </div>
            </div>
          </div>
        ))}
      </div>
    </>
  );
}

function RuntimeBadge({ runtime }: { runtime: string }) {
  const runtimeStyles: Record<string, string> = {
    ollama:   'bg-blue-500/20 text-blue-600 dark:text-blue-400 border border-blue-500/30',
    vllm:     'bg-purple-500/20 text-purple-600 dark:text-purple-400 border border-purple-500/30',
    tgi:      'bg-orange-500/20 text-orange-600 dark:text-orange-400 border border-orange-500/30',
    llamacpp: 'bg-green-500/20 text-green-600 dark:text-green-400 border border-green-500/30',
    mlx:      'bg-pink-500/20 text-pink-600 dark:text-pink-400 border border-pink-500/30',
  };
  const runtimeLabels: Record<string, string> = {
    ollama:   'Ollama',
    vllm:     'vLLM',
    tgi:      'TGI',
    llamacpp: 'llama.cpp',
    mlx:      'MLX (Apple Silicon)',
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

function AgentBadge({ present, version }: { present?: boolean; version?: string }) {
  if (present) {
    return (
      <span
        title={version ? `Node Agent installed (v${version})` : 'Node Agent installed'}
        className="inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-xs font-medium bg-primary/15 text-primary border border-primary/30 whitespace-nowrap"
      >
        <Radio className="w-3 h-3" />
        Agent
      </span>
    );
  }
  return (
    <span
      title="No Node Agent installed - only local nvidia-smi telemetry available"
      className="inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-xs font-medium bg-secondary text-muted-foreground border border-border whitespace-nowrap"
    >
      <Radio className="w-3 h-3 opacity-50" />
      No Agent
    </span>
  );
}

function NodeCard({ node, pinnedModels, onRemove, onDrain, onUndrain, onTogglePrewarm, onEdit, onUnload, onConfigureModel, onManageAgent }: {
  node: GPUNode;
  pinnedModels: string[];
  onRemove: (name: string) => void;
  onDrain: (name: string) => void;
  onUndrain: (name: string) => void;
  onTogglePrewarm: (name: string, disabled: boolean) => void;
  onEdit: (node: GPUNode) => void;
  onUnload: (nodeName: string, model: string) => void;
  onConfigureModel: (modelName: string, nodeName: string, runtime: string) => void;
  onManageAgent: (node: GPUNode) => void;
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
                  className="text-xs font-medium px-1.5 py-0.5 rounded bg-amber-500/15 text-amber-600 dark:text-amber-400 border border-amber-500/30 whitespace-nowrap"
                >
                  DRAINING{node.drainedReason ? ` (${node.drainedReason})` : ''}
                  {' - '}{node.activeConns > 0 ? `${node.activeConns} in-flight` : 'drained'}
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
              {node.warmupErrors && Object.keys(node.warmupErrors).length > 0 && (
                <span
                  title={Object.entries(node.warmupErrors).map(([model, err]) => `${model}: ${err}`).join('\n')}
                  className="text-xs font-medium px-1.5 py-0.5 rounded bg-destructive/10 text-destructive border border-destructive/30 whitespace-nowrap"
                >
                  WARMUP FAILED ({Object.keys(node.warmupErrors).length})
                </span>
              )}
              {node.unloadErrors && Object.keys(node.unloadErrors).length > 0 && (
                <span
                  title={Object.entries(node.unloadErrors).map(([model, err]) => `${model}: ${err}`).join('\n')}
                  className="text-xs font-medium px-1.5 py-0.5 rounded bg-destructive/10 text-destructive border border-destructive/30 whitespace-nowrap"
                >
                  UNLOAD FAILED ({Object.keys(node.unloadErrors).length})
                </span>
              )}
            </div>
            <div className="flex flex-wrap items-center gap-2 mt-1">
              <p className="text-sm text-muted-foreground">{node.gpuModel || 'Unknown GPU'}</p>
              <RuntimeBadge runtime={node.runtime} />
              <AgentBadge present={node.agentPresent} version={node.agentVersion} />
            </div>
          </div>
        </div>
        <div className="flex items-center gap-1 self-end sm:self-auto shrink-0">
          <button
            onClick={() => onManageAgent(node)}
            title="Manage Node Agent (fan/RAM/disk telemetry)"
            className="p-1.5 text-muted-foreground hover:text-primary transition-colors"
          >
            <Radio className="w-4 h-4" />
          </button>
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
            className={`p-1.5 transition-colors ${node.draining ? 'text-amber-600 dark:text-amber-400 hover:text-primary' : 'text-muted-foreground hover:text-amber-600 dark:hover:text-amber-400'}`}
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
      <div className="grid grid-cols-1 sm:grid-cols-2 gap-4 mb-3">
        <div className="bg-secondary rounded-lg p-3">
          <div className="flex items-center gap-2 text-muted-foreground mb-1">
            <Cpu className="w-3.5 h-3.5" />
            <span className="text-xs font-medium">CPU</span>
          </div>
          <span className={`font-mono text-lg font-medium ${healthColor}`}>
            {node.agentPresent && node.cpuPercent != null ? `${node.cpuPercent.toFixed(2)}%` : '--'}
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

      {/* Telemetry Observability */}
      <div className="grid grid-cols-3 gap-2 mb-4 text-xs bg-secondary/40 border border-border/20 rounded-lg p-3">
        <div>
          <span className="text-muted-foreground block text-[10px] uppercase font-semibold tracking-wider">Warm Hit</span>
          <span className="font-semibold text-foreground font-mono text-sm block mt-0.5">
            {node.warmHitRatio !== undefined ? `${(node.warmHitRatio * 100).toFixed(0)}%` : '--'}
          </span>
          {node.coldStarts !== undefined && node.coldStarts > 0 ? (
            <span className="text-[9px] text-muted-foreground block mt-0.5 leading-none">
              ({node.coldStarts} cold)
            </span>
          ) : null}
        </div>
        <div>
          <span className="text-muted-foreground block text-[10px] uppercase font-semibold tracking-wider">Latency</span>
          <span className="font-semibold text-foreground font-mono text-sm block mt-0.5">
            {node.avgLatencyMs !== undefined && node.avgLatencyMs > 0 ? `${node.avgLatencyMs.toFixed(0)}ms` : '--'}
          </span>
        </div>
        <div>
          <span className="text-muted-foreground block text-[10px] uppercase font-semibold tracking-wider">Tokens</span>
          <span className="font-semibold text-foreground font-mono text-sm block mt-0.5 truncate" title={node.tokensTotal?.toLocaleString()}>
            {node.tokensTotal !== undefined ? (node.tokensTotal >= 1000000 ? `${(node.tokensTotal / 1000000).toFixed(1)}M` : node.tokensTotal >= 1000 ? `${(node.tokensTotal / 1000).toFixed(1)}K` : node.tokensTotal) : '--'}
          </span>
        </div>
      </div>

      {/* Node Agent Telemetry - only ever real values from the agent poll;
          '--' whenever agentPresent is false, never a fabricated number (R1). */}
      <div className="grid grid-cols-3 gap-2 mb-4 text-xs bg-secondary/40 border border-border/20 rounded-lg p-3">
        <div>
          <span className="text-muted-foreground flex items-center gap-1 text-[10px] uppercase font-semibold tracking-wider">
            <Fan className="w-3 h-3" /> Fan
          </span>
          <span className="font-semibold text-foreground font-mono text-sm block mt-0.5">
            {node.agentPresent && node.fanPercent != null ? `${node.fanPercent}%` : '--'}
          </span>
        </div>
        <div>
          <span className="text-muted-foreground flex items-center gap-1 text-[10px] uppercase font-semibold tracking-wider">
            <MemoryStick className="w-3 h-3" /> RAM Used
          </span>
          <span className="font-semibold text-foreground font-mono text-sm block mt-0.5">
            {node.agentPresent && node.ramUsedMB != null ? `${(node.ramUsedMB / 1024).toFixed(1)} GB` : '--'}
          </span>
        </div>
        <div>
          <span className="text-muted-foreground flex items-center gap-1 text-[10px] uppercase font-semibold tracking-wider">
            <HardDrive className="w-3 h-3" /> Disk Free
          </span>
          <span className="font-semibold text-foreground font-mono text-sm block mt-0.5">
            {node.agentPresent && node.diskFreeGB != null ? `${node.diskFreeGB.toFixed(1)} GB` : '--'}
          </span>
        </div>
      </div>

      {/* VRAM */}
      <div className="mb-4">
        <VramBar
          used={node.vramUsedMB / 1024}
          total={node.vramTotalMB / 1024}
          source={node.vramSource}
          agentGpuVendor={node.agentGpuVendor}
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
                    {formatBytes(model.sizeVram)}
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
                      title="Pinned - never evicted or unloaded. Unpin on the Warmup page first."
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

import { useDemoMode, currentAppPath } from '../hooks/useDemoMode';

export function GPUNodes() {
  const { demoMode } = useDemoMode();
  const location = useLocation();
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
    runtime: 'auto',
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

  // --- Node Agent management ---
  const [agentNode, setAgentNode] = useState<GPUNode | null>(null);
  const [agentStatus, setAgentStatus] = useState<NodeAgentStatus | null>(null);
  const [agentPort, setAgentPort] = useState('11435');
  const [agentInstallCommand, setAgentInstallCommand] = useState<{ unix: string; windows: string } | null>(null);
  const [agentBusy, setAgentBusy] = useState(false);
  const [agentError, setAgentError] = useState<string | null>(null);
  const [agentCopiedWhich, setAgentCopiedWhich] = useState<'unix' | 'windows' | null>(null);
  const [agentToDisable, setAgentToDisable] = useState<string | null>(null);
  const [healthCheckBusy, setHealthCheckBusy] = useState(false);
  const [healthCheckResult, setHealthCheckResult] = useState<NodeHealthCheckResult | null>(null);
  // Tracks the modal's current node synchronously (unlike agentNode state,
  // which only updates after a render) so an in-flight health check can tell,
  // the instant its response lands, whether the modal has since moved to a
  // different node and the result should be discarded rather than misapplied.
  const agentNodeRef = useRef<GPUNode | null>(null);
  useEffect(() => { agentNodeRef.current = agentNode; }, [agentNode]);

  // --- ControlDriver (P43) - registration flow: probe, confirm, persist.
  // discovered/configured are always shown separately - accepting only
  // ever happens on an explicit operator click, never automatically from a
  // re-scan (node-agent-capabilities.md section 5.6).
  const [controlStatus, setControlStatus] = useState<NodeControlStatus | null>(null);
  const [controlBusy, setControlBusy] = useState(false);
  const [controlError, setControlError] = useState<string | null>(null);
  const [controlManualDriver, setControlManualDriver] = useState('process');
  const [controlManualIdentifier, setControlManualIdentifier] = useState('');
  const [controlManualStartCommand, setControlManualStartCommand] = useState('');
  // --- Runtime lifecycle actions (P43 Step 3) - only enabled once a
  // control driver is configured; demo mode shows the buttons but never
  // hits a real endpoint (matches the existing demo-banner discipline).
  const [runtimeActionBusy, setRuntimeActionBusy] = useState<'start' | 'stop' | 'restart' | null>(null);
  const [runtimeActionError, setRuntimeActionError] = useState<string | null>(null);
  const [runtimeActionConfirm, setRuntimeActionConfirm] = useState<'start' | 'stop' | 'restart' | null>(null);
  const [runtimeActionNotice, setRuntimeActionNotice] = useState<string | null>(null);
  // --- Runtime logs (P58) - a pure read, no confirm dialog needed (R10
  // exemption for non-destructive actions).
  const [logsModalOpen, setLogsModalOpen] = useState(false);
  const [logsBusy, setLogsBusy] = useState(false);
  const [logsError, setLogsError] = useState<string | null>(null);
  const [logsLines, setLogsLines] = useState<string[] | null>(null);

  const openAgentModal = async (node: GPUNode) => {
    setAgentNode(node);
    setAgentError(null);
    setAgentInstallCommand(null);
    setAgentCopiedWhich(null);
    setHealthCheckBusy(false);
    setHealthCheckResult(null);
    setControlStatus(null);
    setControlError(null);
    setControlManualDriver('process');
    setControlManualIdentifier('');
    setControlManualStartCommand('');
    setRuntimeActionBusy(null);
    setRuntimeActionError(null);
    setLogsBusy(false);
    setLogsError(null);
    setLogsLines(null);
    if (demoMode) {
      setAgentStatus({ node: node.name, enabled: !!node.agentPresent, port: 11435 });
      setAgentPort('11435');
      setControlStatus({
        node: node.name,
        configured: true,
        driver: 'systemd',
        identifier: 'ollama.service',
        discovered: { driver: 'systemd', identifier: 'ollama.service', evidence: ['unit ollama.service found', 'unit active'] },
      });
      return;
    }
    try {
      const status = await getNodeAgent(node.name);
      setAgentStatus(status);
      setAgentPort(String(status.port || 11435));
    } catch (e: any) {
      setAgentStatus({ node: node.name, enabled: false, port: 0 });
      setAgentError(e?.message || 'Failed to fetch node agent status');
    }
    try {
      const control = await getNodeControl(node.name);
      setControlStatus(control);
      setControlManualIdentifier(control.discovered.identifier || '');
    } catch (e: any) {
      setControlError(e?.message || 'Failed to fetch control driver status');
    }
  };

  const acceptDiscoveredControl = async () => {
    if (!agentNode || !controlStatus?.discovered.driver) return;
    setControlBusy(true);
    setControlError(null);
    if (demoMode) {
      setControlStatus(s => s && { ...s, configured: true, driver: s.discovered.driver, identifier: s.discovered.identifier });
      setControlBusy(false);
      return;
    }
    try {
      await acceptNodeControl(agentNode.name, controlStatus.discovered.driver, controlStatus.discovered.identifier);
      setControlStatus(await getNodeControl(agentNode.name));
    } catch (e: any) {
      setControlError(e?.message || 'Failed to accept control driver');
    } finally {
      setControlBusy(false);
    }
  };

  const acceptManualControl = async () => {
    if (!agentNode || !controlManualIdentifier.trim()) return;
    setControlBusy(true);
    setControlError(null);
    const startCommand = controlManualDriver === 'process' ? controlManualStartCommand.trim() : '';
    if (demoMode) {
      setControlStatus(s => s && { ...s, configured: true, driver: controlManualDriver, identifier: controlManualIdentifier.trim(), start_command: startCommand });
      setControlBusy(false);
      return;
    }
    try {
      await acceptNodeControl(agentNode.name, controlManualDriver, controlManualIdentifier.trim(), startCommand || undefined);
      setControlStatus(await getNodeControl(agentNode.name));
    } catch (e: any) {
      setControlError(e?.message || 'Failed to accept control driver');
    } finally {
      setControlBusy(false);
    }
  };

  // runRuntimeAction dispatches P43 Step 3's start/stop/restart to the
  // node's agent via the Admin API - only ever called when controlStatus
  // .configured is true (the buttons are disabled otherwise); demo mode is
  // a no-op so a demo click never hits a real endpoint.
  const runRuntimeAction = async (action: 'start' | 'stop' | 'restart') => {
    if (!agentNode || !controlStatus?.configured) return;
    setRuntimeActionConfirm(null);
    setRuntimeActionNotice(null);
    setRuntimeActionBusy(action);
    setRuntimeActionError(null);
    if (demoMode) {
      setTimeout(() => setRuntimeActionBusy(null), 400);
      return;
    }
    try {
      if (action === 'start') await startNodeRuntime(agentNode.name);
      else if (action === 'stop') await stopNodeRuntime(agentNode.name);
      else await restartNodeRuntime(agentNode.name);
    } catch (e: any) {
      setRuntimeActionError(e?.message || `Failed to ${action} runtime`);
    } finally {
      setRuntimeActionBusy(null);
    }
  };

  // viewRuntimeLogs fetches P58's runtime.logs snapshot - only ever called
  // when controlStatus.configured is true (same gate as start/stop/restart);
  // demo mode shows static sample lines without hitting a real endpoint.
  const viewRuntimeLogs = async () => {
    if (!agentNode || !controlStatus?.configured) return;
    setLogsError(null);
    setLogsModalOpen(true);
    if (demoMode) {
      setLogsLines(mockRuntimeLogLines);
      return;
    }
    setLogsBusy(true);
    setLogsLines(null);
    try {
      const { lines } = await getNodeRuntimeLogs(agentNode.name);
      setLogsLines(lines);
    } catch (e: any) {
      setLogsError(e?.message || 'Failed to fetch runtime logs');
    } finally {
      setLogsBusy(false);
    }
  };

  const clearControl = async () => {
    if (!agentNode) return;
    setControlBusy(true);
    setControlError(null);
    if (demoMode) {
      setControlStatus(s => s && { ...s, configured: false, driver: '', identifier: '' });
      setControlBusy(false);
      return;
    }
    try {
      await clearNodeControl(agentNode.name);
      setControlStatus(await getNodeControl(agentNode.name));
    } catch (e: any) {
      setControlError(e?.message || 'Failed to clear control driver');
    } finally {
      setControlBusy(false);
    }
  };

  const closeAgentModal = () => {
    setAgentNode(null);
    setAgentStatus(null);
    setAgentInstallCommand(null);
    setAgentError(null);
    setHealthCheckBusy(false);
    setHealthCheckResult(null);
    setControlStatus(null);
    setControlError(null);
    setRuntimeActionBusy(null);
    setRuntimeActionError(null);
    setRuntimeActionConfirm(null);
    setRuntimeActionNotice(null);
  };

  const handleEnableAgent = async () => {
    if (!agentNode) return;
    const port = parseInt(agentPort, 10);
    if (isNaN(port) || port <= 0 || port > 65535) {
      setAgentError('Port must be between 1 and 65535');
      return;
    }
    setAgentBusy(true);
    setAgentError(null);
    if (demoMode) {
      const enrollCode = `demo-${Math.random().toString(36).slice(2, 10)}`;
      const meshUrl = window.location.origin;
      setAgentStatus({ node: agentNode.name, enabled: true, port });
      setAgentInstallCommand({
        unix: `curl -fsSL https://raw.githubusercontent.com/Anirudhx7/ollama-mesh/main/install.sh | ROLE=agent MESH=${meshUrl} ENROLL=${enrollCode} PORT=${port} sh`,
        windows: `$env:ROLE="agent"; $env:MESH="${meshUrl}"; $env:ENROLL="${enrollCode}"; $env:PORT="${port}"; irm https://raw.githubusercontent.com/Anirudhx7/ollama-mesh/main/install.ps1 | iex`,
      });
      setNodes(prev => prev.map(n => n.name === agentNode.name
        ? {
            ...n,
            agentPresent: true,
            agentVersion: '0.1.0',
            fanPercent: 55,
            ramUsedMB: Math.round(20 * 1024),
            diskFreeGB: 500,
            agentCapabilities: ['telemetry'],
            agentPlatform: 'linux',
            agentArchitecture: 'amd64',
            agentGpuVendor: 'nvidia',
            agentRuntime: 'ollama',
          }
        : n));
      setAgentBusy(false);
      return;
    }
    try {
      const res = await enableNodeAgent(agentNode.name, port);
      setAgentStatus({ node: agentNode.name, enabled: true, port: res.port });
      setAgentInstallCommand({ unix: res.install_command, windows: res.install_command_windows });
      await loadNodes();
    } catch (e: any) {
      setAgentError(e?.message || 'Failed to enable node agent');
    } finally {
      setAgentBusy(false);
    }
  };

  const handleRegenerateAgentToken = async () => {
    if (!agentNode) return;
    setAgentBusy(true);
    setAgentError(null);
    if (demoMode) {
      const enrollCode = `demo-${Math.random().toString(36).slice(2, 10)}`;
      const meshUrl = window.location.origin;
      const port = agentStatus?.port ?? 11435;
      setAgentInstallCommand({
        unix: `curl -fsSL https://raw.githubusercontent.com/Anirudhx7/ollama-mesh/main/install.sh | ROLE=agent MESH=${meshUrl} ENROLL=${enrollCode} PORT=${port} sh`,
        windows: `$env:ROLE="agent"; $env:MESH="${meshUrl}"; $env:ENROLL="${enrollCode}"; $env:PORT="${port}"; irm https://raw.githubusercontent.com/Anirudhx7/ollama-mesh/main/install.ps1 | iex`,
      });
      setAgentBusy(false);
      return;
    }
    try {
      const res = await regenerateNodeAgentToken(agentNode.name);
      setAgentInstallCommand({ unix: res.install_command, windows: res.install_command_windows });
      setAgentStatus({ node: agentNode.name, enabled: true, port: res.port });
    } catch (e: any) {
      setAgentError(e?.message || 'Failed to regenerate node agent token');
    } finally {
      setAgentBusy(false);
    }
  };

  const handleCheckNodeHealth = async () => {
    if (!agentNode) return;
    const targetNodeName = agentNode.name;
    setHealthCheckBusy(true);
    setHealthCheckResult(null);
    if (demoMode) {
      setHealthCheckResult({ ok: true, latencyMs: 42 });
      setHealthCheckBusy(false);
      return;
    }
    try {
      const result = await checkNodeHealth(targetNodeName);
      // The modal may have been closed/switched to a different node while this
      // request was in flight - only apply the result if it's still relevant,
      // so a slow response for node A never gets mislabeled onto node B.
      if (agentNodeRef.current?.name !== targetNodeName) return;
      setHealthCheckResult(result);
    } catch (e: any) {
      if (agentNodeRef.current?.name !== targetNodeName) return;
      setHealthCheckResult({ ok: false, error: e?.message || 'Failed to run health check' });
    } finally {
      if (agentNodeRef.current?.name === targetNodeName) setHealthCheckBusy(false);
    }
  };

  const handleDisableAgent = async () => {
    if (!agentToDisable) return;
    setAgentBusy(true);
    setAgentError(null);
    if (demoMode) {
      setNodes(prev => prev.map(n => n.name === agentToDisable
        ? {
            ...n,
            agentPresent: false,
            agentVersion: undefined,
            fanPercent: undefined,
            ramUsedMB: undefined,
            diskFreeGB: undefined,
            agentCapabilities: undefined,
            agentPlatform: undefined,
            agentArchitecture: undefined,
            agentGpuVendor: undefined,
            agentRuntime: undefined,
          }
        : n));
      setAgentStatus(s => s ? { ...s, enabled: false } : s);
      setAgentInstallCommand(null);
      setAgentToDisable(null);
      setAgentBusy(false);
      return;
    }
    try {
      await disableNodeAgent(agentToDisable);
      setAgentStatus(s => s ? { ...s, enabled: false } : s);
      setAgentInstallCommand(null);
      setAgentToDisable(null);
      await loadNodes();
    } catch (e: any) {
      setAgentError(e?.message || 'Failed to disable node agent');
    } finally {
      setAgentBusy(false);
    }
  };

  const copyAgentCommand = (text: string, which: 'unix' | 'windows') => {
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).catch(() => legacyCopyText(text));
    } else {
      legacyCopyText(text);
    }
    setAgentCopiedWhich(which);
    setTimeout(() => setAgentCopiedWhich(null), 2000);
  };

  // Same fallback approach as APIKeys.tsx's copyToClipboard - works on plain
  // HTTP, must run synchronously inside a user-gesture handler.
  const legacyCopyText = (text: string) => {
    const el = document.createElement('textarea');
    el.value = text;
    el.setAttribute('readonly', '');
    el.style.cssText = 'position:absolute;left:-9999px;top:auto;width:1px;height:1px';
    document.body.appendChild(el);
    el.focus();
    el.select();
    el.setSelectionRange(0, text.length);
    try {
      document.execCommand('copy');
    } catch (_) {
      // Last resort: nothing we can do silently
    }
    document.body.removeChild(el);
  };

  const loadPinned = async (nodeList: GPUNode[], active: boolean = true) => {
    // getPinned() already has its own DEMO branch (returns static mock data),
    // so this must run in demo mode too - otherwise pinnedByNode stays empty
    // forever and every model shows the unload button, pinned or not.
    if (nodeList.length === 0 || !active || currentAppPath() !== '/gpu-nodes') return;
    const entries = await Promise.all(nodeList.map(async (n) => {
      try {
        return [n.name, await getPinned(n.name)] as const;
      } catch {
        return [n.name, []] as const; // pinned-fetch failure just means no badges for this node
      }
    }));
    if (active && currentAppPath() === '/gpu-nodes') {
      setPinnedByNode(Object.fromEntries(entries));
    }
  };

  const loadNodes = async (active: boolean = true) => {
    if (demoMode) {
      if (!active || currentAppPath() !== '/gpu-nodes') return;
      setNodes(mockGPUNodes);
      setIsLive(false);
      setError(null);
      await loadPinned(mockGPUNodes, active);
      return;
    }
    try {
      const data = await fetchNodes();
      if (!active || currentAppPath() !== '/gpu-nodes') return;
      setNodes(data || []);
      setIsLive(true);
      setError(null);
      await loadPinned(data || [], active);
    } catch (e: any) {
      if (!active || currentAppPath() !== '/gpu-nodes') return;
      setIsLive(false);
      setNodes([]);
      setError(e.message || 'Failed to connect to backend');
    }
  };

  const loadModelFit = async (active: boolean = true) => {
    if (demoMode || !active || currentAppPath() !== '/gpu-nodes') return;
    setModelFitLoading(true);
    try {
      const data = await fetchModelFit();
      if (!active || currentAppPath() !== '/gpu-nodes') return;
      setModelFit(data);
      setModelFitError(null);
    } catch (e: unknown) {
      if (!active || currentAppPath() !== '/gpu-nodes') return;
      const msg = e instanceof Error ? e.message : 'Failed to fetch model fit data';
      setModelFitError(msg);
    } finally {
      if (active && currentAppPath() === '/gpu-nodes') {
        setModelFitLoading(false);
      }
    }
  };

  useEffect(() => {
    if (currentAppPath() !== '/gpu-nodes') return;
    let active = true;
    // eslint-disable-next-line react-hooks/set-state-in-effect
    loadNodes(active);
    if (demoMode) return () => { active = false; };
    const interval = setInterval(() => loadNodes(active), 10000);
    return () => {
      active = false;
      clearInterval(interval);
    };
  }, [demoMode, location.pathname]);

  useEffect(() => {
    if (currentAppPath() !== '/gpu-nodes') return;
    let active = true;
    if (!demoMode) {
      // eslint-disable-next-line react-hooks/set-state-in-effect
      loadModelFit(active);
      const interval = setInterval(() => loadModelFit(active), 30000);
      return () => {
        active = false;
        clearInterval(interval);
      };
    }
  }, [demoMode, location.pathname]);

  const filteredNodes = nodes.filter(node =>
    (node.name || '').toLowerCase().includes(searchQuery.toLowerCase()) ||
    (node.gpuModel || '').toLowerCase().includes(searchQuery.toLowerCase())
  );

  const handleAddNode = async () => {
    if (!newNode.name || !newNode.host) return;

    if (demoMode) {
      const added: GPUNode = {
        id: `gpu-node-${Math.random().toString(36).substring(2, 9)}`,
        name: newNode.name,
        host: newNode.host,
        gpuModel: newNode.gpuModel || 'Unknown GPU',
        port: parseInt(newNode.port, 10) || 11434,
        vramTotalMB: 24576,
        vramUsedMB: 0,
        vramSource: 'declared',
        runtime: newNode.runtime === 'auto' ? 'ollama' : newNode.runtime,
        powerDrawW: 0,
        cpuPercent: 0,
        temperature: 45,
        health: 'healthy',
        draining: false,
        activeConns: 0,
        uptime: '0m',
        loadedModels: [],
        healthHistory: [100, 100, 100],
      };
      setNodes(prev => [...prev, added]);
      setActionError(null);
      setIsAddModalOpen(false);
      setNewNode({ name: '', host: '', port: '11434', gpuModel: '', runtime: 'auto' });
      return;
    }

    if (!isLive) return;

    const nodeData = {
      name: newNode.name,
      url: `http://${newNode.host}:${newNode.port}`,
      gpu_model: newNode.gpuModel,
      runtime: newNode.runtime,
    };

    try {
      await addNode(nodeData);
      await loadNodes();
      setActionError(null);
      setIsAddModalOpen(false);
      setNewNode({ name: '', host: '', port: '11434', gpuModel: '', runtime: 'auto' });
    } catch (err: any) {
      setActionError(err?.message || 'Failed to add node');
    }
  };

  const handleRemoveNode = async (name: string): Promise<boolean> => {
    if (demoMode) {
      setNodes(prev => prev.filter(n => n.name !== name));
      setActionError(null);
      return true;
    }
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
    if (demoMode) {
      setNodes(prev => prev.map(n => n.name === name ? { ...n, draining: true } : n));
      setActionError(null);
      return true;
    }
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
    if (demoMode) {
      setNodes(prev => prev.map(n => n.name === name ? { ...n, draining: false } : n));
      setActionError(null);
      return;
    }
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
    if (demoMode) {
      setNodes(prev => prev.map(n => n.name === name ? { ...n, prewarmDisabled: disabled } : n));
      setActionError(null);
      return true;
    }
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
      // right after this page loaded) - the backend still enforces it (409),
      // so surface that clearly instead of silently dropping the click.
      setActionError(e?.message || `Failed to unload ${model} from ${nodeName}`);
      return false;
    }
  };

  const [editNode, setEditNode] = useState<GPUNode | null>(null);
  const [editHost, setEditHost] = useState('');
  const [editPort, setEditPort] = useState('');
  const [editVRAM, setEditVRAM] = useState('');
  const [editGPUModel, setEditGPUModel] = useState('');
  const [editRuntime, setEditRuntime] = useState('');
  const [editSaving, setEditSaving] = useState(false);
  const [editError, setEditError] = useState('');
  const [pendingPatch, setPendingPatch] = useState<{ vram_total_mb?: number; gpu_model?: string; runtime?: string; url?: string } | null>(null);

  const openEditModal = (node: GPUNode) => {
    setEditNode(node);
    setEditHost(node.host ?? '');
    setEditPort(node.port ? String(node.port) : '');
    setEditVRAM(node.vramTotalMB > 0 ? String(node.vramTotalMB) : '');
    setEditGPUModel(node.gpuModel ?? '');
    setEditRuntime(node.runtime || 'ollama');
    setEditError('');
  };

  const buildPatch = (): { vram_total_mb?: number; gpu_model?: string; runtime?: string; url?: string } | 'invalid' | null => {
    if (!editNode) return null;
    const patch: { vram_total_mb?: number; gpu_model?: string; runtime?: string; url?: string } = {};
    if (editVRAM.trim() !== '') {
      const v = parseInt(editVRAM, 10);
      if (isNaN(v) || v < 0) { setEditError('VRAM must be a non-negative integer (MB)'); return 'invalid'; }
      patch.vram_total_mb = v;
    }
    if (editGPUModel.trim() !== '') patch.gpu_model = editGPUModel.trim();
    if (editRuntime && editRuntime !== (editNode.runtime || 'ollama')) patch.runtime = editRuntime;
    const hostChanged = editHost.trim() !== '' && editHost.trim() !== (editNode.host ?? '');
    const portChanged = editPort.trim() !== '' && editPort.trim() !== String(editNode.port ?? '');
    if (hostChanged || portChanged) {
      const host = editHost.trim() || editNode.host;
      const port = editPort.trim() || String(editNode.port);
      if (!host || !port || isNaN(parseInt(port, 10))) { setEditError('Host and port must both be set'); return 'invalid'; }
      patch.url = `http://${host}:${port}`;
    }
    if (Object.keys(patch).length === 0) return null;
    return patch;
  };

  const applyPatch = async (patch: { vram_total_mb?: number; gpu_model?: string; runtime?: string; url?: string }) => {
    if (!editNode) return;
    if (demoMode) {
      setNodes(prev => prev.map(n => n.name === editNode.name
        ? { ...n, vramTotalMB: patch.vram_total_mb ?? n.vramTotalMB, gpuModel: patch.gpu_model ?? n.gpuModel, runtime: patch.runtime ?? n.runtime }
        : n));
      setEditNode(null);
      return;
    }

    if (!isLive) return;
    setEditSaving(true);
    setEditError('');
    try {
      await patchNode(editNode.name, patch);
      await loadNodes();
      setEditNode(null);
    } catch (e: any) {
      setEditError(e?.message || 'Failed to save changes');
    } finally {
      setEditSaving(false);
    }
  };

  const handleSavePatch = async () => {
    const patch = buildPatch();
    if (patch === 'invalid' || patch === null) return;
    if (patch.url) {
      // Address changes are surfaced behind a confirm dialog - the node's
      // live health/warm-state resets (it's now pointing at a different
      // physical backend), so this shouldn't happen from an accidental click.
      setPendingPatch(patch);
      return;
    }
    await applyPatch(patch);
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
            <div className={`w-2 h-2 rounded-full ${isLive || demoMode ? 'bg-success' : 'bg-amber-500'}`} />
            <span className={`text-xs font-medium ${isLive || demoMode ? 'text-success' : 'text-amber-600 dark:text-amber-400'}`}>
              {demoMode ? 'Demo Mode' : (isLive ? 'Live Data' : 'Disconnected')}
            </span>
          </div>
          <button
            onClick={() => { setActionError(null); setIsAddModalOpen(true); }}
            disabled={!isLive && !demoMode}
            title={!isLive && !demoMode ? 'Backend disconnected' : undefined}
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
          <NodeCard key={node.id} node={node} pinnedModels={pinnedByNode[node.name] ?? []} onRemove={(name) => { setActionError(null); setNodeToDelete(name); }} onDrain={(name) => { setActionError(null); setNodeToDrain(name); }} onUndrain={handleUndrainNode} onTogglePrewarm={(name, disabled) => { setActionError(null); setPrewarmToToggle({ name, disabled }); }} onEdit={openEditModal} onUnload={(nodeName, model) => { setActionError(null); setModelToUnload({ nodeName, model }); }} onConfigureModel={(modelName, nodeName, runtime) => setConfigTarget({ model: modelName, node: nodeName, runtime })} onManageAgent={openAgentModal} />
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
                    LIVE_VRAM_TOOL_SOURCES.has(nodeFit.vram_source)
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
          <div>
            <label className="block text-sm font-medium text-muted-foreground mb-1.5">
              Runtime
            </label>
            <CustomSelect
              value={newNode.runtime || 'auto'}
              onChange={(val) => setNewNode({ ...newNode, runtime: val })}
              options={[
                { value: 'auto', label: 'Auto-detect' },
                { value: 'ollama', label: 'Ollama' },
                { value: 'vllm', label: 'vLLM' },
                { value: 'tgi', label: 'TGI (Text Generation Inference)' },
                { value: 'llamacpp', label: 'llama.cpp' },
                { value: 'mlx', label: 'MLX (Apple Silicon)' },
              ]}
            />
            <p className="text-xs text-muted-foreground mt-1">
              Auto-detect probes the node on first health check. Pick a runtime directly if you know it.
            </p>
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
          <div className="grid grid-cols-2 gap-3">
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Host / IP
              </label>
              <input
                type="text"
                value={editHost}
                onChange={(e) => setEditHost(e.target.value)}
                placeholder="e.g., 192.168.1.50"
                className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
              />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Port
              </label>
              <input
                type="number"
                min="1"
                max="65535"
                value={editPort}
                onChange={(e) => setEditPort(e.target.value)}
                placeholder="e.g., 11434"
                className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
              />
            </div>
          </div>
          <p className="text-xs text-muted-foreground -mt-2">
            Changing host or port re-points the mesh at a different address and resets this node's live health/warm state - you'll be asked to confirm.
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
              Runtime
            </label>
            <CustomSelect
              value={editRuntime}
              onChange={setEditRuntime}
              options={[
                { value: 'auto', label: 'Auto-detect' },
                { value: 'ollama', label: 'Ollama' },
                { value: 'vllm', label: 'vLLM' },
                { value: 'tgi', label: 'TGI (Text Generation Inference)' },
                { value: 'llamacpp', label: 'llama.cpp' },
                { value: 'mlx', label: 'MLX (Apple Silicon)' },
              ]}
            />
            <p className="text-xs text-muted-foreground mt-1">
              Setting Auto-detect re-probes the node on the next health check.
            </p>
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

      {/* Confirm Node Address Change Modal */}
      <Modal
        isOpen={pendingPatch !== null}
        onClose={() => setPendingPatch(null)}
        title="Change Node Address"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Change <span className="text-foreground font-semibold">{editNode?.name}</span>'s address to{' '}
            <span className="text-foreground font-semibold">{pendingPatch?.url}</span>?
          </p>
          <p className="text-xs text-muted-foreground">
            The mesh will re-point at this new address immediately. This node's live health and warm-model state reset, since it's now treated as a different physical backend.
          </p>
          {editError && (
            <p className="text-sm text-destructive">{editError}</p>
          )}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setPendingPatch(null)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={async () => {
                if (!pendingPatch) return;
                const patch = pendingPatch;
                setPendingPatch(null);
                await applyPatch(patch);
              }}
              disabled={editSaving}
              className="px-4 py-2 bg-primary hover:bg-primary/90 disabled:opacity-50 disabled:cursor-not-allowed text-primary-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              {editSaving ? 'Saving...' : 'Confirm Change'}
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

      {/* Manage Node Agent Modal */}
      <Modal
        isOpen={agentNode !== null}
        onClose={closeAgentModal}
        title={`Node Agent: ${agentNode?.name ?? ''}`}
        maxWidth="2xl"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            The Node Agent is an optional process you run on this GPU node to report CPU usage, fan speed, RAM usage, and free disk space back to the mesh. Everything else (VRAM, temperature, power) is already collected without it.
          </p>

          {agentError && (
            <p className="text-sm text-destructive">{agentError}</p>
          )}

          <div className="space-y-2">
            <button
              onClick={handleCheckNodeHealth}
              disabled={healthCheckBusy}
              title="Run a live health check against this node's inference runtime right now, instead of waiting for the next automatic poll - works whether or not a Node Agent is installed"
              className="px-4 py-2 bg-secondary hover:bg-secondary/80 disabled:opacity-50 disabled:cursor-not-allowed text-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              {healthCheckBusy ? 'Checking...' : 'Health Check'}
            </button>
            {healthCheckResult && (
              <p className={`text-xs ${healthCheckResult.ok ? 'text-success' : 'text-destructive'}`}>
                <span className="font-medium">Health check result:</span>{' '}
                {healthCheckResult.ok
                  ? `up${healthCheckResult.latencyMs != null ? ` (${healthCheckResult.latencyMs}ms)` : ''}`
                  : `down${healthCheckResult.error ? ` - ${healthCheckResult.error}` : ''}`}
              </p>
            )}
          </div>

          {agentStatus && !agentStatus.enabled && (
            <div className="space-y-3">
              <div>
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                  Agent Port
                </label>
                <input
                  type="number"
                  min="1"
                  max="65535"
                  value={agentPort}
                  onChange={(e) => setAgentPort(e.target.value)}
                  className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50"
                />
                <p className="text-xs text-muted-foreground mt-1">
                  Port the agent process listens on for the mesh to poll (default 11435).
                </p>
              </div>
              <div className="flex justify-end pt-2">
                <button
                  onClick={handleEnableAgent}
                  disabled={agentBusy}
                  className="px-4 py-2 bg-primary hover:bg-primary/90 disabled:opacity-50 disabled:cursor-not-allowed text-primary-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
                >
                  {agentBusy ? 'Enabling...' : 'Enable Agent'}
                </button>
              </div>
            </div>
          )}

          {agentStatus && agentStatus.enabled && (
            <div className="space-y-3">
              <p className="text-sm text-foreground">
                Enabled on port <span className="font-mono">{agentStatus.port}</span>.
              </p>
              {agentNode?.agentPresent && (
                <div className="text-xs text-muted-foreground space-y-1 bg-secondary/40 rounded-lg p-3">
                  <p><span className="font-medium text-foreground">Agent version:</span> {agentNode.agentVersion || '--'}</p>
                  <p><span className="font-medium text-foreground">Platform:</span> {agentNode.agentPlatform || '--'} / {agentNode.agentArchitecture || '--'}</p>
                  <p><span className="font-medium text-foreground">GPU vendor:</span> {agentNode.agentGpuVendor || '--'}{agentNode.driverVersion ? ` (driver ${agentNode.driverVersion}${agentNode.cudaVersion ? `, CUDA ${agentNode.cudaVersion}` : ''})` : ''}</p>
                  {agentNode.agentRuntime && (
                    <p>
                      <span className="font-medium text-foreground">Detected runtime:</span> {agentNode.agentRuntime}
                      {agentNode.runtimeVersion ? ` ${agentNode.runtimeVersion}` : ''}
                      {agentNode.runtimeStatus ? ` (${agentNode.runtimeStatus})` : ''}
                    </p>
                  )}
                  <p><span className="font-medium text-foreground">Capabilities:</span> {agentNode.agentCapabilities?.length ? agentNode.agentCapabilities.join(', ') : '--'}</p>
                  {agentNode.hostname && (
                    <p><span className="font-medium text-foreground">Host:</span> {agentNode.hostname}{agentNode.uptimeSeconds ? ` (up ${formatDurationLong(agentNode.uptimeSeconds)})` : ''}</p>
                  )}
                  {(agentNode.ramTotalMB || agentNode.diskTotalGB) && (
                    <p>
                      <span className="font-medium text-foreground">Host capacity:</span>{' '}
                      {agentNode.ramTotalMB ? `${(agentNode.ramTotalMB / 1024).toFixed(1)} GB RAM` : '--'}
                      {', '}
                      {agentNode.diskTotalGB ? `${agentNode.diskTotalGB.toFixed(0)} GB disk` : '--'}
                    </p>
                  )}
                  {agentNode.agentGpus && agentNode.agentGpus.length > 1 && (
                    <div className="pt-1">
                      <span className="font-medium text-foreground">GPUs ({agentNode.agentGpus.length}):</span>
                      <ul className="mt-1 space-y-0.5">
                        {agentNode.agentGpus.map((gpu) => (
                          <li key={gpu.index} className="font-mono">
                            #{gpu.index}: {gpu.vramUsedMB != null && gpu.vramTotalMB != null ? `${(gpu.vramUsedMB / 1024).toFixed(1)}/${(gpu.vramTotalMB / 1024).toFixed(1)} GB` : '--'}
                            {gpu.corePercent != null ? `, ${gpu.corePercent}% util` : ''}
                            {gpu.temperatureC != null ? `, ${gpu.temperatureC}°C` : ''}
                          </li>
                        ))}
                      </ul>
                    </div>
                  )}
                  {agentNode.agentNodeId && (
                    <p className="pt-1 text-[10px] opacity-70">node_id: {agentNode.agentNodeId}</p>
                  )}
                </div>
              )}
              <div className="flex flex-wrap gap-3">
                <button
                  onClick={handleRegenerateAgentToken}
                  disabled={agentBusy}
                  className="px-4 py-2 bg-secondary hover:bg-secondary/80 disabled:opacity-50 disabled:cursor-not-allowed text-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
                >
                  {agentBusy ? 'Working...' : 'Regenerate Token'}
                </button>
                <button
                  onClick={() => setAgentToDisable(agentNode?.name ?? null)}
                  disabled={agentBusy}
                  className="px-4 py-2 bg-destructive/10 hover:bg-destructive/20 disabled:opacity-50 disabled:cursor-not-allowed text-destructive font-medium rounded-lg text-sm transition-colors shadow-sm"
                >
                  Disable Agent
                </button>
              </div>
            </div>
          )}

          {(controlStatus || controlError) && (
            <div className="p-4 bg-secondary/40 border border-border rounded-xl space-y-3">
              <p className="text-sm font-semibold text-foreground">Runtime Control</p>
              <p className="text-xs text-muted-foreground">
                How the agent starts/stops/restarts the inference runtime process on this node.
              </p>
              {controlError && <p className="text-sm text-destructive">{controlError}</p>}

              {controlStatus && (controlStatus.configured ? (
                <div className="space-y-3">
                  <div className="flex items-center justify-between gap-3">
                    <p className="text-sm text-foreground">
                      Configured: <span className="font-mono">{controlStatus.driver}</span> / <span className="font-mono">{controlStatus.identifier}</span>
                    </p>
                    <button
                      onClick={clearControl}
                      disabled={controlBusy}
                      className="px-3 py-1.5 bg-destructive/10 hover:bg-destructive/20 disabled:opacity-50 disabled:cursor-not-allowed text-destructive font-medium rounded-lg text-xs transition-colors shadow-sm"
                    >
                      {controlBusy ? 'Working...' : 'Clear'}
                    </button>
                  </div>
                  <div className="flex items-center gap-2">
                    <StatusDot status={agentNode?.runtimeStatus === 'up' ? 'online' : agentNode?.runtimeStatus ? 'offline' : 'suspended'} size="sm" />
                    <p className="text-sm text-foreground">
                      Runtime is currently{' '}
                      <span className="font-semibold">
                        {agentNode?.runtimeStatus === 'up' ? 'running' : agentNode?.runtimeStatus ? 'stopped' : 'unknown'}
                      </span>
                    </p>
                  </div>
                  {runtimeActionError && <p className="text-sm text-destructive">{runtimeActionError}</p>}
                  {runtimeActionNotice && <p className="text-sm text-muted-foreground">{runtimeActionNotice}</p>}
                  <div className="flex flex-wrap gap-2 pt-1">
                    <button
                      onClick={() => {
                        setRuntimeActionNotice(null);
                        if (agentNode?.runtimeStatus === 'up') {
                          setRuntimeActionNotice('Runtime is already running on this node.');
                          return;
                        }
                        setRuntimeActionConfirm('start');
                      }}
                      disabled={runtimeActionBusy !== null}
                      title={demoMode ? 'No-op in demo mode' : undefined}
                      className="px-3 py-1.5 bg-success/20 hover:bg-success/30 disabled:opacity-50 disabled:cursor-not-allowed text-success font-medium rounded-lg text-xs transition-colors"
                    >
                      {runtimeActionBusy === 'start' ? 'Starting...' : 'Start'}
                    </button>
                    <button
                      onClick={() => {
                        setRuntimeActionNotice(null);
                        if (agentNode?.runtimeStatus && agentNode.runtimeStatus !== 'up') {
                          setRuntimeActionNotice('Runtime is already stopped on this node.');
                          return;
                        }
                        setRuntimeActionConfirm('stop');
                      }}
                      disabled={runtimeActionBusy !== null}
                      title={demoMode ? 'No-op in demo mode' : undefined}
                      className="px-3 py-1.5 bg-destructive/10 hover:bg-destructive/20 disabled:opacity-50 disabled:cursor-not-allowed text-destructive font-medium rounded-lg text-xs transition-colors"
                    >
                      {runtimeActionBusy === 'stop' ? 'Stopping...' : 'Stop'}
                    </button>
                    <button
                      onClick={() => { setRuntimeActionNotice(null); setRuntimeActionConfirm('restart'); }}
                      disabled={runtimeActionBusy !== null}
                      title={demoMode ? 'No-op in demo mode' : undefined}
                      className="px-3 py-1.5 bg-secondary hover:bg-secondary/80 disabled:opacity-50 disabled:cursor-not-allowed text-foreground font-medium rounded-lg text-xs transition-colors shadow-sm"
                    >
                      {runtimeActionBusy === 'restart' ? 'Restarting...' : 'Restart'}
                    </button>
                    <button
                      onClick={viewRuntimeLogs}
                      title={demoMode ? 'Showing sample logs' : undefined}
                      className="px-3 py-1.5 bg-secondary hover:bg-secondary/80 text-foreground font-medium rounded-lg text-xs transition-colors shadow-sm"
                    >
                      View Logs
                    </button>
                  </div>
                </div>
              ) : controlStatus.discovered.driver ? (
                <div className="space-y-2">
                  <p className="text-sm text-foreground">
                    Discovered: <span className="font-mono">{controlStatus.discovered.driver}</span> / <span className="font-mono">{controlStatus.discovered.identifier}</span>
                  </p>
                  {controlStatus.discovered.evidence && controlStatus.discovered.evidence.length > 0 && (
                    <ul className="text-xs text-muted-foreground list-disc list-inside space-y-0.5">
                      {controlStatus.discovered.evidence.map((e, i) => <li key={i}>{e}</li>)}
                    </ul>
                  )}
                  <button
                    onClick={acceptDiscoveredControl}
                    disabled={controlBusy}
                    className="px-3 py-1.5 bg-success/20 hover:bg-success/30 disabled:opacity-50 disabled:cursor-not-allowed text-success font-medium rounded-lg text-xs transition-colors"
                  >
                    {controlBusy ? 'Working...' : 'Accept'}
                  </button>
                </div>
              ) : (
                <p className="text-sm text-muted-foreground">No control method auto-discovered on this node yet.</p>
              ))}

              <div className="flex flex-wrap items-end gap-2 pt-2 border-t border-border">
                <div>
                  <label className="block text-xs font-medium text-muted-foreground mb-1">Driver</label>
                  <CustomSelect
                    value={controlManualDriver}
                    onChange={setControlManualDriver}
                    size="sm"
                    className="w-40"
                    options={[
                      { value: 'systemd', label: 'systemd' },
                      { value: 'docker', label: 'docker' },
                      { value: 'process', label: 'process' },
                      { value: 'launchd', label: 'launchd' },
                      { value: 'windows_service', label: 'windows_service' },
                    ]}
                  />
                </div>
                <div className="flex-1 min-w-[10rem]">
                  <label className="block text-xs font-medium text-muted-foreground mb-1">Identifier</label>
                  <input
                    type="text"
                    value={controlManualIdentifier}
                    onChange={(e) => setControlManualIdentifier(e.target.value)}
                    placeholder="e.g. ollama.service, container name, PID file path"
                    className="w-full px-2 py-1.5 bg-background border border-border rounded-lg text-xs text-foreground"
                  />
                </div>
                {controlManualDriver === 'process' && (
                  <div className="flex-1 min-w-[14rem]">
                    <label className="block text-xs font-medium text-muted-foreground mb-1">Start command</label>
                    <input
                      type="text"
                      value={controlManualStartCommand}
                      onChange={(e) => setControlManualStartCommand(e.target.value)}
                      placeholder="e.g. /usr/local/bin/ollama serve"
                      className="w-full px-2 py-1.5 bg-background border border-border rounded-lg text-xs text-foreground"
                    />
                  </div>
                )}
                <button
                  onClick={acceptManualControl}
                  disabled={controlBusy || !controlManualIdentifier.trim()}
                  className="px-3 py-1.5 bg-secondary hover:bg-secondary/80 disabled:opacity-50 disabled:cursor-not-allowed text-foreground font-medium rounded-lg text-xs transition-colors shadow-sm"
                >
                  Set Manually
                </button>
              </div>
            </div>
          )}

          <div className="flex justify-end pt-4 border-t border-border">
            <button
              onClick={closeAgentModal}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Close
            </button>
          </div>
        </div>
      </Modal>

      {/* Node Agent Install Command Modal - separate dialog so the enroll token/commands don't get buried below Runtime Control */}
      <Modal
        isOpen={agentInstallCommand !== null}
        onClose={() => setAgentInstallCommand(null)}
        title="Node Agent Install Command"
        maxWidth="lg"
      >
        {agentInstallCommand && (
          <div className="space-y-4">
            <p className="text-sm font-semibold text-success">
              Run one of these on the GPU node - the token is shown once and won't be shown again
            </p>
            <div>
              <p className="text-xs font-medium text-muted-foreground mb-1">Linux / macOS</p>
              <code className="block font-mono text-sm bg-background border border-border rounded-lg px-3 py-2 break-all text-foreground select-all">
                {agentInstallCommand.unix}
              </code>
              <div className="flex justify-end mt-2">
                <button
                  onClick={() => copyAgentCommand(agentInstallCommand.unix, 'unix')}
                  className="flex items-center gap-1.5 px-3 py-1.5 text-xs font-medium bg-success/20 hover:bg-success/30 text-success rounded-lg transition-colors"
                >
                  <Copy className="w-3.5 h-3.5" />
                  {agentCopiedWhich === 'unix' ? 'Copied!' : 'Copy'}
                </button>
              </div>
            </div>
            <div>
              <p className="text-xs font-medium text-muted-foreground mb-1">Windows (PowerShell, run as Administrator)</p>
              <code className="block font-mono text-sm bg-background border border-border rounded-lg px-3 py-2 break-all text-foreground select-all">
                {agentInstallCommand.windows}
              </code>
              <div className="flex justify-end mt-2">
                <button
                  onClick={() => copyAgentCommand(agentInstallCommand.windows, 'windows')}
                  className="flex items-center gap-1.5 px-3 py-1.5 text-xs font-medium bg-success/20 hover:bg-success/30 text-success rounded-lg transition-colors"
                >
                  <Copy className="w-3.5 h-3.5" />
                  {agentCopiedWhich === 'windows' ? 'Copied!' : 'Copy'}
                </button>
              </div>
            </div>
            <div className="flex justify-end pt-2 border-t border-border">
              <button
                onClick={() => setAgentInstallCommand(null)}
                className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
              >
                Close
              </button>
            </div>
          </div>
        )}
      </Modal>

      {/* Disable Node Agent Confirmation Modal */}
      <Modal
        isOpen={agentToDisable !== null}
        onClose={() => setAgentToDisable(null)}
        title="Disable Node Agent"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Are you sure you want to disable the Node Agent on <span className="text-foreground font-semibold">{agentToDisable}</span>?
          </p>
          <p className="text-xs text-muted-foreground">
            Fan/RAM/disk telemetry stops updating for this node. VRAM, temperature, and power readings are unaffected. You can re-enable it later, but it will need a fresh token and a restart of the agent process on the node.
          </p>
          {agentError && (
            <p className="text-sm text-destructive">{agentError}</p>
          )}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setAgentToDisable(null)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={handleDisableAgent}
              disabled={agentBusy}
              className="px-4 py-2 bg-destructive hover:bg-destructive/90 disabled:opacity-50 disabled:cursor-not-allowed text-destructive-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              {agentBusy ? 'Disabling...' : 'Disable Agent'}
            </button>
          </div>
        </div>
      </Modal>

      {/* Runtime Start/Stop/Restart Confirmation Modal */}
      <Modal
        isOpen={runtimeActionConfirm !== null}
        onClose={() => setRuntimeActionConfirm(null)}
        title={
          runtimeActionConfirm === 'start' ? 'Start Runtime'
          : runtimeActionConfirm === 'stop' ? 'Stop Runtime'
          : 'Restart Runtime'
        }
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            {runtimeActionConfirm === 'start' && <>Start the inference runtime process on <span className="text-foreground font-semibold">{agentNode?.name}</span>?</>}
            {runtimeActionConfirm === 'stop' && <>Stop the inference runtime process on <span className="text-foreground font-semibold">{agentNode?.name}</span>?</>}
            {runtimeActionConfirm === 'restart' && <>Restart the inference runtime process on <span className="text-foreground font-semibold">{agentNode?.name}</span>?</>}
          </p>
          <p className="text-xs text-muted-foreground">
            {runtimeActionConfirm === 'start' && 'Reversible: the runtime is idle before this. Any currently-loaded models stay unloaded until requests warm them back up. No in-flight requests are at risk since the runtime is not serving traffic yet.'}
            {runtimeActionConfirm === 'stop' && 'Disruptive and immediate: any in-flight requests on this node fail right now, and all warm/loaded models are evicted from VRAM. The mesh routes new requests to other nodes, but this node serves nothing until you start it again.'}
            {runtimeActionConfirm === 'restart' && 'Disruptive and immediate: the runtime process is killed and relaunched. In-flight requests on this node fail, and all warm/loaded models are evicted and must reload from cold on next use. The process comes back up on its own once restarted - no separate start needed.'}
          </p>
          {runtimeActionError && (
            <p className="text-sm text-destructive">{runtimeActionError}</p>
          )}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setRuntimeActionConfirm(null)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={() => runtimeActionConfirm && runRuntimeAction(runtimeActionConfirm)}
              disabled={runtimeActionBusy !== null}
              className={
                runtimeActionConfirm === 'start'
                  ? "px-4 py-2 bg-success hover:bg-success/90 disabled:opacity-50 disabled:cursor-not-allowed text-success-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
                  : "px-4 py-2 bg-destructive hover:bg-destructive/90 disabled:opacity-50 disabled:cursor-not-allowed text-destructive-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
              }
            >
              {runtimeActionBusy !== null
                ? (runtimeActionConfirm === 'start' ? 'Starting...' : runtimeActionConfirm === 'stop' ? 'Stopping...' : 'Restarting...')
                : (runtimeActionConfirm === 'start' ? 'Start' : runtimeActionConfirm === 'stop' ? 'Stop' : 'Restart')}
            </button>
          </div>
        </div>
      </Modal>

      {/* Runtime Logs Modal (P58) - a pure read, no confirm needed */}
      <Modal
        isOpen={logsModalOpen}
        onClose={() => setLogsModalOpen(false)}
        title={`Runtime Logs — ${agentNode?.name ?? ''}`}
        maxWidth="lg"
      >
        <div className="space-y-4">
          <p className="text-xs text-muted-foreground">
            Point-in-time snapshot, not a live tail.
          </p>
          {logsBusy && <p className="text-sm text-muted-foreground">Loading...</p>}
          {logsError && <p className="text-sm text-destructive">{logsError}</p>}
          {!logsBusy && !logsError && logsLines && (
            <pre className="text-xs font-mono whitespace-pre-wrap break-all max-h-96 overflow-y-auto bg-secondary/40 border border-border rounded-lg p-3">
              {logsLines.length > 0 ? logsLines.join('\n') : 'No log lines returned.'}
            </pre>
          )}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setLogsModalOpen(false)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Close
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
