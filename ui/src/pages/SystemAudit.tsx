import { useState, useEffect, useCallback, useRef } from 'react';
import { useLocation } from 'react-router-dom';
import { Shield, Search, RefreshCw, Eye, Calendar, User, Terminal, Globe, Filter, AlertCircle, X } from 'lucide-react';
import { fetchSystemAudit } from '../lib/api';
import type { SystemAuditEntry } from '../types';
import { Modal } from '../components/Modal';
import { currentAppPath } from '../hooks/useDemoMode';

const AUTO_REFRESH_INTERVAL_MS = 30_000;

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

function getActionColor(action: string) {
  if (action.startsWith('add') || action.startsWith('create') || action.startsWith('approve') || action.startsWith('undrain')) {
    return 'bg-emerald-500/15 text-emerald-400 border-emerald-500/20';
  }
  if (action.startsWith('remove') || action.startsWith('delete') || action.startsWith('revoke') || action.startsWith('suspend') || action.startsWith('drain')) {
    return 'bg-rose-500/15 text-rose-400 border-rose-500/20';
  }
  return 'bg-amber-500/15 text-amber-400 border-amber-500/20';
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

export function SystemAudit() {
  const location = useLocation();
  const [entries, setEntries] = useState<SystemAuditEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [searchQuery, setSearchQuery] = useState('');
  const [actionFilter, setActionFilter] = useState('all');
  const [selectedEntry, setSelectedEntry] = useState<SystemAuditEntry | null>(null);
  const [refreshSpin, setRefreshSpin] = useState(false);
  const [lastRefreshed, setLastRefreshed] = useState<Date | null>(null);
  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const loadLogs = useCallback(async (silent = false, active = true) => {
    if (currentAppPath() !== '/system-audit') return;
    if (!silent && active && currentAppPath() === '/system-audit') setLoading(true);
    if (active && currentAppPath() === '/system-audit') setRefreshSpin(true);
    try {
      const data = await fetchSystemAudit(200);
      if (!active || currentAppPath() !== '/system-audit') return;
      setEntries(data);
      setError(null);
      setLastRefreshed(new Date());
    } catch (err: any) {
      if (!active || currentAppPath() !== '/system-audit') return;
      setError(err.message || 'Failed to load system audit trail');
    } finally {
      if (active && currentAppPath() === '/system-audit') {
        if (!silent) setLoading(false);
        setTimeout(() => {
          if (active && currentAppPath() === '/system-audit') {
            setRefreshSpin(false);
          }
        }, 500);
      }
    }
  }, [location.pathname]);

  // Initial load + auto-refresh every 30 s
  useEffect(() => {
    if (currentAppPath() !== '/system-audit') return;
    let active = true;
    loadLogs(false, active);
    intervalRef.current = setInterval(() => {
      if (active && currentAppPath() === '/system-audit') {
        loadLogs(true, active);
      }
    }, AUTO_REFRESH_INTERVAL_MS);
    return () => {
      active = false;
      if (intervalRef.current) clearInterval(intervalRef.current);
    };
  }, [loadLogs, location.pathname]);

  // Close modal on Escape key
  useEffect(() => {
    if (!selectedEntry) return;
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape') setSelectedEntry(null);
    }
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [selectedEntry]);

  const uniqueActions = ['all', ...Array.from(new Set(entries.map((e) => e.action))).sort()];

  const filtered = entries.filter((e) => {
    const matchesAction = actionFilter === 'all' || e.action === actionFilter;
    const q = searchQuery.toLowerCase().trim();
    const matchesSearch =
      !q ||
      e.username.toLowerCase().includes(q) ||
      e.action.toLowerCase().includes(q) ||
      e.target.toLowerCase().includes(q) ||
      e.details.toLowerCase().includes(q) ||
      (e.source_ip || '').toLowerCase().includes(q);
    return matchesAction && matchesSearch;
  });

  const hasActiveFilters = searchQuery.trim() !== '' || actionFilter !== 'all';

  const totalFetched = entries.length;
  const uniqueOperators = new Set(entries.map((e) => e.username)).size;
  const infrastructureChanges = entries.filter((e) => isInfrastructureAction(e.action)).length;

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold tracking-tight text-foreground flex items-center gap-2">
            <Shield className="w-6 h-6 text-primary" />
            System Audit Trail
          </h1>
          <p className="text-sm text-muted-foreground mt-1">
            Review configuration changes, node updates, and administrative actions.
          </p>
        </div>
        <div className="flex items-center gap-3">
          {lastRefreshed && (
            <span className="text-[11px] text-muted-foreground/60 hidden sm:block">
              Updated {lastRefreshed.toLocaleTimeString()}
            </span>
          )}
          <button
            onClick={() => loadLogs()}
            disabled={loading}
            className="flex items-center gap-2 px-3 py-2 bg-secondary text-foreground hover:bg-secondary/80 rounded-lg text-sm font-medium transition-all duration-200 cursor-pointer disabled:opacity-50"
          >
            <RefreshCw className={`w-4 h-4 ${refreshSpin ? 'animate-spin' : ''}`} />
            Refresh Logs
          </button>
        </div>
      </div>

      {/* Stats Cards */}
      <div className="grid grid-cols-1 md:grid-cols-3 gap-6">
        <div className="bg-card/50 backdrop-blur-sm border border-border/80 rounded-xl p-5 shadow-sm hover:shadow-md transition-all duration-300 relative overflow-hidden group">
          <div className="absolute right-0 top-0 w-24 h-24 bg-gradient-to-br from-blue-500/10 to-transparent rounded-bl-full group-hover:scale-110 transition-transform duration-300" />
          <div className="flex items-start justify-between">
            <div>
              <p className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">Actions (last {totalFetched})</p>
              <h3 className="text-3xl font-extrabold mt-2 text-foreground">{filtered.length}</h3>
              {hasActiveFilters && (
                <p className="text-[10px] text-muted-foreground/60 mt-0.5">filtered from {totalFetched}</p>
              )}
            </div>
            <div className="p-2.5 bg-blue-500/10 text-blue-400 border border-blue-500/20 rounded-lg">
              <Terminal className="w-5 h-5" />
            </div>
          </div>
        </div>

        <div className="bg-card/50 backdrop-blur-sm border border-border/80 rounded-xl p-5 shadow-sm hover:shadow-md transition-all duration-300 relative overflow-hidden group">
          <div className="absolute right-0 top-0 w-24 h-24 bg-gradient-to-br from-purple-500/10 to-transparent rounded-bl-full group-hover:scale-110 transition-transform duration-300" />
          <div className="flex items-start justify-between">
            <div>
              <p className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">Active Operators</p>
              <h3 className="text-3xl font-extrabold mt-2 text-foreground">{uniqueOperators}</h3>
            </div>
            <div className="p-2.5 bg-purple-500/10 text-purple-400 border border-purple-500/20 rounded-lg">
              <User className="w-5 h-5" />
            </div>
          </div>
        </div>

        <div className="bg-card/50 backdrop-blur-sm border border-border/80 rounded-xl p-5 shadow-sm hover:shadow-md transition-all duration-300 relative overflow-hidden group">
          <div className="absolute right-0 top-0 w-24 h-24 bg-gradient-to-br from-amber-500/10 to-transparent rounded-bl-full group-hover:scale-110 transition-transform duration-300" />
          <div className="flex items-start justify-between">
            <div>
              <p className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">Infrastructure Events</p>
              <h3 className="text-3xl font-extrabold mt-2 text-foreground">{infrastructureChanges}</h3>
            </div>
            <div className="p-2.5 bg-amber-500/10 text-amber-400 border border-amber-500/20 rounded-lg">
              <Shield className="w-5 h-5" />
            </div>
          </div>
        </div>
      </div>

      {/* Filters Bar */}
      <div className="bg-card/50 backdrop-blur-sm border border-border/80 rounded-xl p-4 flex flex-col md:flex-row gap-4 items-center justify-between shadow-sm">
        <div className="relative w-full md:max-w-md">
          <Search className="w-4 h-4 absolute left-3 top-1/2 -translate-y-1/2 text-muted-foreground" />
          <input
            type="text"
            placeholder="Search operators, targets, IPs or action details..."
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            className="w-full pl-9 pr-4 py-2 bg-secondary/80 text-foreground border border-border rounded-lg text-sm focus:outline-none focus:ring-1 focus:ring-primary focus:border-primary transition-all duration-150 placeholder:text-muted-foreground/60"
          />
        </div>

        <div className="flex items-center gap-2 w-full md:w-auto">
          <Filter className="w-4 h-4 text-muted-foreground shrink-0" />
          <select
            value={actionFilter}
            onChange={(e) => setActionFilter(e.target.value)}
            className="w-full md:w-64 px-3 py-2 bg-secondary/80 text-foreground border border-border rounded-lg text-sm focus:outline-none focus:ring-1 focus:ring-primary focus:border-primary transition-all duration-150 cursor-pointer"
          >
            {uniqueActions.map((act) => (
              <option key={act} value={act}>
                {act === 'all' ? 'All Action Types' : getActionLabel(act)}
              </option>
            ))}
          </select>
          {hasActiveFilters && (
            <button
              onClick={() => { setSearchQuery(''); setActionFilter('all'); }}
              title="Clear all filters"
              className="p-2 rounded-lg text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors shrink-0"
            >
              <X className="w-4 h-4" />
            </button>
          )}
        </div>
      </div>

      {/* Main Table */}
      <div className="bg-card/30 backdrop-blur-sm border border-border/60 rounded-xl overflow-hidden shadow-sm">
        {loading ? (
          <div className="p-12 text-center text-muted-foreground text-sm flex flex-col items-center justify-center gap-2">
            <RefreshCw className="w-6 h-6 animate-spin text-primary" />
            Loading system events...
          </div>
        ) : error ? (
          <div className="p-8 text-center text-rose-400 text-sm flex flex-col items-center justify-center gap-2">
            <AlertCircle className="w-6 h-6 text-rose-400" />
            {error}
          </div>
        ) : filtered.length === 0 ? (
          <div className="p-12 text-center text-muted-foreground text-sm flex flex-col items-center justify-center gap-2">
            <Search className="w-6 h-6 text-muted-foreground/50" />
            No audit records found matching your filters.
            {hasActiveFilters && (
              <button
                onClick={() => { setSearchQuery(''); setActionFilter('all'); }}
                className="text-primary hover:underline text-xs mt-1"
              >
                Clear filters
              </button>
            )}
          </div>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full border-collapse text-left text-sm">
              <thead>
                <tr className="border-b border-border/60 bg-secondary/40 text-muted-foreground font-medium">
                  <th className="px-5 py-3.5">Time</th>
                  <th className="px-5 py-3.5">Operator</th>
                  <th className="px-5 py-3.5">Action</th>
                  <th className="px-5 py-3.5">Target</th>
                  <th className="px-5 py-3.5">Source IP</th>
                  <th className="px-5 py-3.5">Details</th>
                  <th className="px-5 py-3.5 text-right">View</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border/40">
                {filtered.map((e, index) => (
                  <tr
                    key={`${e.time}-${e.action}-${e.username}-${index}`}
                    className="hover:bg-secondary/20 transition-all duration-150 group cursor-pointer"
                    onClick={() => setSelectedEntry(e)}
                  >
                    <td className="px-5 py-3.5 font-mono text-xs text-muted-foreground whitespace-nowrap">
                      {formatDateTime(e.time)}
                    </td>
                    <td className="px-5 py-3.5 font-medium text-foreground">
                      <div className="flex items-center gap-1.5">
                        <div className="w-5 h-5 rounded bg-primary/10 text-primary flex items-center justify-center text-[10px] font-bold">
                          {e.username.slice(0, 2).toUpperCase()}
                        </div>
                        {e.username}
                      </div>
                    </td>
                    <td className="px-5 py-3.5 whitespace-nowrap">
                      <span className={`inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium border ${getActionColor(e.action)}`}>
                        {getActionLabel(e.action)}
                      </span>
                    </td>
                    <td className="px-5 py-3.5 font-mono text-xs text-foreground max-w-[120px] truncate" title={e.target}>
                      {e.target}
                    </td>
                    <td className="px-5 py-3.5 font-mono text-xs text-muted-foreground whitespace-nowrap">
                      {e.source_ip || '-'}
                    </td>
                    <td className="px-5 py-3.5 text-muted-foreground max-w-[320px] truncate" title={e.details}>
                      {e.details}
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
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {/* Details Inspector Modal */}
      <Modal
        isOpen={selectedEntry !== null}
        onClose={() => setSelectedEntry(null)}
        title="Audit Record Details"
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
                <p className="text-[10px] uppercase tracking-wider font-semibold text-muted-foreground">Action</p>
                <p className="mt-1">
                  <span className={`inline-flex items-center px-2 py-0.5 rounded-full text-xs font-semibold border ${getActionColor(selectedEntry.action)}`}>
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
                {selectedEntry.details}
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
