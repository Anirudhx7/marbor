import { useState, useEffect, useMemo, useCallback, useRef, useSyncExternalStore } from 'react';
import { Package, Download, Check, Server, Loader2, Cpu, HardDrive, Star, ArrowDown, ExternalLink, X, Settings2 } from 'lucide-react';
import { SearchInput } from '../components/SearchInput';
import { VramBar } from '../components/VramBar';
import { ModelConfigModal } from '../components/ModelConfigModal';
import { Modal } from '../components/Modal';
import { CustomSelect } from '../components/Select';
import {
  fetchSystemInfo,
  SystemInfo,
  fetchModelCatalog,
  searchHFModels,
  getHFRepoDetails,
  fetchFavorites,
  addFavorite,
  removeFavorite,
  HFModel,
  HFRepoDetails,
  ModelVariantFit,
} from '../lib/api';
import { startPull, isPullActive, subscribe as subscribePullProgress, getSnapshot as getPullProgressSnapshot } from '../lib/pullProgress';
import { useDemoMode } from '../hooks/useDemoMode';
import { mockHFModels, mockHFRepoDetails, mockSystemInfo, mockModelCatalogResponse, mockFavorites } from '../lib/mockData';
import { CustomDatePicker } from '../components/DateTimePicker';

// LIVE_VRAM_TOOL_SOURCES are every `vram_source` string handleModelFit
// (admin.go) can report for a value read straight from a vendor tool - see
// the identical constant in GPUNodes.tsx for the full reasoning.
const LIVE_VRAM_TOOL_SOURCES = new Set(['nvidia-smi', 'rocm-smi', 'xpu-smi', 'system_profiler', 'agent', 'api']);

// ContextFeasibilityNote renders P71's context-length feasibility advice for
// one variant. confidence is always shown (derived vs. estimated) so an
// operator never mistakes a rough linear guess for a real architecture-
// derived answer - a fabricated/overconfident number is exactly what this
// feature exists to avoid (R1).
function ContextFeasibilityNote({ cf, fit }: { cf: ModelVariantFit['context_feasibility']; fit: 'green' | 'yellow' | 'red' | 'unknown' }) {
  if (!cf) return null;
  const isDerived = cf.confidence === 'derived';
  const fmt = (n: number) => new Intl.NumberFormat().format(n);
  const lines: string[] = [];

  if (cf.exceeds_declared_max && cf.declared_max_context) {
    lines.push(`Requested context exceeds this model's declared maximum of ${fmt(cf.declared_max_context)} tokens.`);
  }
  // Backend always populates limiting_factor once real architecture facts
  // are known, regardless of whether VRAM capacity could be classified - but
  // a VRAM-headroom claim is meaningless when fit is 'unknown' (the node's
  // VRAM capacity itself isn't known), so only 'yellow'/'red' render it here.
  const constrained = fit === 'yellow' || fit === 'red';
  if (isDerived && constrained && cf.limiting_factor) {
    // "yellow" still fits (classifyFit: <=100% of capacity, just past the 85%
    // comfortable margin) - only "red" actually exceeds VRAM. Using "exceeds"
    // for yellow would contradict the "Tight" badge shown right next to it.
    const verb = fit === 'red' ? 'is expected to exceed available VRAM' : 'leaves little VRAM headroom on this node';
    lines.push(
      cf.limiting_factor === 'kv_cache'
        ? `KV cache at ${fmt(cf.requested_ctx)} tokens ${verb}.`
        : `Model weights alone ${verb} at this size.`
    );
  }
  if (isDerived && constrained && cf.recommended_ctx) {
    lines.push(`Recommended context: ~${fmt(cf.recommended_ctx)} tokens.`);
  }

  return (
    <div className="mt-1 space-y-0.5">
      <span className={`text-[9px] font-medium ${isDerived ? 'text-muted-foreground' : 'text-amber-600 dark:text-amber-400'}`}>
        {isDerived
          ? `Context: derived from model architecture${cf.declared_max_context ? ` (max ${fmt(cf.declared_max_context)})` : ''}`
          : 'Context: rough estimate - model architecture unknown'}
      </span>
      {lines.map((l, i) => (
        <p key={i} className="text-[9px] text-muted-foreground leading-snug">{l}</p>
      ))}
      {cf.runtime_caveat && (
        <p className="text-[9px] text-muted-foreground/70 italic leading-snug">{cf.runtime_caveat}</p>
      )}
    </div>
  );
}

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

