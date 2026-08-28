import { useState, useEffect, useCallback, useRef } from 'react';
import { useLocation } from 'react-router-dom';
import { Activity as ActivityIcon, Search, RefreshCw, Eye, Calendar, User, Globe, Filter, AlertCircle, X, Server, Flame, BrainCircuit } from 'lucide-react';
import { fetchSystemAuditFiltered, fetchPredictiveDecisions } from '../lib/api';
import type { SystemAuditEntry, PredictiveDecision } from '../types';
import { Modal } from '../components/Modal';
import { CustomSelect } from '../components/Select';
import { CustomDateTimePicker } from '../components/DateTimePicker';
import { ClearableInput, FilterField } from '../components/FilterField';
import { currentAppPath } from '../hooks/useDemoMode';
import { toActivityKind, getActivityKindLabel, getActivityKindColor, type ActivityKind } from '../lib/activityKind';
import { useTimezone } from '../hooks/useTimezone';
import { formatDateTimeInZone, formatInTimezone, wallDateTimeToUtcIso } from '../lib/time';

const AUTO_REFRESH_INTERVAL_MS = 30_000;
const PAGE_LIMIT = 100;

function toPickerValue(rfc3339: string, tz: string): string {
  try {
    const d = new Date(rfc3339);
    if (isNaN(d.getTime())) return '';
    const tzResolved = tz && tz !== 'Local' ? tz : undefined;
    // en-CA gives YYYY-MM-DD HH:MM wall in tz
    const parts = new Intl.DateTimeFormat('en-CA', { timeZone: tzResolved, year:'numeric', month:'2-digit', day:'2-digit', hour:'2-digit', minute:'2-digit', hour12:false }).formatToParts(d);
    const m: Record<string,string> = {};
    for (const p of parts) if (p.type !== 'literal') m[p.type]=p.value;
    // en-CA hour may be "24" at midnight - normalize to "00"
    const hh = m.hour === '24' ? '00' : m.hour;
    return `${m.year}-${m.month}-${m.day}T${hh}:${m.minute}`;
  } catch { return ''; }
}

function fromPickerValue(v: string, tz: string): string {
  if (!v) return '';
  const iso = wallDateTimeToUtcIso(v, tz);
  if (iso) return iso;
  try { const d=new Date(v); return isNaN(d.getTime())?'':d.toISOString(); } catch {return '';}
}

type DatePreset = 'all' | '1h' | '24h' | '7d' | '30d' | 'custom';

function formatDateTime(isoString: string, tz: string): string {
  return formatDateTimeInZone(isoString, tz);
}

function formatUpdatedTime(d: Date, tz: string): string {
  try {
    return formatInTimezone(d.toISOString(), tz, { hour:'2-digit', minute:'2-digit', second:'2-digit' });
  } catch { return d.toISOString().slice(11,19); }
}

function getActionLabel(action: string): string {
  return action
    .split('_')
    .map((word) => word.charAt(0).toUpperCase() + word.slice(1))
    .join(' ');
}

