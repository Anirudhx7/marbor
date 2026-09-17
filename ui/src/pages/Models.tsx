import { useState, useEffect } from 'react';
import { Link, useLocation, useSearchParams } from 'react-router-dom';
import { Package, Download, Settings2, Trash2, AlertTriangle, Layers, Flame, Copy, CheckCircle2, ArrowUpRight } from 'lucide-react';
import { StatusDot } from '../components/StatusDot';
import { Badge } from '../components/Badge';
import { SearchInput } from '../components/SearchInput';
import { EmptyState } from '../components/EmptyState';
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

// Replica-aware instance count. Every warm node of one resolved replica
// (same head) is ONE sharded instance, never N duplicate copies - each
// remaining warm node is its own instance. Warm nodes with no topology entry
// (standalone, unresolved, or a node the list fetch missed) each count as
// their own instance, matching the old behavior for fleets without
// replicas. Extra warm reports beyond the listed nodes (warm_count above the
// listed warm chips) each count as one more instance so waste can never
// undercount when the node list lags the catalog.
function instancesFor(
  model: ModelEntry,
  replicaByNode: Record<string, { head: string; type?: string; width?: number }>,
): { instances: number; shardedNodes: number; shardTotalNodes: number; shardInstances: number; shardShape: string | null; heads: string[] } {
  const warmNodes = model.nodes.filter((n) => n.warm);
  const groups = new Map<string, string[]>();
  for (const n of warmNodes) {
    const key = replicaByNode[n.name] ? `replica:${replicaByNode[n.name].head}` : `node:${n.name}`;
    const g = groups.get(key);
    if (g) g.push(n.name);
    else groups.set(key, [n.name]);
  }
  let instances = groups.size;
  const listedWarm = warmNodes.length;
  if (model.warm_count > listedWarm) instances += model.warm_count - listedWarm;
  const heads: string[] = [];
  let shardedNodes = 0;
  let shardInstances = 0;
  // Parallelism shape shared by every sharded member (e.g. "TP=8") - shown
  // on the badge when the whole group agrees, omitted when members differ.
  const shapes = new Set<string>();
  for (const [key, members] of groups) {
    if (key.startsWith('replica:')) {
      shardedNodes += members.length;
      shardInstances += 1;
      heads.push(key.slice('replica:'.length));
      for (const name of members) {
        const t = replicaByNode[name]?.type;
        const w = replicaByNode[name]?.width;
        if (t && w) shapes.add(`${t.toUpperCase()}=${w}`);
      }
    }
  }
  heads.sort();
  // Total declared membership of each shard group (fleet-wide, independent of
  // whether every member currently reports this model as warm) - the correct
  // denominator for a "warm/total" fact about the deployment itself, as
  // opposed to model.total_nodes which counts the whole fleet.
  const totalMembersByHead = new Map<string, number>();
  for (const nodeName in replicaByNode) {
    const head = replicaByNode[nodeName].head;
    totalMembersByHead.set(head, (totalMembersByHead.get(head) || 0) + 1);
  }
  // Summed across every distinct shard group this model happens to be warm
  // on. In practice a given model is only ever part of one live replica
  // deployment at a time, so this is normally a single group; if the same
  // model name were ever warm on two unrelated replica deployments
  // simultaneously, this intentionally reports their combined instance/node
  // counts as one figure rather than picking one arbitrarily - a genuinely
  // rare topology, not a miscalculation.
  let shardTotalNodes = 0;
  for (const head of heads) {
    shardTotalNodes += totalMembersByHead.get(head) || 0;
  }
  return { instances, shardedNodes, shardTotalNodes: Math.max(shardTotalNodes, shardedNodes), shardInstances, shardShape: shapes.size === 1 ? [...shapes][0] : null, heads };
}

