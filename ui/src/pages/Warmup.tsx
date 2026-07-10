import React, { useState, useEffect, useCallback } from 'react';
import { Flame, Plus, Trash2, Clock, Server, Pin, ChevronDown, PauseCircle, PlayCircle, Pencil, BrainCircuit } from 'lucide-react';
import {
  fetchNodes, getNodeWarmup, setNodeWarmup,
  listSchedules, createSchedule, deleteSchedule, updateSchedule,
  fetchModels, getPinned, setPinned, fetchSystemInfo, fetchPredictiveDecisions,
  fetchWarmupStatus, setPredictiveEngine,
} from '../lib/api';
import type { GPUNode, PredictiveDecision } from '../types';
import type { Schedule, NodeWarmup } from '../lib/api';
import { Badge } from '../components/Badge';
import { useDemoMode } from '../hooks/useDemoMode';

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

  // Pinned state
  const [pinnedModels, setPinnedModels] = useState<string[]>([]);
  const [pinnedInput, setPinnedInput] = useState('');
  const [pinnedLoading, setPinnedLoading] = useState(true);
  const [pinnedSaving, setPinnedSaving] = useState(false);
  const [pinnedError, setPinnedError] = useState<string | null>(null);
  const [showPinned, setShowPinned] = useState(false);

  useEffect(() => {
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
  // Models that are pinned but not in availableModels (custom entries)
  const extraPinned = pinnedModels.filter(m => !availableModels.includes(m));
  const allPinnedModels = Array.from(new Set([...availableModels, ...extraPinned]));

  return (
    <div className="bg-card border border-border rounded-xl overflow-hidden">
      {/* Node header */}
      <div className="flex items-center justify-between px-4 py-3">
        <div className="flex items-center gap-2">
          <Server className="w-4 h-4 text-primary" />
          <span className="font-medium text-foreground">{node.name}</span>
          {initial.enabled && <Badge variant="success">warm</Badge>}
          {selectedModels.length > 0 && (
            <span className="text-xs text-muted-foreground font-mono">{selectedModels.length} model{selectedModels.length !== 1 ? 's' : ''}</span>
          )}
        </div>
        <div className="flex items-center gap-2">
          <label className="flex items-center gap-1.5 text-xs text-muted-foreground cursor-pointer select-none">
            <input type="checkbox" checked={enabled} onChange={e => setEnabled(e.target.checked)}
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
            <ModelPills allModels={allModels} selected={selectedModels} onChange={setSelectedModels} />
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

// ── Schedule row with inline edit ────────────────────────────────────────────

function ScheduleRow({ schedule, nodes, availableModels, onToggle, onSave, onDelete }: {
  schedule: Schedule;
  nodes: GPUNode[];
  availableModels: string[];
  onToggle: (enabled: boolean) => void;
  onSave: (patch: Partial<Omit<Schedule, 'id'>>) => Promise<void>;
  onDelete: () => void;
}) {
  const [editing, setEditing] = useState(false);
  const [action, setAction] = useState<Schedule['action']>(schedule.action);
  const [node, setNode] = useState(schedule.node);
  const [selectedModels, setSelectedModels] = useState<string[]>(schedule.models ?? []);
  const [at, setAt] = useState(schedule.at);
  const [days, setDays] = useState<number[]>(schedule.days ?? []);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

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
    <div className="overflow-hidden transition-opacity duration-300">
      {/* View row */}
      <div className="flex items-center justify-between px-4 py-2.5 text-sm">
        <div className="flex items-center gap-3 flex-wrap">
          <Badge variant={s.action === 'warmup' ? 'success' : s.action === 'unload' ? 'destructive' : s.action === 'drain' ? 'warning' : 'muted'}>{s.action}</Badge>
          <span className="font-medium text-foreground">{s.node}</span>
          {s.models && s.models.length > 0 && (
            <span className="font-mono text-xs text-muted-foreground">{s.models.join(', ')}</span>
          )}
          <span className="text-muted-foreground text-xs">
            {s.at} &middot; {(s.days && s.days.length > 0) ? s.days.map(d => DAYS[d]).join(' ') : 'every day'}
          </span>
        </div>
        <div className="flex items-center gap-1 ml-2 shrink-0">
          <button onClick={() => onToggle(!s.enabled)} title={s.enabled ? 'Pause' : 'Resume'}
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

      {/* Inline edit form — visually attached, distinct bg */}
      {editing && (
        <div className="border-t border-border bg-secondary/30 px-4 py-4 space-y-3">
          <div className="grid grid-cols-1 sm:grid-cols-3 gap-3">
            <div>
              <label className="block text-xs font-medium text-muted-foreground mb-1">Action</label>
              <select value={action} onChange={e => setAction(e.target.value as Schedule['action'])} className={sel}>
                <option value="warmup">Warm up</option>
                <option value="unload">Unload</option>
                <option value="drain">Drain</option>
                <option value="undrain">Undrain</option>
              </select>
            </div>
            <div>
              <label className="block text-xs font-medium text-muted-foreground mb-1">Node</label>
              <select value={node} onChange={e => setNode(e.target.value)} className={sel}>
                {/* If the schedule's stored node is no longer registered (renamed/removed),
                    render it as an explicit option so the <select>'s displayed value always
                    matches the `node` state that will actually be submitted. Without this,
                    the browser silently falls back to visually showing the first real
                    <option> while `node` state still holds the stale name — so Save appears
                    to target a live node but actually submits the dead one and gets
                    rejected by the backend. */}
                {node && !nodes.some(n => n.name === node) && (
                  <option value={node}>{node} (not found)</option>
                )}
                {nodes.map(n => <option key={n.name} value={n.name}>{n.name}</option>)}
              </select>
              {node && !nodes.some(n => n.name === node) && (
                <p className="text-[10px] text-destructive mt-1">
                  Node "{node}" is no longer registered. Pick a live node to fix this schedule.
                </p>
              )}
            </div>
            <div>
              <label className="block text-xs font-medium text-muted-foreground mb-1">Time (24h)</label>
              <input type="time" value={at} onChange={e => setAt(e.target.value)} className={sel} />
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

function ScheduleForm({ nodes, availableModels, onCreate }: {
  nodes: GPUNode[];
  availableModels: string[];
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

  // Keep `node` pointed at a live node: also self-heal if the previously
  // selected node disappears from the list (e.g. removed/renamed) while this
  // form is open, not just when it starts out empty — otherwise the <select>
  // could fall into the same display/state divergence as ScheduleRow (see
  // that component's Node <select> for the full explanation).
  useEffect(() => {
    if (nodes.length > 0 && !nodes.some(n => n.name === node)) setNode(nodes[0].name);
  }, [nodes, node]);

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
        <div className="bg-card border border-border rounded-xl p-4 space-y-3">
          <div className="grid grid-cols-1 sm:grid-cols-4 gap-3">
            <div>
              <label className="block text-xs font-medium text-muted-foreground mb-1">Action</label>
              <select value={action} onChange={e => setAction(e.target.value as Schedule['action'])}
                className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground">
                <option value="warmup">Warm up</option>
                <option value="unload">Unload</option>
                <option value="drain">Drain</option>
                <option value="undrain">Undrain</option>
              </select>
            </div>
            <div>
              <label className="block text-xs font-medium text-muted-foreground mb-1">Node</label>
              <select value={node} onChange={e => setNode(e.target.value)}
                className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground">
                {nodes.map(n => <option key={n.name} value={n.name}>{n.name}</option>)}
              </select>
            </div>
            <div>
              <label className="block text-xs font-medium text-muted-foreground mb-1">Time (24h)</label>
              <input type="time" value={at} onChange={e => setAt(e.target.value)}
                className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground" />
            </div>
            <div className="flex items-end gap-2">
              <button onClick={() => { setOpen(false); setError(null); }}
                className="flex-1 px-3 py-2 border border-border text-sm text-muted-foreground rounded-lg hover:bg-secondary">
                Cancel
              </button>
              <button onClick={submit} disabled={saving}
                className="flex-1 flex items-center justify-center gap-1 px-3 py-2 bg-primary text-primary-foreground text-sm font-medium rounded-lg hover:bg-primary/90 disabled:opacity-50">
                <Plus className="w-3.5 h-3.5" /> {saving ? 'Adding…' : 'Add'}
              </button>
            </div>
          </div>

          {(action === 'warmup' || action === 'unload') && availableModels.length > 0 && (
            <div>
              <label className="block text-xs font-medium text-muted-foreground mb-1.5">{action === 'unload' ? 'Models to unload' : 'Models to warm up'}</label>
              <div className="flex flex-wrap gap-1.5">
                {availableModels.map(model => (
                  <label key={model}
                    className={`flex items-center gap-1.5 px-2 py-1 rounded-md border text-xs font-mono cursor-pointer transition-colors ${
                      selectedModels.includes(model)
                        ? 'bg-primary/10 border-primary/40 text-primary'
                        : 'border-border text-muted-foreground hover:bg-secondary'
                    }`}>
                    <input type="checkbox" checked={selectedModels.includes(model)}
                      onChange={e => {
                        if (e.target.checked) setSelectedModels(p => [...p, model]);
                        else setSelectedModels(p => p.filter(m => m !== model));
                      }}
                      className="sr-only" />
                    {model}
                  </label>
                ))}
              </div>
            </div>
          )}

          <div>
            <label className="block text-xs font-medium text-muted-foreground mb-1">Days</label>
            <div className="flex flex-wrap gap-1.5">
              {DAYS.map((d, i) => (
                <button key={d} onClick={() => toggleDay(i)}
                  className={`px-2.5 py-1 text-xs rounded-lg border transition-colors ${days.includes(i) ? 'bg-primary/10 border-primary text-primary' : 'border-border text-muted-foreground hover:bg-secondary'}`}>
                  {d}
                </button>
              ))}
              <span className="text-xs text-muted-foreground/60 self-center ml-1">none = every day</span>
            </div>
          </div>

          {error && <p className="text-xs text-destructive">{error}</p>}
        </div>
      )}
    </div>
  );
}

// ── Paused schedules collapsible section ─────────────────────────────────────

function PausedSection({ paused, renderRow }: { paused: Schedule[]; renderRow: (s: Schedule) => React.ReactNode }) {
  const [open, setOpen] = useState(false);
  return (
    <div className="bg-card border border-border rounded-xl overflow-hidden">
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
          {paused.map(renderRow)}
        </div>
      )}
    </div>
  );
}

// ── Main page ─────────────────────────────────────────────────────────────────

export function Warmup() {
  const { demoMode } = useDemoMode();
  const [nodes, setNodes] = useState<GPUNode[]>([]);
  const [warmup, setWarmup] = useState<Record<string, NodeWarmup>>({});
  const [schedules, setSchedules] = useState<Schedule[]>([]);
  const [decisions, setDecisions] = useState<PredictiveDecision[]>([]);
  const [availableModels, setAvailableModels] = useState<string[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [tab, setTab] = useState<'warmup' | 'schedules' | 'predictions'>('warmup');
  const [serverTime, setServerTime] = useState<Date | null>(null);
  const [serverTimezone, setServerTimezone] = useState<string>('');
  const [predictiveEnabled, setPredictiveEnabled] = useState(true);
  const [togglingPredictive, setTogglingPredictive] = useState(false);

  const load = useCallback(async () => {
    try {
      const ns = await fetchNodes();
      setNodes(ns);
      const w: Record<string, NodeWarmup> = {};
      await Promise.all(ns.map(async n => {
        try { w[n.name] = await getNodeWarmup(n.name); } catch { w[n.name] = { enabled: false, models: [] }; }
      }));
      setWarmup(w);
      setSchedules(await listSchedules());
      setDecisions(await fetchPredictiveDecisions().catch(() => []));

      const status = await fetchWarmupStatus().catch(() => ({ predictive_engine_enabled: true }));
      setPredictiveEnabled(status.predictive_engine_enabled);

      const sys = await fetchSystemInfo().catch(() => null);
      if (sys && sys.server_time && sys.timezone) {
        const parts = sys.server_time.split(' ');
        if (parts.length === 2) {
          const dParts = parts[0].split('-');
          const tParts = parts[1].split(':');
          const date = new Date(
            parseInt(dParts[0], 10),
            parseInt(dParts[1], 10) - 1,
            parseInt(dParts[2], 10),
            parseInt(tParts[0], 10),
            parseInt(tParts[1], 10),
            parseInt(tParts[2], 10)
          );
          setServerTime(date);
        }
        setServerTimezone(sys.timezone);
      }

      if (demoMode) {
        setAvailableModels(['llama3.3:8b', 'mistral:7b', 'llama3.3:70b', 'qwen2.5-coder:14b', 'gemma2:9b', 'phi3:medium']);
      } else {
        try {
          const data = await fetchModels();
          setAvailableModels((data.models || []).map((m: any) => m.name));
        } catch {
          setAvailableModels([]);
        }
      }
      setError(null);
    } catch (e: any) { setError(e.message || 'Failed to load'); }
    finally { setLoading(false); }
  }, [demoMode]);

  useEffect(() => { load(); }, [load]);

  useEffect(() => {
    if (!serverTime) return;
    const timer = setInterval(() => {
      setServerTime(prev => prev ? new Date(prev.getTime() + 1000) : null);
    }, 1000);
    return () => clearInterval(timer);
  }, [serverTime]);

  const formatServerTime = (d: Date | null) => {
    if (!d) return 'Loading clock...';
    const pad = (n: number) => n.toString().padStart(2, '0');
    const yyyy = d.getFullYear();
    const mm = pad(d.getMonth() + 1);
    const dd = pad(d.getDate());
    const hh = pad(d.getHours());
    const min = pad(d.getMinutes());
    const ss = pad(d.getSeconds());
    return `${yyyy}-${mm}-${dd} ${hh}:${min}:${ss}`;
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
    }
  }

  async function addSchedule(s: Omit<Schedule, 'id'>) {
    const created = await createSchedule(s);
    setSchedules(prev => [...prev, created]);
  }

  async function removeSchedule(id: string) {
    try { await deleteSchedule(id); setSchedules(prev => prev.filter(s => s.id !== id)); }
    catch (e: any) { alert(e.message || 'Delete failed'); }
  }

  async function editSchedule(id: string, patch: Partial<Omit<Schedule, 'id'>>) {
    const updated = await updateSchedule(id, patch);
    setSchedules(prev => prev.map(s => s.id === id ? updated : s));
  }

  return (
    <div className="space-y-4 animate-fade-in max-w-4xl mx-auto">
      {/* Header + tab toggle */}
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-2.5">
          <Flame className="w-5 h-5 text-primary" />
          <h1 className="text-lg font-bold text-foreground">Warmup &amp; Scheduling</h1>
        </div>
        <div className="flex items-center bg-secondary rounded-lg p-0.5 text-sm">
          <button onClick={() => setTab('warmup')}
            className={`flex items-center gap-1.5 px-3 py-1.5 rounded-md font-medium transition-colors ${tab === 'warmup' ? 'bg-card text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'}`}>
            <Flame className="w-3.5 h-3.5" /> Warmup
          </button>
          <button onClick={() => setTab('schedules')}
            className={`flex items-center gap-1.5 px-3 py-1.5 rounded-md font-medium transition-colors ${tab === 'schedules' ? 'bg-card text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'}`}>
            <Clock className="w-3.5 h-3.5" /> Schedules
            {schedules.length > 0 && (
              <span className="px-1.5 py-0.5 bg-primary/10 text-primary rounded text-[10px] font-mono">{schedules.length}</span>
            )}
          </button>
          <button onClick={() => setTab('predictions')}
            className={`flex items-center gap-1.5 px-3 py-1.5 rounded-md font-medium transition-colors ${tab === 'predictions' ? 'bg-card text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'}`}>
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
                availableModels={availableModels}
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
              <BrainCircuit className="w-4 h-4 text-amber-500 shrink-0 mt-0.5" />
              <div>
                <p className="text-xs font-semibold text-amber-600 dark:text-amber-400">Predictive Warmup Engine is disabled</p>
                <p className="text-[11px] text-amber-600/80 dark:text-amber-400/80 mt-0.5">
                  Scheduled predictive warmups will not fire while the engine is off. Manual schedules below still run normally.
                  Enable the engine in the <button onClick={() => setTab('predictions')} className="underline underline-offset-2 hover:text-amber-500 transition-colors">Predictions</button> tab.
                </p>
              </div>
            </div>
          )}

          {(() => {
            const active = schedules.filter(s => s.enabled);
            const paused = schedules.filter(s => !s.enabled);
            const row = (s: Schedule) => (
              <ScheduleRow
                key={s.id} schedule={s} nodes={nodes} availableModels={availableModels}
                onToggle={(enabled) => editSchedule(s.id, { enabled })}
                onSave={(patch) => editSchedule(s.id, patch)}
                onDelete={() => removeSchedule(s.id)}
              />
            );
            return (
              <>
                {/* Active schedules */}
                {active.length > 0 && (
                  <div className="bg-card border border-border rounded-xl divide-y divide-border overflow-hidden">
                    {active.map(row)}
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
          <ScheduleForm nodes={nodes} availableModels={availableModels} onCreate={addSchedule} />
        </section>
      )}

      {/* Predictions tab */}
      {tab === 'predictions' && (
        <section className="space-y-4">
          <div className={`bg-card border rounded-xl p-4 flex items-center justify-between shadow-sm transition-colors ${
            predictiveEnabled ? 'border-border' : 'border-amber-500/40 bg-amber-500/5'
          }`}>
            <div className="flex items-center gap-3">
              <BrainCircuit className={`w-5 h-5 shrink-0 ${predictiveEnabled ? 'text-primary' : 'text-amber-500'}`} />
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
              onClick={handleTogglePredictive}
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
              {!predictiveEnabled && <span className="text-amber-500/80"> (engine paused - list is frozen)</span>}
            </p>
            {loading ? (
              <p className="text-sm text-muted-foreground">Loading...</p>
            ) : decisions.length === 0 ? (
              <p className="text-sm text-muted-foreground">No predictive decisions recorded yet.</p>
            ) : (
            <div className="bg-card border border-border rounded-xl divide-y divide-border overflow-hidden">
              {[...decisions].reverse().map((d, i) => (
                <div key={i} className="flex items-center justify-between gap-4 px-4 py-3 text-sm">
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
                  <div className="flex items-center gap-2 shrink-0">
                    <Badge variant={d.was_already_warm ? 'muted' : d.warmup_triggered ? 'success' : 'muted'} size="sm">
                      {d.was_already_warm ? 'already warm' : d.warmup_triggered ? 'warmup triggered' : 'skipped'}
                    </Badge>
                    <span className="text-xs text-muted-foreground font-mono">
                      {new Date(d.timestamp).toLocaleTimeString()}
                    </span>
                  </div>
                </div>
              ))}
            </div>
          )}
          </div>
        </section>
      )}
    </div>
  );
}
