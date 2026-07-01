import { useState, useEffect, useCallback } from 'react';
import { Flame, Plus, Trash2, Clock, Server, Pin } from 'lucide-react';
import {
  fetchNodes, getNodeWarmup, setNodeWarmup,
  listSchedules, createSchedule, deleteSchedule,
  fetchModels, getPinned, setPinned,
} from '../lib/api';
import type { GPUNode } from '../types';
import type { Schedule, NodeWarmup } from '../lib/api';
import { Badge } from '../components/Badge';
import { useDemoMode } from '../hooks/useDemoMode';

const DAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];

// ── Per-node warmup row ───────────────────────────────────────────────────────

function WarmupRow({ node, initial, availableModels, onSave }: {
  node: GPUNode;
  initial: NodeWarmup;
  availableModels: string[];
  onSave: (name: string, nw: NodeWarmup) => Promise<void>;
}) {
  const [enabled, setEnabled] = useState(initial.enabled);
  const [selectedModels, setSelectedModels] = useState<string[]>(initial.models || []);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    setEnabled(initial.enabled);
    setSelectedModels(initial.models || []);
  }, [initial]);

  async function save() {
    setSaving(true);
    await onSave(node.name, { enabled, models: selectedModels });
    setSaving(false);
  }

  const shownModels = Array.from(new Set([...availableModels, ...selectedModels]));

  return (
    <div className="bg-card border border-border rounded-xl p-4">
      <div className="flex items-center justify-between mb-3">
        <div className="flex items-center gap-2">
          <Server className="w-4 h-4 text-primary" />
          <span className="font-medium text-foreground">{node.name}</span>
          {initial.enabled && <Badge variant="success">warmup on</Badge>}
        </div>
        <label className="flex items-center gap-2 text-sm text-muted-foreground cursor-pointer">
          <input type="checkbox" checked={enabled} onChange={e => setEnabled(e.target.checked)}
            className="rounded border-border bg-background text-primary focus:ring-primary/20" />
          Enable warmup
        </label>
      </div>
      <label className="block text-xs font-medium text-muted-foreground mb-1.5">
        Models to keep warm
      </label>
      <div className="flex flex-col sm:flex-row gap-3">
        <div className="flex-1 max-h-32 overflow-y-auto bg-secondary/50 border border-border rounded-lg p-2 w-full">
          {shownModels.length === 0 ? (
            <p className="text-xs text-muted-foreground p-1">No models available</p>
          ) : (
            shownModels.map((model) => (
              <label key={model} className="flex items-center gap-2 p-1.5 hover:bg-secondary rounded cursor-pointer">
                <input
                  type="checkbox"
                  checked={selectedModels.includes(model)}
                  onChange={(e) => {
                    if (e.target.checked) {
                      setSelectedModels([...selectedModels, model]);
                    } else {
                      setSelectedModels(selectedModels.filter(m => m !== model));
                    }
                  }}
                  className="rounded border-border bg-background text-primary focus:ring-primary/20"
                />
                <span className="text-sm font-mono text-muted-foreground">{model}</span>
              </label>
            ))
          )}
        </div>
        <div className="flex items-end justify-end shrink-0 w-full sm:w-auto">
          <button onClick={save} disabled={saving}
            className="w-full sm:w-auto px-4 py-2 bg-primary text-primary-foreground text-sm font-medium rounded-lg hover:bg-primary/90 disabled:opacity-50 h-10">
            {saving ? 'Saving…' : 'Save'}
          </button>
        </div>
      </div>
    </div>
  );
}

// ── Per-node pinned-models row ────────────────────────────────────────────────