function wasteFor(
  model: ModelEntry,
  replicaByNode: Record<string, { head: string; type?: string; width?: number }>,
): { copies: number; vram: number } {
  const { instances } = instancesFor(model, replicaByNode);
  const copies = instances > 1 ? instances - 1 : 0;
  const vram = copies > 0 && model.size_vram ? copies * model.size_vram : 0;
  return { copies, vram };
}

// Precomputed status data for ModelStatusBadges - all replica grouping and
// waste math happens here (instancesFor/wasteFor), never inside the
// presentation component below, so the badges can't drift into recomputing
// their own version of this logic.
interface ModelStatusData {
  wasteCopies: number;
  wasteVRAM: number;
  shardedNodes: number;
  shardTotalNodes: number;
  shardInstances: number;
  shardShape: string | null;
  shardHeads: string[];
}

function modelStatusFor(
  model: ModelEntry,
  replicaByNode: Record<string, { head: string; type?: string; width?: number }>,
): ModelStatusData {
  const { copies: wasteCopies, vram: wasteVRAM } = wasteFor(model, replicaByNode);
  const { shardedNodes, shardTotalNodes, shardInstances, shardShape, heads: shardHeads } = instancesFor(model, replicaByNode);
  return { wasteCopies, wasteVRAM, shardedNodes, shardTotalNodes, shardInstances, shardShape, shardHeads };
}