// NodeVramCard shows the active node's VRAM headroom. Extracted so it can be
// rendered in both tabs (Favourites keeps its original position; Browse
// moves it below the search/filter controls so HF Browse itself reads as
// the primary surface - see P77).
function NodeVramCard({ node }: { node: any }) {
  return (
    <div className="bg-card border border-border rounded-xl p-5 shadow-sm">
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2 mb-3">
        <div>
          <span className="text-xs font-bold text-muted-foreground uppercase tracking-wider">GPU Node</span>
          <h3 className="font-semibold text-foreground mt-0.5">{node.name}</h3>
        </div>
        <span className="text-xs text-muted-foreground self-start sm:self-auto">
          VRAM source:{' '}
          <span className={`px-1.5 py-0.5 rounded font-semibold ${
            LIVE_VRAM_TOOL_SOURCES.has(node.vram_source)
              ? 'bg-green-500/15 text-green-600 dark:text-green-400'
              : node.vram_source === 'inferred'
              ? 'bg-amber-500/15 text-amber-600 dark:text-amber-400'
              : node.vram_source === 'declared'
              ? 'bg-blue-500/15 text-blue-600 dark:text-blue-400'
              : 'bg-secondary text-muted-foreground'
          }`}>
            {node.vram_source === 'declared' ? 'declared' : node.vram_source}
          </span>
        </span>
      </div>
      {node.vram_total_bytes > 0 ? (
        <>
          <VramBar
            used={(node.vram_total_bytes - node.vram_free_bytes) / (1024 * 1024 * 1024)}
            total={node.vram_total_bytes / (1024 * 1024 * 1024)}
          />
          <p className="text-xs text-muted-foreground font-medium mt-2">
            {bytesToGB(node.vram_free_bytes)} free of {bytesToGB(node.vram_total_bytes)} VRAM
            {node.vram_fit_basis === 'combined' && (
              <span className="ml-1">&middot; combined across {node.gpu_count} GPUs</span>
            )}
            {node.vram_fit_basis === 'largest' && (
              <span className="ml-1">&middot; largest of {node.gpu_count} GPUs ({node.runtime || 'this runtime'} does not shard across GPUs)</span>
            )}
          </p>
          {node.gpu_count_unknown && (
            <p className="text-xs text-amber-600 dark:text-amber-400 font-medium mt-1">
              GPU count unknown - no agent has confirmed a per-device reading, so this number may not reflect the full node.
            </p>
          )}
        </>
      ) : (node.vram_used_bytes ?? 0) > 0 ? (
        <>
          <div className="w-full bg-secondary rounded-md h-2 mt-1">
            <div className="bg-amber-500 h-2 rounded-md w-full opacity-40" />
          </div>
          <p className="text-xs text-muted-foreground font-medium mt-2">
            {bytesToGB(node.vram_used_bytes!)} in use &middot; total unknown
          </p>
        </>
      ) : (
        <p className="text-xs text-muted-foreground font-medium">
          VRAM totals unavailable - nvidia-smi reads the marbor host only.
        </p>
      )}
    </div>
  );
}