function PinnedRow({ node, availableModels }: {
  node: GPUNode;
  availableModels: string[];
}) {
  const [pinnedModels, setPinnedModels] = useState<string[]>([]);
  const [input, setInput] = useState('');
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    getPinned(node.name)
      .then(models => { if (!cancelled) { setPinnedModels(models); setLoading(false); } })
      .catch(e => { if (!cancelled) { setError(e.message || 'Failed to load pinned'); setLoading(false); } });
    return () => { cancelled = true; };
  }, [node.name]);

  function addModel() {
    const trimmed = input.trim();
    if (!trimmed || pinnedModels.includes(trimmed)) { setInput(''); return; }
    setPinnedModels(prev => [...prev, trimmed]);
    setInput('');
  }

  function removeModel(model: string) {
    setPinnedModels(prev => prev.filter(m => m !== model));
  }

  async function save() {
    setSaving(true);
    setError(null);
    try {
      await setPinned(node.name, pinnedModels);
    } catch (e: any) {
      setError(e.message || 'Save failed');
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="bg-card border border-border rounded-xl p-4 mt-2">
      <div className="flex items-center gap-2 mb-3">
        <Pin className="w-4 h-4 text-primary" />
        <span className="text-sm font-medium text-foreground">Pinned models</span>
        <span className="text-xs text-muted-foreground ml-1" title="Pinned models are never evicted from VRAM">
          - never evicted from VRAM
        </span>
      </div>

      {loading ? (
        <p className="text-xs text-muted-foreground">Loading…</p>
      ) : (
        <div className="space-y-3">
          <div className="flex flex-wrap gap-1.5 min-h-[28px]">
            {pinnedModels.length === 0 ? (
              <span className="text-xs text-muted-foreground">No pinned models</span>
            ) : (
              pinnedModels.map(model => (
                <span key={model} className="flex items-center gap-1 px-2 py-0.5 bg-primary/10 border border-primary/20 text-primary text-xs rounded-md font-mono">
                  {model}
                  <button
                    onClick={() => removeModel(model)}
                    className="ml-0.5 hover:text-destructive transition-colors leading-none"
                    aria-label={`Remove ${model}`}
                  >
                    ×
                  </button>
                </span>
              ))
            )}
          </div>

          <div className="flex gap-2">
            <input
              type="text"
              value={input}
              onChange={e => setInput(e.target.value)}
              onKeyDown={e => { if (e.key === 'Enter') addModel(); }}
              placeholder="model:tag"
              list={`pinned-models-${node.name}`}
              className="flex-1 px-3 py-1.5 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder:text-muted-foreground/60 focus:outline-none focus:ring-1 focus:ring-primary/30"
            />
            <datalist id={`pinned-models-${node.name}`}>
              {availableModels.map(m => <option key={m} value={m} />)}
            </datalist>
            <button
              onClick={addModel}
              className="px-3 py-1.5 bg-secondary border border-border text-sm text-foreground rounded-lg hover:bg-secondary/80 flex items-center gap-1"
            >
              <Plus className="w-3.5 h-3.5" /> Add
            </button>
            <button
              onClick={save}
              disabled={saving}
              className="px-4 py-1.5 bg-primary text-primary-foreground text-sm font-medium rounded-lg hover:bg-primary/90 disabled:opacity-50"
            >
              {saving ? 'Saving…' : 'Save'}
            </button>
          </div>

          {error && <p className="text-xs text-destructive bg-destructive/10 px-3 py-2 rounded-lg">{error}</p>}
        </div>
      )}
    </div>
  );
}

// ── Schedule create form ──────────────────────────────────────────────────────