// Single source of the sharded/waste badges - rendered by both the desktop
// table and the card so they can never re-diverge in styling. Sharded is
// informational (a correctly-sharded model, not a problem) so it uses the
// `info` (blue) token; waste is actionable (reclaimable VRAM) so it keeps
// the amber/warning treatment. Deliberately NOT `primary` here - this
// brand's primary color is itself a gold/amber hue (see index.css), so a
// "primary vs amber" pairing would barely read as different colors at all.
function ModelStatusBadges({ data, hideSharded }: { data: ModelStatusData; hideSharded?: boolean }) {
  const { wasteCopies, wasteVRAM, shardedNodes, shardInstances, shardShape, shardHeads } = data;
  if (wasteCopies === 0 && (shardedNodes === 0 || hideSharded)) return null;
  return (
    <>
      {shardedNodes > 0 && !hideSharded && (
        <span
          className="inline-flex items-center gap-1.5 px-2 py-1 bg-info/10 border border-info/20 rounded-md font-medium text-info"
          title="One sharded instance across these nodes - fully in use, not a reclaimable copy."
        >
          <Layers className="w-3 h-3" />
          Sharded{shardShape ? ` ${shardShape}` : ''}{shardHeads.length === 1 ? ` · ${shardHeads[0]}` : ''} · {shardedNodes} node{shardedNodes === 1 ? '' : 's'}, {shardInstances} instance{shardInstances === 1 ? '' : 's'}
        </span>
      )}
      {wasteCopies > 0 && (
        <span
          className="inline-flex items-center gap-1.5 px-2 py-1 bg-amber-500/10 border border-amber-500/20 rounded-md font-medium text-amber-700 dark:text-amber-400"
          title="Duplicated warm instances beyond 1 - VRAM that could hold a different model."
        >
          <Copy className="w-3 h-3" />
          +{wasteCopies} dup · {wasteVRAM ? formatVRAM(wasteVRAM) : '-'} waste
        </span>
      )}
    </>
  );
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

function ModelFleetCard({ model, demoMode, replicaByNode, onConfigure, onDeleted }: { model: ModelEntry; demoMode: boolean; replicaByNode: Record<string, { head: string; type?: string; width?: number }>; onConfigure: () => void; onDeleted: (modelName: string, nodeName: string) => void }) {
  const isWarm = model.warm_count > 0;
  const totalVRAM = totalVRAMFor(model);
  const isDrifted = !!model.digest_mismatch;
  const driftDetails = model.drift_details || '';
  const status = modelStatusFor(model, replicaByNode);

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

  const shardNodes = model.nodes.filter((n) => replicaByNode[n.name]);
  const otherNodes = model.nodes.filter((n) => !replicaByNode[n.name]);

  return (
    <div className={`bg-card border shadow-sm rounded-xl p-5 hover:border-primary/50 transition-[box-shadow,border-color] duration-200 ease-out hover:shadow-md flex flex-col h-full ${isWarm ? 'border-border' : 'border-border opacity-80'}`}>
      {/* Header */}
      <div className="flex items-start justify-between mb-3">
        <div className="flex items-start gap-3 min-w-0">
          <div className="p-2 bg-secondary rounded-lg shrink-0">
            <Package className="w-4 h-4 text-muted-foreground" />
          </div>
          <div className="min-w-0 flex-1">
            <h3 className="font-mono font-semibold text-foreground truncate text-sm">
              {model.name}
            </h3>
            <div className="min-h-[30px]">
              <p className="text-xs text-muted-foreground mt-0.5 leading-tight">
                {formatVRAM(model.size_vram)} per copy
                {totalVRAM ? ` · ${formatVRAM(totalVRAM)} total warm` : ''}
                {model.size_disk ? ` · ${formatVRAM(model.size_disk)} on disk` : ''}
              </p>
              <p className="text-[11px] text-muted-foreground/70 font-mono mt-0.5 min-h-[14px] leading-tight">{model.family || '\u00A0'}</p>
            </div>
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

      {/* Fleet residency summary - pinned height so single vs multi-chip rows stay aligned */}
      <div className="flex flex-wrap items-center gap-2 text-xs mb-3 min-h-[44px] content-start">
        {status.shardedNodes > 0 ? (
          <>
            <span
              className="inline-flex items-center gap-1.5 px-2 py-1 bg-success/10 border border-success/20 rounded-md font-medium text-success"
              title="Warm shard members vs. this deployment's own declared node count"
            >
              <Layers className="w-3 h-3" />
              {status.shardedNodes} / {status.shardTotalNodes} nodes warm
            </span>
            <span
              className="inline-flex items-center gap-1.5 px-2 py-1 bg-info/10 border border-info/20 rounded-md font-semibold text-info"
              title="One deployment split across these nodes - not independent warm copies"
            >
              <Layers className="w-3 h-3" />
              {status.shardInstances} instance{status.shardInstances === 1 ? '' : 's'}, sharded across {status.shardTotalNodes} node{status.shardTotalNodes === 1 ? '' : 's'}
            </span>
            {status.wasteCopies > 0 && (
              <span
                className="inline-flex items-center gap-1.5 px-2 py-1 bg-secondary rounded-md font-medium text-foreground"
                title="This model is also warm on nodes outside this sharded deployment - see the duplication badge below"
              >
                <Layers className="w-3 h-3 text-muted-foreground" />
                {model.warm_count} / {model.total_nodes} nodes warm fleet-wide
              </span>
            )}
          </>
        ) : (
          <span className="inline-flex items-center gap-1.5 px-2 py-1 bg-secondary rounded-md font-medium text-foreground">
            <Layers className="w-3 h-3 text-muted-foreground" />
            {model.warm_count} / {model.total_nodes} nodes warm
          </span>
        )}
        {totalVRAM > 0 && (
          <span className="inline-flex items-center gap-1.5 px-2 py-1 bg-secondary rounded-md font-medium text-foreground" title="Sum of VRAM across all warm copies (live, not estimated)">
            <Flame className="w-3 h-3 text-muted-foreground" />
            {formatVRAM(totalVRAM)} warm total
          </span>
        )}
        <ModelStatusBadges data={status} hideSharded />
        {isDrifted && driftDetails && (
          <span className="inline-flex items-center gap-1.5 px-2 py-1 bg-warning/10 border border-warning/20 rounded-md font-mono text-[11px] text-warning" title="Distinct digests (short hex) seen across nodes for this model">
            <AlertTriangle className="w-3 h-3" />
            {driftDetails}
          </span>
        )}
      </div>

      {/* Node chips - read-first, link to GPU Nodes for mutations */}
      <div className="border-t border-border pt-3 flex-1 flex flex-col">
        <div className="flex items-center justify-between mb-2 gap-2">
          <p className="text-xs font-medium text-muted-foreground">Resident on</p>
          <Link to={`/gpu-nodes?highlight=${encodeURIComponent(model.nodes.map((n) => n.name).join(','))}&from=models`} className="text-xs text-primary hover:text-primary/80 inline-flex items-center gap-1 font-medium min-h-[32px] px-2 py-1 rounded-md hover:bg-secondary transition-colors shrink-0">
            View nodes <ArrowUpRight className="w-3 h-3 shrink-0" />
          </Link>
        </div>
        <div className="flex flex-wrap gap-1.5">
          {otherNodes.map((node) => (
            <Link
              key={node.name}
              to={`/gpu-nodes?highlight=${encodeURIComponent(node.name)}&from=models`}
              title={`${node.name} · ${node.runtime || 'runtime unknown'} · ${node.warm ? 'warm' : 'cold'} · digest ${node.digest || '-'}${node.vram_bytes ? ` · ${formatVRAM(node.vram_bytes)}` : ''}`}
              className="inline-flex items-center gap-1.5 px-2 py-1 bg-secondary hover:bg-secondary/80 border border-transparent hover:border-border rounded-md text-xs font-medium text-foreground transition-colors duration-200 ease-out"
            >
              <StatusDot status={node.healthy ? 'healthy' : 'down'} />
              <span className="font-mono truncate max-w-[110px]">{node.name}</span>
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
            </Link>
          ))}
          {shardNodes.length > 0 && (
            <div
              className="inline-flex flex-wrap items-center gap-1.5 p-1.5 border border-info/30 bg-info/5 rounded-lg"
              title="These nodes together host one sharded deployment - not independent copies"
            >
              {shardNodes.map((node) => (
                <Link
                  key={node.name}
                  to={`/gpu-nodes?highlight=${encodeURIComponent(node.name)}&from=models`}
                  title={`${node.name} · ${node.runtime || 'runtime unknown'} · ${node.warm ? 'warm' : 'cold'} · shard of deployment headed by ${replicaByNode[node.name].head} · digest ${node.digest || '-'}${node.vram_bytes ? ` · ${formatVRAM(node.vram_bytes)}` : ''}`}
                  className="inline-flex items-center gap-1.5 px-2 py-1 bg-card hover:bg-secondary/80 border border-transparent hover:border-info/40 rounded-md text-xs font-medium text-foreground transition-colors duration-200 ease-out"
                >
                  <StatusDot status={node.healthy ? 'healthy' : 'down'} />
                  <span className="font-mono truncate max-w-[110px]">{node.name}</span>
                  {node.runtime && (
                    <span className="text-[10px] px-1 py-0.5 bg-card border border-border rounded font-mono text-muted-foreground">
                      {node.runtime}
                    </span>
                  )}
                  <span className="text-[10px] font-mono text-muted-foreground" title={node.digest ? `Digest: ${node.digest}` : 'Digest unknown'}>
                    {shortDigest(node.digest)}
                  </span>
                </Link>
              ))}
            </div>
          )}
          {model.nodes.length === 0 && (
            <span className="text-xs text-muted-foreground">No node reports this model</span>
          )}
        </div>

        {/* Delete section - secondary, link-first mutations stay on GPU Nodes */}
        {model.nodes.length > 0 && (
          <div className="mt-auto pt-3 border-t border-border">
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
        title="Delete local model"
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
  // Resolved replica topology per node name, joined from the same node-list
  // fetch above. Lets the waste/shard math below tell one sharded instance
  // (a resolved head + its workers) apart from genuinely duplicated warm
  // copies. Unresolved nodes stay out of this map on purpose: their
  // component disagrees, so each counts as its own instance until the
  // operator reconciles the declarations on the GPU Nodes page.
  const [replicaByNode, setReplicaByNode] = useState<Record<string, { head: string; type?: string; width?: number }>>({});
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
        setReplicaByNode(Object.fromEntries(
          (list || [])
            .filter((n) => (n.schedulingRole === 'head' || n.schedulingRole === 'worker') && n.replicaHead)
            .map((n) => [n.name, { head: n.replicaHead as string, type: n.parallelismType, width: n.parallelismWidth }]),
        ));
      } catch {
        if (!active || currentAppPath() !== '/models') return;
        setRuntimeByNode({});
        setReplicaByNode({});
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
  const duplicatedCopies = models.reduce((a, m) => a + wasteFor(m, replicaByNode).copies, 0);
  const duplicatedVRAM = models.reduce((a, m) => a + wasteFor(m, replicaByNode).vram, 0);
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
          <h1 className="text-2xl font-bold tracking-tight text-foreground">Models</h1>
          <p className="text-sm text-muted-foreground mt-1">
            Fleet residency · where warm, how many copies, total VRAM, drift.
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
          className={`px-4 py-1.5 text-sm font-medium rounded-md transition-colors duration-200 ease-out ${activeTab === 'fleet' ? 'bg-card shadow-sm text-foreground border border-border' : 'text-muted-foreground hover:text-foreground'}`}
        >
          Fleet
        </button>
        <button
          onClick={() => setActiveTab('catalog')}
          className={`px-4 py-1.5 text-sm font-medium rounded-md transition-colors duration-200 ease-out ${activeTab === 'catalog' ? 'bg-card shadow-sm text-foreground border border-border' : 'text-muted-foreground hover:text-foreground'}`}
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
              <label className={`inline-flex items-center gap-2 px-3 py-1.5 border rounded-lg text-xs font-medium cursor-pointer transition-colors duration-200 ease-out ${driftedOnly ? 'bg-primary/10 border-primary/30 text-primary shadow-sm' : 'bg-secondary border-border hover:bg-secondary/80'}`}>
                <input
                  type="checkbox"
                  checked={driftedOnly}
                  onChange={(e) => setDriftedOnly(e.target.checked)}
                  className="accent-primary cursor-pointer"
                />
                Drifted only
              </label>
              <label className={`inline-flex items-center gap-2 px-3 py-1.5 border rounded-lg text-xs font-medium cursor-pointer transition-colors duration-200 ease-out ${warmOnly ? 'bg-primary/10 border-primary/30 text-primary shadow-sm' : 'bg-secondary border-border hover:bg-secondary/80'}`}>
                <input
                  type="checkbox"
                  checked={warmOnly}
                  onChange={(e) => setWarmOnly(e.target.checked)}
                  className="accent-primary cursor-pointer"
                />
                Warm only
              </label>
              <div className={`transition-[opacity,translate,scale] duration-200 ease-out ${driftedOnly || warmOnly || searchQuery ? 'opacity-100 translate-y-0 scale-100' : 'opacity-0 -translate-y-1 scale-95 pointer-events-none w-0 overflow-hidden'}`}>
                <button
                  onClick={clearAllFilters}
                  className="text-xs text-muted-foreground hover:text-foreground px-2 py-1 rounded-md hover:bg-secondary transition-colors duration-200 ease-out whitespace-nowrap"
                >
                  Clear filters
                </button>
              </div>
            </div>
          </div>

          {/* Fleet content */}
          {loading ? (
            <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-6 auto-rows-fr animate-fade-in">
              <SkeletonCard />
              <SkeletonCard />
              <SkeletonCard />
            </div>
          ) : filteredModels.length > 0 ? (
            <div className="space-y-6 animate-fade-in">
              {/* Desktop table - hidden on mobile, no horizontal scroll at 375px because hidden */}
<div className="hidden md:block bg-card border border-border rounded-xl overflow-hidden animate-fade-in">
<div className="overflow-x-auto scroll-region" tabIndex={0} role="region" aria-label="Models table: scroll horizontally for more columns">
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
                        const status = modelStatusFor(model, replicaByNode);
                        return (
                          <tr key={model.name} className="border-b border-border last:border-0 hover:bg-secondary/30 transition-colors">
                            <td className="px-4 py-3 align-top">
                              <div className="font-mono font-semibold text-foreground text-sm truncate max-w-[200px]" title={model.name}>{model.name}</div>
                              <div className="text-xs text-muted-foreground font-mono">{formatVRAM(model.size_vram)} per copy{model.size_disk ? ` · ${formatVRAM(model.size_disk)} disk` : ''}</div>
                              {model.family && <div className="text-[11px] text-muted-foreground/70 font-mono">{model.family}</div>}
                            </td>
                            <td className="px-4 py-3 align-top">
                              {status.shardedNodes > 0 ? (
                                <div className="flex flex-col items-start gap-1 max-w-[220px]">
                                  <span title="Warm shard members vs. this deployment's own declared node count">
                                    <Badge variant="success" size="sm">
                                      {status.shardedNodes}/{status.shardTotalNodes}
                                    </Badge>
                                  </span>
                                  <span
                                    className="inline-flex items-center gap-1 px-2 py-0.5 bg-info/10 border border-info/20 rounded-full text-[11px] font-semibold text-info"
                                    title="One deployment split across these nodes - not independent warm copies"
                                  >
                                    {status.shardInstances} instance{status.shardInstances === 1 ? '' : 's'}, sharded across {status.shardTotalNodes} node{status.shardTotalNodes === 1 ? '' : 's'}
                                  </span>
                                  {status.wasteCopies > 0 && (
                                    <span
                                      className="inline-flex items-center gap-1 px-2 py-0.5 bg-secondary rounded-full text-[11px] font-medium text-foreground"
                                      title="This model is also warm on nodes outside this sharded deployment - see the duplication badge below"
                                    >
                                      {model.warm_count}/{model.total_nodes} fleet-wide
                                    </span>
                                  )}
                                </div>
                              ) : (
                                <Badge variant={model.warm_count > 0 ? 'success' : 'muted'} size="sm">
                                  {model.warm_count}/{model.total_nodes}
                                </Badge>
                              )}
                              <div className="flex flex-col items-start gap-1 mt-1.5 max-w-[220px]">
                                <ModelStatusBadges data={status} hideSharded />
                              </div>
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
                                {model.nodes.filter((n) => !replicaByNode[n.name]).map((node) => (
                                  <Link
                                    key={node.name}
                                    to={`/gpu-nodes?highlight=${encodeURIComponent(node.name)}&from=models`}
                                    title={`${node.name} ${node.runtime || ''} ${node.warm ? 'warm' : 'cold'} ${node.digest || ''}`}
                                    className="inline-flex items-center gap-1 px-1.5 py-0.5 bg-secondary rounded text-xs font-medium hover:bg-secondary/80 transition-colors duration-200 ease-out"
                                  >
                                    <StatusDot status={node.healthy ? 'healthy' : 'down'} size="sm" />
                                    <span className="font-mono truncate max-w-[90px]">{node.name}</span>
                                    <span className="text-[10px] font-mono text-muted-foreground">{shortDigest(node.digest)}</span>
                                  </Link>
                                ))}
                                {model.nodes.some((n) => replicaByNode[n.name]) && (
                                  <div
                                    className="inline-flex flex-wrap items-center gap-1 p-1 border border-info/30 bg-info/5 rounded"
                                    title="These nodes together host one sharded deployment - not independent copies"
                                  >
                                    {model.nodes.filter((n) => replicaByNode[n.name]).map((node) => (
                                      <Link
                                        key={node.name}
                                        to={`/gpu-nodes?highlight=${encodeURIComponent(node.name)}&from=models`}
                                        title={`${node.name} ${node.runtime || ''} ${node.warm ? 'warm' : 'cold'} · shard of deployment headed by ${replicaByNode[node.name].head} · ${node.digest || ''}`}
                                        className="inline-flex items-center gap-1 px-1.5 py-0.5 bg-card rounded text-xs font-medium hover:bg-secondary/80 transition-colors duration-200 ease-out"
                                      >
                                        <StatusDot status={node.healthy ? 'healthy' : 'down'} size="sm" />
                                        <span className="font-mono truncate max-w-[90px]">{node.name}</span>
                                        <span className="text-[10px] font-mono text-muted-foreground">{shortDigest(node.digest)}</span>
                                      </Link>
                                    ))}
                                  </div>
                                )}
                              </div>
                            </td>
                            <td className="px-4 py-3 align-top">
                              <div className="flex items-center gap-1.5">
                                <button
                                  onClick={() => setConfigModel(model.name)}
                                  title={`Settings for ${model.name}`}
                                  className="p-1.5 text-muted-foreground hover:text-primary hover:bg-secondary rounded transition-colors duration-200 ease-out"
                                >
                                  <Settings2 className="w-3.5 h-3.5" />
                                </button>
                                <Link
                                  to={`/gpu-nodes?highlight=${encodeURIComponent(model.nodes.map((n) => n.name).join(','))}&from=models`}
                                  title="Manage on GPU nodes (mutations live there)"
                                  className="p-1.5 text-muted-foreground hover:text-primary hover:bg-secondary rounded transition-colors duration-200 ease-out"
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
                  <ModelFleetCard key={model.name} model={model} demoMode={demoMode} replicaByNode={replicaByNode} onConfigure={() => setConfigModel(model.name)} onDeleted={handleModelDeleted} />
                ))}
              </div>
            </div>
          ) : (
            <div className="bg-card border border-border rounded-xl shadow-sm animate-fade-in px-4">
              {catalog && catalog.total_nodes === 0 ? (
                <EmptyState
                  icon={Package}
                  title="No GPU nodes connected"
                  copy="Connect your first node to view fleet residency and monitor warm VRAM."
                  action={
                    <Link to="/gpu-nodes" className="text-sm font-medium text-primary hover:underline">
                      Go to GPU Nodes
                    </Link>
                  }
                />
              ) : (
                <EmptyState
                  icon={Package}
                  title="No models found"
                  copy={
                    searchQuery || driftedOnly || warmOnly
                      ? 'No models matching your filters.'
                      : 'No models reported across any nodes. Start a request or pull a model to populate the fleet.'
                  }
                />
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
              Model catalog
            </h3>
            <p className="text-sm text-muted-foreground mt-2 leading-normal">
              Catalog is secondary to fleet intelligence. Browse curated popular models and check per-node fit in{' '}
              <Link to="/model-advisor" className="text-primary hover:underline font-medium">
                Model advisor
              </Link>
              , or pull any model directly to a node below. The fleet view above is the source of truth for what is actually warm and where.
            </p>
            <div className="flex flex-wrap gap-3 mt-4">
              <Link
                to="/model-advisor"
                className="inline-flex items-center gap-2 px-4 py-2 bg-secondary hover:bg-secondary/80 border border-border rounded-lg text-sm font-semibold text-foreground transition-colors"
              >
                <Layers className="w-4 h-4" />
                Open Model advisor
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
            <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-6 auto-rows-fr">
              <SkeletonCard />
              <SkeletonCard />
              <SkeletonCard />
            </div>
          ) : filteredModels.length > 0 ? (
            <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-6 auto-rows-fr">
              {filteredModels.map((model) => (
                <ModelFleetCard key={model.name} model={model} demoMode={demoMode} replicaByNode={replicaByNode} onConfigure={() => setConfigModel(model.name)} onDeleted={handleModelDeleted} />
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
        title="Pull model from registry"
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
