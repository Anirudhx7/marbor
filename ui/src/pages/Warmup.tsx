import React, { useState, useEffect, useCallback, useRef } from 'react';
import { useLocation } from 'react-router-dom';
import { Flame, Plus, Trash2, Clock, Server, Pin, ChevronDown, ChevronUp, PauseCircle, PlayCircle, Pencil, BrainCircuit, CheckCircle2, XCircle, Zap } from 'lucide-react';
import {
  fetchNodes, getNodeWarmup, setNodeWarmup,
  listSchedules, createSchedule, deleteSchedule, updateSchedule,
  fetchModels, getPinned, setPinned, fetchSystemInfo, fetchPredictiveDecisions,
  fetchWarmupStatus, setPredictiveEngine, triggerWarmupPing,
} from '../lib/api';
import type { GPUNode, PredictiveDecision } from '../types';
import type { Schedule, NodeWarmup } from '../lib/api';
import { Badge } from '../components/Badge';
import { Modal } from '../components/Modal';
import { CustomSelect } from '../components/Select';
import { CustomTimePicker } from '../components/DateTimePicker';
import { useTimezone } from '../hooks/useTimezone';
import { formatDateTimeInZone, formatTimeInZone, formatInTimezone, wallDateTimeToUtcIso } from '../lib/time';
import { useDemoMode, currentAppPath } from '../hooks/useDemoMode';

const DAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];

// ── Combined per-node card (warmup + pinned) ──────────────────────────────────

function ModelPills({ allModels, selected, onChange }: {
  allModels: string[];
  selected: string[];
  onChange: (models: string[]) => void;
}) {
  if (allModels.length === 0) return <p className="text-xs text-muted-foreground">No models available.</p>;
  return (
    <div className="flex flex-wrap gap-1.5">
      {allModels.map(model => {
        const active = selected.includes(model);
        return (
          <button key={model} type="button"
            onClick={() => onChange(active ? selected.filter(m => m !== model) : [...selected, model])}
            className={`px-2 py-1 rounded-md border text-xs font-mono transition-colors ${
              active ? 'bg-primary/10 border-primary/40 text-primary' : 'border-border text-muted-foreground hover:bg-secondary'
            }`}>
            {model}
          </button>
        );
      })}
    </div>
  );
}

// KeepWarmList renders the "keep warm" set as an ordered, reorderable list -
// list position doubles as priority (see internal/router/eviction.go
// EvictForHeadroom): the top model always wins a VRAM contest over ones below
// it when a node can't fit all of them at once. Reorder controls only show up
// once there's more than one model, since order is meaningless otherwise.
const SUPPRESSION_REASON_LABEL: Record<string, string> = {
  manual_unload: 'manually unloaded',
  scheduled_unload: 'unloaded by schedule',
};

function KeepWarmList({ models, onChange, warmupErrors, warmupState }: {
  models: string[];
  onChange: (models: string[]) => void;
  warmupErrors?: Record<string, string>;
  warmupState?: { model: string; state: string; reason: string; since: string }[];
}) {
  const tz = useTimezone();
  if (models.length === 0) return null;

  function move(index: number, dir: -1 | 1) {
    const target = index + dir;
    if (target < 0 || target >= models.length) return;
    const next = [...models];
    [next[index], next[target]] = [next[target], next[index]];
    onChange(next);
  }

  return (
    <div className="space-y-1.5 mb-2.5">
      {models.map((model, index) => {
        const error = warmupErrors?.[model];
        const suppressed = warmupState?.find(s => s.model === model && s.state === 'suppressed');
        return (
        <div key={model} className="flex items-center justify-between gap-2 pl-1 pr-1.5 py-1.5 rounded-lg border border-border bg-secondary/30">
          <div className="flex items-center gap-2 min-w-0">
            <span className="flex items-center justify-center w-4 h-4 shrink-0 rounded bg-primary/10 text-primary text-[10px] font-mono font-medium">
              {index + 1}
            </span>
            <div className="min-w-0">
              <span className="text-xs font-mono text-foreground truncate block">{model}</span>
              {error && (
                <span title={error} className="text-[10px] text-destructive truncate block">
                  Warmup failed: {error}
                </span>
              )}
              {!error && suppressed && (
                <span
                  title={`Suppressed since ${formatDateTimeInZone(suppressed.since, tz)} - ${SUPPRESSION_REASON_LABEL[suppressed.reason] || suppressed.reason}`}
                  className="text-[10px] text-amber-700 dark:text-amber-400 truncate block"
                >
                  Suppressed - {SUPPRESSION_REASON_LABEL[suppressed.reason] || suppressed.reason}, resumes on next warmup
                </span>
              )}
            </div>
          </div>
          <div className="flex items-center gap-0.5 shrink-0">
            {models.length > 1 && (
              <>
                <button type="button" onClick={() => move(index, -1)} disabled={index === 0}
                  title="Raise priority"
                  className="p-1 text-muted-foreground hover:text-foreground rounded-md hover:bg-secondary transition-colors disabled:opacity-30 disabled:pointer-events-none">
                  <ChevronUp className="w-3.5 h-3.5" />
                </button>
                <button type="button" onClick={() => move(index, 1)} disabled={index === models.length - 1}
                  title="Lower priority"
                  className="p-1 text-muted-foreground hover:text-foreground rounded-md hover:bg-secondary transition-colors disabled:opacity-30 disabled:pointer-events-none">
                  <ChevronDown className="w-3.5 h-3.5" />
                </button>
              </>
            )}
            <button type="button" onClick={() => onChange(models.filter(m => m !== model))}
              title="Remove from keep-warm"
              className="p-1 text-muted-foreground hover:text-destructive rounded-md hover:bg-secondary transition-colors">
              <Trash2 className="w-3 h-3" />
            </button>
          </div>
        </div>
        );
      })}
    </div>
  );
}

// AddModelPills lists models NOT yet in the keep-warm set as click-to-add
// chips - a distinct affordance from KeepWarmList's assigned/ordered rows, so
// "add" and "reorder what's already added" never look like the same action.
function AddModelPills({ options, onAdd }: { options: string[]; onAdd: (model: string) => void }) {
  if (options.length === 0) return null;
  return (
    <div className="flex flex-wrap gap-1.5">
      {options.map(model => (
        <button key={model} type="button" onClick={() => onAdd(model)}
          className="px-2 py-1 rounded-md border border-border text-xs font-mono text-muted-foreground hover:bg-secondary hover:text-foreground transition-colors">
          + {model}
        </button>
      ))}
    </div>
  );
}

