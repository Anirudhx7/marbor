import { useState, useEffect, useCallback, useRef } from 'react';
import { useLocation, useSearchParams } from 'react-router-dom';
import { Activity as ActivityIcon, Search, RefreshCw, Eye, Calendar, User, Globe, Filter, AlertCircle, X, Server, Flame, BrainCircuit } from 'lucide-react';
import { fetchSystemAuditFiltered, fetchPredictiveDecisions } from '../lib/api';
import type { SystemAuditEntry, PredictiveDecision } from '../types';
import { Modal } from '../components/Modal';
import { CustomSelect } from '../components/Select';
import { CustomDateTimePicker } from '../components/DateTimePicker';
import { currentAppPath } from '../hooks/useDemoMode';
import { toActivityKind, getActivityKindLabel, getActivityKindColor, type ActivityKind } from '../lib/activityKind';

const AUTO_REFRESH_INTERVAL_MS = 30_000;
const PAGE_LIMIT = 100;

function toPickerValue(rfc3339: string): string {
  try {
    const d = new Date(rfc3339);
    if (isNaN(d.getTime())) return '';
    const yyyy = d.getFullYear();
    const mm = String(d.getMonth() + 1).padStart(2, '0');
    const dd = String(d.getDate()).padStart(2, '0');
    const hh = String(d.getHours()).padStart(2, '0');
    const min = String(d.getMinutes()).padStart(2, '0');
    return `${yyyy}-${mm}-${dd}T${hh}:${min}`;
  } catch {
    return '';
  }
}

function fromPickerValue(v: string): string {
  if (!v) return '';
  try {
    const d = new Date(v);
    if (isNaN(d.getTime())) return '';
    return d.toISOString();
  } catch {
    return '';
  }
}

type DatePreset = 'all' | '1h' | '24h' | '7d' | '30d' | 'custom';

function formatDateTime(isoString: string): string {
  try {
    const d = new Date(isoString);
    if (isNaN(d.getTime())) return isoString;
    return d.toLocaleString(undefined, {
      year: 'numeric',
      month: 'short',
      day: 'numeric',
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
    });
  } catch {
    return isoString;
  }
}

function getActionLabel(action: string): string {
  return action
    .split('_')
    .map((word) => word.charAt(0).toUpperCase() + word.slice(1))
    .join(' ');
}

