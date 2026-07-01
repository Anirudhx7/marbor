import React, { useState, useEffect, useCallback, useRef } from 'react';
import { Flame, Plus, Trash2, Clock, Server, Pin, ChevronDown, PauseCircle, PlayCircle, Pencil } from 'lucide-react';
import {
  fetchNodes, getNodeWarmup, setNodeWarmup,
  listSchedules, createSchedule, deleteSchedule, updateSchedule,
  fetchModels, getPinned, setPinned,
} from '../lib/api';
import type { GPUNode } from '../types';
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
    setSaving(true); setError(null);
    try {
      await onSave({ action, node, models: action === 'warmup' ? selectedModels : undefined, at, days });
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
          <Badge variant={s.action === 'warmup' ? 'success' : s.action === 'drain' ? 'warning' : 'muted'}>{s.action}</Badge>
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
          <button onClick={startEdit} title="Edit"
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
                <option value="drain">Drain</option>
                <option value="undrain">Undrain</option>
              </select>
            </div>
            <div>
              <label className="block text-xs font-medium text-muted-foreground mb-1">Node</label>
              <select value={node} onChange={e => setNode(e.target.value)} className={sel}>
                {nodes.map(n => <option key={n.name} value={n.name}>{n.name}</option>)}
              </select>
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
          {action === 'warmup' && availableModels.length > 0 && (
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

  useEffect(() => {
    if (!node && nodes.length > 0) setNode(nodes[0].name);
  }, [nodes, node]);

  function toggleDay(d: number) {
    setDays(prev => prev.includes(d) ? prev.filter(x => x !== d) : [...prev, d].sort());
  }

  async function submit() {
    if (!node) { setError('Pick a node'); return; }
    if (!/^\d{2}:\d{2}$/.test(at)) { setError('Time must be HH:MM'); return; }
    setSaving(true); setError(null);
    try {
      await onCreate({ action, node, models: action === 'warmup' ? selectedModels : undefined, at, days, enabled: true });
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

          {action === 'warmup' && availableModels.length > 0 && (
            <div>
              <label className="block text-xs font-medium text-muted-foreground mb-1.5">Models to warm up</label>
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

function PausedSection({ paused, renderRow }: { paused: Schedule[]; renderRow: (s: Schedule) => JSX.Element }) {
  const [open, setOpen] = useState(false);
  const contentRef = useRef<HTMLDivElement>(null);
  const [height, setHeight] = useState(0);

  useEffect(() => {
    if (open && contentRef.current) setHeight(contentRef.current.scrollHeight);
    else setHeight(0);
  }, [open, paused.length]);

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
      <div
        ref={contentRef}
        style={{ maxHeight: height, overflow: 'hidden', transition: 'max-height 250ms ease' }}
        className="border-t border-border"
      >
        <div className="divide-y divide-border opacity-60">
          {paused.map(renderRow)}
        </div>
      </div>
    </div>
  );
}

// ── Main page ─────────────────────────────────────────────────────────────────

export function Warmup() {
  const { demoMode } = useDemoMode();
  const [nodes, setNodes] = useState<GPUNode[]>([]);
  const [warmup, setWarmup] = useState<Record<string, NodeWarmup>>({});
  const [schedules, setSchedules] = useState<Schedule[]>([]);
  const [availableModels, setAvailableModels] = useState<string[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [tab, setTab] = useState<'warmup' | 'schedules'>('warmup');

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

      if (demoMode) {
        setAvailableModels(['llama3.1:8b', 'mistral:7b', 'llama3.1:70b', 'codellama:13b', 'gemma2:9b', 'phi3:medium']);
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

  async function saveWarmup(name: string, nw: NodeWarmup) {
    try {
      const saved = await setNodeWarmup(name, nw);
      setWarmup(prev => ({ ...prev, [name]: saved }));
    } catch (e: any) { alert(e.message || 'Save failed'); }
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
    </div>
  );
}