function NodeCard({ node, initial, availableModels, onSave }: {
  node: GPUNode;
  initial: NodeWarmup;
  availableModels: string[];
  onSave: (name: string, nw: NodeWarmup) => Promise<void>;
}) {
  const [enabled, setEnabled] = useState(initial.enabled);
  const [selectedModels, setSelectedModels] = useState<string[]>(initial.models || []);
  const [saving, setSaving] = useState(false);
  const [showModels, setShowModels] = useState(false);
  // dirty tracks an in-progress, unsaved edit (checkbox toggle or model list
  // change) - the page's 10s poll rebuilds `initial` from scratch every
  // cycle with a new object reference regardless of whether server data
  // actually changed, so without this guard the sync effect below silently
  // discards any unsaved edit mid-cycle.
  const dirty = useRef(false);

  // Pinned state
  const [pinnedModels, setPinnedModels] = useState<string[]>([]);
  const [pinnedInput, setPinnedInput] = useState('');
  const [pinnedLoading, setPinnedLoading] = useState(true);
  const [pinnedSaving, setPinnedSaving] = useState(false);
  const [pinnedError, setPinnedError] = useState<string | null>(null);
  const [showPinned, setShowPinned] = useState(false);

  useEffect(() => {
    if (dirty.current) return;
    setEnabled(initial.enabled);
    setSelectedModels(initial.models || []);
  }, [initial]);

  useEffect(() => {
    let cancelled = false;
    getPinned(node.name)
      .then(m => { if (!cancelled) { setPinnedModels(m); setPinnedLoading(false); } })
      .catch(() => { if (!cancelled) setPinnedLoading(false); });
    return () => { cancelled = true; };
  }, [node.name]);

  async function saveWarmup() {
    setSaving(true);
    await onSave(node.name, { enabled, models: selectedModels });
    dirty.current = false;
    setSaving(false);
  }

  function addPinned() {
    const t = pinnedInput.trim();
    if (!t || pinnedModels.includes(t)) { setPinnedInput(''); return; }
    setPinnedModels(p => [...p, t]);
    setPinnedInput('');
  }

  async function savePinned() {
    setPinnedSaving(true);
    setPinnedError(null);
    try { await setPinned(node.name, pinnedModels); }
    catch (e: any) { setPinnedError(e.message || 'Save failed'); }
    finally { setPinnedSaving(false); }
  }

  const allModels = Array.from(new Set([...availableModels, ...selectedModels]));
  // Models that are pinned but not on this node's catalog (custom entries)
  const extraPinned = pinnedModels.filter(m => !availableModels.includes(m));
  const allPinnedModels = Array.from(new Set([...availableModels, ...extraPinned]));

  return (
    <div className="bg-card border border-border rounded-xl overflow-hidden">
      {/* Node header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between px-4 py-3 gap-2">
        <div className="flex items-center gap-2 flex-wrap min-w-0">
          <Server className="w-4 h-4 text-primary shrink-0" />
          <span className="font-medium text-foreground truncate max-w-[12rem] sm:max-w-xs">{node.name}</span>
          {initial.enabled && <Badge variant="success">warm</Badge>}
          {selectedModels.length > 0 && (
            <span className="text-xs text-muted-foreground font-mono">{selectedModels.length} model{selectedModels.length !== 1 ? 's' : ''}</span>
          )}
        </div>
        <div className="flex items-center gap-2 self-end sm:self-auto shrink-0">
          <label className="flex items-center gap-1.5 text-xs text-muted-foreground cursor-pointer select-none">
            <input type="checkbox" checked={enabled} onChange={e => { dirty.current = true; setEnabled(e.target.checked); }}
              className="rounded border-border bg-background text-primary focus:ring-primary/20" />
            Warmup
          </label>
          <button onClick={saveWarmup} disabled={saving}
            className="px-3 py-1.5 bg-primary text-primary-foreground text-xs font-medium rounded-lg hover:bg-primary/90 disabled:opacity-50">
            {saving ? 'Saving…' : 'Save'}
          </button>
        </div>
      </div>

      {/* Models to keep warm - collapsible */}
      <div className="border-t border-border">
        <button onClick={() => setShowModels(p => !p)}
          className="w-full flex items-center justify-between px-4 py-2.5 text-xs text-muted-foreground hover:bg-secondary/40 transition-colors">
          <div className="flex items-center gap-1.5">
            <Flame className="w-3.5 h-3.5" />
            <span>Models to keep warm</span>
            {selectedModels.length > 0 && (
              <span className="px-1.5 py-0.5 bg-primary/10 text-primary rounded text-[10px] font-mono">{selectedModels.length}</span>
            )}
          </div>
          <ChevronDown className={`w-3.5 h-3.5 transition-transform ${showModels ? 'rotate-180' : ''}`} />
        </button>
        {showModels && (
          <div className="px-4 pb-3">
            <KeepWarmList models={selectedModels} onChange={m => { dirty.current = true; setSelectedModels(m); }} warmupErrors={node.warmupErrors} warmupState={node.warmupState} />
            {selectedModels.length > 1 && (
              <p className="text-[10px] text-muted-foreground/60 mb-2.5">Order sets priority - if this node can't fit them all, #1 always stays warm first.</p>
            )}
            <AddModelPills
              options={allModels.filter(m => !selectedModels.includes(m))}
              onAdd={model => { dirty.current = true; setSelectedModels(prev => [...prev, model]); }}
            />
            {allModels.length === 0 && <p className="text-xs text-muted-foreground">No models available.</p>}
          </div>
        )}
      </div>

      {/* Pinned models - collapsible */}
      <div className="border-t border-border">
        <button onClick={() => setShowPinned(p => !p)}
          className="w-full flex items-center justify-between px-4 py-2.5 text-xs text-muted-foreground hover:bg-secondary/40 transition-colors">
          <div className="flex items-center gap-1.5">
            <Pin className="w-3.5 h-3.5" />
            <span>Pinned models</span>
            {pinnedModels.length > 0 && (
              <span className="px-1.5 py-0.5 bg-primary/10 text-primary rounded text-[10px] font-mono">{pinnedModels.length}</span>
            )}
          </div>
          <ChevronDown className={`w-3.5 h-3.5 transition-transform ${showPinned ? 'rotate-180' : ''}`} />
        </button>
        {showPinned && (
          <div className="px-4 pb-3 space-y-2.5">
            {pinnedLoading ? (
              <p className="text-xs text-muted-foreground">Loading…</p>
            ) : (
              <>
                <ModelPills allModels={allPinnedModels} selected={pinnedModels} onChange={setPinnedModels} />
                {/* Input for custom model names not in the available list */}
                <div className="flex gap-2 pt-0.5">
                  <input type="text" value={pinnedInput} onChange={e => setPinnedInput(e.target.value)}
                    onKeyDown={e => { if (e.key === 'Enter') addPinned(); }}
                    placeholder="custom model:tag"
                    className="flex-1 px-2.5 py-1.5 bg-secondary/50 border border-border rounded-lg text-xs text-foreground placeholder:text-muted-foreground/60 focus:outline-none focus:ring-1 focus:ring-primary/30" />
                  <button onClick={addPinned}
                    className="px-2.5 py-1.5 bg-secondary border border-border text-xs text-foreground rounded-lg hover:bg-secondary/80 flex items-center gap-1">
                    <Plus className="w-3 h-3" /> Add
                  </button>
                  <button onClick={savePinned} disabled={pinnedSaving}
                    className="px-3 py-1.5 bg-primary text-primary-foreground text-xs font-medium rounded-lg hover:bg-primary/90 disabled:opacity-50">
                    {pinnedSaving ? 'Saving…' : 'Save'}
                  </button>
                </div>
                <p className="text-[10px] text-muted-foreground/60">Pinned models are never evicted from VRAM.</p>
                {pinnedError && <p className="text-xs text-destructive">{pinnedError}</p>}
              </>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

// ── Schedule create form (collapsed by default) ───────────────────────────────

function formatScheduleRelative(isoString: string): string {
  const diffMs = Date.now() - new Date(isoString).getTime();
  const diffSecs = Math.floor(diffMs / 1000);
  if (diffSecs < 60) return `${diffSecs}s ago`;
  const diffMins = Math.floor(diffSecs / 60);
  if (diffMins < 60) return `${diffMins}m ago`;
  const diffHours = Math.floor(diffMins / 60);
  if (diffHours < 24) return `${diffHours}h ago`;
  return `${Math.floor(diffHours / 24)}d ago`;
}

// ── Schedule row with inline edit ────────────────────────────────────────────

function ScheduleRow({ schedule, nodes, modelsByNode, onToggle, onSave, onDelete, isLast }: {
  schedule: Schedule;
  nodes: GPUNode[];
  modelsByNode: Record<string, string[]>;
  onToggle: (enabled: boolean) => Promise<void>;
  onSave: (patch: Partial<Omit<Schedule, 'id'>>) => Promise<void>;
  onDelete: () => void;
  isLast?: boolean;
}) {
  const [editing, setEditing] = useState(false);
  const [action, setAction] = useState<Schedule['action']>(schedule.action);
  const [node, setNode] = useState(schedule.node);
  const [selectedModels, setSelectedModels] = useState<string[]>(schedule.models ?? []);
  const [at, setAt] = useState(schedule.at);
  const [days, setDays] = useState<number[]>(schedule.days ?? []);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Union with already-selected models so a schedule saved against a model
  // no longer on this node still shows it (rather than silently dropping it).
  const availableModels = Array.from(new Set([...(modelsByNode[node] ?? []), ...selectedModels]));

  function startEdit() {
    setAction(schedule.action); setNode(schedule.node);
    setSelectedModels(schedule.models ?? []); setAt(schedule.at);
    setDays(schedule.days ?? []); setError(null); setEditing(true);
  }

  async function save() {
    if (!node) { setError('Pick a node'); return; }
    if (!/^\d{2}:\d{2}$/.test(at)) { setError('Time must be HH:MM'); return; }
    if ((action === 'warmup' || action === 'unload') && selectedModels.length === 0) {
      setError('Pick at least one model'); return;
    }
    setSaving(true); setError(null);
    try {
      await onSave({ action, node, models: (action === 'warmup' || action === 'unload') ? selectedModels : undefined, at, days });
      setEditing(false);
    } catch (e: any) { setError(e.message || 'Save failed'); }
    finally { setSaving(false); }
  }

  const s = schedule;
  const sel = `w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground`;

  return (
    <div className="transition-opacity duration-300 first:rounded-t-xl last:rounded-b-xl">
      {/* View row */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between px-4 py-2.5 gap-2 text-sm first:rounded-t-xl last:rounded-b-xl">
        <div className="flex items-center gap-3 flex-wrap min-w-0">
          <Badge variant={s.action === 'warmup' ? 'success' : s.action === 'unload' ? 'destructive' : s.action === 'drain' ? 'warning' : 'muted'}>{s.action}</Badge>
          <span className="font-medium text-foreground">{s.node}</span>
          {s.models && s.models.length > 0 && (
            <span className="font-mono text-xs text-muted-foreground truncate max-w-full sm:max-w-xs md:max-w-md lg:max-w-lg" title={s.models.join(', ')}>{s.models.join(', ')}</span>
          )}
          <span className="text-muted-foreground text-xs whitespace-nowrap">
            {s.at} &middot; {(s.days && s.days.length > 0) ? s.days.map(d => DAYS[d]).join(' ') : 'every day'}
          </span>
          {s.last_run_at ? (
            <span
              className={`flex items-center gap-1 text-xs whitespace-nowrap ${s.last_status === 'error' ? 'text-destructive' : 'text-muted-foreground'}`}
              title={s.last_status === 'error' ? `Last ran ${formatScheduleRelative(s.last_run_at)} - ${s.last_error || 'failed'}` : `Last ran ${formatScheduleRelative(s.last_run_at)}`}
            >
              {s.last_status === 'error' ? <XCircle className="w-3 h-3" /> : <CheckCircle2 className="w-3 h-3 text-success" />}
              ran {formatScheduleRelative(s.last_run_at)}
            </span>
          ) : (
            <span className="text-muted-foreground/50 text-xs whitespace-nowrap">never ran</span>
          )}
        </div>
        <div className="flex items-center gap-1 self-end sm:self-auto shrink-0">
          <button onClick={async () => {
            // Neither this handler nor editSchedule (the underlying update
            // call, in the parent) previously had a try/catch - a failure
            // (e.g. an expired session) became an unhandled promise
            // rejection with zero UI feedback, unlike the row's own Save
            // action, which already surfaces errors inline via `error`.
            try {
              await onToggle(!s.enabled);
            } catch (e: any) {
              setError(e.message || 'Failed to update schedule');
            }
          }} title={s.enabled ? 'Pause' : 'Resume'}
            className="p-1.5 rounded-md text-muted-foreground hover:text-primary hover:bg-primary/10 transition-colors">
            {s.enabled ? <PauseCircle className="w-3.5 h-3.5" /> : <PlayCircle className="w-3.5 h-3.5" />}
          </button>
          <button onClick={editing ? () => setEditing(false) : startEdit} title={editing ? 'Close edit' : 'Edit'}
            className={`p-1.5 rounded-md transition-colors ${editing ? 'text-primary bg-primary/10' : 'text-muted-foreground hover:text-foreground hover:bg-secondary'}`}>
            <Pencil className="w-3.5 h-3.5" />
          </button>
          <button onClick={onDelete} title="Delete"
            className="p-1.5 rounded-md text-muted-foreground hover:text-destructive hover:bg-destructive/10 transition-colors">
            <Trash2 className="w-3.5 h-3.5" />
          </button>
        </div>
      </div>

      {/* Inline edit form - visually attached, distinct bg */}
      {editing && (
        <div className={`border-t border-border bg-secondary/30 px-4 py-4 space-y-3 ${isLast ? 'rounded-b-xl' : ''}`}>
          <div className="grid grid-cols-1 sm:grid-cols-3 gap-3">
            <div>
              <label className="block text-xs font-medium text-muted-foreground mb-1">Action</label>
              <CustomSelect
                value={action}
                onChange={val => setAction(val as Schedule['action'])}
                options={[
                  { value: 'warmup', label: 'Warm up' },
                  { value: 'unload', label: 'Unload' },
                  { value: 'drain', label: 'Drain' },
                  { value: 'undrain', label: 'Undrain' },
                ]}
              />
            </div>
            <div>
              <label className="block text-xs font-medium text-muted-foreground mb-1">Node</label>
              <CustomSelect
                value={node}
                onChange={setNode}
                options={[
                  ...(node && !nodes.some(n => n.name === node) ? [{ value: node, label: `${node} (not found)` }] : []),
                  ...nodes.map(n => ({ value: n.name, label: n.name })),
                ]}
              />
              {node && !nodes.some(n => n.name === node) && (
                <p className="text-[10px] text-destructive mt-1">
                  Node "{node}" is no longer registered. Pick a live node to fix this schedule.
                </p>
              )}
            </div>
            <div>
              <label className="block text-xs font-medium text-muted-foreground mb-1">Time (24h)</label>
              <CustomTimePicker value={at} onChange={setAt} />
            </div>
          </div>
          <div>
            <label className="block text-xs font-medium text-muted-foreground mb-1">Days</label>
            <div className="flex flex-wrap gap-1.5">
              {DAYS.map((d, i) => (
                <button key={d} type="button" onClick={() => setDays(p => p.includes(i) ? p.filter(x => x !== i) : [...p, i].sort())}
                  className={`px-2.5 py-1 text-xs rounded-lg border transition-colors ${days.includes(i) ? 'bg-primary/10 border-primary text-primary' : 'border-border text-muted-foreground hover:bg-secondary'}`}>
                  {d}
                </button>
              ))}
              <span className="text-xs text-muted-foreground/60 self-center ml-1">none = every day</span>
            </div>
          </div>
          {(action === 'warmup' || action === 'unload') && availableModels.length > 0 && (
            <div>
              <label className="block text-xs font-medium text-muted-foreground mb-1">Models</label>
              <ModelPills allModels={availableModels} selected={selectedModels} onChange={setSelectedModels} />
            </div>
          )}
          {error && <p className="text-xs text-destructive">{error}</p>}
          <div className="flex gap-2">
            <button onClick={save} disabled={saving}
              className="px-4 py-1.5 bg-primary text-primary-foreground text-xs font-medium rounded-lg hover:bg-primary/90 disabled:opacity-50">
              {saving ? 'Saving…' : 'Save'}
            </button>
            <button onClick={() => setEditing(false)}
              className="px-4 py-1.5 border border-border text-xs text-muted-foreground rounded-lg hover:bg-secondary">
              Cancel
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

function ScheduleForm({ nodes, modelsByNode, onCreate }: {
  nodes: GPUNode[];
  modelsByNode: Record<string, string[]>;
  onCreate: (s: Omit<Schedule, 'id'>) => Promise<void>;
}) {
  const [open, setOpen] = useState(false);
  const [action, setAction] = useState<Schedule['action']>('warmup');
  const [node, setNode] = useState(nodes[0]?.name ?? '');
  const [selectedModels, setSelectedModels] = useState<string[]>([]);
  const [at, setAt] = useState('08:30');
  const [days, setDays] = useState<number[]>([1, 2, 3, 4, 5]);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const availableModels = modelsByNode[node] ?? [];

  // Keep `node` pointed at a live node: also self-heal if the previously
  // selected node disappears from the list (e.g. removed/renamed) while this
  // form is open, not just when it starts out empty - otherwise the <select>
  // could fall into the same display/state divergence as ScheduleRow (see
  // that component's Node <select> for the full explanation).
  useEffect(() => {
    if (nodes.length > 0 && !nodes.some(n => n.name === node)) setNode(nodes[0].name);
  }, [nodes, node]);

  // Switching nodes changes which models are even valid to pick - drop any
  // selection that doesn't exist on the newly chosen node.
  useEffect(() => {
    setSelectedModels(prev => prev.filter(m => availableModels.includes(m)));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [node]);

  function toggleDay(d: number) {
    setDays(prev => prev.includes(d) ? prev.filter(x => x !== d) : [...prev, d].sort());
  }

  async function submit() {
    if (!node) { setError('Pick a node'); return; }
    if (!/^\d{2}:\d{2}$/.test(at)) { setError('Time must be HH:MM'); return; }
    if ((action === 'warmup' || action === 'unload') && selectedModels.length === 0) {
      setError('Pick at least one model'); return;
    }
    setSaving(true); setError(null);
    try {
      await onCreate({ action, node, models: (action === 'warmup' || action === 'unload') ? selectedModels : undefined, at, days, enabled: true });
      setSelectedModels([]);
      setOpen(false);
    } catch (e: any) { setError(e.message || 'Create failed'); }
    finally { setSaving(false); }
  }

  return (
    <div>
      {!open ? (
        <button onClick={() => setOpen(true)}
          className="flex items-center gap-2 px-3 py-2 text-sm text-muted-foreground border border-dashed border-border rounded-xl hover:border-primary/40 hover:text-foreground transition-colors w-full">
          <Plus className="w-4 h-4" /> New schedule
        </button>
      ) : (
        <div className="bg-card border border-border rounded-xl p-4 space-y-4 shadow-sm">
          <div className="grid grid-cols-1 sm:grid-cols-3 gap-3">
            <div>
              <label className="block text-xs font-medium text-muted-foreground mb-1">Action</label>
              <CustomSelect
                value={action}
                onChange={val => setAction(val as Schedule['action'])}
                options={[
                  { value: 'warmup', label: 'Warm up' },
                  { value: 'unload', label: 'Unload' },
                  { value: 'drain', label: 'Drain' },
                  { value: 'undrain', label: 'Undrain' },
                ]}
              />
            </div>
            <div>
              <label className="block text-xs font-medium text-muted-foreground mb-1">Node</label>
              <CustomSelect
                value={node}
                onChange={setNode}
                options={nodes.map(n => ({ value: n.name, label: n.name }))}
              />
            </div>
            <div>
              <label className="block text-xs font-medium text-muted-foreground mb-1">Time (24h)</label>
              <CustomTimePicker value={at} onChange={setAt} />
            </div>
          </div>

          {(action === 'warmup' || action === 'unload') && availableModels.length > 0 && (
            <div>
              <label className="block text-xs font-medium text-muted-foreground mb-1.5">{action === 'unload' ? 'Models to unload' : 'Models to warm up'}</label>
              <ModelPills allModels={availableModels} selected={selectedModels} onChange={setSelectedModels} />
            </div>
          )}

          <div>
            <label className="block text-xs font-medium text-muted-foreground mb-1">Days</label>
            <div className="flex flex-wrap gap-1.5">
              {DAYS.map((d, i) => (
                <button key={d} type="button" onClick={() => toggleDay(i)}
                  className={`px-2.5 py-1 text-xs rounded-lg border transition-colors ${days.includes(i) ? 'bg-primary/10 border-primary text-primary' : 'border-border text-muted-foreground hover:bg-secondary'}`}>
                  {d}
                </button>
              ))}
              <span className="text-xs text-muted-foreground/60 self-center ml-1">none = every day</span>
            </div>
          </div>

          {error && <p className="text-xs text-destructive">{error}</p>}

          <div className="flex justify-end gap-3 pt-3 border-t border-border/50">
            <button onClick={() => { setOpen(false); setError(null); }}
              className="px-4 py-2 border border-border text-xs text-muted-foreground rounded-lg hover:bg-secondary transition-colors">
              Cancel
            </button>
            <button onClick={submit} disabled={saving}
              className="flex items-center justify-center gap-1 px-4 py-2 bg-primary text-primary-foreground text-xs font-medium rounded-lg hover:bg-primary/90 disabled:opacity-50 transition-colors shadow-sm">
              <Plus className="w-3.5 h-3.5" /> {saving ? 'Adding…' : 'Add Schedule'}
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

// ── Paused schedules collapsible section ─────────────────────────────────────

function PausedSection({ paused, renderRow }: { paused: Schedule[]; renderRow: (s: Schedule, isLast?: boolean) => React.ReactNode }) {
  const [open, setOpen] = useState(false);
  return (
    <div className="bg-card border border-border rounded-xl">
      <button onClick={() => setOpen(p => !p)}
        className="w-full flex items-center justify-between px-4 py-2.5 text-xs text-muted-foreground hover:bg-secondary/40 transition-colors">
        <div className="flex items-center gap-1.5">
          <PauseCircle className="w-3.5 h-3.5" />
          <span>Paused</span>
          <span className="px-1.5 py-0.5 bg-secondary text-muted-foreground rounded text-[10px] font-mono">{paused.length}</span>
        </div>
        <ChevronDown className={`w-3.5 h-3.5 transition-transform duration-200 ${open ? 'rotate-180' : ''}`} />
      </button>
      {open && (
        <div className="border-t border-border divide-y divide-border opacity-60">
          {paused.map((s, i) => renderRow(s, i === paused.length - 1))}
        </div>
      )}
    </div>
  );
}

// ── Main page ─────────────────────────────────────────────────────────────────

export function Warmup() {
  const tz = useTimezone();
  const { demoMode } = useDemoMode();
  const location = useLocation();
  const [nodes, setNodes] = useState<GPUNode[]>([]);
  const [warmup, setWarmup] = useState<Record<string, NodeWarmup>>({});
  const [schedules, setSchedules] = useState<Schedule[]>([]);
  const [decisions, setDecisions] = useState<PredictiveDecision[]>([]);
  const [availableModels, setAvailableModels] = useState<string[]>([]);
  // Which of availableModels actually exist on each node (warm or on-disk) -
  // NodeCard add/pin options must be scoped to this, not the marbor-wide list,
  // or every node's card shows every other node's models as locally present.
  const [modelsByNode, setModelsByNode] = useState<Record<string, string[]>>({});
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [tab, setTab] = useState<'warmup' | 'schedules' | 'predictions'>('warmup');
  const [serverTime, setServerTime] = useState<Date | null>(null);
  const [serverTimezone, setServerTimezone] = useState<string>('');
  const [predictiveEnabled, setPredictiveEnabled] = useState(true);
  const [togglingPredictive, setTogglingPredictive] = useState(false);
  const [predictiveConfirmOpen, setPredictiveConfirmOpen] = useState(false);
  const [scheduleToDelete, setScheduleToDelete] = useState<Schedule | null>(null);
  const [scheduleDeleteError, setScheduleDeleteError] = useState<string | null>(null);
  const [pinging, setPinging] = useState(false);
  const [pingMessage, setPingMessage] = useState<{ text: string; error: boolean } | null>(null);

  // loadRequestId guards against a slower, older poll cycle resolving after
  // a newer one and overwriting fresher state - each call to load() claims a
  // new id and every state-set checks it still owns the latest one, mirroring
  // Settings.tsx's backupListRequestId pattern for the same six-request-deep
  // waterfall problem.
  const loadRequestId = useRef(0);

  const load = useCallback(async (active: boolean) => {
    const requestId = ++loadRequestId.current;
    try {
      const ns = await fetchNodes();
      if (!active || requestId !== loadRequestId.current || currentAppPath() !== '/warmup') return;
      const safeNs = Array.isArray(ns) ? ns : [];
      setNodes(safeNs);
      const w: Record<string, NodeWarmup> = {};
      await Promise.all(safeNs.map(async n => {
        try {
          const res = await getNodeWarmup(n.name);
          if (active && requestId === loadRequestId.current && currentAppPath() === '/warmup') {
            w[n.name] = res;
          }
        } catch {
          if (active && requestId === loadRequestId.current && currentAppPath() === '/warmup') {
            w[n.name] = { enabled: false, models: [] };
          }
        }
      }));
      if (!active || requestId !== loadRequestId.current || currentAppPath() !== '/warmup') return;
      setWarmup(w);
      const schedList = await listSchedules();
      if (!active || requestId !== loadRequestId.current || currentAppPath() !== '/warmup') return;
      setSchedules(schedList || []);
      const decs = await fetchPredictiveDecisions().catch(() => []);
      if (!active || requestId !== loadRequestId.current || currentAppPath() !== '/warmup') return;
      setDecisions(decs);

      const status = await fetchWarmupStatus().catch(() => ({ predictive_engine_enabled: true }));
      if (!active || requestId !== loadRequestId.current || currentAppPath() !== '/warmup') return;
      setPredictiveEnabled(status.predictive_engine_enabled);

      const sys = await fetchSystemInfo().catch(() => null);
      if (!active || requestId !== loadRequestId.current || currentAppPath() !== '/warmup') return;
      if (sys && sys.server_time && sys.timezone) {
        // server_time is wall "YYYY-MM-DD HH:MM:SS" in sys.timezone - convert to
        // a true UTC instant via wall->UTC so Intl with that zone re-emits same wall.
        const wallIso = sys.server_time.replace(' ', 'T');
        const utcIso = wallDateTimeToUtcIso(wallIso, sys.timezone) ?? new Date(sys.server_time.replace(' ', 'T')).toISOString();
        const d = new Date(utcIso);
        if (!isNaN(d.getTime())) setServerTime(d);
        setServerTimezone(sys.timezone);
      }

      if (demoMode) {
        const demoModels = ['llama3.3:8b', 'mistral:7b', 'llama3.3:70b', 'qwen2.5-coder:14b', 'gemma2:9b', 'phi3:medium'];
        setAvailableModels(demoModels);
        setModelsByNode(Object.fromEntries(safeNs.map(n => [n.name, demoModels])));
      } else {
        try {
          const data = await fetchModels();
          if (!active || requestId !== loadRequestId.current || currentAppPath() !== '/warmup') return;
          const entries = data.models || [];
          setAvailableModels(entries.map((m: any) => m.name));
          const byNode: Record<string, string[]> = {};
          for (const m of entries) {
            for (const n of m.nodes || []) {
              (byNode[n.name] ??= []).push(m.name);
            }
          }
          setModelsByNode(byNode);
        } catch {
          if (!active || requestId !== loadRequestId.current || currentAppPath() !== '/warmup') return;
          setAvailableModels([]);
          setModelsByNode({});
        }
      }
      setError(null);
    } catch (e: any) {
      if (!active || requestId !== loadRequestId.current || currentAppPath() !== '/warmup') return;
      setError(e.message || 'Failed to load');
    }
    finally {
      if (active && requestId === loadRequestId.current && currentAppPath() === '/warmup') {
        setLoading(false);
      }
    }
  }, [demoMode, location.pathname]);

  useEffect(() => {
    if (currentAppPath() !== '/warmup') return;
    let active = true;
    // eslint-disable-next-line react-hooks/set-state-in-effect
    load(active);
    if (demoMode) return () => { active = false; };
    // TriggerWarmup (the "Ping warmup" button, POST /warmup/ping) is
    // fire-and-forget on the backend - it returns immediately while the
    // actual per-model pings (and any resulting warmupErrors) run in the
    // background. Without a poll here, a ping's result - or any other
    // keep-warm state change - was only ever visible after a manual reload.
    const interval = setInterval(() => load(active), 10000);
    return () => {
      active = false;
      clearInterval(interval);
    };
  }, [load, demoMode, location.pathname]);

  useEffect(() => {
    if (currentAppPath() !== '/warmup' || !serverTime) return;
    const timer = setInterval(() => {
      if (currentAppPath() === '/warmup') {
        setServerTime(prev => prev ? new Date(prev.getTime() + 1000) : null);
      }
    }, 1000);
    return () => clearInterval(timer);
  }, [serverTime, location.pathname]);

  // Formats the serverTime UTC instant as wall time in `serverTimezone`
  // (configured zone). No browser-local Date getters - Intl with zone.
  const formatServerTime = (d: Date | null) => {
    if (!d) return 'Loading clock...';
    try {
      return formatInTimezone(d.toISOString(), serverTimezone || tz, {
        year: 'numeric',
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
        hour12: false,
      }).replace(',', '');
    } catch {
      const pad = (n: number) => n.toString().padStart(2, '0');
      const yyyy = d.getFullYear();
      const mm = pad(d.getMonth() + 1);
      const dd = pad(d.getDate());
      const hh = pad(d.getHours());
      const min = pad(d.getMinutes());
      const ss = pad(d.getSeconds());
      return `${yyyy}-${mm}-${dd} ${hh}:${min}:${ss}`;
    }
  };

  async function saveWarmup(name: string, nw: NodeWarmup) {
    try {
      const saved = await setNodeWarmup(name, nw);
      setWarmup(prev => ({ ...prev, [name]: saved }));
    } catch (e: any) { alert(e.message || 'Save failed'); }
  }

  async function handleTogglePredictive() {
    setTogglingPredictive(true);
    try {
      const res = await setPredictiveEngine(!predictiveEnabled);
      setPredictiveEnabled(res.predictive_engine_enabled);
    } catch (err: any) {
      alert(err.message || 'Failed to toggle predictive engine');
    } finally {
      setTogglingPredictive(false);
      setPredictiveConfirmOpen(false);
    }
  }

  async function handlePingWarmup() {
    setPinging(true);
    setPingMessage(null);
    try {
      await triggerWarmupPing();
      setPingMessage({ text: 'Warmup triggered.', error: false });
      // The ping itself runs async on the backend (TriggerWarmup returns
      // immediately) - a short delay gives it time to actually reach each
      // node and update warmupErrors before this page's next fetch, instead
      // of waiting on the full 10s poll interval for feedback.
      setTimeout(() => load(true), 3000);
    } catch (e: any) {
      setPingMessage({ text: e.message || 'Failed to trigger warmup', error: true });
    } finally {
      setPinging(false);
      setTimeout(() => setPingMessage(null), 4000);
    }
  }

  async function addSchedule(s: Omit<Schedule, 'id'>) {
    const created = await createSchedule(s);
    setSchedules(prev => [...prev, created]);
  }

  async function removeSchedule(id: string): Promise<boolean> {
    try {
      await deleteSchedule(id);
      setSchedules(prev => prev.filter(s => s.id !== id));
      return true;
    } catch (e: any) {
      setScheduleDeleteError(e.message || 'Delete failed');
      return false;
    }
  }

  async function editSchedule(id: string, patch: Partial<Omit<Schedule, 'id'>>) {
    const updated = await updateSchedule(id, patch);
    setSchedules(prev => prev.map(s => s.id === id ? updated : s));
  }

  return (
    <div className="space-y-4 animate-fade-in max-w-4xl mx-auto">
      {/* Header + tab toggle */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div className="flex items-center gap-2.5">
          <Flame className="w-5 h-5 text-primary" />
          <h1 className="text-lg font-bold text-foreground">Warmup &amp; Scheduling</h1>
          <button onClick={handlePingWarmup} disabled={pinging} title="Manually trigger a warmup pass on every node right now"
            className="flex items-center gap-1.5 px-2.5 py-1 border border-border rounded-lg text-xs font-medium text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors disabled:opacity-50">
            <Zap className="w-3.5 h-3.5" /> {pinging ? 'Pinging…' : 'Ping warmup'}
          </button>
          {pingMessage && (
            <span className={`text-xs ${pingMessage.error ? 'text-destructive' : 'text-success'}`}>{pingMessage.text}</span>
          )}
        </div>
        <div className="flex items-center bg-secondary rounded-lg p-0.5 text-sm w-full sm:w-auto overflow-x-auto no-scrollbar">
          <button onClick={() => setTab('warmup')}
            className={`flex-1 sm:flex-none flex items-center justify-center gap-1 sm:gap-1.5 px-2 sm:px-3 py-1.5 rounded-md font-medium transition-colors whitespace-nowrap min-w-0 ${tab === 'warmup' ? 'bg-card text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'}`}>
            <Flame className="w-3.5 h-3.5 shrink-0" /> Warmup
          </button>
          <button onClick={() => setTab('schedules')}
            className={`flex-1 sm:flex-none flex items-center justify-center gap-1 sm:gap-1.5 px-2 sm:px-3 py-1.5 rounded-md font-medium transition-colors whitespace-nowrap min-w-0 ${tab === 'schedules' ? 'bg-card text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'}`}>
            <Clock className="w-3.5 h-3.5 shrink-0" /> Schedules
            {schedules.length > 0 && (
              <span className="px-1 py-0.5 sm:px-1.5 bg-primary/10 text-primary rounded text-[10px] font-mono shrink-0">{schedules.length}</span>
            )}
          </button>
          <button onClick={() => setTab('predictions')}
            className={`flex-1 sm:flex-none flex items-center justify-center gap-1 sm:gap-1.5 px-2 sm:px-3 py-1.5 rounded-md font-medium transition-colors whitespace-nowrap min-w-0 ${tab === 'predictions' ? 'bg-card text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'}`}>
            <BrainCircuit className="w-3.5 h-3.5" /> Predictions
          </button>
        </div>
      </div>

      {error && <div className="bg-destructive/10 border border-destructive/20 rounded-xl p-3 text-sm text-destructive">{error}</div>}

      {/* Warmup tab */}
      {tab === 'warmup' && (
        <section className="space-y-2">
          {loading ? (
            <p className="text-sm text-muted-foreground">Loading…</p>
          ) : nodes.length === 0 ? (
            <p className="text-sm text-muted-foreground">No nodes registered.</p>
          ) : (
            nodes.map(n => (
              <NodeCard
                key={n.name}
                node={n}
                initial={warmup[n.name] ?? { enabled: false, models: [] }}
                availableModels={modelsByNode[n.name] ?? []}
                onSave={saveWarmup}
              />
            ))
          )}
        </section>
      )}

      {/* Schedules tab */}
      {tab === 'schedules' && (
        <section className="space-y-3">
          <div className="flex flex-col sm:flex-row justify-between items-start sm:items-center bg-secondary/35 border border-border/80 rounded-xl px-4 py-3 gap-2.5 text-xs text-muted-foreground shadow-sm">
            <div className="flex items-center gap-2">
              <div className="relative flex h-2 w-2">
                {predictiveEnabled ? (
                  <>
                    <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-emerald-400 opacity-75"></span>
                    <span className="relative inline-flex rounded-full h-2 w-2 bg-emerald-500"></span>
                  </>
                ) : (
                  <span className="relative inline-flex rounded-full h-2 w-2 bg-amber-500"></span>
                )}
              </div>
              <span className="font-medium text-foreground/80">Server Clock:</span>
              <span className="font-semibold text-foreground font-mono bg-secondary/85 px-2 py-0.5 rounded border border-border/50">
                {serverTime ? `${formatServerTime(serverTime)} ${serverTimezone}` : 'Loading server time…'}
              </span>
            </div>
            <span className="text-[10px] text-muted-foreground/70">
              * Schedules run on server time. Check server/container timezone configuration.
            </span>
          </div>

          {/* Predictive engine disabled banner */}
          {!predictiveEnabled && (
            <div className="flex items-start gap-3 bg-amber-500/10 border border-amber-500/30 rounded-xl px-4 py-3">
              <BrainCircuit className="w-4 h-4 text-amber-700 dark:text-amber-400 shrink-0 mt-0.5" />
              <div>
                <p className="text-xs font-semibold text-amber-600 dark:text-amber-400">Predictive Warmup Engine is disabled</p>
                <p className="text-[11px] text-amber-600/80 dark:text-amber-400/80 mt-0.5">
                  Scheduled predictive warmups will not fire while the engine is off. Manual schedules below still run normally.
                  Enable the engine in the <button onClick={() => setTab('predictions')} className="underline underline-offset-2 hover:text-amber-700 dark:hover:text-amber-400 transition-colors">Predictions</button> tab.
                </p>
              </div>
            </div>
          )}

          {(() => {
            const active = schedules.filter(s => s.enabled);
            const paused = schedules.filter(s => !s.enabled);
            const row = (s: Schedule, isLast?: boolean) => (
              <ScheduleRow
                key={s.id} schedule={s} nodes={nodes} modelsByNode={modelsByNode}
                onToggle={(enabled) => editSchedule(s.id, { enabled })}
                onSave={(patch) => editSchedule(s.id, patch)}
                onDelete={() => { setScheduleDeleteError(null); setScheduleToDelete(s); }}
                isLast={isLast}
              />
            );
            return (
              <>
                {/* Active schedules */}
                {active.length > 0 && (
                  <div className="bg-card border border-border rounded-xl divide-y divide-border">
                    {active.map((s, i) => row(s, i === active.length - 1))}
                  </div>
                )}

                {/* Paused schedules - collapsible */}
                {paused.length > 0 && (
                  <PausedSection paused={paused} renderRow={row} />
                )}

                {schedules.length === 0 && (
                  <p className="text-sm text-muted-foreground">No schedules yet.</p>
                )}
              </>
            );
          })()}
          <ScheduleForm nodes={nodes} modelsByNode={modelsByNode} onCreate={addSchedule} />
        </section>
      )}

      {/* Predictions tab */}
      {tab === 'predictions' && (
        <section className="space-y-4">
          <div className={`bg-card border rounded-xl p-4 flex items-center justify-between shadow-sm transition-colors ${
            predictiveEnabled ? 'border-border' : 'border-amber-500/40 bg-amber-500/5'
          }`}>
            <div className="flex items-center gap-3">
              <BrainCircuit className={`w-5 h-5 shrink-0 ${predictiveEnabled ? 'text-primary' : 'text-amber-700 dark:text-amber-400'}`} />
              <div>
                <h4 className="text-sm font-semibold text-foreground">Predictive Warmup Engine</h4>
                <p className="text-xs text-muted-foreground">
                  Auto-preloads next-likely models in background VRAM based on historical model-transition patterns.
                </p>
                {!predictiveEnabled && (
                  <p className="text-xs text-amber-600 dark:text-amber-400 font-medium mt-1">Engine is paused - no new decisions will be recorded.</p>
                )}
              </div>
            </div>
            <button
              onClick={() => setPredictiveConfirmOpen(true)}
              disabled={togglingPredictive}
              className={`relative inline-flex h-6 w-11 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors duration-200 ease-in-out focus:outline-none focus:ring-2 focus:ring-primary focus:ring-offset-2 focus:ring-offset-background ${
                predictiveEnabled ? 'bg-primary' : 'bg-secondary'
              }`}
            >
              <span
                aria-hidden="true"
                className={`pointer-events-none inline-block h-5 w-5 transform rounded-full bg-white shadow ring-0 transition duration-200 ease-in-out ${
                  predictiveEnabled ? 'translate-x-5' : 'translate-x-0'
                }`}
              />
            </button>
          </div>

          <div className={`space-y-2 transition-opacity duration-200 ${!predictiveEnabled ? 'opacity-50 pointer-events-none' : ''}`}>
            <p className="text-xs text-muted-foreground">
              Last {decisions.length} predictive-warmup decisions. Newest first - this is a log of what the engine actually did on each tick, not a schedule.
              {!predictiveEnabled && <span className="text-amber-800 dark:text-amber-400"> (engine paused - list is frozen)</span>}
            </p>
            {loading ? (
              <p className="text-sm text-muted-foreground">Loading...</p>
            ) : decisions.length === 0 ? (
              <p className="text-sm text-muted-foreground">No predictive decisions recorded yet.</p>
            ) : (
            <div className="bg-card border border-border rounded-xl divide-y divide-border overflow-hidden">
              {[...decisions].reverse().map((d, i) => (
                <div key={i} className="flex flex-col sm:flex-row sm:items-center justify-between gap-2 sm:gap-4 px-4 py-3 text-sm">
                  <div className="flex items-center gap-3 min-w-0">
                    <BrainCircuit className="w-4 h-4 text-primary shrink-0" />
                    <div className="min-w-0">
                      <p className="text-foreground font-medium truncate">
                        {d.predicted_model} <span className="text-muted-foreground font-normal">on</span> {d.node}
                      </p>
                      <p className="text-xs text-muted-foreground truncate">
                        triggered by {d.trigger_model} - seen together {d.transition_count}x at hour {d.hour}
                      </p>
                    </div>
                  </div>
                  <div className="flex items-center gap-2 self-start sm:self-auto mt-1 sm:mt-0 shrink-0">
                    <Badge variant={d.was_already_warm ? 'muted' : d.warmup_triggered ? 'success' : 'muted'} size="sm">
                      {d.was_already_warm ? 'already warm' : d.warmup_triggered ? 'warmup triggered' : 'skipped'}
                    </Badge>
                    <span className="text-xs text-muted-foreground font-mono">
                      {formatTimeInZone(d.timestamp, tz)}
                    </span>
                  </div>
                </div>
              ))}
            </div>
          )}
          </div>
        </section>
      )}

      <Modal
        isOpen={predictiveConfirmOpen}
        onClose={() => setPredictiveConfirmOpen(false)}
        title={predictiveEnabled ? 'Disable Predictive Warmup Engine' : 'Enable Predictive Warmup Engine'}
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Are you sure you want to {predictiveEnabled ? 'disable' : 'enable'} the Predictive Warmup Engine?
          </p>
          <p className="text-xs text-muted-foreground">
            {predictiveEnabled
              ? 'No new predictive warmup decisions will be recorded or acted on across the whole marbor until re-enabled. Manual schedules and live traffic are unaffected.'
              : 'The engine will resume auto-preloading next-likely models in background VRAM based on historical model-transition patterns.'}
          </p>
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setPredictiveConfirmOpen(false)}
              disabled={togglingPredictive}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors disabled:opacity-50"
            >
              Cancel
            </button>
            <button
              onClick={handleTogglePredictive}
              disabled={togglingPredictive}
              className={`px-4 py-2 font-medium rounded-lg text-sm transition-colors shadow-sm disabled:opacity-50 ${
                predictiveEnabled ? 'bg-amber-600 hover:bg-amber-600/90 text-white' : 'bg-primary hover:bg-primary/90 text-primary-foreground'
              }`}
            >
              {predictiveEnabled ? 'Disable Engine' : 'Enable Engine'}
            </button>
          </div>
        </div>
      </Modal>

      <Modal
        isOpen={scheduleToDelete !== null}
        onClose={() => setScheduleToDelete(null)}
        title="Delete Schedule"
        maxWidth="sm"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Are you sure you want to delete this schedule?
          </p>
          <p className="text-xs text-muted-foreground">
            <span className="text-foreground font-semibold">{scheduleToDelete?.action}</span> on{' '}
            <span className="text-foreground font-semibold">{scheduleToDelete?.node}</span> at{' '}
            <span className="text-foreground font-semibold">{scheduleToDelete?.at}</span>
            {scheduleToDelete?.days && scheduleToDelete.days.length > 0
              ? ` (${scheduleToDelete.days.map((d) => DAYS[d]).join(', ')})`
              : ' (every day)'}
            . This action cannot be undone.
          </p>
          {scheduleDeleteError && (
            <p className="text-sm text-destructive">{scheduleDeleteError}</p>
          )}
          <div className="flex justify-end gap-3 pt-4 border-t border-border">
            <button
              onClick={() => setScheduleToDelete(null)}
              className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={async () => {
                if (!scheduleToDelete) return;
                const ok = await removeSchedule(scheduleToDelete.id);
                if (ok) setScheduleToDelete(null);
              }}
              className="px-4 py-2 bg-destructive hover:bg-destructive/90 text-destructive-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
            >
              Delete Schedule
            </button>
          </div>
        </div>
      </Modal>
    </div>
  );
}
