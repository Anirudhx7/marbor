import { useState, useEffect, useMemo, type InputHTMLAttributes } from 'react';
import { useLocation } from 'react-router-dom';
import { X } from 'lucide-react';
import { RequestEntry } from '../types';
import { fetchAuditLog, fetchNodes, fetchKeys } from '../lib/api';
import { useDemoMode, currentAppPath } from '../hooks/useDemoMode';
import { filterMockRequests, mockGPUNodes, mockAPIKeys } from '../lib/mockData';

function formatRelative(isoString: string): string {
  const diffMs = Date.now() - new Date(isoString).getTime();
  const diffSecs = Math.floor(diffMs / 1000);
  if (diffSecs < 60) return `${diffSecs}s ago`;
  const diffMins = Math.floor(diffSecs / 60);
  if (diffMins < 60) return `${diffMins}m ago`;
  const diffHours = Math.floor(diffMins / 60);
  if (diffHours < 24) return `${diffHours}h ago`;
  return `${Math.floor(diffHours / 24)}d ago`;
}

function StatusBadge({ status }: { status: number }) {
  let cls = 'text-xs font-mono font-semibold px-1.5 py-0.5 rounded ';
  if (status >= 200 && status < 300) cls += 'bg-green-500/15 text-green-400';
  else if (status >= 400 && status < 500) cls += 'bg-yellow-500/15 text-yellow-400';
  else cls += 'bg-red-500/15 text-red-400';
  return <span className={cls}>{status}</span>;
}

function SkeletonRow() {
  return (
    <tr className="border-b border-border">
      {[...Array(7)].map((_, i) => (
        <td key={i} className="px-4 py-3">
          <div className="h-4 bg-secondary rounded animate-pulse" style={{ width: `${40 + (i * 17) % 50}%` }} />
        </td>
      ))}
    </tr>
  );
}

// ClearableInput wraps a text/datetime filter input with an inline "x"
// button once it has a value, so undoing a filter (including one picked
// from a datalist) doesn't require manually backspacing the whole thing.
function ClearableInput(props: InputHTMLAttributes<HTMLInputElement> & { onClear: () => void }) {
  const { onClear, className, value, ...rest } = props;
  return (
    <div className="relative w-full sm:w-auto sm:flex-1 sm:max-w-[220px]">
      <input
        {...rest}
        value={value}
        className={`${className ?? ''} w-full ${value ? 'pr-7' : ''}`}
      />
      {!!value && (
        <button
          type="button"
          onClick={onClear}
          title="Clear"
          className="absolute right-1.5 top-1/2 -translate-y-1/2 p-0.5 rounded text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors"
        >
          <X className="w-3.5 h-3.5" />
        </button>
      )}
    </div>
  );
}

// Cloud filter tri-state: 'all' | 'local' | 'cloud'.
type CloudFilter = 'all' | 'local' | 'cloud';

// Status category filter: 'all' sends no status param.
type StatusFilter = 'all' | 'success' | 'client_error' | 'server_error';

// Since filter presets map to a lookback window; 'all' sends no since param.
type SincePreset = 'all' | '15m' | '1h' | '24h';

function sinceIso(preset: SincePreset): string | undefined {
  if (preset === 'all') return undefined;
  const ms = { '15m': 15 * 60_000, '1h': 60 * 60_000, '24h': 24 * 60 * 60_000 }[preset];
  return new Date(Date.now() - ms).toISOString();
}