function ScheduleForm({ nodes, availableModels, onCreate }: {
  nodes: GPUNode[];
  availableModels: string[];
  onCreate: (s: Omit<Schedule, 'id'>) => Promise<void>;
}) {
  const [action, setAction] = useState<Schedule['action']>('warmup');
  const [node, setNode] = useState(nodes[0]?.name ?? '');
  const [selectedModels, setSelectedModels] = useState<string[]>([]);

  useEffect(() => {
    if (!node && nodes.length > 0) setNode(nodes[0].name);
  }, [nodes, node]);
  const [at, setAt] = useState('08:30');
  const [days, setDays] = useState<number[]>([1, 2, 3, 4, 5]);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

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
    } catch (e: any) { setError(e.message || 'Create failed'); }
    finally { setSaving(false); }
  }

  return (
    <div className="bg-card border border-border rounded-xl p-4 space-y-3">
      <div className="grid grid-cols-1 sm:grid-cols-4 gap-3">
        <div>
          <label className="block text-xs font-medium text-muted-foreground mb-1.5">Action</label>
          <select value={action} onChange={e => setAction(e.target.value as Schedule['action'])}
            className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground">
            <option value="warmup">Warm up</option>
            <option value="drain">Drain</option>
            <option value="undrain">Undrain</option>
          </select>
        </div>
        <div>
          <label className="block text-xs font-medium text-muted-foreground mb-1.5">Node</label>
          <select value={node} onChange={e => setNode(e.target.value)}
            className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground">
            {nodes.map(n => <option key={n.name} value={n.name}>{n.name}</option>)}
          </select>
        </div>
        <div>
          <label className="block text-xs font-medium text-muted-foreground mb-1.5">Time (24h)</label>
          <input type="time" value={at} onChange={e => setAt(e.target.value)}
            className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground" />
        </div>
        <div className="flex items-end">
          <button onClick={submit} disabled={saving}
            className="w-full flex items-center justify-center gap-2 px-3 py-2 bg-primary text-primary-foreground text-sm font-medium rounded-lg hover:bg-primary/90 disabled:opacity-50">
            <Plus className="w-4 h-4" /> {saving ? 'Adding…' : 'Add'}
          </button>
        </div>
      </div>
      {action === 'warmup' && (
        <div>
          <label className="block text-xs font-medium text-muted-foreground mb-1.5">Models to warm up</label>
          <div className="max-h-32 overflow-y-auto bg-secondary/50 border border-border rounded-lg p-2">
            {availableModels.length === 0 ? (
              <p className="text-xs text-muted-foreground p-1">No models available</p>
            ) : (
              availableModels.map((model) => (
                <label key={model} className="flex items-center gap-2 p-1.5 hover:bg-secondary rounded cursor-pointer">
                  <input
                    type="checkbox"
                    checked={selectedModels.includes(model)}
                    onChange={(e) => {
                      if (e.target.checked) {
                        setSelectedModels([...selectedModels, model]);
                      } else {
                        setSelectedModels(selectedModels.filter(m => m !== model));
                      }
                    }}
                    className="rounded border-border bg-background text-primary focus:ring-primary/20"
                  />
                  <span className="text-sm font-mono text-muted-foreground">{model}</span>
                </label>
              ))
            )}
          </div>
        </div>
      )}
      <div>
        <label className="block text-xs font-medium text-muted-foreground mb-1.5">Days</label>
        <div className="flex flex-wrap gap-1.5">
          {DAYS.map((d, i) => (
            <button key={d} onClick={() => toggleDay(i)}
              className={`px-2.5 py-1 text-xs rounded-lg border transition-colors ${days.includes(i) ? 'bg-primary/10 border-primary text-primary' : 'border-border text-muted-foreground hover:bg-secondary'}`}>
              {d}
            </button>
          ))}
          <span className="text-xs text-muted-foreground/60 self-center ml-1">(none = every day)</span>
        </div>
      </div>
      {error && <p className="text-xs text-destructive bg-destructive/10 px-3 py-2 rounded-lg">{error}</p>}
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

  return (
    <div className="space-y-8 animate-fade-in max-w-5xl mx-auto">
      <div className="flex items-center gap-3">
        <Flame className="w-6 h-6 text-primary" />
        <div>
          <h1 className="text-xl font-bold text-foreground">Warmup &amp; Scheduling</h1>
          <p className="text-xs text-muted-foreground">Keep models resident in VRAM and schedule warmup / drain so users never hit a cold start.</p>
        </div>
      </div>

      {error && <div className="bg-destructive/10 border border-destructive/20 rounded-xl p-4 text-sm text-destructive">{error}</div>}

      <section className="space-y-3">
        <h2 className="text-sm font-semibold text-foreground">Per-node warmup</h2>
        {loading ? (
          <p className="text-sm text-muted-foreground">Loading…</p>
        ) : nodes.length === 0 ? (
          <p className="text-sm text-muted-foreground">No nodes registered.</p>
        ) : (
          nodes.map(n => (
            <div key={n.name} className="space-y-0">
              <WarmupRow node={n} initial={warmup[n.name] ?? { enabled: false, models: [] }} availableModels={availableModels} onSave={saveWarmup} />
              <PinnedRow node={n} availableModels={availableModels} />
            </div>
          ))
        )}
      </section>

      <section className="space-y-3">
        <div className="flex items-center gap-2">
          <Clock className="w-4 h-4 text-primary" />
          <h2 className="text-sm font-semibold text-foreground">Schedules</h2>
        </div>
        <ScheduleForm nodes={nodes} availableModels={availableModels} onCreate={addSchedule} />
        {schedules.length === 0 ? (
          <p className="text-sm text-muted-foreground">No schedules yet.</p>
        ) : (
          <div className="bg-card border border-border rounded-xl divide-y divide-border">
            {schedules.map(s => (
              <div key={s.id} className="flex items-center justify-between px-4 py-3 text-sm">
                <div className="flex items-center gap-3">
                  <Badge variant={s.action === 'warmup' ? 'success' : s.action === 'drain' ? 'warning' : 'muted'}>{s.action}</Badge>
                  <span className="font-medium text-foreground">{s.node}</span>
                  {s.models && s.models.length > 0 && <span className="font-mono text-xs text-muted-foreground">{s.models.join(', ')}</span>}
                  <span className="text-muted-foreground">at <strong className="text-foreground">{s.at}</strong></span>
                  <span className="text-xs text-muted-foreground">{(s.days && s.days.length > 0) ? s.days.map(d => DAYS[d]).join(' ') : 'every day'}</span>
                </div>
                <button onClick={() => removeSchedule(s.id)} className="p-1.5 rounded-md text-destructive hover:bg-destructive/10">
                  <Trash2 className="w-4 h-4" />
                </button>
              </div>
            ))}
          </div>
        )}
      </section>
    </div>
  );
}