function ModelDetailPanel({
  model,
  nodeName,
  nodeRuntime,
  actualRuntime,
  agentPullCapable,
  nodeVRAMTotalBytes,
  isLive,
  demoMode,
  onClose,
}: {
  model: HFModel;
  nodeName: string | null;
  nodeRuntime: string | null;
  actualRuntime: string | null;
  agentPullCapable: boolean;
  nodeVRAMTotalBytes: number;
  isLive: boolean;
  demoMode: boolean;
  onClose: () => void;
}) {
  const panelRef = useRef<HTMLDivElement>(null);
  const [visible, setVisible] = useState(false);
  const [loading, setLoading] = useState(false);
  const [details, setDetails] = useState<HFRepoDetails | null>(null);
  const [ctxLen, setCtxLen] = useState(8192);
  const [error, setError] = useState<string | null>(null);
  const [configTag, setConfigTag] = useState<string | null>(null);
  // Defaults to checked: every model here comes from a live Hugging Face
  // search (searchHFModels), never Ollama's own curated library - none of it
  // is vetted for compatibility with this node's installed runtime.
  const [verifyLoad, setVerifyLoad] = useState(true);
  const [vramConfirmVariant, setVramConfirmVariant] = useState<ModelVariantFit | null>(null);
  const pullJobs = useSyncExternalStore(subscribePullProgress, getPullProgressSnapshot);
  // Mirrors the node-identity guard pattern used in GPUNodes.tsx - a slower
  // fetchDetails response for a previously-selected node can otherwise land
  // after a newer node's response and overwrite its displayed VRAM-fit
  // badge, which an operator could act on to pull a model sized for the
  // wrong GPU.
  const currentNodeRef = useRef(nodeName);
  useEffect(() => { currentNodeRef.current = nodeName; }, [nodeName]);

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
      const diskFreeGB = mock?.disk_free_gb ?? 500;
      const diskFit = (sizeMB: number): 'ok' | 'insufficient' | 'unknown' =>
        sizeMB / 1024 > diskFreeGB ? 'insufficient' : 'ok';
      if (mock) {
        const adjustedVariants = mock.variants.map((v: any) => {
          const estVram = v.size_mb * 1.10 + len * 0.15;
          let fit: 'green' | 'yellow' | 'red' = 'green';
          if (estVram > 24576) fit = 'red';
          else if (estVram > 10240) fit = 'yellow';
          // Demo-only: context_feasibility is static mock data keyed to
          // whatever context length the mock author picked, so it must be
          // re-pinned to the slider's actual value here or the note goes
          // stale (wrong requested_ctx, and a recommended_ctx/limiting_factor
          // left over from a fit color that no longer matches the badge
          // above) the moment the slider moves away from that value.
          const stillConstrained = fit === 'yellow' || fit === 'red';
          const cf = v.context_feasibility
            ? {
                ...v.context_feasibility,
                requested_ctx: len,
                exceeds_declared_max: v.context_feasibility.declared_max_context
                  ? len > v.context_feasibility.declared_max_context
                  : v.context_feasibility.exceeds_declared_max,
                limiting_factor: stillConstrained ? v.context_feasibility.limiting_factor : undefined,
                kv_cache_est_mb: stillConstrained ? v.context_feasibility.kv_cache_est_mb : undefined,
                // The mock's recommended_ctx is a fixed number picked for one
                // specific mock scenario - only trust it here if it's still
                // meaningfully below the slider's current position, so a
                // slider change can never make the demo suggest a "lower"
                // context that isn't actually lower than what's selected.
                recommended_ctx: stillConstrained && v.context_feasibility.recommended_ctx < len
                  ? v.context_feasibility.recommended_ctx
                  : undefined,
              }
            : v.context_feasibility;
          return { ...v, vram_est_mb: Math.round(estVram), fit, disk_fit: diskFit(v.size_mb), context_feasibility: cf };
        });
        // disk_free_gb/disk_total_gb/disk_known explicitly set (not just
        // spread from mock) - most mock entries don't declare them, and the
        // render below unconditionally calls details.disk_free_gb.toFixed(1)
        // whenever a variant's disk_fit is 'insufficient'.
        setDetails({
          ...mock,
          variants: adjustedVariants,
          disk_free_gb: diskFreeGB,
          disk_total_gb: mock.disk_total_gb ?? 1000,
          disk_known: mock.disk_known ?? true,
        });
      } else {
        setDetails({
          id: model.id,
          downloads: model.downloads,
          likes: model.likes,
          tags: model.tags,
          last_modified: model.lastModified,
          variants: [{ tag: `hf.co/${model.id}:Q4_K_M`, quantization: 'Q4_K_M', vram_est_mb: 4000, size_mb: 3500, fit: 'green', disk_fit: diskFit(3500), downloaded: false, context_feasibility: { confidence: 'estimated', requested_ctx: len } }],
          disk_free_gb: diskFreeGB,
          disk_total_gb: 1000,
          disk_known: true,
        });
      }
      return;
    }
    const targetNode = nodeName;
    setLoading(true);
    setError(null);
    try {
      const resp = await getHFRepoDetails(model.id, targetNode || undefined, len, nodeRuntime || undefined);
      if (currentNodeRef.current !== targetNode) return;
      setDetails(resp);
    } catch (e: unknown) {
      if (currentNodeRef.current !== targetNode) return;
      setError(e instanceof Error ? e.message : 'Failed to load variants');
    } finally {
      if (currentNodeRef.current === targetNode) setLoading(false);
    }
  }, [demoMode, model.id, model.downloads, model.likes, model.tags, model.lastModified, nodeName, nodeRuntime]);

  useEffect(() => { fetchDetails(ctxLen); }, [ctxLen, fetchDetails]);

  const handlePull = (variant: ModelVariantFit) => {
    if (!nodeName) return;
    // Disk space is a hard block, no override (unlike VRAM's confirm-anyway
    // above) - unlike VRAM, free disk isn't a transient snapshot the marbor's
    // own scheduling causes to fluctuate wrongly; a pull exceeding free disk
    // WILL fail. The Pull button is already disabled for this case (see
    // render below); this is a defense-in-depth guard against any stale-prop
    // race, not the primary gate.
    if (variant.disk_fit === 'insufficient') return;
    if (variant.fit === 'red') {
      setVramConfirmVariant(variant);
      return;
    }
    startPull(nodeName, variant.tag, demoMode, verifyLoad);
  };

  // The pull-progress widget owns the download UI; this only needs to know
  // when a pull it started here finishes, so the "Ready" checkmark (driven
  // by the server's own `downloaded` flag, not local state) reflects it even
  // after the admin dismisses the widget.
  useEffect(() => {
    if (pullJobs.some(j => j.node === nodeName && j.status === 'success')) {
      fetchDetails(ctxLen);
    }
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pullJobs, nodeName]);

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
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground mt-1.5">
            <span className="flex items-center gap-1">
              <Star className="w-3 h-3 text-amber-700 dark:text-amber-400 fill-amber-500" /> {formattedLikes}
            </span>
            <span className="flex items-center gap-1">
              <ArrowDown className="w-3.5 h-3.5" /> {formattedDownloads} downloads
            </span>
            {model.pipeline_tag && (
              <span className="px-1.5 py-0.5 rounded bg-primary/10 text-primary capitalize text-[10px]">
                {model.pipeline_tag.replaceAll('-', ' ')}
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
      <div className="overflow-y-auto flex-1 p-5 pb-28 space-y-4">
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

        <label className="flex items-start gap-2 cursor-pointer">
          <input
            type="checkbox"
            checked={verifyLoad}
            onChange={(e) => setVerifyLoad(e.target.checked)}
            className="mt-0.5 accent-primary cursor-pointer"
          />
          <span className="text-[10px] text-muted-foreground leading-normal">
            Verify each pull actually loads before reporting success. This is a community model,
            not Ollama's own curated library - some architectures download fine but fail to load;
            this catches that at pull time instead of the first time something tries to use it.
          </span>
        </label>

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
            {details.docker_deployed && (
              <p className="text-[10px] text-amber-600 dark:text-amber-400 bg-amber-500/10 border border-amber-500/20 rounded-lg px-2.5 py-1.5 leading-normal">
                This node's runtime runs in Docker - the disk figures below reflect the host machine, not necessarily the container's own storage volume. Marbor checks the container's real free space before actually pulling, so a pull is never started if it would genuinely run out of room.
              </p>
            )}
            <span className="text-xs font-semibold text-muted-foreground uppercase tracking-wider block">
              {(!nodeRuntime || nodeRuntime === 'ollama' || nodeRuntime === 'llamacpp') ? 'GGUF File Quantizations' : 'Safetensors Repository'}
            </span>
            <div className="space-y-1.5">
              {details.variants.map((v) => {
                const isPulled = v.downloaded;
                const isPulling = pullJobs.some(j => j.node === nodeName && j.model === v.tag && isPullActive(j.status));
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
                        <span className="text-[10px] text-muted-foreground whitespace-nowrap" title="Estimated from registry size + requested context">{sizeGB} size · {vramGB} est. VRAM</span>
                      </div>
                      <span className="text-[9px] text-muted-foreground font-mono block truncate" title={v.tag}>
                        {v.tag}
                      </span>
                      <ContextFeasibilityNote cf={v.context_feasibility} fit={v.fit} />
                    </div>
                    <div className="flex items-center gap-2 shrink-0 self-start sm:self-auto">
                      <FitBadge fit={v.fit} />
                      {v.disk_fit === 'insufficient' && (
                        <span
                          className="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-red-500/15 text-red-600 dark:text-red-400 border border-red-500/30"
                          title={`Needs ~${sizeGB} disk, node "${nodeName}" has ${details.disk_free_gb.toFixed(1)} GB free`}
                        >
                          No Disk Space
                        </span>
                      )}
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
                      ) : (!actualRuntime || actualRuntime === 'ollama' || agentPullCapable) ? (
                        <button
                          onClick={() => handlePull(v)}
                          disabled={(!demoMode && !isLive) || !nodeName || isPulling || v.disk_fit === 'insufficient'}
                          className="inline-flex items-center gap-1 px-2.5 py-1 bg-primary hover:bg-primary/90 disabled:opacity-40 disabled:hover:bg-primary text-[11px] font-medium text-primary-foreground rounded transition-colors cursor-pointer"
                          title={
                            v.disk_fit === 'insufficient'
                              ? `Insufficient disk space on node "${nodeName}" - needs ~${sizeGB}, ${details.disk_free_gb.toFixed(1)} GB free. Free up space to pull.`
                              : v.fit === 'red' ? 'May exceed this node\'s total VRAM - you will be asked to confirm' : ''
                          }
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
        nodes={nodeName ? [{ name: nodeName, runtime: actualRuntime ?? 'ollama' }] : []}
        presetNumCtx={ctxLen}
        onClose={() => setConfigTag(null)}
      />

      <Modal
        isOpen={vramConfirmVariant !== null}
        onClose={() => setVramConfirmVariant(null)}
        title="VRAM May Be Insufficient"
        maxWidth="sm"
      >
        {vramConfirmVariant && (
          <div className="space-y-4">
            <p className="text-sm text-foreground leading-relaxed">
              This variant needs ~{(vramConfirmVariant.vram_est_mb / 1024).toFixed(1)} GB VRAM,
              but node <strong>"{nodeName}"</strong> has{' '}
              {nodeVRAMTotalBytes > 0 ? `${(nodeVRAMTotalBytes / (1024 * 1024 * 1024)).toFixed(1)} GB` : 'an unknown amount of'}{' '}
              total. The pull may fail to load once downloaded.
            </p>
            <div className="flex justify-end gap-2">
              <button
                onClick={() => setVramConfirmVariant(null)}
                className="px-3 py-1.5 text-xs font-medium rounded-lg bg-secondary text-foreground hover:bg-secondary/80 transition-colors cursor-pointer"
              >
                Cancel
              </button>
              <button
                onClick={() => {
                  if (nodeName && vramConfirmVariant) {
                    startPull(nodeName, vramConfirmVariant.tag, demoMode, verifyLoad);
                  }
                  setVramConfirmVariant(null);
                }}
                className="px-3 py-1.5 text-xs font-medium rounded-lg bg-destructive text-destructive-foreground hover:bg-destructive/90 transition-colors cursor-pointer"
              >
                Pull Anyway
              </button>
            </div>
          </div>
        )}
      </Modal>
    </div>
  );
}

function ModelCard({
  model,
  selected,
  onSelect,
  selectLabel = 'View Quantizations & Pull',
  isFavorite,
  onToggleFavorite,
}: {
  model: HFModel;
  selected: boolean;
  onSelect: () => void;
  selectLabel?: string;
  isFavorite: boolean;
  onToggleFavorite: () => void;
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
        <div className="flex items-center gap-1.5 shrink-0 ml-2">
          <span className="inline-flex items-center gap-1 px-2 py-0.5 rounded bg-secondary text-[11px] font-medium text-foreground">
            <Star className="w-3 h-3 text-amber-700 dark:text-amber-400 fill-amber-500" /> {formattedLikes}
          </span>
          <button
            onClick={onToggleFavorite}
            title={isFavorite ? 'Remove from favourites' : 'Add to favourites'}
            aria-label={isFavorite ? 'Remove from favourites' : 'Add to favourites'}
            className="p-1 rounded-lg hover:bg-secondary transition-colors cursor-pointer"
          >
            <Star className={`w-4 h-4 ${isFavorite ? 'text-primary fill-primary' : 'text-muted-foreground'}`} />
          </button>
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground mb-3">
        <span className="flex items-center gap-1">
          <ArrowDown className="w-3.5 h-3.5" /> {formattedDownloads} downloads
        </span>
        {model.pipeline_tag && (
          <span className="px-1.5 py-0.5 rounded bg-primary/10 text-primary capitalize text-[10px]">
            {model.pipeline_tag.replaceAll('-', ' ')}
          </span>
        )}
      </div>

      <button
        onClick={onSelect}
        className={`mt-auto w-full py-2 text-foreground text-xs font-medium rounded-lg transition-colors cursor-pointer ${selected ? 'bg-primary/10 text-primary hover:bg-primary/20' : 'bg-secondary hover:bg-secondary/80'}`}
      >
        {selected ? 'Hide Details' : selectLabel}
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

  const [tab, setTab] = useState<'browse' | 'favourites'>('browse');
  const [favoriteIds, setFavoriteIds] = useState<Set<string>>(new Set());
  const [favoriteDetails, setFavoriteDetails] = useState<Record<string, HFModel>>({});
  const searchSeqRef = useRef(0);

  const activeNode = useMemo(
    () => (!selectedNode || nodes.length === 0) ? null : nodes.find(n => n.name === selectedNode) || null,
    [nodes, selectedNode]
  );

  // Browse format defaults to the node's declared runtime but admins can
  // override it manually (e.g. browse GGUF repos while the node itself runs
  // MLX) - no auto-switch magic, always an explicit admin choice.
  const [runtimeOverride, setRuntimeOverride] = useState<string | null>(null);
  useEffect(() => { setRuntimeOverride(null); }, [selectedNode]);
  const browseRuntime = runtimeOverride ?? activeNode?.runtime ?? null;
  // Mirrors ggufOnlyRuntime(runtime) in internal/admin/catalog.go - used only
  // to phrase the "how to add models" copy, not to gate any request.
  const browseRuntimeIsGGUF = browseRuntime == null || browseRuntime === '' || browseRuntime === 'ollama' || browseRuntime === 'llamacpp';

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
    // A stale response applying after a newer one would show results
    // matching none of the currently-visible filters on a page used to
    // decide GPU-affecting pulls - guard with a monotonic sequence number.
    searchSeqRef.current += 1;
    const seq = searchSeqRef.current;
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
        if (searchSeqRef.current !== seq) return;
        setModels(sorted);
        return;
      }
      setSearching(true);
      setSearchError(null);
      try {
        const resp = await searchHFModels(debouncedSearch, {
          runtime: browseRuntime ?? undefined,
          sort: sortBy,
          minDownloads: minDl,
          minLikes: minLk,
          createdAfter: createdAfter || undefined,
        });
        if (searchSeqRef.current !== seq) return;
        setModels(resp || []);
      } catch (e: unknown) {
        if (searchSeqRef.current !== seq) return;
        setSearchError(e instanceof Error ? e.message : 'Failed to search Hugging Face models. Make sure the backend has internet access.');
        setModels([]);
      } finally {
        if (searchSeqRef.current === seq) setSearching(false);
      }
    };
    doSearch();
  }, [debouncedSearch, demoMode, sortBy, minDownloads, minLikes, createdAfter, browseRuntime]);

  // Close panel when search results change
  useEffect(() => { setSelectedModelId(null); }, [models]);

  useEffect(() => {
    if (demoMode) {
      setFavoriteIds(new Set(mockFavorites));
      return;
    }
    fetchFavorites().then(ids => setFavoriteIds(new Set(ids))).catch(() => {});
  }, [demoMode]);

  // favoriteIdsRef always mirrors the latest favoriteIds synchronously, so two
  // rapid clicks (before React re-renders and hands toggleFavorite a fresh
  // closure) see each other's effect instead of both reading the same stale
  // "wasFavorite" and firing the same API call twice.
  const favoriteIdsRef = useRef(favoriteIds);
  useEffect(() => { favoriteIdsRef.current = favoriteIds; }, [favoriteIds]);

  const toggleFavorite = useCallback((modelId: string) => {
    const wasFavorite = favoriteIdsRef.current.has(modelId);
    const next = new Set(favoriteIdsRef.current);
    if (wasFavorite) next.delete(modelId); else next.add(modelId);
    favoriteIdsRef.current = next;
    setFavoriteIds(next);
    if (demoMode) return;
    const revert = () => setFavoriteIds(prev => {
      const reverted = new Set(prev);
      if (wasFavorite) reverted.add(modelId); else reverted.delete(modelId);
      favoriteIdsRef.current = reverted;
      return reverted;
    });
    (wasFavorite ? removeFavorite(modelId) : addFavorite(modelId)).catch(err => {
      console.error(`Failed to ${wasFavorite ? 'remove' : 'add'} favourite:`, err);
      revert();
    });
  }, [demoMode]);

  const uniqueModels = useMemo(() => {
    const seen = new Set<string>();
    return models.filter(m => {
      if (!m.id || seen.has(m.id)) return false;
      seen.add(m.id);
      return true;
    });
  }, [models]);

  // Favourites tab shows the starred models as cards without requiring a
  // node/search context - fetch repo details for any starred id not already
  // present in the current search results or the cache, so the list still
  // renders something useful even for models the user isn't currently
  // browsing. failedFavoriteIdsRef tracks ids whose fetch already failed
  // (e.g. the repo was deleted/renamed on Hugging Face) so this effect gives
  // up on them instead of retrying every time favoriteIds/uniqueModels change.
  const failedFavoriteIdsRef = useRef<Set<string>>(new Set());
  useEffect(() => {
    if (tab !== 'favourites' || demoMode) return;
    const missing = [...favoriteIds].filter(id =>
      !favoriteDetails[id] && !uniqueModels.some(m => m.id === id) && !failedFavoriteIdsRef.current.has(id)
    );
    if (missing.length === 0) return;
    missing.forEach(id => {
      getHFRepoDetails(id).then(details => {
        setFavoriteDetails(prev => ({
          ...prev,
          [id]: {
            id: details.id,
            downloads: details.downloads,
            likes: details.likes,
            tags: details.tags,
            lastModified: details.last_modified,
            pipeline_tag: '',
          },
        }));
      }).catch(() => { failedFavoriteIdsRef.current.add(id); });
    });
  }, [tab, demoMode, favoriteIds, favoriteDetails, uniqueModels]);

  const favoriteModels = useMemo(() => {
    return [...favoriteIds]
      .map(id => (demoMode ? mockHFModels.find(m => m.id === id) : (uniqueModels.find(m => m.id === id) ?? favoriteDetails[id])))
      .filter((m): m is HFModel => !!m);
  }, [favoriteIds, demoMode, uniqueModels, favoriteDetails]);

  // Build grid items: cards + panel inserted after end of the clicked card's row.
  // Parameterized on the source list so Browse (uniqueModels) and Favourites
  // (favoriteModels) share identical inline-detail-panel behavior.
  type GridItem = { kind: 'card'; model: HFModel } | { kind: 'panel'; model: HFModel };
  const buildGridItems = useCallback((list: HFModel[]): GridItem[] => {
    if (!selectedModelId) return list.map(m => ({ kind: 'card' as const, model: m }));
    const idx = list.findIndex(m => m.id === selectedModelId);
    if (idx < 0) return list.map(m => ({ kind: 'card' as const, model: m }));

    // Insert panel after the last card in the row containing the selected card
    const selected = list[idx];
    const rowEnd = Math.ceil((idx + 1) / columnCount) * columnCount - 1;
    const insertAfter = Math.min(rowEnd, list.length - 1);

    const items: GridItem[] = [];
    list.forEach((m, i) => {
      items.push({ kind: 'card', model: m });
      if (i === insertAfter) {
        items.push({ kind: 'panel', model: selected });
      }
    });
    return items;
  }, [selectedModelId, columnCount]);

  const gridItems = useMemo(() => buildGridItems(uniqueModels), [buildGridItems, uniqueModels]);
  const favoriteGridItems = useMemo(() => buildGridItems(favoriteModels), [buildGridItems, favoriteModels]);

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
          <span className={`text-xs font-semibold ${demoMode ? 'text-success' : (loading && !isLive) ? 'text-blue-600 dark:text-blue-400 animate-pulse' : isLive ? 'text-success' : 'text-amber-600 dark:text-amber-400'}`}>
            {demoMode ? 'Demo Mode' : (loading && !isLive) ? 'Connecting...' : isLive ? 'Live Data' : 'Disconnected'}
          </span>
        </div>
      </div>

      {demoMode && (
        <div className="p-4 bg-amber-500/10 border border-amber-500/20 rounded-xl text-amber-700 dark:text-amber-400 text-sm font-medium">
          Demo mode - 5 inference nodes (Ollama, vLLM, TGI, llama.cpp, MLX). Connect real nodes to see live data.
        </div>
      )}

      {error && !demoMode && (
        <div className="p-4 bg-destructive/10 border border-destructive/20 rounded-xl text-destructive text-sm font-semibold">
          {error}
        </div>
      )}

      <div className="flex flex-wrap gap-2">
        {(['browse', 'favourites'] as const).map((t) => (
          <button
            key={t}
            onClick={() => setTab(t)}
            className={`px-3 py-1.5 rounded-lg text-sm font-semibold transition-colors cursor-pointer ${
              tab === t
                ? 'bg-primary text-primary-foreground'
                : 'bg-secondary text-muted-foreground hover:text-foreground'
            }`}
          >
            {t === 'browse' ? 'Browse' : `Favourites (${favoriteIds.size})`}
          </button>
        ))}
      </div>

      {activeNode && (
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-xs font-bold text-muted-foreground uppercase tracking-wider shrink-0">Node</span>
          {nodes.length > 1 ? (
            nodes.map((n) => (
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
            ))
          ) : (
            <span className="px-3 py-1.5 rounded-lg text-sm font-semibold bg-secondary text-foreground">{activeNode.name}</span>
          )}
        </div>
      )}

      {sysInfo && (
        <div className="bg-card border border-border rounded-xl px-5 py-3 flex flex-wrap gap-5 items-center text-xs shadow-sm">
          <span className="text-xs font-bold text-muted-foreground uppercase tracking-wider shrink-0">Marbor Host</span>
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

      {tab === 'favourites' ? (
        <>
          {activeNode && <NodeVramCard node={activeNode} />}
          {favoriteModels.length === 0 ? (
          <div className="text-center py-16 bg-card border border-border rounded-xl shadow-sm">
            <Star className="w-12 h-12 text-muted-foreground/30 mx-auto mb-4" />
            <p className="text-muted-foreground font-medium">No favourites yet. Star a model in Browse to save it here.</p>
          </div>
        ) : (
          <div className="grid grid-cols-1 lg:grid-cols-2 xl:grid-cols-3 gap-5">
            {favoriteGridItems.map((item) =>
              item.kind === 'card' ? (
                <ModelCard
                  key={item.model.id}
                  model={item.model}
                  selected={item.model.id === selectedModelId}
                  onSelect={() => setSelectedModelId(
                    item.model.id === selectedModelId ? null : item.model.id
                  )}
                  isFavorite={favoriteIds.has(item.model.id)}
                  onToggleFavorite={() => toggleFavorite(item.model.id)}
                />
              ) : (
                <ModelDetailPanel
                  key="__panel__"
                  model={item.model}
                  nodeName={selectedNode}
                  nodeRuntime={browseRuntime}
                  actualRuntime={activeNode?.runtime ?? null}
                  agentPullCapable={activeNode?.capabilities?.includes('models.pull') ?? false}
                  nodeVRAMTotalBytes={activeNode?.vram_total_bytes ?? 0}
                  isLive={isLive}
                  demoMode={demoMode}
                  onClose={() => setSelectedModelId(null)}
                />
              )
            )}
          </div>
        )}
        </>
      ) : (
      <>
      {activeNode && (
        <>
          <div className="bg-secondary/30 border border-border rounded-xl p-4 text-xs text-muted-foreground leading-relaxed shadow-sm">
            <span className="font-semibold text-foreground block mb-1">How to add models to nodes:</span>
            1. Search Hugging Face for any model (e.g. <code className="font-mono text-primary font-semibold">llama-3.2</code> or <code className="font-mono text-primary font-semibold">qwen2.5</code>) using the search bar below - results are already filtered to what {browseRuntimeIsGGUF ? 'this runtime (GGUF)' : `this runtime (${browseRuntime})`} can load.
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

          <div className="bg-secondary/20 border border-border rounded-xl p-3 sm:p-4 min-w-0 overflow-hidden">
            <div className="flex flex-col sm:flex-row sm:flex-wrap gap-3 sm:gap-x-6 sm:gap-y-3 items-stretch sm:items-end">
              <label className="flex flex-col gap-1.5 min-w-0 sm:min-w-[12rem] flex-1 sm:flex-initial" title="Overrides the node's declared runtime for browsing only - Pull still respects the node's actual runtime.">
                <span className="text-[11px] uppercase tracking-wide text-muted-foreground font-semibold shrink-0">Browse format</span>
                <CustomSelect
                  value={runtimeOverride ?? '__auto__'}
                  onChange={(val) => setRuntimeOverride(val === '__auto__' ? null : val)}
                  size="md"
                  className="w-full"
                  options={[
                    { value: '__auto__', label: `Auto (${activeNode?.runtime ?? 'ollama'})` },
                    { value: 'ollama', label: 'Ollama (GGUF)' },
                    { value: 'llamacpp', label: 'llama.cpp (GGUF)' },
                    { value: 'mlx', label: 'MLX (safetensors)' },
                    { value: 'vllm', label: 'vLLM (safetensors)' },
                    { value: 'tgi', label: 'TGI (safetensors)' },
                  ]}
                />
              </label>
              <label className="flex flex-col gap-1.5 min-w-0 sm:min-w-[10rem] flex-1 sm:flex-initial">
                <span className="text-[11px] uppercase tracking-wide text-muted-foreground font-semibold shrink-0">Sort</span>
                <CustomSelect
                  value={sortBy}
                  onChange={(val) => setSortBy(val as typeof sortBy)}
                  size="md"
                  className="w-full"
                  options={[
                    { value: 'downloads', label: 'Most Downloads' },
                    { value: 'likes', label: 'Most Likes' },
                    { value: 'newest', label: 'Newest' },
                    { value: 'oldest', label: 'Oldest' },
                  ]}
                />
              </label>
              <label className="flex flex-col gap-1.5">
                <span className="text-[11px] uppercase tracking-wide text-muted-foreground font-semibold">Min Downloads</span>
                <input
                  type="number"
                  min="0"
                  value={minDownloads}
                  onChange={(e) => setMinDownloads(e.target.value)}
                  placeholder="0"
                  className="w-28 bg-secondary border border-border rounded-lg px-3 py-2 text-sm text-foreground font-medium"
                />
              </label>
              <label className="flex flex-col gap-1.5">
                <span className="text-[11px] uppercase tracking-wide text-muted-foreground font-semibold">Min Likes</span>
                <input
                  type="number"
                  min="0"
                  value={minLikes}
                  onChange={(e) => setMinLikes(e.target.value)}
                  placeholder="0"
                  className="w-24 bg-secondary border border-border rounded-lg px-3 py-2 text-sm text-foreground font-medium"
                />
              </label>
              <label className="flex flex-col gap-1.5">
                <span className="text-[11px] uppercase tracking-wide text-muted-foreground font-semibold">Created After</span>
                <CustomDatePicker
                  value={createdAfter}
                  onChange={setCreatedAfter}
                  className="w-40"
                />
              </label>
              {(minDownloads || minLikes || createdAfter || sortBy !== 'downloads') && (
                <button
                  onClick={() => { setSortBy('downloads'); setMinDownloads(''); setMinLikes(''); setCreatedAfter(''); }}
                  className="text-xs text-primary hover:underline font-medium cursor-pointer pb-2"
                >
                  Reset filters
                </button>
              )}
            </div>
          </div>

          {activeNode && <NodeVramCard node={activeNode} />}

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
                    isFavorite={favoriteIds.has(item.model.id)}
                    onToggleFavorite={() => toggleFavorite(item.model.id)}
                  />
                ) : (
                  <ModelDetailPanel
                    key="__panel__"
                    model={item.model}
                    nodeName={selectedNode}
                    nodeRuntime={browseRuntime}
                    actualRuntime={activeNode?.runtime ?? null}
                    agentPullCapable={activeNode?.capabilities?.includes('models.pull') ?? false}
                    nodeVRAMTotalBytes={activeNode?.vram_total_bytes ?? 0}
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
            Marbor requires at least one active Ollama node to calculate VRAM capacity and check model compatibility.
            Connect your first node in the <strong>GPU Nodes</strong> page.
          </p>
        </div>
      )}
      </>
      )}
    </div>
  );
}

function bytesToGB(bytes: number): string {
  const gb = bytes / (1024 * 1024 * 1024);
  return gb >= 1 ? `${gb.toFixed(1)} GB` : `${(bytes / (1024 * 1024)).toFixed(0)} MB`;
}