export function Requests() {
  const { demoMode: isDemoMode } = useDemoMode();
  const location = useLocation();
  const [entries, setEntries] = useState<RequestEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [fetchError, setFetchError] = useState<string | null>(null);

  // Raw text inputs (debounced before becoming active filters).
  const [modelInput, setModelInput] = useState('');
  const [keyInput, setKeyInput] = useState('');
  const [nodeInput, setNodeInput] = useState('');
  const [cloudFilter, setCloudFilter] = useState<CloudFilter>('all');
  const [statusFilter, setStatusFilter] = useState<StatusFilter>('all');
  const [sincePreset, setSincePreset] = useState<SincePreset>('all');
  const [sinceInput, setSinceInput] = useState(''); // datetime-local value, overrides sincePreset
  const [untilInput, setUntilInput] = useState(''); // datetime-local value

  // Debounced text filters actually sent as query params.
  const [modelFilter, setModelFilter] = useState('');
  const [keyFilter, setKeyFilter] = useState('');
  const [nodeFilter, setNodeFilter] = useState('');

  // Known node names / key names for this mesh's current config, so the
  // filter bar offers a searchable list instead of demanding an exact
  // hand-typed match (nodes/keys are a bounded, dynamic set - not free text).
  const [nodeOptions, setNodeOptions] = useState<string[]>([]);
  const [keyOptions, setKeyOptions] = useState<string[]>([]);

  useEffect(() => {
    let cancelled = false;
    if (isDemoMode) {
      setNodeOptions(mockGPUNodes.map((n) => n.name));
      setKeyOptions(mockAPIKeys.map((k) => k.name));
      return;
    }
    Promise.all([fetchNodes(), fetchKeys()])
      .then(([nodes, keys]) => {
        if (cancelled) return;
        setNodeOptions(nodes.map((n) => n.name));
        setKeyOptions(keys.map((k) => k.name));
      })
      .catch(() => {
        // Filter bar still works as free text if this fails - non-fatal.
      });
    return () => {
      cancelled = true;
    };
  }, [isDemoMode]);

  useEffect(() => {
    const t = setTimeout(() => setModelFilter(modelInput.trim()), 350);
    return () => clearTimeout(t);
  }, [modelInput]);

  useEffect(() => {
    const t = setTimeout(() => setKeyFilter(keyInput.trim()), 350);
    return () => clearTimeout(t);
  }, [keyInput]);

  useEffect(() => {
    const t = setTimeout(() => setNodeFilter(nodeInput.trim()), 350);
    return () => clearTimeout(t);
  }, [nodeInput]);

  const activeFilters = useMemo(
    () => ({
      model: modelFilter || undefined,
      key: keyFilter || undefined,
      node: nodeFilter || undefined,
      status: statusFilter === 'all' ? undefined : statusFilter,
      cloud: cloudFilter === 'all' ? undefined : cloudFilter === 'cloud',
      since: sinceInput ? new Date(sinceInput).toISOString() : sinceIso(sincePreset),
      until: untilInput ? new Date(untilInput).toISOString() : undefined,
    }),
    [modelFilter, keyFilter, nodeFilter, statusFilter, cloudFilter, sincePreset, sinceInput, untilInput]
  );

  useEffect(() => {
    if (currentAppPath() !== '/requests') return;
    if (isDemoMode) {
      setEntries(filterMockRequests(activeFilters));
      setLoading(false);
      return;
    }

    let cancelled = false;

    async function poll() {
      try {
        const data = await fetchAuditLog(activeFilters);
        if (!cancelled && currentAppPath() === '/requests') {
          setEntries(Array.isArray(data) ? data : []);
          setFetchError(null);
          setLoading(false);
        }
      } catch (err: any) {
        if (!cancelled && currentAppPath() === '/requests') {
          setFetchError(err.message || 'Failed to load requests');
          setLoading(false);
        }
      }
    }

    setLoading(true);
    poll();
    const interval = setInterval(poll, 3000);
    return () => {
      cancelled = true;
      clearInterval(interval);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isDemoMode, modelFilter, keyFilter, nodeFilter, statusFilter, cloudFilter, sincePreset, sinceInput, untilInput, location.pathname]);

  const filtered = entries;
  const hasActiveFilter =
    !!modelFilter || !!keyFilter || !!nodeFilter || statusFilter !== 'all' || cloudFilter !== 'all' || sincePreset !== 'all' || !!sinceInput || !!untilInput;

  const localCount = filtered.filter((e) => !e.cloud).length;
  const cloudCount = filtered.filter((e) => e.cloud).length;
  const avgLatency =
    filtered.length > 0
      ? Math.round(filtered.reduce((sum, e) => sum + e.latency_ms, 0) / filtered.length)
      : 0;

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex items-start justify-between">
        <div>
          <div className="flex items-center gap-2">
            <h1 className="text-2xl font-semibold text-foreground">Request Log</h1>
            <span className="flex items-center gap-1.5 text-xs font-medium text-green-400 bg-green-500/10 px-2 py-0.5 rounded-full">
              <span className="w-1.5 h-1.5 rounded-full bg-green-400 animate-pulse" />
              Live
            </span>
          </div>
          <p className="text-sm text-muted-foreground mt-1">
            Server-side filtered - auto-refreshes every 3s
          </p>
        </div>
      </div>

      {/* Filter bar */}
      <div className="flex flex-wrap items-center gap-2 sm:gap-3">
        <ClearableInput
          type="text"
          placeholder="Filter by model..."
          value={modelInput}
          onChange={(e) => setModelInput(e.target.value)}
          onClear={() => setModelInput('')}
          className="px-3 py-2 text-sm rounded-md border border-border bg-card text-foreground placeholder:text-muted-foreground focus:outline-none focus:ring-2 focus:ring-primary/40"
        />
        <ClearableInput
          type="text"
          list="request-key-options"
          placeholder="Filter by key name..."
          value={keyInput}
          onChange={(e) => setKeyInput(e.target.value)}
          onClear={() => setKeyInput('')}
          className="px-3 py-2 text-sm rounded-md border border-border bg-card text-foreground placeholder:text-muted-foreground focus:outline-none focus:ring-2 focus:ring-primary/40"
        />
        <datalist id="request-key-options">
          {keyOptions.map((name) => (
            <option key={name} value={name} />
          ))}
        </datalist>
        <ClearableInput
          type="text"
          list="request-node-options"
          placeholder="Filter by node..."
          value={nodeInput}
          onChange={(e) => setNodeInput(e.target.value)}
          onClear={() => setNodeInput('')}
          className="px-3 py-2 text-sm rounded-md border border-border bg-card text-foreground placeholder:text-muted-foreground focus:outline-none focus:ring-2 focus:ring-primary/40"
        />
        <datalist id="request-node-options">
          {nodeOptions.map((name) => (
            <option key={name} value={name} />
          ))}
        </datalist>
        <select
          value={cloudFilter}
          onChange={(e) => setCloudFilter(e.target.value as CloudFilter)}
          className="w-full sm:w-auto px-3 py-2 text-sm rounded-md border border-border bg-card text-foreground focus:outline-none focus:ring-2 focus:ring-primary/40"
        >
          <option value="all">All (local + cloud)</option>
          <option value="local">Local only</option>
          <option value="cloud">Cloud only</option>
        </select>
        <select
          value={statusFilter}
          onChange={(e) => setStatusFilter(e.target.value as StatusFilter)}
          className="w-full sm:w-auto px-3 py-2 text-sm rounded-md border border-border bg-card text-foreground focus:outline-none focus:ring-2 focus:ring-primary/40"
        >
          <option value="all">All statuses</option>
          <option value="success">Success (2xx)</option>
          <option value="client_error">Client error (4xx)</option>
          <option value="server_error">Server error (5xx)</option>
        </select>
        <select
          value={sincePreset}
          onChange={(e) => {
            setSincePreset(e.target.value as SincePreset);
            setSinceInput(''); // a quick preset always wins over a stale custom "From" value
          }}
          disabled={!!sinceInput}
          title={sinceInput ? 'Clear the custom "From" date to use a quick preset' : undefined}
          className="w-full sm:w-auto px-3 py-2 text-sm rounded-md border border-border bg-card text-foreground focus:outline-none focus:ring-2 focus:ring-primary/40 disabled:opacity-50"
        >
          <option value="all">Any time</option>
          <option value="15m">Last 15 min</option>
          <option value="1h">Last hour</option>
          <option value="24h">Last 24h</option>
        </select>
        <ClearableInput
          type="datetime-local"
          value={sinceInput}
          onChange={(e) => {
            setSinceInput(e.target.value);
            if (e.target.value) setSincePreset('all'); // custom "From" wins over a stale preset
          }}
          onClear={() => setSinceInput('')}
          title="Only show requests at or after this time"
          className="px-3 py-2 text-sm rounded-md border border-border bg-card text-foreground focus:outline-none focus:ring-2 focus:ring-primary/40"
        />
        <ClearableInput
          type="datetime-local"
          value={untilInput}
          onChange={(e) => setUntilInput(e.target.value)}
          onClear={() => setUntilInput('')}
          title="Only show requests before this time"
          className="px-3 py-2 text-sm rounded-md border border-border bg-card text-foreground focus:outline-none focus:ring-2 focus:ring-primary/40"
        />
        {hasActiveFilter && (
          <button
            onClick={() => {
              setModelInput('');
              setKeyInput('');
              setNodeInput('');
              setCloudFilter('all');
              setStatusFilter('all');
              setSincePreset('all');
              setSinceInput('');
              setUntilInput('');
            }}
            className="text-xs text-muted-foreground hover:text-foreground transition-colors px-2 py-2"
          >
            Clear filters
          </button>
        )}
      </div>

      {/* Stats chips */}
      <div className="flex flex-wrap gap-3">
        {[
          { label: 'Total shown', value: filtered.length },
          { label: 'Local', value: localCount },
          { label: 'Cloud', value: cloudCount },
          { label: 'Avg latency', value: `${avgLatency} ms` },
        ].map((stat) => (
          <div
            key={stat.label}
            className="flex items-center gap-2 bg-card border border-border rounded-lg px-4 py-2 text-sm"
          >
            <span className="text-muted-foreground">{stat.label}</span>
            <span className="font-semibold text-foreground">{stat.value}</span>
          </div>
        ))}
      </div>

      {fetchError && (
        <div className="p-4 bg-destructive/10 border border-destructive/20 rounded-xl text-destructive text-sm font-medium">
          {fetchError}
        </div>
      )}

      {/* Table */}
      <div className="bg-card border border-border rounded-lg overflow-hidden">
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-border bg-secondary/30">
                <th className="px-4 py-3 text-left text-xs font-medium text-muted-foreground uppercase tracking-wider">Time</th>
                <th className="px-4 py-3 text-left text-xs font-medium text-muted-foreground uppercase tracking-wider">Request ID</th>
                <th className="px-4 py-3 text-left text-xs font-medium text-muted-foreground uppercase tracking-wider">Key</th>
                <th className="px-4 py-3 text-left text-xs font-medium text-muted-foreground uppercase tracking-wider">Model</th>
                <th className="px-4 py-3 text-left text-xs font-medium text-muted-foreground uppercase tracking-wider">Node</th>
                <th className="px-4 py-3 text-left text-xs font-medium text-muted-foreground uppercase tracking-wider">Status</th>
                <th className="px-4 py-3 text-right text-xs font-medium text-muted-foreground uppercase tracking-wider">Latency</th>
              </tr>
            </thead>
            <tbody>
              {loading ? (
                [...Array(5)].map((_, i) => <SkeletonRow key={i} />)
              ) : filtered.length === 0 ? (
                <tr>
                  <td colSpan={7} className="px-4 py-16 text-center text-muted-foreground text-sm">
                    {hasActiveFilter
                      ? 'No requests match your filter.'
                      : 'No requests yet. Send a request through the proxy to see it here.'}
                  </td>
                </tr>
              ) : (
                filtered.map((entry) => (
                  <tr
                    key={entry.id}
                    className="border-b border-border last:border-0 hover:bg-secondary/50 transition-colors"
                  >
                    <td className="px-4 py-3 text-muted-foreground whitespace-nowrap">
                      {formatRelative(entry.time)}
                    </td>
                    <td className="px-4 py-3">
                      <span
                        className="font-mono text-xs text-foreground"
                        title={entry.id}
                      >
                        {entry.id}
                      </span>
                    </td>
                    <td className="px-4 py-3 whitespace-nowrap">
                      {entry.key_name ? (
                        <span className="font-mono text-xs bg-primary/10 text-primary px-1.5 py-0.5 rounded">
                          {entry.key_name}
                        </span>
                      ) : entry.source_ip ? (
                        <span className="font-mono text-xs text-muted-foreground" title="No API key - showing source IP">
                          {entry.source_ip}
                        </span>
                      ) : (
                        <span className="text-muted-foreground/40 text-xs">-</span>
                      )}
                    </td>
                    <td className="px-4 py-3">
                      <span className="font-mono text-xs text-foreground">{entry.model}</span>
                    </td>
                    <td className="px-4 py-3 whitespace-nowrap">
                      {entry.cloud ? (
                        <span className="text-xs font-medium px-2 py-0.5 rounded-full bg-amber-500/15 text-amber-400">
                          cloud:{entry.node.replace('cloud:', '')}
                        </span>
                      ) : (
                        <span className="text-foreground">{entry.node || '-'}</span>
                      )}
                    </td>
                    <td className="px-4 py-3">
                      <StatusBadge status={entry.status} />
                    </td>
                    <td className="px-4 py-3 text-right font-mono text-xs text-muted-foreground">
                      {entry.latency_ms} ms
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  );
}