export function Activity() {
  const tz = useTimezone();
  const location = useLocation();

  const [entries, setEntries] = useState<SystemAuditEntry[]>([]);
  const [decisions, setDecisions] = useState<PredictiveDecision[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [hasMore, setHasMore] = useState(false);
  const [searchQuery, setSearchQuery] = useState('');
  const [kindFilter, setKindFilter] = useState<ActivityKind | 'all'>('all');
  const [actionFilter, setActionFilter] = useState('all');
  const [userFilter, setUserFilter] = useState('all');
  const [targetInput, setTargetInput] = useState('');
  const [sourceIpInput, setSourceIpInput] = useState('');
  const [debouncedTarget, setDebouncedTarget] = useState('');
  const [debouncedSourceIp, setDebouncedSourceIp] = useState('');
  const [preset, setPreset] = useState<DatePreset>('all');
  const [fromPicker, setFromPicker] = useState('');
  const [toPicker, setToPicker] = useState('');
  const [selectedEntry, setSelectedEntry] = useState<SystemAuditEntry | null>(null);
  const [refreshSpin, setRefreshSpin] = useState(false);
  const [lastRefreshed, setLastRefreshed] = useState<Date | null>(null);
  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null);

  useEffect(() => {
    const t = setTimeout(() => setDebouncedTarget(targetInput), 250);
    return () => clearTimeout(t);
  }, [targetInput]);
  useEffect(() => {
    const t = setTimeout(() => setDebouncedSourceIp(sourceIpInput), 250);
    return () => clearTimeout(t);
  }, [sourceIpInput]);

  useEffect(() => {
    if (preset === 'all') {
      setFromPicker('');
      setToPicker('');
      return;
    }
    if (preset === 'custom') return; // pickers are the source of truth here
    const now = new Date();
    const presetHours: Record<'1h' | '24h' | '7d' | '30d', number> = { '1h': 1, '24h': 24, '7d': 24 * 7, '30d': 24 * 30 };
    const from = new Date(now.getTime() - presetHours[preset] * 3600_000);
    setFromPicker(toPickerValue(from.toISOString(), tz));
    setToPicker(toPickerValue(now.toISOString(), tz));
  }, [preset, tz]);

  // A quick-range preset and a hand-picked custom range are mutually
  // exclusive, same relationship as Requests.tsx's sincePreset/sinceInput:
  // typing into From or To always wins and switches the Quick range select
  // into 'custom' (disabling it, same as ClearableInput's own affordance
  // pattern), and clearing both fields (via their own inline "x") falls back
  // to 'all' so the select's displayed value matches what's actually being
  // sent to the server.
  const handleFromPickerChange = (val: string) => {
    setFromPicker(val);
    setPreset(val || toPicker ? 'custom' : 'all');
  };
  const handleToPickerChange = (val: string) => {
    setToPicker(val);
    setPreset(val || fromPicker ? 'custom' : 'all');
  };

  const buildFilter = useCallback((before?: string, beforeId?: number) => {
    const f: any = { limit: PAGE_LIMIT };
    const fromRfc = fromPicker ? fromPickerValue(fromPicker, tz) : '';
    const toRfc = toPicker ? fromPickerValue(toPicker, tz) : '';
    if (fromRfc) f.from = fromRfc;
    if (toRfc) f.to = toRfc;
    if (before) f.before = before;
    if (beforeId != null) f.before_id = beforeId;
    if (kindFilter !== 'all') f.kind = kindFilter;
    if (actionFilter !== 'all') f.action = actionFilter;
    if (userFilter !== 'all') f.user = userFilter;
    if (debouncedTarget.trim()) f.target = debouncedTarget.trim();
    if (debouncedSourceIp.trim()) f.source_ip = debouncedSourceIp.trim();
    return f;
  }, [fromPicker, toPicker, kindFilter, actionFilter, userFilter, debouncedTarget, debouncedSourceIp, tz]);

  const loadActivity = useCallback(async (opts: { silent?: boolean; append?: boolean; before?: string; beforeId?: number; active?: boolean } = {}) => {
    const { silent = false, append = false, before, beforeId, active = true } = opts;
    const path = currentAppPath();
    const isActivityPath = path === '/activity' || path.startsWith('/activity') || path === '/system-audit';
    if (!isActivityPath) return;
    if (!silent && active && !append) setLoading(true);
    if (active) setRefreshSpin(true);
    try {
      const filter = buildFilter(before, beforeId);
      const [audit, preds] = await Promise.all([
        fetchSystemAuditFiltered(filter),
        fetchPredictiveDecisions().catch(() => [] as PredictiveDecision[]),
      ]);
      if (!active) return;
      const curPath = currentAppPath();
      if (!(curPath === '/activity' || curPath.startsWith('/activity') || curPath === '/system-audit')) return;
      if (append) {
        setEntries((prev) => [...prev, ...audit]);
      } else {
        setEntries(audit);
      }
      setDecisions(preds);
      setHasMore(audit.length === PAGE_LIMIT);
      setError(null);
      setLastRefreshed(new Date());
    } catch (err: any) {
      if (!active) return;
      const curPath = currentAppPath();
      if (!(curPath === '/activity' || curPath.startsWith('/activity') || curPath === '/system-audit')) return;
      setError(err.message || 'Failed to load activity feed');
    } finally {
      if (active) {
        if (!silent && !append) setLoading(false);
        setLoadingMore(false);
        setTimeout(() => setRefreshSpin(false), 500);
      }
    }
  }, [buildFilter]);

  useEffect(() => {
    const path = currentAppPath();
    const isActivityPath = path === '/activity' || path.startsWith('/activity') || path === '/system-audit';
    if (!isActivityPath) return;
    let active = true;
    loadActivity({ silent: false, active });
    intervalRef.current = setInterval(() => {
      const cur = currentAppPath();
      const isCurActivity = cur === '/activity' || cur.startsWith('/activity') || cur === '/system-audit';
      if (active && isCurActivity) {
        loadActivity({ silent: true, active });
      }
    }, AUTO_REFRESH_INTERVAL_MS);
    return () => {
      active = false;
      if (intervalRef.current) clearInterval(intervalRef.current);
    };
  }, [loadActivity, location.pathname, location.search]);

  useEffect(() => {
    if (!selectedEntry) return;
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape') setSelectedEntry(null);
    }
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [selectedEntry]);

  const kindOptions: (ActivityKind | 'all')[] = ['all', 'drain', 'agent', 'runtime', 'node', 'warmup', 'schedule', 'predictive', 'config'];

  const uniqueActions = ['all', ...Array.from(new Set(entries.map((e) => e.action))).sort()];
  const uniqueUsers = ['all', ...Array.from(new Set(entries.map((e) => e.username))).sort()];

  const filteredEntries = entries.filter((e) => {
    const kind = toActivityKind(e.action);
    // Default to hiding the low-signal config bucket when no kind filter is
    // set - same default the old fleet view had, now unconditional since
    // there is only one view.
    if (kindFilter === 'all' && kind === 'config') return false;
    if (kindFilter === 'predictive') return false;
    const q = searchQuery.toLowerCase().trim();
    if (!q) return true;
    return (
      e.username.toLowerCase().includes(q) ||
      e.action.toLowerCase().includes(q) ||
      e.target.toLowerCase().includes(q) ||
      e.details.toLowerCase().includes(q) ||
      (e.source_ip || '').toLowerCase().includes(q) ||
      kind.toLowerCase().includes(q)
    );
  });

  const filteredDecisions = decisions.filter((d) => {
    if (kindFilter !== 'all' && kindFilter !== 'predictive') return false;
    const q = searchQuery.toLowerCase().trim();
    if (!q) return true;
    return (
      d.predicted_model.toLowerCase().includes(q) ||
      d.trigger_model.toLowerCase().includes(q) ||
      d.node.toLowerCase().includes(q)
    );
  });

  const hasActiveFilters = searchQuery.trim() !== '' || kindFilter !== 'all' || actionFilter !== 'all' || userFilter !== 'all' || debouncedTarget.trim() !== '' || debouncedSourceIp.trim() !== '' || fromPicker !== '' || toPicker !== '' || preset !== 'all';

  const totalFetched = entries.length;
  const uniqueOperators = new Set(entries.map((e) => e.username)).size;
  const drainCount = entries.filter((e) => toActivityKind(e.action) === 'drain').length;
  const warmupCount = entries.filter((e) => toActivityKind(e.action) === 'warmup').length;

  const handleLoadMore = () => {
    if (entries.length === 0 || loadingMore) return;
    const oldest = entries[entries.length - 1];
    if (!oldest) return;
    setLoadingMore(true);
    loadActivity({ silent: true, append: true, before: oldest.time, beforeId: (oldest as any).id, active: true });
  };

  const clearAllFilters = () => {
    setSearchQuery('');
    setKindFilter('all');
    setActionFilter('all');
    setUserFilter('all');
    setTargetInput('');
    setSourceIpInput('');
    setPreset('all');
    setFromPicker('');
    setToPicker('');
  };

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold tracking-tight text-foreground flex items-center gap-2">
            <ActivityIcon className="w-6 h-6 text-primary" />
            Activity
          </h1>
          <p className="text-sm text-muted-foreground mt-1">
            Fleet operations and administrative audit timeline - what changed, when, who, and from where.
          </p>
        </div>
        <div className="flex items-center gap-3">
          {lastRefreshed && (
            <span className="text-[11px] text-muted-foreground/60 hidden sm:block">
              Updated {formatUpdatedTime(lastRefreshed, tz)}
            </span>
          )}
          <button
            onClick={() => loadActivity({})}
            disabled={loading}
            className="flex items-center gap-2 px-3 py-2 bg-secondary text-foreground hover:bg-secondary/80 rounded-lg text-sm font-medium transition-all duration-200 cursor-pointer disabled:opacity-50"
          >
            <RefreshCw className={`w-4 h-4 ${refreshSpin ? 'animate-spin' : ''}`} />
            Refresh
          </button>
        </div>
      </div>

      {/* Stats Cards */}
      <div className="grid grid-cols-1 md:grid-cols-4 gap-6">
        <div className="bg-card/50 backdrop-blur-sm border border-border/80 rounded-xl p-5 shadow-sm hover:shadow-md transition-all duration-300 relative overflow-hidden group">
          <div className="absolute right-0 top-0 w-24 h-24 bg-gradient-to-br from-blue-500/10 to-transparent rounded-bl-full group-hover:scale-110 transition-transform duration-300" />
          <div className="flex items-start justify-between">
            <div>
              <p className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">Events (last {totalFetched})</p>
              <h3 className="text-3xl font-extrabold mt-2 text-foreground">{filteredEntries.length}</h3>
              {hasActiveFilters && kindFilter !== 'predictive' && (
                <p className="text-[10px] text-muted-foreground/60 mt-0.5">filtered from {totalFetched}</p>
              )}
            </div>
            <div className="p-2.5 bg-blue-500/10 text-blue-600 dark:text-blue-400 border border-blue-500/20 rounded-lg">
              <ActivityIcon className="w-5 h-5" />
            </div>
          </div>
        </div>

        <div className="bg-card/50 backdrop-blur-sm border border-border/80 rounded-xl p-5 shadow-sm hover:shadow-md transition-all duration-300 relative overflow-hidden group">
          <div className="absolute right-0 top-0 w-24 h-24 bg-gradient-to-br from-purple-500/10 to-transparent rounded-bl-full group-hover:scale-110 transition-transform duration-300" />
          <div className="flex items-start justify-between">
            <div>
              <p className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">Operators</p>
              <h3 className="text-3xl font-extrabold mt-2 text-foreground">{uniqueOperators}</h3>
            </div>
            <div className="p-2.5 bg-purple-500/10 text-purple-600 dark:text-purple-400 border border-purple-500/20 rounded-lg">
              <User className="w-5 h-5" />
            </div>
          </div>
        </div>

        <div className="bg-card/50 backdrop-blur-sm border border-border/80 rounded-xl p-5 shadow-sm hover:shadow-md transition-all duration-300 relative overflow-hidden group">
          <div className="absolute right-0 top-0 w-24 h-24 bg-gradient-to-br from-amber-500/10 to-transparent rounded-bl-full group-hover:scale-110 transition-transform duration-300" />
          <div className="flex items-start justify-between">
            <div>
              <p className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">Drain Events</p>
              <h3 className="text-3xl font-extrabold mt-2 text-foreground">{drainCount}</h3>
            </div>
            <div className="p-2.5 bg-amber-500/10 text-amber-600 dark:text-amber-400 border border-amber-500/20 rounded-lg">
              <Server className="w-5 h-5" />
            </div>
          </div>
        </div>

        <div className="bg-card/50 backdrop-blur-sm border border-border/80 rounded-xl p-5 shadow-sm hover:shadow-md transition-all duration-300 relative overflow-hidden group">
          <div className="absolute right-0 top-0 w-24 h-24 bg-gradient-to-br from-orange-500/10 to-transparent rounded-bl-full group-hover:scale-110 transition-transform duration-300" />
          <div className="flex items-start justify-between">
            <div>
              <p className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">Warmup Events</p>
              <h3 className="text-3xl font-extrabold mt-2 text-foreground">{warmupCount}</h3>
              <p className="text-[10px] text-muted-foreground/60 mt-0.5">{decisions.length} predictive decisions</p>
            </div>
            <div className="p-2.5 bg-orange-500/10 text-orange-600 dark:text-orange-400 border border-orange-500/20 rounded-lg">
              <Flame className="w-5 h-5" />
            </div>
          </div>
        </div>
      </div>

      {/* Predictive decisions - distinct section, not interleaved */}
      <div className="bg-card border border-border/60 rounded-xl overflow-hidden shadow-sm">
        <div className="flex items-center justify-between px-5 py-3 border-b border-border/60 bg-secondary/30">
          <div className="flex items-center gap-2">
            <BrainCircuit className="w-4 h-4 text-primary" />
            <h2 className="text-sm font-semibold text-foreground">Predictive Warmup Decisions</h2>
            <span className={`inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium border ${getActivityKindColor('predictive')}`}>
              {filteredDecisions.length} recent
            </span>
          </div>
          <span className="text-[11px] text-muted-foreground hidden sm:block">System-generated, not interleaved with operator timeline</span>
        </div>
        {filteredDecisions.length === 0 ? (
          <div className="p-6 text-center text-sm text-muted-foreground">
            No predictive decisions recorded yet.
          </div>
        ) : (
          <div className="divide-y divide-border/40">
            {filteredDecisions.slice(0, 10).map((d, i) => (
              <div key={`${d.timestamp}-${d.predicted_model}-${i}`} className="flex flex-col sm:flex-row sm:items-center justify-between gap-2 sm:gap-4 px-5 py-3 text-sm">
                <div className="min-w-0">
                  <div className="flex items-center gap-2 flex-wrap">
                    <span className="font-mono text-xs font-medium text-foreground">{d.predicted_model}</span>
                    <span className="text-muted-foreground text-xs">on {d.node}</span>
                    <span className={`inline-flex items-center px-1.5 py-0.5 rounded text-xs font-medium border ${d.was_already_warm ? 'bg-secondary text-muted-foreground border-border' : d.warmup_triggered ? 'bg-emerald-500/15 text-emerald-600 dark:text-emerald-400 border-emerald-500/20' : 'bg-amber-500/15 text-amber-600 dark:text-amber-400 border-amber-500/20'}`}>
                      {d.was_already_warm ? 'already warm' : d.warmup_triggered ? 'warmup triggered' : 'skipped'}
                    </span>
                  </div>
                  <p className="text-xs text-muted-foreground mt-0.5 truncate">
                    triggered by {d.trigger_model} - seen {d.transition_count}x at hour {d.hour}
                  </p>
                </div>
                <span className="font-mono text-xs text-muted-foreground whitespace-nowrap">{formatDateTime(d.timestamp, tz)}</span>
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Enterprise Filter Bar */}
      <div className="bg-card/50 backdrop-blur-sm border border-border/80 rounded-xl p-4 shadow-sm space-y-4">
        {/* Row 1: Quick range select + always-visible From/To - mirrors
            Requests.tsx's Quick range / From / Until layout instead of a
            pill toggle that only reveals the date pickers behind a "Custom"
            click (that hid affordance and jumped the layout on click). */}
        <div className="grid grid-cols-1 sm:grid-cols-3 gap-3">
          <FilterField label="Quick range">
            <CustomSelect
              value={preset}
              onChange={(val) => setPreset(val as DatePreset)}
              disabled={preset === 'custom'}
              placeholder="Clear custom range to use preset"
              options={[
                { value: 'all', label: 'All time' },
                { value: '1h', label: 'Last 1h' },
                { value: '24h', label: 'Last 24h' },
                { value: '7d', label: 'Last 7d' },
                { value: '30d', label: 'Last 30d' },
              ]}
            />
          </FilterField>
          <FilterField label="From (custom)">
            <CustomDateTimePicker value={fromPicker} onChange={handleFromPickerChange} placeholder="Any start time" />
          </FilterField>
          <FilterField label="To (custom)">
            <CustomDateTimePicker value={toPicker} onChange={handleToPickerChange} placeholder="Any end time" />
          </FilterField>
        </div>

        {/* Row 2: Kind, Action, Operator */}
        <div className="grid grid-cols-1 md:grid-cols-3 gap-3">
          <div>
            <label className="block text-[10px] uppercase tracking-wider font-semibold text-muted-foreground mb-1">Kind</label>
            <CustomSelect
              value={kindFilter}
              onChange={(v) => setKindFilter(v as ActivityKind | 'all')}
              options={(['all', 'drain', 'agent', 'runtime', 'node', 'warmup', 'schedule', 'predictive', 'config'] as const).map((k) => ({
                value: k,
                label: k === 'all' ? 'All Kinds' : getActivityKindLabel(k as ActivityKind),
              }))}
            />
          </div>
          <div>
            <label className="block text-[10px] uppercase tracking-wider font-semibold text-muted-foreground mb-1">Action</label>
            <CustomSelect
              value={actionFilter}
              onChange={setActionFilter}
              options={uniqueActions.map((a) => ({
                value: a,
                label: a === 'all' ? 'All Actions' : getActionLabel(a),
              }))}
            />
          </div>
          <div>
            <label className="block text-[10px] uppercase tracking-wider font-semibold text-muted-foreground mb-1">Operator</label>
            <CustomSelect
              value={userFilter}
              onChange={setUserFilter}
              options={uniqueUsers.map((u) => ({
                value: u,
                label: u === 'all' ? 'All Operators' : u,
              }))}
            />
          </div>
        </div>

        {/* Row 3: Target, Source IP, Search - ClearableInput brings the same
            inline "x" affordance Requests.tsx's text filters already have,
            so undoing one of these doesn't mean hand-clearing the text. */}
        <div className="grid grid-cols-1 md:grid-cols-3 gap-3">
          <FilterField label="Target contains">
            <ClearableInput
              type="text"
              placeholder="gpu-node-*, model name..."
              value={targetInput}
              onChange={(e) => setTargetInput(e.target.value)}
              onClear={() => setTargetInput('')}
              icon={<Search className="w-3.5 h-3.5" />}
              className="py-2 bg-secondary/80 text-foreground border border-border rounded-lg text-sm focus:outline-none focus:ring-1 focus:ring-primary placeholder:text-muted-foreground/60"
            />
          </FilterField>
          <FilterField label="Source IP contains">
            <ClearableInput
              type="text"
              placeholder="192.168.1.5"
              value={sourceIpInput}
              onChange={(e) => setSourceIpInput(e.target.value)}
              onClear={() => setSourceIpInput('')}
              icon={<Globe className="w-3.5 h-3.5" />}
              className="py-2 bg-secondary/80 text-foreground border border-border rounded-lg text-sm focus:outline-none focus:ring-1 focus:ring-primary placeholder:text-muted-foreground/60 font-mono"
            />
          </FilterField>
          <FilterField label="Search details">
            <ClearableInput
              type="text"
              placeholder="Search details, payload..."
              value={searchQuery}
              onChange={(e) => setSearchQuery(e.target.value)}
              onClear={() => setSearchQuery('')}
              icon={<Search className="w-3.5 h-3.5" />}
              className="py-2 bg-secondary/80 text-foreground border border-border rounded-lg text-sm focus:outline-none focus:ring-1 focus:ring-primary placeholder:text-muted-foreground/60"
            />
          </FilterField>
        </div>

        {hasActiveFilters && (
          <div className="flex items-center justify-between pt-2 border-t border-border/40">
            <span className="text-xs text-muted-foreground">Server-filtered to {totalFetched} events, {filteredEntries.length} after local detail search</span>
            <button
              onClick={clearAllFilters}
              className="px-3 py-1.5 bg-secondary text-foreground hover:bg-secondary/80 rounded-lg text-xs font-medium"
            >
              Clear all filters
            </button>
          </div>
        )}
      </div>

      {/* Main Timeline Table */}
      <div className="bg-card border border-border/60 rounded-xl overflow-hidden shadow-sm">
        {loading ? (
          <div className="p-12 text-center text-muted-foreground text-sm flex flex-col items-center justify-center gap-2">
            <RefreshCw className="w-6 h-6 animate-spin text-primary" />
            Loading fleet activity...
          </div>
        ) : error ? (
          <div className="p-8 text-center text-rose-600 dark:text-rose-400 text-sm flex flex-col items-center justify-center gap-2">
            <AlertCircle className="w-6 h-6 text-rose-600 dark:text-rose-400" />
            {error}
          </div>
        ) : filteredEntries.length === 0 ? (
          <div className="p-12 text-center text-muted-foreground text-sm flex flex-col items-center justify-center gap-2">
            <Search className="w-6 h-6 text-muted-foreground/50" />
            No activity records found matching your filters.
            {hasActiveFilters && (
              <button
                onClick={clearAllFilters}
                className="text-primary hover:underline text-xs mt-1"
              >
                Clear filters
              </button>
            )}
          </div>
        ) : (
          <div className="hidden md:block">
          <div className="overflow-x-auto">
            <table className="w-full border-collapse text-left text-sm">
              <thead>
                <tr className="border-b border-border/60 bg-secondary/40 text-muted-foreground font-medium">
                  <th className="px-5 py-3.5">Time</th>
                  <th className="px-5 py-3.5">Kind</th>
                  <th className="px-5 py-3.5">Action</th>
                  <th className="px-5 py-3.5">Target</th>
                  <th className="px-5 py-3.5">Who</th>
                  <th className="px-5 py-3.5">Source IP</th>
                  <th className="px-5 py-3.5">Details</th>
                  <th className="px-5 py-3.5 text-right">View</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border/40">
                {filteredEntries.map((e, index) => {
                  const kind = toActivityKind(e.action);
                  return (
                  <tr
                    key={`${e.time}-${e.action}-${e.username}-${index}`}
                    className="hover:bg-secondary/20 transition-all duration-150 group cursor-pointer"
                    onClick={() => setSelectedEntry(e)}
                  >
                    <td className="px-5 py-3.5 font-mono text-xs text-muted-foreground whitespace-nowrap">
                      {formatDateTime(e.time, tz)}
                    </td>
                    <td className="px-5 py-3.5 whitespace-nowrap">
                      <span className={`inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium border ${getActivityKindColor(kind)}`}>
                        {getActivityKindLabel(kind)}
                      </span>
                    </td>
                    <td className="px-5 py-3.5 whitespace-nowrap">
                      <span className="inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium border bg-amber-500/10 text-amber-700 dark:text-amber-400 border-amber-500/20">
                        {getActionLabel(e.action)}
                      </span>
                    </td>
                    <td className="px-5 py-3.5 font-mono text-xs text-foreground max-w-[120px] truncate" title={e.target}>
                      {e.target}
                    </td>
                    <td className="px-5 py-3.5 font-medium text-foreground">
                      <div className="flex items-center gap-1.5">
                        <div className="w-5 h-5 rounded bg-primary/10 text-primary flex items-center justify-center text-[10px] font-bold">
                          {e.username.slice(0, 2).toUpperCase()}
                        </div>
                        {e.username}
                      </div>
                    </td>
                    <td className="px-5 py-3.5 font-mono text-xs text-muted-foreground whitespace-nowrap">
                      {e.source_ip || '-'}
                    </td>
                    <td className="px-5 py-3.5 text-muted-foreground max-w-[320px] truncate" title={e.details}>
                      {e.details || '-'}
                    </td>
                    <td className="px-5 py-3.5 text-right">
                      <button
                        onClick={(evt) => {
                          evt.stopPropagation();
                          setSelectedEntry(e);
                        }}
                        className="p-1 rounded-md text-muted-foreground hover:text-primary hover:bg-secondary transition-colors cursor-pointer"
                      >
                        <Eye className="w-4 h-4" />
                      </button>
                    </td>
                  </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
          </div>
        )}
        {!loading && !error && filteredEntries.length > 0 && (
          <div className="md:hidden space-y-3 p-3">
            {filteredEntries.map((e, index) => {
              const kind = toActivityKind(e.action);
              return (
              <div
                key={`${e.time}-${e.action}-${e.username}-${index}-card`}
                onClick={() => setSelectedEntry(e)}
                className="bg-card border border-border/60 rounded-xl p-4 cursor-pointer hover:bg-secondary/20 transition-all duration-150"
              >
                <div className="flex items-start justify-between gap-2">
                  <div className="flex items-center gap-1.5 flex-wrap">
                    <span className={`inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium border ${getActivityKindColor(kind)}`}>
                      {getActivityKindLabel(kind)}
                    </span>
                    <span className="inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium border bg-amber-500/10 text-amber-700 dark:text-amber-400 border-amber-500/20">
                      {getActionLabel(e.action)}
                    </span>
                  </div>
                  <button
                    onClick={(evt) => {
                      evt.stopPropagation();
                      setSelectedEntry(e);
                    }}
                    className="p-1 rounded-md text-muted-foreground hover:text-primary hover:bg-secondary transition-colors cursor-pointer shrink-0"
                  >
                    <Eye className="w-4 h-4" />
                  </button>
                </div>
                <div className="mt-2 flex items-center justify-between gap-2">
                  <div className="flex items-center gap-1.5 font-medium text-foreground text-sm">
                    <div className="w-5 h-5 rounded bg-primary/10 text-primary flex items-center justify-center text-[10px] font-bold shrink-0">
                      {e.username.slice(0, 2).toUpperCase()}
                    </div>
                    <span className="truncate">{e.username}</span>
                  </div>
                  <span className="font-mono text-xs text-muted-foreground whitespace-nowrap">
                    {formatDateTime(e.time, tz)}
                  </span>
                </div>
                <div className="mt-2 font-mono text-xs text-foreground truncate" title={e.target}>
                  {e.target}
                </div>
                {e.source_ip && (
                  <div className="mt-1 font-mono text-xs text-muted-foreground">IP: {e.source_ip}</div>
                )}
                <div className="mt-1 text-xs text-muted-foreground truncate" title={e.details}>
                  {e.details || '-'}
                </div>
              </div>
              );
            })}
          </div>
        )}
        {/* Pagination */}
        {!loading && !error && (
          <div className="flex items-center justify-between px-5 py-3 border-t border-border/60 bg-secondary/20 text-xs text-muted-foreground">
            <span>Showing {filteredEntries.length} of {totalFetched} server-filtered events</span>
            {hasMore && (
              <button
                onClick={handleLoadMore}
                disabled={loadingMore}
                className="px-3 py-1.5 bg-secondary text-foreground hover:bg-secondary/80 rounded-lg text-xs font-medium disabled:opacity-50"
              >
                {loadingMore ? 'Loading...' : 'Load more'}
              </button>
            )}
          </div>
        )}
      </div>

      {/* Details Inspector Modal */}
      <Modal
        isOpen={selectedEntry !== null}
        onClose={() => setSelectedEntry(null)}
        title="Activity Record Details"
        maxWidth="lg"
      >
        {selectedEntry && (
          <div className="space-y-5">
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
              <div className="p-3 bg-secondary/40 border border-border/60 rounded-lg">
                <p className="text-[10px] uppercase tracking-wider font-semibold text-muted-foreground">Operator</p>
                <p className="text-sm font-semibold text-foreground mt-1 flex items-center gap-1.5">
                  <User className="w-4 h-4 text-primary" />
                  {selectedEntry.username}
                </p>
              </div>

              <div className="p-3 bg-secondary/40 border border-border/60 rounded-lg">
                <p className="text-[10px] uppercase tracking-wider font-semibold text-muted-foreground">Source IP</p>
                <p className="text-sm font-mono font-semibold text-foreground mt-1 flex items-center gap-1.5">
                  <Globe className="w-4 h-4 text-primary" />
                  {selectedEntry.source_ip || '-'}
                </p>
              </div>

              <div className="p-3 bg-secondary/40 border border-border/60 rounded-lg">
                <p className="text-[10px] uppercase tracking-wider font-semibold text-muted-foreground">Timestamp</p>
                <p className="text-sm font-mono text-foreground mt-1 flex items-center gap-1.5">
                  <Calendar className="w-4 h-4 text-primary" />
                  {formatDateTime(selectedEntry.time, tz)}
                </p>
              </div>

              <div className="p-3 bg-secondary/40 border border-border/60 rounded-lg">
                <p className="text-[10px] uppercase tracking-wider font-semibold text-muted-foreground">Kind / Action</p>
                <p className="mt-1 flex items-center gap-2 flex-wrap">
                  <span className={`inline-flex items-center px-2 py-0.5 rounded-full text-xs font-semibold border ${getActivityKindColor(toActivityKind(selectedEntry.action))}`}>
                    {getActivityKindLabel(toActivityKind(selectedEntry.action))}
                  </span>
                  <span className="inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium border bg-amber-500/10 text-amber-700 dark:text-amber-400 border-amber-500/20">
                    {getActionLabel(selectedEntry.action)}
                  </span>
                </p>
              </div>
            </div>

            <div>
              <p className="text-[10px] uppercase tracking-wider font-semibold text-muted-foreground mb-1">Target Object</p>
              <div className="p-3 bg-secondary/60 font-mono text-xs text-foreground border border-border/80 rounded-lg select-all">
                {selectedEntry.target}
              </div>
            </div>

            <div>
              <p className="text-[10px] uppercase tracking-wider font-semibold text-muted-foreground mb-1">Event Payload / Details</p>
              <div className="p-4 bg-secondary/80 font-mono text-xs text-foreground border border-border rounded-lg whitespace-pre-wrap select-all max-h-60 overflow-y-auto">
                {selectedEntry.details || '-'}
              </div>
            </div>

            <div className="flex justify-end pt-2 border-t border-border">
              <button
                onClick={() => setSelectedEntry(null)}
                className="px-4 py-2 bg-secondary text-foreground hover:bg-secondary/80 rounded-lg text-sm font-medium transition-colors cursor-pointer"
              >
                Close
              </button>
            </div>
          </div>
        )}
      </Modal>
    </div>
  );
}
