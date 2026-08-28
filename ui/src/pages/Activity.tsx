import { useState, useEffect, useCallback, useRef, useMemo } from 'react';
import { useLocation, useSearchParams } from 'react-router-dom';
import { Activity as ActivityIcon, Search, RefreshCw, Eye, Calendar, User, Globe, Filter, AlertCircle, X, Server, Flame, BrainCircuit, Shield, Terminal } from 'lucide-react';
import { fetchSystemAudit, fetchPredictiveDecisions } from '../lib/api';
import type { SystemAuditEntry, PredictiveDecision } from '../types';
import { Modal } from '../components/Modal';
import { CustomSelect } from '../components/Select';
import { currentAppPath } from '../hooks/useDemoMode';
import { toActivityKind, getActivityKindLabel, getActivityKindColor, type ActivityKind } from '../lib/activityKind';
import { useTimezone } from '../hooks/useTimezone';
import { formatDateTimeInZone, formatInTimezone } from '../lib/time';

const AUTO_REFRESH_INTERVAL_MS = 30_000;
const AUDIT_LIMIT = 200;

// currentAppPath() returns the raw path including any query string under the
// hash-routed public demo (forcedDemo), unlike BrowserRouter's pathname-only
// window.location.pathname. The view toggle below adds a ?view=audit query
// param to this same /activity route, so an exact string match against
// '/activity' would wrongly bail out of the load/refresh effects whenever
// that query param is present in demo mode - strip it before comparing.
function isOnActivityPage(): boolean {
  const p = currentAppPath();
  const q = p.indexOf('?');
  return (q === -1 ? p : p.slice(0, q)) === '/activity';
}

function formatUpdatedTime(d: Date, tz: string): string {
  try {
    return formatInTimezone(d.toISOString(), tz, {
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
    });
  } catch {
    // Should never throw (formatInTimezone already swallows), but keep a
    // non-locale-dependent fallback so this header never renders blank.
    return d.toISOString().slice(11, 19);
  }
}

function getActionLabel(action: string): string {
  return action
    .split('_')
    .map((word) => word.charAt(0).toUpperCase() + word.slice(1))
    .join(' ');
}

/** Returns true for actions that represent an infrastructure-level mutation. */
function isInfrastructureAction(action: string): boolean {
  const infraKeywords = ['node', 'routing', 'drain', 'undrain', 'warmup', 'schedule', 'pinned', 'settings', 'key', 'user', 'allowlist'];
  return infraKeywords.some((kw) => action.includes(kw));
}

// getAuditActionColor color-codes a raw action verb (add/remove/etc) for the
// Audit Trail view - distinct from the fleet-kind badges used in Fleet view.
function getAuditActionColor(action: string) {
  if (action.startsWith('add') || action.startsWith('create') || action.startsWith('approve') || action.startsWith('undrain')) {
    return 'bg-emerald-500/15 text-emerald-600 dark:text-emerald-400 border-emerald-500/20';
  }
  if (action.startsWith('remove') || action.startsWith('delete') || action.startsWith('revoke') || action.startsWith('suspend') || action.startsWith('drain')) {
    return 'bg-rose-500/15 text-rose-600 dark:text-rose-400 border-rose-500/20';
  }
  return 'bg-amber-500/15 text-amber-600 dark:text-amber-400 border-amber-500/20';
}

type ActivityView = 'fleet' | 'audit';