export function Activity() {
  const location = useLocation();
  const [searchParams] = useSearchParams();
  const view = searchParams.get('view') === 'audit' ? 'audit' : 'fleet';
  const isAuditView = view === 'audit';

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
    } else if (preset === 'custom') {
      // keep current pickers
    } else {
      const now = new Date();
      let from: Date | null = null;
      if (preset === '1h') from = new Date(now.getTime() - 1 * 3600_000);
      else if (preset === '24h') from = new Date(now.getTime() - 24 * 3600_000);
      else if (preset === '7d') from = new Date(now.getTime() - 7 * 86400_000);
      else if (preset === '30d') from = new Date(now.getTime() - 30 * 86400_000);
      if (from) {
        setFromPicker(toPickerValue(from.toISOString()));
        setToPicker(toPickerValue(now.toISOString()));
      }
    }
  }, [preset]);

  const buildFilter = useCallback((before?: string) => {
    const f: any = { limit: PAGE_LIMIT };
    const fromRfc = fromPicker ? fromPickerValue(fromPicker) : '';
    const toRfc = toPicker ? fromPickerValue(toPicker) : '';
    if (fromRfc) f.from = fromRfc;
    if (toRfc) f.to = toRfc;
    if (before) f.before = before;
    if (kindFilter !== 'all') f.kind = kindFilter;
    if (actionFilter !== 'all') f.action = actionFilter;
    if (userFilter !== 'all') f.user = userFilter;
    if (debouncedTarget.trim()) f.target = debouncedTarget.trim();
    if (isAuditView && debouncedSourceIp.trim()) f.source_ip = debouncedSourceIp.trim();
    return f;
  }, [fromPicker, toPicker, kindFilter, actionFilter, userFilter, debouncedTarget, debouncedSourceIp, isAuditView]);

  const loadActivity = useCallback(async (opts: { silent?: boolean; append?: boolean; before?: string; active?: boolean } = {}) => {
    const { silent = false, append = false, before, active = true } = opts;
    const path = currentAppPath();
    const isActivityPath = path === '/activity' || path.startsWith('/activity') || path === '/system-audit';
    if (!isActivityPath) return;
    if (!silent && active && !append) setLoading(true);
    if (active) setRefreshSpin(true);
    try {
      const filter = buildFilter(before);
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
    // Fleet view hides config when Kind==all
    if (!isAuditView && kindFilter === 'all' && kind === 'config') return false;
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
    loadActivity({ silent: true, append: true, before: oldest.time, active: true });
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
            {isAuditView ? 'System Audit Trail' : 'Activity'}
          </h1>
          <p className="text-sm text-muted-foreground mt-1">
            {isAuditView
              ? 'Compliance view of all administrative actions with enterprise filters.'
              : 'Unified fleet operations timeline - drain, agent, runtime, node, and warmup events with what, when, and who.'}
          </p>
        </div>
        <div className="flex items-center gap-3">
          {lastRefreshed && (
            <span className="text-[11px] text-muted-foreground/60 hidden sm:block">
              Updated {lastRefreshed.toLocaleTimeString()}
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
                <span className="font-mono text-xs text-muted-foreground whitespace-nowrap">{formatDateTime(d.timestamp)}</span>
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Enterprise Filter Bar */}
      <div className="bg-card/50 backdrop-blur-sm border border-border/80 rounded-xl p-4 shadow-sm space-y-4">
        {/* Row 1: Date presets + from/to */}
        <div className="flex flex-col lg:flex-row gap-4">
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-xs font-medium text-muted-foreground mr-1">Range:</span>
            {(['all', '1h', '24h', '7d', '30d', 'custom'] as DatePreset[]).map((p) => (
              <button
                key={p}
                onClick={() => setPreset(p)}
                className={`px-2.5 py-1 rounded-lg text-xs font-medium border transition-colors ${preset === p ? 'bg-primary text-primary-foreground border-primary' : 'bg-secondary text-muted-foreground border-border hover:bg-secondary/80'}`}
              >
                {p === 'all' ? 'All time' : p === '1h' ? 'Last 1h' : p === '24h' ? 'Last 24h' : p === '7d' ? 'Last 7d' : p === '30d' ? 'Last 30d' : 'Custom'}
              </button>
            ))}
          </div>
          {preset === 'custom' && (
            <div className="flex flex-col sm:flex-row gap-2 flex-1">
              <div className="flex-1 min-w-0">
                <label className="block text-[10px] uppercase tracking-wider font-semibold text-muted-foreground mb-1">From</label>
                <CustomDateTimePicker value={fromPicker} onChange={setFromPicker} placeholder="Start time" />
              </div>
              <div className="flex-1 min-w-0">
                <label className="block text-[10px] uppercase tracking-wider font-semibold text-muted-foreground mb-1">To</label>
                <CustomDateTimePicker value={toPicker} onChange={setToPicker} placeholder="End time" />
              </div>
            </div>
          )}
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

        {/* Row 3: Target, Source IP, Search */}
        <div className="grid grid-cols-1 md:grid-cols-3 gap-3">
          <div>
            <label className="block text-[10px] uppercase tracking-wider font-semibold text-muted-foreground mb-1">Target contains</label>
            <div className="relative">
              <Search className="w-3.5 h-3.5 absolute left-3 top-1/2 -translate-y-1/2 text-muted-foreground" />
              <input
                type="text"
                placeholder="gpu-node-*, model name..."
                value={targetInput}
                onChange={(e) => setTargetInput(e.target.value)}
                className="w-full pl-9 pr-3 py-2 bg-secondary/80 text-foreground border border-border rounded-lg text-sm focus:outline-none focus:ring-1 focus:ring-primary placeholder:text-muted-foreground/60"
              />
            </div>
          </div>
          {isAuditView ? (
            <div>
              <label className="block text-[10px] uppercase tracking-wider font-semibold text-muted-foreground mb-1">Source IP contains</label>
              <div className="relative">
                <Globe className="w-3.5 h-3.5 absolute left-3 top-1/2 -translate-y-1/2 text-muted-foreground" />
                <input
                  type="text"
                  placeholder="192.168.1.5"
                  value={sourceIpInput}
                  onChange={(e) => setSourceIpInput(e.target.value)}
                  className="w-full pl-9 pr-3 py-2 bg-secondary/80 text-foreground border border-border rounded-lg text-sm focus:outline-none focus:ring-1 focus:ring-primary placeholder:text-muted-foreground/60 font-mono"
                />
              </div>
            </div>
          ) : (
            <div className="hidden md:block" />
          )}
          <div>
            <label className="block text-[10px] uppercase tracking-wider font-semibold text-muted-foreground mb-1">Search details</label>
            <div className="relative">
              <Search className="w-3.5 h-3.5 absolute left-3 top-1/2 -translate-y-1/2 text-muted-foreground" />
              <input
                type="text"
                placeholder="Search details, payload..."
                value={searchQuery}
                onChange={(e) => setSearchQuery(e.target.value)}
                className="w-full pl-9 pr-3 py-2 bg-secondary/80 text-foreground border border-border rounded-lg text-sm focus:outline-none focus:ring-1 focus:ring-primary placeholder:text-muted-foreground/60"
              />
            </div>
          </div>
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
                  {isAuditView && <th className="px-5 py-3.5">Source IP</th>}
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
                      {formatDateTime(e.time)}
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
                    {isAuditView && (
                      <td className="px-5 py-3.5 font-mono text-xs text-muted-foreground whitespace-nowrap">
                        {e.source_ip || '-'}
                      </td>
                    )}
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
                    {formatDateTime(e.time)}
                  </span>
                </div>
                <div className="mt-2 font-mono text-xs text-foreground truncate" title={e.target}>
                  {e.target}
                </div>
                {isAuditView && e.source_ip && (
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
                  {formatDateTime(selectedEntry.time)}
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
