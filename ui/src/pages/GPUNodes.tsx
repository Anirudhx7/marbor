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
import { fetchNodes, addNode, removeNode, drainNode, undrainNode, setNodePrewarm, patchNode, probeNodeTLS, fetchModelFit, unloadModel, getPinned, getMarborAgent, enableMarborAgent, regenerateMarborAgentToken, disableMarborAgent, checkNodeHealth, getNodeControl, acceptNodeControl, clearNodeControl, startNodeRuntime, stopNodeRuntime, restartNodeRuntime, getNodeRuntimeLogs } from '../lib/api';
import type { MarborAgentStatus, NodeHealthCheckResult, NodeControlStatus } from '../lib/api';
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
// "nvidia-smi" for the marbor's own local card, or whatever tool the Node
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
        title={version ? `marbor agent installed (v${version})` : 'marbor agent installed'}
        className="inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-xs font-medium bg-primary/15 text-primary border border-primary/30 whitespace-nowrap"
      >
        <Radio className="w-3 h-3" />
        Agent
      </span>
    );
  }
  return (
    <span
      title="No marbor agent installed - only local nvidia-smi telemetry available"
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
    degraded: 'text-amber-700 dark:text-amber-400',
    down: 'text-destructive',
  }[node.health];

  return (
    <div className={`bg-card border shadow-sm rounded-xl p-5 transition-colors ${node.draining ? 'border-amber-500/20 hover:border-amber-500/40 bg-amber-500/[0.02]' : 'border-border hover:border-primary/50'}`}>
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
                  title="Predictive engine will not warm new models onto this node until re-enabled or marbor restarts"
                  className="text-xs font-medium px-1.5 py-0.5 rounded bg-secondary text-muted-foreground border border-border whitespace-nowrap"
                >
                  PREWARM OFF
                </span>
              )}
              {node.tlsFingerprintMismatch && (
                <span
                  title="The node's agent is presenting a certificate that doesn't match the pinned fingerprint - connections are refused (possible MITM or an unexpected cert rotation). Open Edit Node to review and re-confirm."
                  className="text-xs font-medium px-1.5 py-0.5 rounded bg-destructive/10 text-destructive dark:text-red-400 border border-destructive/30 whitespace-nowrap"
                >
                  TLS MISMATCH
                </span>
              )}
              {node.warmupErrors && Object.keys(node.warmupErrors).length > 0 && (
                <span
                  title={Object.entries(node.warmupErrors).map(([model, err]) => `${model}: ${err}`).join('\n')}
                  className="text-xs font-medium px-1.5 py-0.5 rounded bg-destructive/10 text-destructive dark:text-red-400 border border-destructive/30 whitespace-nowrap"
                >
                  WARMUP FAILED ({Object.keys(node.warmupErrors).length})
                </span>
              )}
              {node.unloadErrors && Object.keys(node.unloadErrors).length > 0 && (
                <span
                  title={Object.entries(node.unloadErrors).map(([model, err]) => `${model}: ${err}`).join('\n')}
                  className="text-xs font-medium px-1.5 py-0.5 rounded bg-destructive/10 text-destructive dark:text-red-400 border border-destructive/30 whitespace-nowrap"
                >
                  UNLOAD FAILED ({Object.keys(node.unloadErrors).length})
                </span>
              )}
              {node.warmupState && node.warmupState.filter(s => s.state === 'suppressed').length > 0 && (
                <span
                  title={node.warmupState.filter(s => s.state === 'suppressed').map(s => `${s.model}: ${s.reason}`).join('\n')}
                  className="text-xs font-medium px-1.5 py-0.5 rounded bg-amber-500/10 text-amber-700 dark:text-amber-400 border border-amber-500/30 whitespace-nowrap"
                >
                  SUPPRESSED ({node.warmupState.filter(s => s.state === 'suppressed').length})
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
            title="Manage marbor agent (fan/RAM/disk telemetry)"
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
            node.temperature && node.temperature > 70 ? 'text-amber-700 dark:text-amber-400' : 'text-primary'
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

      {/* marbor agent Telemetry - only ever real values from the agent poll;
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
                    className="tap-expand ml-1.5 opacity-50 hover:opacity-100 hover:text-primary transition-opacity"
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
                      className="tap-expand ml-1.5 -mr-0.5 opacity-50 hover:opacity-100 hover:text-destructive transition-opacity"
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
    // P24: opt-in per node, defaults off - matches every pre-P24 node's
    // plaintext behavior unchanged.
    useHttps: false,
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

  // --- marbor agent management ---
  const [agentNode, setAgentNode] = useState<GPUNode | null>(null);
  const [agentStatus, setAgentStatus] = useState<MarborAgentStatus | null>(null);
  const [agentPort, setAgentPort] = useState('9200');
  // agentUseHttps is the marbor agent's OWN transport scheme toggle -
  // independent of a node's runtime URL scheme (editUseHttps below, which
  // governs the Ollama/vLLM/etc. endpoint). Enabling this does not touch
  // the node's runtime URL at all.
  const [agentUseHttps, setAgentUseHttps] = useState(false);
  const [agentInstallCommand, setAgentInstallCommand] = useState<{ unix: string; windows: string } | null>(null);
  const [agentBusy, setAgentBusy] = useState(false);
  const [agentError, setAgentError] = useState<string | null>(null);
  const [agentCopiedWhich, setAgentCopiedWhich] = useState<'unix' | 'windows' | null>(null);
  const [agentToDisable, setAgentToDisable] = useState<string | null>(null);
  // pendingAgentReconfigure gates the "Reconfigure marbor agent connection?"
  // confirm - only shown when changing port/scheme on an ALREADY-enabled
  // agent (nothing to disrupt on the very first Enable, so that path stays
  // immediate-apply, same as handleEnableAgent always has been).
  const [pendingAgentReconfigure, setPendingAgentReconfigure] = useState(false);
  // pendingRegenerateToken gates the "Regenerate marbor agent token?" confirm
  // (R10): handleRegenerateMarborAgentToken swaps the live token in the
  // router's in-memory state immediately (admin.go SetMarborAgent), so the
  // currently-running agent process - still presenting the old token - is
  // rejected on its very next poll until reinstalled with the new command.
  const [pendingRegenerateToken, setPendingRegenerateToken] = useState(false);
  const [healthCheckBusy, setHealthCheckBusy] = useState(false);
  const [healthCheckResult, setHealthCheckResult] = useState<NodeHealthCheckResult | null>(null);
  // Tracks the modal's current node synchronously (unlike agentNode state,
  // which only updates after a render) so an in-flight health check can tell,
  // the instant its response lands, whether the modal has since moved to a
  // different node and the result should be discarded rather than misapplied.
  const agentNodeRef = useRef<GPUNode | null>(null);
  useEffect(() => { agentNodeRef.current = agentNode; }, [agentNode]);

  // Keeps the open modal's agentNode (runtimeStatus, health, etc.) synced
  // with the polled nodes list (10s interval, see loadNodes below) instead
  // of freezing at whatever it was when the modal opened - otherwise
  // "Runtime is currently running" can keep showing stale state for a full
  // poll interval (or longer) after the runtime was actually stopped or
  // started, even though the list itself is refreshing in the background.
  useEffect(() => {
    if (!agentNode) return;
    const fresh = nodes.find(n => n.name === agentNode.name);
    if (fresh && fresh !== agentNode) setAgentNode(fresh);
  }, [nodes, agentNode]);

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
  // Set Manually reconfigures what Start/Stop/Restart actually execute
  // against - a wrong driver/identifier pair silently breaks runtime
  // control (e.g. a "process" driver has no log/restart supervisor), so it
  // gets the same confirm-before-persist treatment as the Start/Stop/
  // Restart actions themselves rather than applying on a single click.
  const [controlManualConfirm, setControlManualConfirm] = useState(false);
  // Clearing the control driver disables Start/Stop/Restart/Logs on this
  // node until a new driver is configured - same confirm-before-persist
  // discipline as Set Manually, since it's an equally consequential
  // one-click action on the same panel.
  const [controlClearConfirm, setControlClearConfirm] = useState(false);
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
    // TLS certificate probe/pin state (P90: this modal, not Edit Node, now
    // owns the Agent's TLS fingerprint - reset it per-node same as the rest.
    setTlsProbedFingerprint(null);
    setTlsProbeError('');
    setTlsExpectedFingerprint('');
    setPendingResetTLSPin(false);
    setPendingAgentReconfigure(false);
    setPendingRegenerateToken(false);
    setControlStatus(null);
    setControlError(null);
    setControlManualConfirm(false);
    setControlClearConfirm(false);
    setControlManualDriver('process');
    setControlManualIdentifier('');
    setControlManualStartCommand('');
    setRuntimeActionBusy(null);
    setRuntimeActionError(null);
    setLogsBusy(false);
    setLogsError(null);
    setLogsLines(null);
    if (demoMode) {
      // Demo mock nodes have no dedicated agent-scheme field - a pinned
      // tlsFingerprint is the mock's stand-in for "this demo agent is
      // HTTPS" (mockData.ts's node-3/node-4 comments), so derive from that
      // instead of hardcoding 'http' - otherwise the TLS Certificate
      // section below would never render for the two demo nodes that exist
      // specifically to demonstrate the pinned/mismatch cases.
      const demoAgentScheme: 'http' | 'https' = node.tlsFingerprint ? 'https' : 'http';
      setAgentStatus({ node: node.name, enabled: !!node.agentPresent, port: 9200, scheme: demoAgentScheme });
      setAgentPort('9200');
      setAgentUseHttps(demoAgentScheme === 'https');
      setControlStatus({
        node: node.name,
        configured: true,
        driver: 'systemd',
        identifier: 'ollama.service',
        discovered: { driver: 'systemd', identifier: 'ollama.service', evidence: ['unit ollama.service found', 'unit active'] },
      });
      return;
    }
    const targetNodeName = node.name;
    try {
      const status = await getMarborAgent(targetNodeName);
      // Same node-identity guard as handleCheckNodeHealth/handleEnableAgent -
      // a slower response for a previously-opened node can otherwise land
      // after a newer node's response and overwrite it.
      if (agentNodeRef.current?.name !== targetNodeName) return;
      setAgentStatus(status);
      setAgentPort(String(status.port || 9200));
      setAgentUseHttps(status.scheme === 'https');
    } catch (e: any) {
      if (agentNodeRef.current?.name !== targetNodeName) return;
      setAgentStatus({ node: targetNodeName, enabled: false, port: 0 });
      setAgentError(e?.message || 'Failed to fetch marbor agent status');
    }
    try {
      const control = await getNodeControl(targetNodeName);
      if (agentNodeRef.current?.name !== targetNodeName) return;
      setControlStatus(control);
      // Deliberately never pre-fill the manual Driver/Identifier fields from
      // control.discovered here: the Discovered section above already has
      // its own "Accept" button that applies driver+identifier as a
      // matched pair. Pre-filling just the identifier into the manual form
      // (whose Driver dropdown defaults to "process", unrelated to
      // whatever was discovered) let an operator submit a mismatched
      // combination without ever touching the fields themselves.
    } catch (e: any) {
      if (agentNodeRef.current?.name !== targetNodeName) return;
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
    const targetNodeName = agentNode.name;
    try {
      await acceptNodeControl(targetNodeName, controlStatus.discovered.driver, controlStatus.discovered.identifier);
      const refreshed = await getNodeControl(targetNodeName);
      if (agentNodeRef.current?.name !== targetNodeName) return;
      setControlStatus(refreshed);
    } catch (e: any) {
      if (agentNodeRef.current?.name !== targetNodeName) return;
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
    const targetNodeName = agentNode.name;
    try {
      await acceptNodeControl(targetNodeName, controlManualDriver, controlManualIdentifier.trim(), startCommand || undefined);
      const refreshed = await getNodeControl(targetNodeName);
      if (agentNodeRef.current?.name !== targetNodeName) return;
      setControlStatus(refreshed);
    } catch (e: any) {
      if (agentNodeRef.current?.name !== targetNodeName) return;
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
      // Refresh immediately instead of waiting up to 10s for the next poll
      // tick - the modal's agentNode.runtimeStatus otherwise looks stale
      // right after a successful action even though the change already
      // took effect.
      await loadNodes();
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
    const targetNodeName = agentNode.name;
    try {
      await clearNodeControl(targetNodeName);
      const refreshed = await getNodeControl(targetNodeName);
      if (agentNodeRef.current?.name !== targetNodeName) return;
      setControlStatus(refreshed);
    } catch (e: any) {
      if (agentNodeRef.current?.name !== targetNodeName) return;
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
    setControlManualConfirm(false);
    setControlClearConfirm(false);
    setRuntimeActionBusy(null);
    setRuntimeActionError(null);
    setRuntimeActionConfirm(null);
    setRuntimeActionNotice(null);
    // TLS/reconfigure/regenerate-token state is reset on open (openAgentModal
    // above), not here - nothing reads it while this modal is closed
    // (agentNode is null), so resetting it in both places was redundant.
  };

  // validateAgentPort parses/range-checks the Agent Port field, shared by
  // handleEnableAgent (fresh enable) and requestAgentReconfigure (editing an
  // already-enabled agent) so the valid range and error text can't drift
  // between the two forms. Returns null (after setting agentError) on an
  // invalid value.
  const validateAgentPort = (): number | null => {
    const port = parseInt(agentPort, 10);
    if (isNaN(port) || port <= 0 || port > 65535) {
      setAgentError('Port must be between 1 and 65535');
      return null;
    }
    return port;
  };

  const handleEnableAgent = async () => {
    if (!agentNode) return;
    const port = validateAgentPort();
    if (port === null) return;
    setAgentBusy(true);
    setAgentError(null);
    const scheme: 'http' | 'https' = agentUseHttps ? 'https' : 'http';
    if (demoMode) {
      const enrollCode = `demo-${Math.random().toString(36).slice(2, 10)}`;
      const meshUrl = window.location.origin;
      setAgentStatus({ node: agentNode.name, enabled: true, port, scheme });
      setAgentInstallCommand({
        unix: `curl -fsSL https://raw.githubusercontent.com/Anirudhx7/marbor/main/install.sh | ROLE=agent MARBOR_SERVER=${meshUrl} MARBOR_ENROLL=${enrollCode} PORT=${port} sh`,
        windows: `$env:ROLE="agent"; $env:MARBOR_SERVER="${meshUrl}"; $env:MARBOR_ENROLL="${enrollCode}"; $env:PORT="${port}"; irm https://raw.githubusercontent.com/Anirudhx7/marbor/main/install.ps1 | iex`,
      });
      setNodes(prev => prev.map(n => n.name === agentNode.name
        // A node that's already enrolled (reconfigure) keeps its real
        // platform/architecture/GPU-vendor/capabilities - only a genuinely
        // fresh enable should stamp these placeholder defaults onto it.
        ? (n.agentPresent
            ? n
            : {
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
              })
        : n));
      setAgentBusy(false);
      return;
    }
    const targetNodeName = agentNode.name;
    try {
      const res = await enableMarborAgent(targetNodeName, port, scheme);
      await loadNodes();
      // The modal may have moved to a different node while this request was
      // in flight - only apply the result if it's still relevant (same
      // guard as handleCheckNodeHealth), so a slow response for node A never
      // gets displayed as node B's freshly (re)enabled agent/install command.
      if (agentNodeRef.current?.name !== targetNodeName) return;
      setAgentStatus({ node: targetNodeName, enabled: true, port: res.port, scheme: res.scheme });
      setAgentInstallCommand({ unix: res.install_command, windows: res.install_command_windows });
    } catch (e: any) {
      if (agentNodeRef.current?.name !== targetNodeName) return;
      setAgentError(e?.message || 'Failed to enable marbor agent');
    } finally {
      // Always clear busy regardless of which node is now open - it gates
      // this modal's buttons in general, not per-node state.
      setAgentBusy(false);
    }
  };

  // requestAgentReconfigure is the click handler for the "Save Connection"
  // button on an already-enabled agent. It never calls the API directly -
  // it only validates and, if the port/scheme actually changed, opens the
  // "Reconfigure marbor agent connection?" confirm. handleEnableAgent (below)
  // is reused for the actual POST once confirmed - the backend endpoint is
  // the same for a fresh enable and a reconfigure.
  const requestAgentReconfigure = () => {
    if (!agentNode || !agentStatus) return;
    const port = validateAgentPort();
    if (port === null) return;
    const scheme: 'http' | 'https' = agentUseHttps ? 'https' : 'http';
    if (port === agentStatus.port && scheme === (agentStatus.scheme || 'http')) return;
    setAgentError(null);
    setPendingAgentReconfigure(true);
  };

  const handleRegenerateAgentToken = async () => {
    if (!agentNode) return;
    setAgentBusy(true);
    setAgentError(null);
    if (demoMode) {
      const enrollCode = `demo-${Math.random().toString(36).slice(2, 10)}`;
      const meshUrl = window.location.origin;
      const port = agentStatus?.port ?? 9200;
      setAgentInstallCommand({
        unix: `curl -fsSL https://raw.githubusercontent.com/Anirudhx7/marbor/main/install.sh | ROLE=agent MARBOR_SERVER=${meshUrl} MARBOR_ENROLL=${enrollCode} PORT=${port} sh`,
        windows: `$env:ROLE="agent"; $env:MARBOR_SERVER="${meshUrl}"; $env:MARBOR_ENROLL="${enrollCode}"; $env:PORT="${port}"; irm https://raw.githubusercontent.com/Anirudhx7/marbor/main/install.ps1 | iex`,
      });
      setAgentBusy(false);
      return;
    }
    const targetNodeName = agentNode.name;
    try {
      const res = await regenerateMarborAgentToken(targetNodeName);
      // Same node-identity guard as handleCheckNodeHealth/handleEnableAgent -
      // without it, a slow response for node A could hand node B's now-open
      // modal node A's freshly minted token/install command.
      if (agentNodeRef.current?.name !== targetNodeName) return;
      setAgentInstallCommand({ unix: res.install_command, windows: res.install_command_windows });
      setAgentStatus({ node: targetNodeName, enabled: true, port: res.port, scheme: res.scheme });
    } catch (e: any) {
      if (agentNodeRef.current?.name !== targetNodeName) return;
      setAgentError(e?.message || 'Failed to regenerate marbor agent token');
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
      await disableMarborAgent(agentToDisable);
      setAgentStatus(s => s ? { ...s, enabled: false } : s);
      setAgentInstallCommand(null);
      setAgentToDisable(null);
      await loadNodes();
    } catch (e: any) {
      setAgentError(e?.message || 'Failed to disable marbor agent');
    } finally {
      setAgentBusy(false);
    }
  };

  const copyText = (text: string) => {
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).catch(() => legacyCopyText(text));
    } else {
      legacyCopyText(text);
    }
  };

  const copyAgentCommand = (text: string, which: 'unix' | 'windows') => {
    copyText(text);
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
        scheme: newNode.useHttps ? 'https' : 'http',
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
      setNewNode({ name: '', host: '', port: '11434', gpuModel: '', runtime: 'auto', useHttps: false });
      return;
    }

    if (!isLive) return;

    const nodeData = {
      name: newNode.name,
      url: `${newNode.useHttps ? 'https' : 'http'}://${newNode.host}:${newNode.port}`,
      gpu_model: newNode.gpuModel,
      runtime: newNode.runtime,
    };

    try {
      await addNode(nodeData);
      await loadNodes();
      setActionError(null);
      setIsAddModalOpen(false);
      setNewNode({ name: '', host: '', port: '11434', gpuModel: '', runtime: 'auto', useHttps: false });
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
  const [editUseHttps, setEditUseHttps] = useState(false);
  const [editVRAM, setEditVRAM] = useState('');
  const [editVRAMUnit, setEditVRAMUnit] = useState<'MB' | 'GB'>('MB');
  const [editGPUModel, setEditGPUModel] = useState('');
  const [editRuntime, setEditRuntime] = useState('');
  const [editGPUIndices, setEditGPUIndices] = useState('');
  const [editMaxInFlight, setEditMaxInFlight] = useState('');
  const [editSaving, setEditSaving] = useState(false);
  const [editError, setEditError] = useState('');
  const [pendingPatch, setPendingPatch] = useState<{ vram_total_mb?: number; gpu_model?: string; runtime?: string; url?: string; gpu_indices?: number[]; max_in_flight?: number } | null>(null);
  // P24: TLS fingerprint probe/pin state - probing only ever populates
  // tlsProbedFingerprint for display; pinning happens exclusively via the
  // "Confirm & Pin" click (patchNode), never automatically from a probe
  // result.
  const [tlsProbing, setTlsProbing] = useState(false);
  const [tlsProbedFingerprint, setTlsProbedFingerprint] = useState<string | null>(null);
  const [tlsProbeError, setTlsProbeError] = useState('');
  const [tlsPinning, setTlsPinning] = useState(false);
  const [pendingResetTLSPin, setPendingResetTLSPin] = useState(false);
  const [tlsResetting, setTlsResetting] = useState(false);
  const [tlsExpectedFingerprint, setTlsExpectedFingerprint] = useState('');
  const [tlsStatusCmdCopied, setTlsStatusCmdCopied] = useState(false);
  // Normalized for comparison only - a pasted value differing merely in case
  // or wrapped whitespace/newlines (common with terminal copy) must not read
  // as a mismatch; the fingerprint pinned is always tlsProbedFingerprint
  // verbatim (handleConfirmAndPinTLS), never the typed value.
  const normalizeFingerprint = (fp: string) => fp.replace(/\s+/g, '').toLowerCase();
  const tlsExpectedFingerprintNormalized = normalizeFingerprint(tlsExpectedFingerprint);
  const tlsProbedFingerprintNormalized = normalizeFingerprint(tlsProbedFingerprint || '');

  const openEditModal = (node: GPUNode) => {
    setEditNode(node);
    setEditHost(node.host ?? '');
    setEditPort(node.port ? String(node.port) : '');
    setEditUseHttps(node.scheme === 'https');
    setEditVRAM(node.vramTotalMB > 0 ? String(node.vramTotalMB) : '');
    setEditVRAMUnit('MB');
    setEditGPUModel(node.gpuModel ?? '');
    setEditRuntime(node.runtime || 'ollama');
    setEditGPUIndices((node.gpuIndices ?? []).join(', '));
    setEditMaxInFlight(node.maxInFlight && node.maxInFlight > 0 ? String(node.maxInFlight) : '');
    setEditError('');
  };

  // Keeps the open Edit Node modal synced with the polled nodes list, same
  // reasoning as the agentNode sync effect above - editNode otherwise stays
  // frozen at whatever it was when the modal opened, so buildPatch's
  // "changed from editNode" comparisons silently revert a field that
  // changed server-side (another admin's edit, auto-discovery) while this
  // modal stayed open with that field left untouched. A field is only
  // refreshed when its current form value still equals what openEditModal
  // would have derived from the PRIOR editNode snapshot - i.e. the operator
  // hasn't touched it since the last sync - so an in-progress edit is never
  // clobbered.
  useEffect(() => {
    if (!editNode) return;
    const fresh = nodes.find(n => n.name === editNode.name);
    if (!fresh || fresh === editNode) return;
    const prev = editNode;
    if (editHost === (prev.host ?? '')) setEditHost(fresh.host ?? '');
    if (editPort === (prev.port ? String(prev.port) : '')) setEditPort(fresh.port ? String(fresh.port) : '');
    if (editUseHttps === (prev.scheme === 'https')) setEditUseHttps(fresh.scheme === 'https');
    if (editVRAM === (prev.vramTotalMB > 0 ? String(prev.vramTotalMB) : '')) {
      setEditVRAM(fresh.vramTotalMB > 0 ? String(fresh.vramTotalMB) : '');
    }
    if (editGPUModel === (prev.gpuModel ?? '')) setEditGPUModel(fresh.gpuModel ?? '');
    if (editRuntime === (prev.runtime || 'ollama')) setEditRuntime(fresh.runtime || 'ollama');
    if (editGPUIndices === (prev.gpuIndices ?? []).join(', ')) {
      setEditGPUIndices((fresh.gpuIndices ?? []).join(', '));
    }
    if (editMaxInFlight === (prev.maxInFlight && prev.maxInFlight > 0 ? String(prev.maxInFlight) : '')) {
      setEditMaxInFlight(fresh.maxInFlight && fresh.maxInFlight > 0 ? String(fresh.maxInFlight) : '');
    }
    setEditNode(fresh);
  }, [nodes, editNode, editHost, editPort, editUseHttps, editVRAM, editGPUModel, editRuntime, editGPUIndices, editMaxInFlight]);

  const buildPatch = (): { vram_total_mb?: number; gpu_model?: string; runtime?: string; url?: string; gpu_indices?: number[]; max_in_flight?: number } | 'invalid' | null => {
    if (!editNode) return null;
    const patch: { vram_total_mb?: number; gpu_model?: string; runtime?: string; url?: string; gpu_indices?: number[]; max_in_flight?: number } = {};
    if (editVRAM.trim() !== '') {
      const v = parseFloat(editVRAM);
      if (isNaN(v) || v < 0) { setEditError(`VRAM must be a non-negative number (${editVRAMUnit})`); return 'invalid'; }
      patch.vram_total_mb = Math.round(editVRAMUnit === 'GB' ? v * 1024 : v);
    }
    if (editGPUModel.trim() !== '') patch.gpu_model = editGPUModel.trim();
    const priorMaxInFlight = editNode.maxInFlight ?? 0;
    const newMaxInFlight = editMaxInFlight.trim() === '' ? 0 : parseInt(editMaxInFlight, 10);
    if (editMaxInFlight.trim() !== '' && (isNaN(newMaxInFlight) || newMaxInFlight < 0)) {
      setEditError('Max in-flight must be a non-negative integer (0 = use global default)');
      return 'invalid';
    }
    if (newMaxInFlight !== priorMaxInFlight) patch.max_in_flight = newMaxInFlight;
    if (editRuntime && editRuntime !== (editNode.runtime || 'ollama')) patch.runtime = editRuntime;
    const priorIndices = editNode.gpuIndices ?? [];
    let newIndices: number[] = [];
    if (editGPUIndices.trim() !== '') {
      const parts = editGPUIndices.split(',').map(s => s.trim()).filter(s => s !== '');
      newIndices = parts.map(s => parseInt(s, 10));
      if (newIndices.some(n => isNaN(n) || n < 0)) {
        setEditError('GPU indices must be a comma-separated list of non-negative numbers (e.g. "0, 1")');
        return 'invalid';
      }
    }
    const indicesChanged = newIndices.length !== priorIndices.length || newIndices.some((n, i) => n !== priorIndices[i]);
    if (indicesChanged) patch.gpu_indices = newIndices;
    const hostChanged = editHost.trim() !== '' && editHost.trim() !== (editNode.host ?? '');
    const portChanged = editPort.trim() !== '' && editPort.trim() !== String(editNode.port ?? '');
    const schemeChanged = editUseHttps !== (editNode.scheme === 'https');
    if (hostChanged || portChanged || schemeChanged) {
      const host = editHost.trim() || editNode.host;
      const port = editPort.trim() || String(editNode.port);
      if (!host || !port || isNaN(parseInt(port, 10))) { setEditError('Host and port must both be set'); return 'invalid'; }
      patch.url = `${editUseHttps ? 'https' : 'http'}://${host}:${port}`;
    }
    if (Object.keys(patch).length === 0) return null;
    return patch;
  };

  // mergeNodePatch applies a patch's fields onto a node the same way the
  // backend would, so the modal (which holds its own editNode snapshot, not
  // a live reference into `nodes`) can reflect a save without needing to
  // close and reopen.
  const mergeNodePatch = (n: GPUNode, patch: { vram_total_mb?: number; gpu_model?: string; runtime?: string; url?: string; gpu_indices?: number[]; max_in_flight?: number; tls_fingerprint?: string }): GPUNode => {
    const scheme = patch.url ? (patch.url.startsWith('https://') ? 'https' as const : 'http' as const) : undefined;
    return {
      ...n,
      vramTotalMB: patch.vram_total_mb ?? n.vramTotalMB,
      gpuModel: patch.gpu_model ?? n.gpuModel,
      runtime: patch.runtime ?? n.runtime,
      gpuIndices: patch.gpu_indices ?? n.gpuIndices,
      maxInFlight: patch.max_in_flight ?? n.maxInFlight,
      scheme: scheme ?? n.scheme,
      tlsFingerprint: patch.tls_fingerprint !== undefined ? (patch.tls_fingerprint || undefined) : n.tlsFingerprint,
      tlsFingerprintMismatch: patch.tls_fingerprint !== undefined ? false : n.tlsFingerprintMismatch,
    };
  };

  // sendNodePatch performs the demo/live patch application for a single
  // node (demo: merge into `nodes` in place; live: PATCH + reload) - shared
  // by applyPatch (Runtime edits, editNode-scoped) and applyAgentTLSPatch
  // (Agent TLS pin/reset, agentNode-scoped) so a future fix to this shared
  // core doesn't have to be applied twice.
  const sendNodePatch = async (nodeName: string, patch: { vram_total_mb?: number; gpu_model?: string; runtime?: string; url?: string; gpu_indices?: number[]; max_in_flight?: number; tls_fingerprint?: string }) => {
    if (demoMode) {
      setNodes(prev => prev.map(n => n.name === nodeName ? mergeNodePatch(n, patch) : n));
      return;
    }
    if (!isLive) return;
    await patchNode(nodeName, patch);
    await loadNodes();
  };

  // closeOnSuccess defaults to true for the main "Save Changes" submit. The
  // TLS probe/pin and reset actions pass false - they're inline actions
  // inside the still-open modal, not a form submit, so closing on success
  // would yank the operator out mid-workflow (they still may want to probe,
  // paste-verify, or reset again).
  const applyPatch = async (patch: { vram_total_mb?: number; gpu_model?: string; runtime?: string; url?: string; gpu_indices?: number[]; max_in_flight?: number; tls_fingerprint?: string }, closeOnSuccess = true) => {
    if (!editNode) return;
    if (demoMode) {
      await sendNodePatch(editNode.name, patch);
      if (closeOnSuccess) {
        setEditNode(null);
      } else {
        setEditNode(prev => prev ? mergeNodePatch(prev, patch) : prev);
      }
      return;
    }

    if (!isLive) return;
    setEditSaving(true);
    setEditError('');
    try {
      await sendNodePatch(editNode.name, patch);
      if (closeOnSuccess) {
        setEditNode(null);
      } else {
        setEditNode(prev => prev ? mergeNodePatch(prev, patch) : prev);
      }
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

  // handleProbeTLS retrieves the node's currently-presented certificate
  // fingerprint for display only (P24 spec section 2) - it never pins
  // anything. Demo mode has no real node to dial, so it surfaces the
  // already-known demo fingerprint (or a clearly-fake placeholder) instead
  // of attempting a network call. Operates on agentNode (Manage marbor agent
  // modal), NOT editNode - TLS pinning secures the Agent connection, not
  // the runtime URL, so it moved out of the Edit Node (Runtime) modal (P90).
  const handleProbeTLS = async () => {
    if (!agentNode) return;
    setTlsProbeError('');
    setTlsExpectedFingerprint('');
    if (demoMode) {
      setTlsProbedFingerprint(agentNode.tlsFingerprint || 'SHA256:' + '00'.repeat(32));
      return;
    }
    setTlsProbing(true);
    const targetNodeName = agentNode.name;
    try {
      const result = await probeNodeTLS(targetNodeName);
      // Same node-identity guard as handleCheckNodeHealth - a slow probe for
      // node A must not land in node B's now-open modal.
      if (agentNodeRef.current?.name !== targetNodeName) return;
      setTlsProbedFingerprint(result.fingerprint);
    } catch (e: any) {
      if (agentNodeRef.current?.name !== targetNodeName) return;
      setTlsProbeError(e?.message || 'Failed to probe TLS certificate');
    } finally {
      setTlsProbing(false);
    }
  };

  // applyAgentTLSPatch pins/resets this node's Agent TLS fingerprint via the
  // same generic node PATCH endpoint used by Runtime edits (admin.go
  // handlePatchNode), but sends ONLY tls_fingerprint - never bundled with
  // any Runtime field (url/gpu_model/etc.) - so it can never accidentally
  // change the runtime address while pinning a certificate.
  const applyAgentTLSPatch = async (patch: { tls_fingerprint: string }, onSuccess?: () => void) => {
    if (!agentNode) return;
    if (demoMode) {
      await sendNodePatch(agentNode.name, patch);
      setAgentNode(prev => prev ? mergeNodePatch(prev, patch) : prev);
      onSuccess?.();
      return;
    }
    if (!isLive) return;
    const targetNodeName = agentNode.name;
    try {
      await sendNodePatch(targetNodeName, patch);
      // Same node-identity guard as handleCheckNodeHealth - a slow pin/reset
      // for node A must not be stamped onto node B's now-open modal.
      if (agentNodeRef.current?.name !== targetNodeName) return;
      setAgentNode(prev => prev ? mergeNodePatch(prev, patch) : prev);
      onSuccess?.();
    } catch (e: any) {
      if (agentNodeRef.current?.name !== targetNodeName) return;
      setAgentError(e?.message || 'Failed to update TLS pin');
    }
  };

  // handleConfirmAndPinTLS is the ONLY code path that ever pins a
  // fingerprint - it only runs when the operator clicks "Confirm & Pin"
  // after reviewing a probed value, never automatically from a probe
  // result (P24 spec section 1/2).
  const handleConfirmAndPinTLS = async () => {
    if (!agentNode || !tlsProbedFingerprint) return;
    setTlsPinning(true);
    setAgentError(null);
    await applyAgentTLSPatch({ tls_fingerprint: tlsProbedFingerprint }, () => {
      setTlsProbedFingerprint(null);
      setTlsExpectedFingerprint('');
    });
    setTlsPinning(false);
  };

  // handleResetTLSPin clears an existing pin (R10: destructive - disrupts
  // this node's established trust state - gated by the confirm modal below,
  // same pattern as the Disable-Agent/runtime-action confirm modals in this
  // file). Runs only from that modal's confirm click, never directly from
  // the Reset button.
  const handleResetTLSPin = async () => {
    if (!agentNode) return;
    setTlsResetting(true);
    setAgentError(null);
    await applyAgentTLSPatch({ tls_fingerprint: '' }, () => setPendingResetTLSPin(false));
    setTlsResetting(false);
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
            <label className="flex items-center gap-2 mt-2 cursor-pointer">
              <input
                type="checkbox"
                checked={newNode.useHttps}
                onChange={(e) => setNewNode({ ...newNode, useHttps: e.target.checked })}
                className="rounded border-border bg-background text-primary focus:ring-primary/20"
              />
              <span className="text-xs text-muted-foreground">Use HTTPS for this node's inference runtime endpoint (only if the runtime itself - Ollama, vLLM, etc. - serves TLS on this port; most don't by default). This is separate from the marbor agent's own HTTPS setting, which is configured after adding the node.</span>
            </label>
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
            Changing host or port re-points marbor at a different address and resets this node's live health/warm state - you'll be asked to confirm.
          </p>
          <label className="flex items-center gap-2 -mt-2 cursor-pointer">
            <input
              type="checkbox"
              checked={editUseHttps}
              onChange={(e) => setEditUseHttps(e.target.checked)}
              className="rounded border-border bg-background text-primary focus:ring-primary/20"
            />
            <span className="text-xs text-muted-foreground">Use HTTPS for this node's inference runtime endpoint (only if the runtime itself - Ollama, vLLM, etc. - serves TLS on this port; most don't by default). This is separate from the marbor agent's own HTTPS setting in the Agent panel.</span>
          </label>
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
              Declared GPU Indices
            </label>
            <input
              type="text"
              value={editGPUIndices}
              onChange={(e) => setEditGPUIndices(e.target.value)}
              placeholder="e.g., 0, 1 (leave blank if undeclared)"
              className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
            />
            <p className="text-xs text-muted-foreground mt-1">
              Only needed when this node shares a physical host with another node (e.g. two runtimes on one box, each pinned to a different GPU via CUDA_VISIBLE_DEVICES). Declares which physical GPU indices this node actually uses, so the Model Advisor doesn't size it against the whole host's combined VRAM. Leave blank to keep host-level sizing.
            </p>
          </div>
          <div>
            <label className="block text-sm font-medium text-muted-foreground mb-1.5">
              VRAM Total Override
            </label>
            <div className="flex gap-2">
              <input
                type="number"
                min="0"
                value={editVRAM}
                onChange={(e) => setEditVRAM(e.target.value)}
                placeholder={editVRAMUnit === 'GB' ? 'e.g., 24' : 'e.g., 24576'}
                className="flex-1 min-w-0 px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
              />
              <div className="w-24 shrink-0">
                <CustomSelect
                  value={editVRAMUnit}
                  onChange={(v) => {
                    const unit = v as 'MB' | 'GB';
                    if (unit !== editVRAMUnit) {
                      const n = parseFloat(editVRAM);
                      if (!isNaN(n)) {
                        setEditVRAM(String(unit === 'GB' ? n / 1024 : Math.round(n * 1024)));
                      }
                    }
                    setEditVRAMUnit(unit);
                  }}
                  options={[
                    { value: 'MB', label: 'MB' },
                    { value: 'GB', label: 'GB' },
                  ]}
                />
              </div>
            </div>
            <p className="text-xs text-muted-foreground mt-1">
              Only applied when no live GPU telemetry exists (nvidia-smi, ROCm, etc). When applied, it directly drives placement decisions and Model Advisor fit checks.
            </p>
          </div>
          <div>
            <label className="block text-sm font-medium text-muted-foreground mb-1.5">
              Max In-Flight Override
            </label>
            <input
              type="number"
              min="0"
              value={editMaxInFlight}
              onChange={(e) => setEditMaxInFlight(e.target.value)}
              placeholder="e.g., 4 (leave blank to use the global default)"
              className="w-full px-3 py-2 bg-secondary border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
            />
            <p className="text-xs text-muted-foreground mt-1">
              A node at or above this many in-flight requests is shed immediately (failover/cloud/503) instead of queued. Leave blank to use Settings &rarr; Routing's global Max In-Flight Per Node.
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
            Marbor will re-point at this new address immediately. This node's live health and warm-model state reset, since it's now treated as a different physical backend.
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

      {/* Reset TLS Pin Confirmation Modal (R10: destructive - disrupts an established trust state) */}
      <Modal
        isOpen={pendingResetTLSPin}
        onClose={() => setPendingResetTLSPin(false)}
        title="Reset TLS Pin"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Clear the pinned TLS certificate fingerprint for <span className="text-foreground font-semibold">{agentNode?.name}</span>?
          </p>
          <p className="text-xs text-muted-foreground">
            This node reverts to requiring re-enrollment: marbor will refuse to connect over HTTPS again until you probe and confirm a fingerprint for it. Not reversible from here - you'll need to re-pin afterward. In-flight requests are not affected.
          </p>
          {agentError && (
            <p className="text-sm text-destructive">{agentError}</p>
          )}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setPendingResetTLSPin(false)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={handleResetTLSPin}
              disabled={tlsResetting}
              className="px-4 py-2 bg-destructive hover:bg-destructive/90 disabled:opacity-50 disabled:cursor-not-allowed text-destructive-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              {tlsResetting ? 'Resetting...' : 'Reset Pin'}
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
            Are you sure you want to remove the node <span className="text-foreground font-semibold">{nodeToDelete}</span> from the marbor?
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
              ? 'The predictive engine will not warm new models onto this node until re-enabled or marbor restarts. Live traffic and warm-state routing are unaffected.'
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

      {/* Manage marbor agent Modal */}
      <Modal
        isOpen={agentNode !== null}
        onClose={closeAgentModal}
        title={`marbor agent: ${agentNode?.name ?? ''}`}
        maxWidth="2xl"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            The marbor agent is an optional process you run on this GPU node to report CPU usage, fan speed, RAM usage, and free disk space back to the marbor. Everything else (VRAM, temperature, power) is already collected without it.
          </p>

          {agentNode && (() => {
            const siblings = nodes.filter((n) => n.name !== agentNode.name && n.host === agentNode.host);
            if (siblings.length === 0) return null;
            return (
              <p className="text-xs text-muted-foreground bg-secondary/50 rounded-lg px-3 py-2">
                Shared with {siblings.map((n) => n.name).join(', ')} - this physical machine runs
                more than one node row, so enabling/disabling/regenerating the agent here applies
                to all of them (one agent process per host, not per runtime).
              </p>
            );
          })()}

          {agentError && (
            <p className="text-sm text-destructive">{agentError}</p>
          )}

          <div className="space-y-2">
            <div className="flex flex-wrap gap-3">
              <button
                onClick={handleCheckNodeHealth}
                disabled={healthCheckBusy}
                title="Run a live health check against this node's inference runtime right now, instead of waiting for the next automatic poll - works whether or not a marbor agent is installed"
                className="px-4 py-2 bg-secondary hover:bg-secondary/80 disabled:opacity-50 disabled:cursor-not-allowed text-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
              >
                {healthCheckBusy ? 'Checking...' : 'Health Check'}
              </button>
              {agentStatus?.enabled && (
                <>
                  <button
                    onClick={() => setPendingRegenerateToken(true)}
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
                </>
              )}
            </div>
            {healthCheckResult && (
              <p className={`text-xs ${healthCheckResult.ok ? 'text-success' : 'text-destructive'}`}>
                <span className="font-medium">Health check result:</span>{' '}
                {healthCheckResult.ok
                  ? `up${healthCheckResult.latencyMs != null ? ` (${healthCheckResult.latencyMs}ms)` : ''}`
                  : `down${healthCheckResult.error ? ` - ${healthCheckResult.error}` : ''}`}
              </p>
            )}
          </div>

          {agentNode && !agentStatus && (
            <p className="text-sm text-muted-foreground">Loading Agent status...</p>
          )}

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
                  Port the agent process listens on for marbor to poll (default 9200).
                </p>
              </div>
              <div className="flex items-center gap-2">
                <input
                  id="agent-use-https"
                  type="checkbox"
                  checked={agentUseHttps}
                  onChange={(e) => setAgentUseHttps(e.target.checked)}
                  className="h-4 w-4 rounded border-border"
                />
                <label htmlFor="agent-use-https" className="text-sm text-foreground">
                  Use HTTPS for the Agent connection
                </label>
              </div>
              <p className="text-xs text-muted-foreground -mt-2">
                Encrypts the marbor's connection to this node's Agent only - this node's inference
                runtime endpoint is unaffected and keeps its own URL/scheme. Once enabled, this
                panel shows a "Probe &amp; Pin" control to verify and pin the Agent's certificate
                fingerprint (required - an unpinned Agent connection will be rejected).
              </p>
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
              {/* Agent Connection - reconfigure port/scheme on an already-enabled
                  agent. Changing this NEVER touches this node's runtime URL
                  (node.url/scheme) - only this Agent's own port/scheme (P90).
                  The enabled/port/scheme status lives ONLY in this card's
                  header pill + its own editable fields below - no separate
                  "Enabled on port X (scheme)" line, since that was just a
                  read-only restatement of the same two values shown (and
                  editable) right underneath it. */}
              <div className="space-y-2 p-3 bg-secondary/40 border border-border rounded-lg">
                {/* The marbor REJECTS an HTTPS Agent connection with no pinned
                    (or a mismatched) fingerprint - see the TLS Certificate
                    card below - so this pill must not read "Enabled" in
                    plain green when that's true, or it contradicts the
                    "connection is rejected" message directly underneath it. */}
                {(() => {
                  const httpsUnusable = agentStatus.scheme === 'https'
                    && (!agentNode?.tlsFingerprint || agentNode?.tlsFingerprintMismatch);
                  const label = agentStatus.scheme !== 'https'
                    ? 'Enabled · HTTP'
                    : agentNode?.tlsFingerprintMismatch
                      ? 'Enabled · HTTPS (cert mismatch)'
                      : httpsUnusable
                        ? 'Enabled · HTTPS (unpinned)'
                        : 'Enabled · HTTPS';
                  return (
                    <div className="flex items-center justify-between">
                      <p className="text-sm font-medium text-foreground">Agent Connection</p>
                      <span className={`flex items-center gap-1.5 text-xs font-medium ${httpsUnusable ? 'text-amber-600 dark:text-amber-400' : 'text-success'}`}>
                        <span className={`w-2 h-2 rounded-full ${httpsUnusable ? 'bg-amber-500' : 'bg-success'}`} />
                        {label}
                      </span>
                    </div>
                  );
                })()}
                <div className="flex flex-wrap items-end gap-3">
                  <div className="flex-1 min-w-[100px]">
                    <label className="block text-xs font-medium text-muted-foreground mb-1">
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
                  </div>
                  <label htmlFor="agent-reconfigure-https" className="flex items-center gap-2 h-[38px] cursor-pointer shrink-0">
                    <input
                      id="agent-reconfigure-https"
                      type="checkbox"
                      checked={agentUseHttps}
                      onChange={(e) => setAgentUseHttps(e.target.checked)}
                      className="h-4 w-4 rounded border-border"
                    />
                    <span className="text-sm text-foreground">HTTPS</span>
                  </label>
                  <button
                    type="button"
                    onClick={requestAgentReconfigure}
                    disabled={agentBusy}
                    className="h-[38px] px-3 bg-secondary hover:bg-secondary/80 disabled:opacity-50 disabled:cursor-not-allowed text-foreground text-xs font-medium rounded-lg border border-border transition-colors shrink-0"
                  >
                    Save Connection
                  </button>
                </div>
              </div>

              {/* TLS Certificate - pins/probes THIS Agent's own certificate.
                  Reads only agentStatus.scheme (never node.scheme/node.url) so
                  it can never be confused with, or derived from, the Runtime
                  HTTPS setting in the Edit Node modal (P90 hard invariant).
                  Same bordered-card treatment as Agent Connection above. */}
              <div className="space-y-2 p-3 bg-secondary/40 border border-border rounded-lg">
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                  TLS Certificate Fingerprint
                </label>
                {!agentUseHttps ? (
                  <p className="text-xs text-muted-foreground">
                    Enable HTTPS above to pin this Agent's certificate. Pinning secures the
                    marbor-to-Agent connection only - it is unrelated to this node's runtime
                    URL/scheme in the Edit Node modal.
                  </p>
                ) : (
                  <div className="space-y-2">
                    {agentNode?.tlsFingerprintMismatch && (
                      <p className="text-xs font-medium text-destructive">
                        Mismatch: the node's agent is presenting a different certificate than what's pinned. Connections are being refused. If you rotated the certificate intentionally (e.g. after "agent service regen-cert"), probe below and re-confirm.
                      </p>
                    )}
                    {agentNode?.tlsFingerprint ? (
                      <p className="text-xs text-muted-foreground font-mono break-all">
                        Pinned: {agentNode.tlsFingerprint}
                      </p>
                    ) : (
                      <p className="text-xs text-muted-foreground">Not pinned yet - an unpinned Agent HTTPS connection is rejected.</p>
                    )}
                    <div className="flex items-center gap-2">
                      <button
                        type="button"
                        onClick={handleProbeTLS}
                        disabled={tlsProbing}
                        className="px-3 py-1.5 bg-secondary hover:bg-secondary/80 disabled:opacity-50 disabled:cursor-not-allowed text-foreground text-xs font-medium rounded-lg border border-border transition-colors"
                      >
                        {tlsProbing ? 'Probing...' : 'Probe Certificate'}
                      </button>
                      {agentNode?.tlsFingerprint && (
                        <button
                          type="button"
                          onClick={() => setPendingResetTLSPin(true)}
                          className="px-3 py-1.5 bg-secondary hover:bg-destructive/10 hover:text-destructive disabled:opacity-50 disabled:cursor-not-allowed text-foreground text-xs font-medium rounded-lg border border-border transition-colors"
                        >
                          Reset Pin
                        </button>
                      )}
                    </div>
                    {tlsProbeError && (
                      <p className="text-xs text-destructive">{tlsProbeError}</p>
                    )}
                    {tlsProbedFingerprint && (
                      <div className="p-3 bg-secondary border border-border rounded-lg space-y-2">
                        <p className="text-xs text-muted-foreground">
                          Node presented certificate fingerprint:
                        </p>
                        <p className="text-xs font-mono break-all text-foreground">{tlsProbedFingerprint}</p>
                        <p className="text-xs text-muted-foreground">
                          Run this on the node and confirm it matches before pinning:
                        </p>
                        <div className="flex items-center gap-2">
                          <code className="flex-1 min-w-0 font-mono text-xs bg-background border border-border rounded-lg px-3 py-2 break-all text-foreground select-all">
                            marbor-agent service status
                          </code>
                          <button
                            type="button"
                            onClick={() => {
                              copyText('marbor-agent service status');
                              setTlsStatusCmdCopied(true);
                              setTimeout(() => setTlsStatusCmdCopied(false), 2000);
                            }}
                            className="shrink-0 flex items-center gap-1.5 px-3 py-2 text-xs font-medium bg-success/20 hover:bg-success/30 text-success rounded-lg transition-colors"
                          >
                            <Copy className="w-3.5 h-3.5" />
                            {tlsStatusCmdCopied ? 'Copied!' : 'Copy'}
                          </button>
                        </div>
                        <input
                          type="text"
                          value={tlsExpectedFingerprint}
                          onChange={(e) => setTlsExpectedFingerprint(e.target.value)}
                          placeholder="Optional: paste the fingerprint printed on the node to verify"
                          className="w-full px-2 py-1.5 bg-background border border-border rounded-lg text-xs font-mono text-foreground placeholder:text-muted-foreground placeholder:font-sans"
                        />
                        {tlsExpectedFingerprintNormalized !== '' && (
                          tlsExpectedFingerprintNormalized === tlsProbedFingerprintNormalized ? (
                            <p className="text-xs font-medium text-green-600 dark:text-green-500">Matches - safe to pin.</p>
                          ) : (
                            <p className="text-xs font-medium text-destructive">Doesn't match the probed fingerprint - do not pin. Re-check the node.</p>
                          )
                        )}
                        <button
                          type="button"
                          onClick={handleConfirmAndPinTLS}
                          disabled={tlsPinning || (tlsExpectedFingerprintNormalized !== '' && tlsExpectedFingerprintNormalized !== tlsProbedFingerprintNormalized)}
                          className="px-3 py-1.5 bg-primary hover:bg-primary/90 disabled:opacity-50 disabled:cursor-not-allowed text-primary-foreground text-xs font-medium rounded-lg transition-colors shadow-sm"
                        >
                          {tlsPinning ? 'Pinning...' : 'Confirm & Pin'}
                        </button>
                      </div>
                    )}
                  </div>
                )}
              </div>

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
                      onClick={() => setControlClearConfirm(true)}
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
                  onClick={() => setControlManualConfirm(true)}
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

      {/* marbor agent Install Command Modal - separate dialog so the enroll token/commands don't get buried below Runtime Control */}
      <Modal
        isOpen={agentInstallCommand !== null}
        onClose={() => setAgentInstallCommand(null)}
        title="marbor agent Install Command"
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

      {/* Disable marbor agent Confirmation Modal */}
      <Modal
        isOpen={agentToDisable !== null}
        onClose={() => setAgentToDisable(null)}
        title="Disable marbor agent"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Are you sure you want to disable the marbor agent on <span className="text-foreground font-semibold">{agentToDisable}</span>?
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

      {/* Reconfigure marbor agent Connection Confirmation Modal - only for
          changing port/scheme on an ALREADY-enabled agent (P90). Text is
          scoped entirely to "Agent connection" and never says "node address"
          so it can't be mistaken for the Runtime "Change Node Address" modal
          above - and confirming here never touches this node's runtime URL. */}
      <Modal
        isOpen={pendingAgentReconfigure}
        onClose={() => setPendingAgentReconfigure(false)}
        title="Reconfigure marbor agent connection?"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Change the Agent connection for <span className="text-foreground font-semibold">{agentNode?.name}</span> to{' '}
            <span className="text-foreground font-semibold">{agentUseHttps ? 'https' : 'http'}://{agentNode?.host}:{agentPort}</span>?
          </p>
          <p className="text-xs text-muted-foreground">
            Marbor will use this address to reach the Agent going forward. This does not change this node's inference runtime endpoint. This also issues a new Agent token and invalidates the current one - you'll need to run the install command shown next on the node to re-enroll it before marbor can reach it again.
          </p>
          {agentError && (
            <p className="text-sm text-destructive">{agentError}</p>
          )}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setPendingAgentReconfigure(false)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={async () => {
                setPendingAgentReconfigure(false);
                // A probed-but-unpinned fingerprint was probed against the
                // pre-reconfigure connection - clear it so it can't be
                // mistaken for (and pinned as) a fingerprint of the new one.
                setTlsProbedFingerprint(null);
                setTlsExpectedFingerprint('');
                setTlsProbeError('');
                await handleEnableAgent();
              }}
              disabled={agentBusy}
              className="px-4 py-2 bg-primary hover:bg-primary/90 disabled:opacity-50 disabled:cursor-not-allowed text-primary-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              {agentBusy ? 'Applying...' : 'Confirm'}
            </button>
          </div>
        </div>
      </Modal>

      {/* Regenerate marbor agent Token Confirmation Modal (R10: swaps the live
          token immediately - the currently-running agent process is
          rejected on its very next poll until reinstalled with the new
          command shown after confirming). */}
      <Modal
        isOpen={pendingRegenerateToken}
        onClose={() => setPendingRegenerateToken(false)}
        title="Regenerate marbor agent token?"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Regenerate the Agent token for <span className="text-foreground font-semibold">{agentNode?.name}</span>?
          </p>
          <p className="text-xs text-muted-foreground">
            This immediately invalidates the currently-running agent's token - marbor will refuse its connection until you run the new install command (shown next) on the node to re-enroll it. This does not change this node's inference runtime endpoint.
          </p>
          {agentError && (
            <p className="text-sm text-destructive">{agentError}</p>
          )}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setPendingRegenerateToken(false)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={async () => {
                setPendingRegenerateToken(false);
                await handleRegenerateAgentToken();
              }}
              disabled={agentBusy}
              className="px-4 py-2 bg-destructive hover:bg-destructive/90 disabled:opacity-50 disabled:cursor-not-allowed text-destructive-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              {agentBusy ? 'Regenerating...' : 'Regenerate Token'}
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
            {runtimeActionConfirm === 'stop' && 'Disruptive and immediate: any in-flight requests on this node fail right now, and all warm/loaded models are evicted from VRAM. Marbor routes new requests to other nodes, but this node serves nothing until you start it again.'}
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

      {/* Set Manually Confirmation Modal - reconfiguring the control driver
          silently changes what Start/Stop/Restart execute against, so it
          gets a review step instead of applying on the first click. */}
      <Modal
        isOpen={controlManualConfirm}
        onClose={() => setControlManualConfirm(false)}
        title="Set Control Driver Manually"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Set the control driver for <span className="text-foreground font-semibold">{agentNode?.name}</span> to:
          </p>
          <div className="bg-background border border-border rounded-lg px-3 py-2 space-y-1">
            <p className="text-sm text-foreground">
              Driver: <span className="font-mono font-semibold">{controlManualDriver}</span>
            </p>
            <p className="text-sm text-foreground">
              Identifier: <span className="font-mono font-semibold">{controlManualIdentifier}</span>
            </p>
            {controlManualDriver === 'process' && controlManualStartCommand.trim() && (
              <p className="text-sm text-foreground">
                Start command: <span className="font-mono font-semibold">{controlManualStartCommand}</span>
              </p>
            )}
          </div>
          <p className="text-xs text-muted-foreground">
            This replaces any previously configured driver. Start/Stop/Restart/Logs will run against this driver and identifier from now on - double-check they match what actually runs the inference runtime on this node, not the marbor agent itself.
          </p>
          {controlError && <p className="text-sm text-destructive">{controlError}</p>}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setControlManualConfirm(false)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={async () => { setControlManualConfirm(false); await acceptManualControl(); }}
              disabled={controlBusy}
              className="px-4 py-2 bg-secondary hover:bg-secondary/80 disabled:opacity-50 disabled:cursor-not-allowed text-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              {controlBusy ? 'Setting...' : 'Set Manually'}
            </button>
          </div>
        </div>
      </Modal>

      {/* Clear Control Driver Confirmation Modal - clearing disables
          Start/Stop/Restart/Logs on this node until a new driver is
          configured, same review-before-persist discipline as Set
          Manually. */}
      <Modal
        isOpen={controlClearConfirm}
        onClose={() => setControlClearConfirm(false)}
        title="Clear Control Driver"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Clear the configured control driver (<span className="font-mono text-foreground">{controlStatus?.driver}</span> / <span className="font-mono text-foreground">{controlStatus?.identifier}</span>) for <span className="text-foreground font-semibold">{agentNode?.name}</span>?
          </p>
          <p className="text-xs text-muted-foreground">
            Start/Stop/Restart/Logs will stop working on this node until a driver is configured again (via Accept or Set Manually). This does not stop the runtime itself - it only removes the marbor's ability to control it.
          </p>
          {controlError && <p className="text-sm text-destructive">{controlError}</p>}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setControlClearConfirm(false)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={async () => { setControlClearConfirm(false); await clearControl(); }}
              disabled={controlBusy}
              className="px-4 py-2 bg-destructive hover:bg-destructive/90 disabled:opacity-50 disabled:cursor-not-allowed text-destructive-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              {controlBusy ? 'Clearing...' : 'Clear'}
            </button>
          </div>
        </div>
      </Modal>

      {/* Runtime Logs Modal (P58) - a pure read, no confirm needed */}
      <Modal
        isOpen={logsModalOpen}
        onClose={() => setLogsModalOpen(false)}
        title={`Runtime Logs - ${agentNode?.name ?? ''}`}
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