export function Activity() {
  const tz = useTimezone();
  const location = useLocation();
  const [searchParams, setSearchParams] = useSearchParams();
  const view: ActivityView = searchParams.get('view') === 'audit' ? 'audit' : 'fleet';

  const [entries, setEntries] = useState<SystemAuditEntry[]>([]);
  const [decisions, setDecisions] = useState<PredictiveDecision[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [searchQuery, setSearchQuery] = useState('');
  const [kindFilter, setKindFilter] = useState<ActivityKind | 'all'>('all');
  const [actionFilter, setActionFilter] = useState('all');
  const [selectedEntry, setSelectedEntry] = useState<SystemAuditEntry | null>(null);
  const [refreshSpin, setRefreshSpin] = useState(false);
  const [lastRefreshed, setLastRefreshed] = useState<Date | null>(null);
  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const loadActivity = useCallback(async (silent = false, active = true) => {
    if (!isOnActivityPage()) return;
    if (!silent && active && isOnActivityPage()) setLoading(true);
    if (active && isOnActivityPage()) setRefreshSpin(true);
    try {
      const [audit, preds] = await Promise.all([
        fetchSystemAudit(AUDIT_LIMIT),
        fetchPredictiveDecisions().catch(() => [] as PredictiveDecision[]),
      ]);
      if (!active || !isOnActivityPage()) return;
      setEntries(audit);
      setDecisions(preds);
      setError(null);
      setLastRefreshed(new Date());
    } catch (err: any) {
      if (!active || !isOnActivityPage()) return;
      setError(err.message || 'Failed to load activity feed');
    } finally {
      if (active && isOnActivityPage()) {
        if (!silent) setLoading(false);
        setTimeout(() => {
          if (active && isOnActivityPage()) {
            setRefreshSpin(false);
          }
        }, 500);
      }
    }
  }, [location.pathname]);

  useEffect(() => {
    if (!isOnActivityPage()) return;
    let active = true;
    loadActivity(false, active);
    intervalRef.current = setInterval(() => {
      if (active && isOnActivityPage()) {
        loadActivity(true, active);
      }
    }, AUTO_REFRESH_INTERVAL_MS);
    return () => {
      active = false;
      if (intervalRef.current) clearInterval(intervalRef.current);
    };
  }, [loadActivity, location.pathname]);

  useEffect(() => {
    if (!selectedEntry) return;
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape') setSelectedEntry(null);
    }
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [selectedEntry]);

  function setView(next: ActivityView) {
    setSelectedEntry(null);
    setSearchQuery('');
    setSearchParams(
      (prev) => {
        const p = new URLSearchParams(prev);
        if (next === 'audit') p.set('view', 'audit');
        else p.delete('view');
        return p;
      },
      { replace: true }
    );
  }

  const kindOptions: (ActivityKind | 'all')[] = ['all', 'drain', 'agent', 'runtime', 'node', 'warmup', 'predictive', 'config'];

  const uniqueActions = useMemo(
    () => ['all', ...Array.from(new Set(entries.map((e) => e.action))).sort()],
    [entries]
  );

  const filteredEntries = entries.filter((e) => {
    const q = searchQuery.toLowerCase().trim();
    if (view === 'audit') {
      const matchesAction = actionFilter === 'all' || e.action === actionFilter;
      const matchesSearch =
        !q ||
        e.username.toLowerCase().includes(q) ||
        e.action.toLowerCase().includes(q) ||
        e.target.toLowerCase().includes(q) ||
        e.details.toLowerCase().includes(q) ||
        (e.source_ip || '').toLowerCase().includes(q);
      return matchesAction && matchesSearch;
    }
    const kind = toActivityKind(e.action);
    const matchesKind = kindFilter === 'all' || kind === kindFilter;
    if (kindFilter === 'predictive') return false;
    const matchesSearch =
      !q ||
      e.username.toLowerCase().includes(q) ||
      e.action.toLowerCase().includes(q) ||
      e.target.toLowerCase().includes(q) ||
      e.details.toLowerCase().includes(q) ||
      (e.source_ip || '').toLowerCase().includes(q) ||
      kind.toLowerCase().includes(q);
    return matchesKind && matchesSearch;
  });

  const filteredDecisions = decisions.filter((d) => {
    if (view === 'audit') return false;
    if (kindFilter !== 'all' && kindFilter !== 'predictive') return false;
    const q = searchQuery.toLowerCase().trim();
    if (!q) return true;
    return (
      d.predicted_model.toLowerCase().includes(q) ||
      d.trigger_model.toLowerCase().includes(q) ||
      d.node.toLowerCase().includes(q)
    );
  });

  const hasActiveFilters =
    view === 'audit'
      ? searchQuery.trim() !== '' || actionFilter !== 'all'
      : searchQuery.trim() !== '' || kindFilter !== 'all';

  function clearFilters() {
    setSearchQuery('');
    if (view === 'audit') setActionFilter('all');
    else setKindFilter('all');
  }

  const totalFetched = entries.length;
  const uniqueOperators = new Set(entries.map((e) => e.username)).size;
  const drainCount = entries.filter((e) => toActivityKind(e.action) === 'drain').length;
  const warmupCount = entries.filter((e) => toActivityKind(e.action) === 'warmup').length;
  const infrastructureChanges = entries.filter((e) => isInfrastructureAction(e.action)).length;

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold tracking-tight text-foreground flex items-center gap-2">
            {view === 'audit' ? <Shield className="w-6 h-6 text-primary" /> : <ActivityIcon className="w-6 h-6 text-primary" />}
            {view === 'audit' ? 'Audit Trail' : 'Activity'}
          </h1>
          <p className="text-sm text-muted-foreground mt-1">
            {view === 'audit'
              ? 'Review configuration changes, node updates, and administrative actions.'
              : 'Unified fleet operations timeline - drain, agent, runtime, node, and warmup events with what, when, and who.'}
          </p>
        </div>
        <div className="flex items-center gap-3">
          {lastRefreshed && (
            <span className="text-[11px] text-muted-foreground/60 hidden sm:block">
              Updated {formatUpdatedTime(lastRefreshed, tz)}
            </span>
          )}
          <button
            onClick={() => loadActivity()}
            disabled={loading}
            className="flex items-center gap-2 px-3 py-2 bg-secondary text-foreground hover:bg-secondary/80 rounded-lg text-sm font-medium transition-all duration-200 cursor-pointer disabled:opacity-50"
          >
            <RefreshCw className={`w-4 h-4 ${refreshSpin ? 'animate-spin' : ''}`} />
            Refresh
          </button>
        </div>
      </div>

      {/* View toggle - Fleet Activity vs Audit Trail, both read the same
          system-audit feed, just filtered/labeled differently. */}
      <div className="inline-flex items-center gap-1 p-1 bg-secondary/60 border border-border/60 rounded-lg">
        <button
          onClick={() => setView('fleet')}
          className={`flex items-center gap-1.5 px-3 py-1.5 rounded-md text-xs font-medium transition-colors cursor-pointer ${
            view === 'fleet' ? 'bg-card text-foreground shadow-sm border border-border/60' : 'text-muted-foreground hover:text-foreground'
          }`}
        >
          <ActivityIcon className="w-3.5 h-3.5" />
          Fleet Activity
        </button>
        <button
          onClick={() => setView('audit')}
          className={`flex items-center gap-1.5 px-3 py-1.5 rounded-md text-xs font-medium transition-colors cursor-pointer ${
            view === 'audit' ? 'bg-card text-foreground shadow-sm border border-border/60' : 'text-muted-foreground hover:text-foreground'
          }`}
        >
          <Shield className="w-3.5 h-3.5" />
          Audit Trail
        </button>
      </div>

      {/* Stats Cards */}
      {view === 'audit' ? (
        <div className="grid grid-cols-1 sm:grid-cols-3 gap-3 sm:gap-6">
          <div className="bg-card/50 backdrop-blur-sm border border-border/80 rounded-xl p-4 sm:p-5 shadow-sm hover:shadow-md transition-all duration-300 relative overflow-hidden group min-w-0">
            <div className="absolute right-0 top-0 w-24 h-24 bg-gradient-to-br from-blue-500/10 to-transparent rounded-bl-full group-hover:scale-110 transition-transform duration-300" />
            <div className="flex items-start justify-between gap-2 min-w-0">
              <div className="min-w-0">
                <p className="text-[10px] sm:text-xs font-semibold uppercase tracking-wider text-muted-foreground truncate">Actions (last {totalFetched})</p>
                <h3 className="text-2xl sm:text-3xl font-extrabold mt-2 text-foreground">{filteredEntries.length}</h3>
                {hasActiveFilters && (
                  <p className="text-[10px] text-muted-foreground/60 mt-0.5">filtered from {totalFetched}</p>
                )}
              </div>
              <div className="p-2 sm:p-2.5 bg-blue-500/10 text-blue-600 dark:text-blue-400 border border-blue-500/20 rounded-lg shrink-0">
                <Terminal className="w-4 h-4 sm:w-5 sm:h-5" />
              </div>
            </div>
          </div>

          <div className="bg-card/50 backdrop-blur-sm border border-border/80 rounded-xl p-4 sm:p-5 shadow-sm hover:shadow-md transition-all duration-300 relative overflow-hidden group min-w-0">
            <div className="absolute right-0 top-0 w-24 h-24 bg-gradient-to-br from-purple-500/10 to-transparent rounded-bl-full group-hover:scale-110 transition-transform duration-300" />
            <div className="flex items-start justify-between gap-2 min-w-0">
              <div className="min-w-0">
                <p className="text-[10px] sm:text-xs font-semibold uppercase tracking-wider text-muted-foreground">Active Operators</p>
                <h3 className="text-2xl sm:text-3xl font-extrabold mt-2 text-foreground">{uniqueOperators}</h3>
              </div>
              <div className="p-2 sm:p-2.5 bg-purple-500/10 text-purple-600 dark:text-purple-400 border border-purple-500/20 rounded-lg shrink-0">
                <User className="w-4 h-4 sm:w-5 sm:h-5" />
              </div>
            </div>
          </div>

          <div className="bg-card/50 backdrop-blur-sm border border-border/80 rounded-xl p-4 sm:p-5 shadow-sm hover:shadow-md transition-all duration-300 relative overflow-hidden group min-w-0">
            <div className="absolute right-0 top-0 w-24 h-24 bg-gradient-to-br from-amber-500/10 to-transparent rounded-bl-full group-hover:scale-110 transition-transform duration-300" />
            <div className="flex items-start justify-between gap-2 min-w-0">
              <div className="min-w-0">
                <p className="text-[10px] sm:text-xs font-semibold uppercase tracking-wider text-muted-foreground">Infrastructure Events</p>
                <h3 className="text-2xl sm:text-3xl font-extrabold mt-2 text-foreground">{infrastructureChanges}</h3>
              </div>
              <div className="p-2 sm:p-2.5 bg-amber-500/10 text-amber-600 dark:text-amber-400 border border-amber-500/20 rounded-lg shrink-0">
                <Shield className="w-4 h-4 sm:w-5 sm:h-5" />
              </div>
            </div>
          </div>
        </div>
      ) : (
        <div className="grid grid-cols-2 md:grid-cols-4 gap-3 sm:gap-6">
          <div className="bg-card/50 backdrop-blur-sm border border-border/80 rounded-xl p-4 sm:p-5 shadow-sm hover:shadow-md transition-all duration-300 relative overflow-hidden group min-w-0">
            <div className="absolute right-0 top-0 w-24 h-24 bg-gradient-to-br from-blue-500/10 to-transparent rounded-bl-full group-hover:scale-110 transition-transform duration-300" />
            <div className="flex items-start justify-between gap-2 min-w-0">
              <div className="min-w-0">
                <p className="text-[10px] sm:text-xs font-semibold uppercase tracking-wider text-muted-foreground truncate">Events (last {totalFetched})</p>
                <h3 className="text-2xl sm:text-3xl font-extrabold mt-2 text-foreground">{filteredEntries.length}</h3>
                {hasActiveFilters && kindFilter !== 'predictive' && (
                  <p className="text-[10px] text-muted-foreground/60 mt-0.5">filtered from {totalFetched}</p>
                )}
              </div>
              <div className="p-2 sm:p-2.5 bg-blue-500/10 text-blue-600 dark:text-blue-400 border border-blue-500/20 rounded-lg shrink-0">
                <ActivityIcon className="w-4 h-4 sm:w-5 sm:h-5" />
              </div>
            </div>
          </div>

          <div className="bg-card/50 backdrop-blur-sm border border-border/80 rounded-xl p-4 sm:p-5 shadow-sm hover:shadow-md transition-all duration-300 relative overflow-hidden group min-w-0">
            <div className="absolute right-0 top-0 w-24 h-24 bg-gradient-to-br from-purple-500/10 to-transparent rounded-bl-full group-hover:scale-110 transition-transform duration-300" />
            <div className="flex items-start justify-between gap-2 min-w-0">
              <div className="min-w-0">
                <p className="text-[10px] sm:text-xs font-semibold uppercase tracking-wider text-muted-foreground">Operators</p>
                <h3 className="text-2xl sm:text-3xl font-extrabold mt-2 text-foreground">{uniqueOperators}</h3>
              </div>
              <div className="p-2 sm:p-2.5 bg-purple-500/10 text-purple-600 dark:text-purple-400 border border-purple-500/20 rounded-lg shrink-0">
                <User className="w-4 h-4 sm:w-5 sm:h-5" />
              </div>
            </div>
          </div>

          <div className="bg-card/50 backdrop-blur-sm border border-border/80 rounded-xl p-4 sm:p-5 shadow-sm hover:shadow-md transition-all duration-300 relative overflow-hidden group min-w-0">
            <div className="absolute right-0 top-0 w-24 h-24 bg-gradient-to-br from-amber-500/10 to-transparent rounded-bl-full group-hover:scale-110 transition-transform duration-300" />
            <div className="flex items-start justify-between gap-2 min-w-0">
              <div className="min-w-0">
                <p className="text-[10px] sm:text-xs font-semibold uppercase tracking-wider text-muted-foreground">Drain Events</p>
                <h3 className="text-2xl sm:text-3xl font-extrabold mt-2 text-foreground">{drainCount}</h3>
              </div>
              <div className="p-2 sm:p-2.5 bg-amber-500/10 text-amber-600 dark:text-amber-400 border border-amber-500/20 rounded-lg shrink-0">
                <Server className="w-4 h-4 sm:w-5 sm:h-5" />
              </div>
            </div>
          </div>

          <div className="bg-card/50 backdrop-blur-sm border border-border/80 rounded-xl p-4 sm:p-5 shadow-sm hover:shadow-md transition-all duration-300 relative overflow-hidden group min-w-0">
            <div className="absolute right-0 top-0 w-24 h-24 bg-gradient-to-br from-orange-500/10 to-transparent rounded-bl-full group-hover:scale-110 transition-transform duration-300" />
            <div className="flex items-start justify-between gap-2 min-w-0">
              <div className="min-w-0">
                <p className="text-[10px] sm:text-xs font-semibold uppercase tracking-wider text-muted-foreground">Warmup Events</p>
                <h3 className="text-2xl sm:text-3xl font-extrabold mt-2 text-foreground">{warmupCount}</h3>
                <p className="text-[10px] text-muted-foreground/60 mt-0.5">{decisions.length} predictive decisions</p>
              </div>
              <div className="p-2 sm:p-2.5 bg-orange-500/10 text-orange-600 dark:text-orange-400 border border-orange-500/20 rounded-lg shrink-0">
                <Flame className="w-4 h-4 sm:w-5 sm:h-5" />
              </div>
            </div>
          </div>
        </div>
      )}

      {/* Predictive decisions - fleet view only, distinct section, not interleaved */}
      {view === 'fleet' && (
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
                  <span className="font-mono text-xs text-muted-foreground whitespace-nowrap">{formatDateTimeInZone(d.timestamp, tz)}</span>
                </div>
              ))}
            </div>
          )}
        </div>
      )}

      {/* Filters Bar */}
      <div className="bg-card/50 backdrop-blur-sm border border-border/80 rounded-xl p-4 flex flex-col md:flex-row gap-4 items-center justify-between shadow-sm">
        <div className="relative w-full md:max-w-md">
          <Search className="w-4 h-4 absolute left-3 top-1/2 -translate-y-1/2 text-muted-foreground" />
          <input
            type="text"
            placeholder={view === 'audit' ? 'Search operators, targets, IPs or action details...' : 'Search operators, targets, kinds or details...'}
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            className="w-full pl-9 pr-4 py-2 bg-secondary/80 text-foreground border border-border rounded-lg text-sm focus:outline-none focus:ring-1 focus:ring-primary focus:border-primary transition-all duration-150 placeholder:text-muted-foreground/60"
          />
        </div>

        <div className="flex items-center gap-2 w-full md:w-80">
          <Filter className="w-4 h-4 text-muted-foreground shrink-0" />
          {view === 'audit' ? (
            <CustomSelect
              value={actionFilter}
              onChange={setActionFilter}
              options={uniqueActions.map((act) => ({
                value: act,
                label: act === 'all' ? 'All Action Types' : getActionLabel(act),
              }))}
            />
          ) : (
            <CustomSelect
              value={kindFilter}
              onChange={(v) => setKindFilter(v as ActivityKind | 'all')}
              options={kindOptions.map((k) => ({
                value: k,
                label: k === 'all' ? 'All Kinds' : getActivityKindLabel(k as ActivityKind),
              }))}
            />
          )}
          {hasActiveFilters && (
            <button
              onClick={clearFilters}
              title="Clear all filters"
              className="p-2 rounded-lg text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors shrink-0"
            >
              <X className="w-4 h-4" />
            </button>
          )}
        </div>
      </div>

      {/* Main Timeline Table */}
      <div className="bg-card border border-border/60 rounded-xl overflow-hidden shadow-sm">
        {loading ? (
          <div className="p-12 text-center text-muted-foreground text-sm flex flex-col items-center justify-center gap-2">
            <RefreshCw className="w-6 h-6 animate-spin text-primary" />
            {view === 'audit' ? 'Loading system events...' : 'Loading fleet activity...'}
          </div>
        ) : error ? (
          <div className="p-8 text-center text-rose-600 dark:text-rose-400 text-sm flex flex-col items-center justify-center gap-2">
            <AlertCircle className="w-6 h-6 text-rose-600 dark:text-rose-400" />
            {error}
          </div>
        ) : filteredEntries.length === 0 ? (
          <div className="p-12 text-center text-muted-foreground text-sm flex flex-col items-center justify-center gap-2">
            <Search className="w-6 h-6 text-muted-foreground/50" />
            {view === 'audit' ? 'No audit records found matching your filters.' : 'No activity records found matching your filters.'}
            {hasActiveFilters && (
              <button
                onClick={clearFilters}
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
                  {view === 'fleet' && <th className="px-5 py-3.5">Kind</th>}
                  <th className="px-5 py-3.5">Action</th>
                  <th className="px-5 py-3.5">Target</th>
                  <th className="px-5 py-3.5">{view === 'audit' ? 'Operator' : 'Who'}</th>
                  {view === 'audit' && <th className="px-5 py-3.5">Source IP</th>}
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
                      {formatDateTimeInZone(e.time, tz)}
                    </td>
                    {view === 'fleet' && (
                      <td className="px-5 py-3.5 whitespace-nowrap">
                        <span className={`inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium border ${getActivityKindColor(kind)}`}>
                          {getActivityKindLabel(kind)}
                        </span>
                      </td>
                    )}
                    <td className="px-5 py-3.5 whitespace-nowrap">
                      <span className={`inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium border ${view === 'audit' ? getAuditActionColor(e.action) : 'bg-amber-500/10 text-amber-700 dark:text-amber-400 border-amber-500/20'}`}>
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
                    {view === 'audit' && (
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
                    {view === 'fleet' && (
                      <span className={`inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium border ${getActivityKindColor(kind)}`}>
                        {getActivityKindLabel(kind)}
                      </span>
                    )}
                    <span className={`inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium border ${view === 'audit' ? getAuditActionColor(e.action) : 'bg-amber-500/10 text-amber-700 dark:text-amber-400 border-amber-500/20'}`}>
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
                    {formatDateTimeInZone(e.time, tz)}
                  </span>
                </div>
                <div className="mt-2 font-mono text-xs text-foreground truncate" title={e.target}>
                  {e.target}
                </div>
                {view === 'audit' && (
                  <div className="mt-1 flex items-center justify-between gap-2 text-xs text-muted-foreground">
                    <span className="font-mono truncate">{e.source_ip || '-'}</span>
                  </div>
                )}
                <div className="mt-1 text-xs text-muted-foreground truncate" title={e.details}>
                  {e.details || '-'}
                </div>
              </div>
              );
            })}
          </div>
        )}
      </div>

      {/* Details Inspector Modal */}
      <Modal
        isOpen={selectedEntry !== null}
        onClose={() => setSelectedEntry(null)}
        title={view === 'audit' ? 'Audit Record Details' : 'Activity Record Details'}
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
                  {formatDateTimeInZone(selectedEntry.time, tz)}
                </p>
              </div>

              <div className="p-3 bg-secondary/40 border border-border/60 rounded-lg">
                <p className="text-[10px] uppercase tracking-wider font-semibold text-muted-foreground">{view === 'audit' ? 'Action' : 'Kind / Action'}</p>
                <p className="mt-1 flex items-center gap-2 flex-wrap">
                  {view === 'fleet' && (
                    <span className={`inline-flex items-center px-2 py-0.5 rounded-full text-xs font-semibold border ${getActivityKindColor(toActivityKind(selectedEntry.action))}`}>
                      {getActivityKindLabel(toActivityKind(selectedEntry.action))}
                    </span>
                  )}
                  <span className={`inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium border ${view === 'audit' ? getAuditActionColor(selectedEntry.action) : 'bg-amber-500/10 text-amber-700 dark:text-amber-400 border-amber-500/20'}`}>
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
