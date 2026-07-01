import { useState, useEffect, useCallback } from 'react';
import { Flame, Plus, Trash2, Clock, Server, Pin, ChevronDown } from 'lucide-react';
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

// ── Combined per-node card (warmup + pinned) ──────────────────────────────────

function NodeCard({ node, initial, availableModels, onSave }: {
  node: GPUNode;
  initial: NodeWarmup;
  availableModels: string[];
  onSave: (name: string, nw: NodeWarmup) => Promise<void>;
}) {
  const [enabled, setEnabled] = useState(initial.enabled);
  const [selectedModels, setSelectedModels] = useState<string[]>(initial.models || []);
  const [saving, setSaving] = useState(false);

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

  const shownModels = Array.from(new Set([...availableModels, ...selectedModels]));

  return (
    <div className="bg-card border border-border rounded-xl overflow-hidden">
      {/* Warmup section */}
      <div className="p-4">
        <div className="flex items-center justify-between mb-3">
          <div className="flex items-center gap-2">
            <Server className="w-4 h-4 text-primary" />
            <span className="font-medium text-foreground">{node.name}</span>
            {initial.enabled && <Badge variant="success">warm</Badge>}
          </div>
          <div className="flex items-center gap-3">
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

        {/* Model checkboxes - compact grid */}
        {shownModels.length > 0 ? (
          <div className="flex flex-wrap gap-1.5">
            {shownModels.map(model => (
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
        ) : (
          <p className="text-xs text-muted-foreground">No models available on this node.</p>
        )}
      </div>

      {/* Pinned section - collapsible */}
      <div className="border-t border-border">
        <button
          onClick={() => setShowPinned(p => !p)}
          className="w-full flex items-center justify-between px-4 py-2.5 text-xs text-muted-foreground hover:bg-secondary/40 transition-colors"
        >
          <div className="flex items-center gap-1.5">
            <Pin className="w-3.5 h-3.5" />
            <span>Pinned models</span>
            {pinnedModels.length > 0 && (
              <span className="px-1.5 py-0.5 bg-primary/10 text-primary rounded text-[10px] font-mono">
                {pinnedModels.length}
              </span>
            )}
          </div>
          <ChevronDown className={`w-3.5 h-3.5 transition-transform ${showPinned ? 'rotate-180' : ''}`} />
        </button>

        {showPinned && (
          <div className="px-4 pb-3 space-y-2">
            {pinnedLoading ? (
              <p className="text-xs text-muted-foreground">Loading…</p>
            ) : (
              <>
                <div className="flex flex-wrap gap-1.5 min-h-[24px]">
                  {pinnedModels.length === 0 ? (
                    <span className="text-xs text-muted-foreground">None — pinned models are never evicted from VRAM.</span>
                  ) : (
                    pinnedModels.map(m => (
                      <span key={m} className="flex items-center gap-1 px-2 py-0.5 bg-primary/10 border border-primary/20 text-primary text-xs rounded font-mono">
                        {m}
                        <button onClick={() => setPinnedModels(p => p.filter(x => x !== m))}
                          className="hover:text-destructive transition-colors" aria-label={`Remove ${m}`}>×</button>
                      </span>
                    ))
                  )}
                </div>
                <div className="flex gap-2">
                  <input type="text" value={pinnedInput} onChange={e => setPinnedInput(e.target.value)}
                    onKeyDown={e => { if (e.key === 'Enter') addPinned(); }}
                    placeholder="model:tag" list={`pinned-dl-${node.name}`}
                    className="flex-1 px-2.5 py-1.5 bg-secondary/50 border border-border rounded-lg text-xs text-foreground placeholder:text-muted-foreground/60 focus:outline-none focus:ring-1 focus:ring-primary/30" />
                  <datalist id={`pinned-dl-${node.name}`}>
                    {availableModels.map(m => <option key={m} value={m} />)}
                  </datalist>
                  <button onClick={addPinned}
                    className="px-2.5 py-1.5 bg-secondary border border-border text-xs text-foreground rounded-lg hover:bg-secondary/80 flex items-center gap-1">
                    <Plus className="w-3 h-3" /> Add
                  </button>
                  <button onClick={savePinned} disabled={pinnedSaving}
                    className="px-3 py-1.5 bg-primary text-primary-foreground text-xs font-medium rounded-lg hover:bg-primary/90 disabled:opacity-50">
                    {pinnedSaving ? 'Saving…' : 'Save'}
                  </button>
                </div>
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
    <div className="space-y-6 animate-fade-in max-w-4xl mx-auto">
      <div className="flex items-center gap-3">
        <Flame className="w-5 h-5 text-primary" />
        <div>
          <h1 className="text-lg font-bold text-foreground">Warmup &amp; Scheduling</h1>
          <p className="text-xs text-muted-foreground">Keep models resident in VRAM. Schedule warmup / drain events.</p>
        </div>
      </div>

      {error && <div className="bg-destructive/10 border border-destructive/20 rounded-xl p-3 text-sm text-destructive">{error}</div>}

      {/* Per-node cards */}
      <section className="space-y-2">
        <h2 className="text-xs font-semibold text-muted-foreground uppercase tracking-wide">Per-node warmup</h2>
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

      {/* Schedules */}
      <section className="space-y-2">
        <div className="flex items-center gap-2">
          <Clock className="w-3.5 h-3.5 text-muted-foreground" />
          <h2 className="text-xs font-semibold text-muted-foreground uppercase tracking-wide">Schedules</h2>
        </div>

        {schedules.length > 0 && (
          <div className="bg-card border border-border rounded-xl divide-y divide-border mb-2">
            {schedules.map(s => (
              <div key={s.id} className="flex items-center justify-between px-4 py-2.5 text-sm">
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
                <button onClick={() => removeSchedule(s.id)}
                  className="p-1.5 rounded-md text-muted-foreground hover:text-destructive hover:bg-destructive/10 transition-colors ml-2">
                  <Trash2 className="w-3.5 h-3.5" />
                </button>
              </div>
            ))}
          </div>
        )}

        <ScheduleForm nodes={nodes} availableModels={availableModels} onCreate={addSchedule} />
      </section>
    </div>
  );
}
