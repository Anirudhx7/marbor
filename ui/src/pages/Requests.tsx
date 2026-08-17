import { useState, useEffect, useMemo, Fragment, type InputHTMLAttributes, type ReactNode } from 'react';
import { useLocation } from 'react-router-dom';
import { X, ChevronDown, ChevronRight } from 'lucide-react';
import { CustomSelect, CustomCombobox } from '../components/Select';
import { CustomDateTimePicker } from '../components/DateTimePicker';
import { RequestEntry, RoutingDecision } from '../types';
import { fetchAuditLog, fetchNodes, fetchKeys, fetchRequestExplain } from '../lib/api';
import { useDemoMode, currentAppPath } from '../hooks/useDemoMode';
import { filterMockRequests, mockGPUNodes, mockAPIKeys, mockExplainForRequest } from '../lib/mockData';
import { formatRelativeTime } from '../lib/time';

const REASON_LABELS: Record<string, string> = {
  session_affinity: 'Session affinity',
  pinned_warm: 'Pinned + warm',
  score_based: 'Score-based',
};

function ReasonBadge({ reason }: { reason?: string }) {
  if (!reason) return <span className="text-muted-foreground/40 text-xs">-</span>;
  return (
    <span className="text-xs font-medium px-1.5 py-0.5 rounded bg-secondary text-foreground/80">
      {REASON_LABELS[reason] ?? reason}
    </span>
  );
}

// ExplainPanel renders the P41 routing-decision breakdown for one request,
// lazily fetched (or looked up from static mock data in demo mode) on first
// expand. State is one of: undefined (not yet fetched), 'loading', 'error',
// or the resolved RoutingDecision.
function ExplainPanel({ state }: { state: RoutingDecision | 'loading' | 'error' | undefined }) {
  if (state === 'loading' || state === undefined) {
    return <div className="text-sm text-muted-foreground">Loading routing explanation...</div>;
  }
  if (state === 'error') {
    return <div className="text-sm text-muted-foreground">No routing decision recorded for this request.</div>;
  }
  return (
    <div className="space-y-2 text-sm">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-muted-foreground">Reason:</span>
        <ReasonBadge reason={state.reason} />
        {state.affinityLost && (
          <span className="text-xs font-medium px-1.5 py-0.5 rounded bg-amber-500/15 text-amber-600 dark:text-amber-400">
            affinity lost
          </span>
        )}
      </div>
      {state.detail && <div className="text-muted-foreground text-xs">{state.detail}</div>}
      {state.components && state.components.length > 0 && (
        <div className="overflow-x-auto">
          <table className="text-xs font-mono">
            <thead>
              <tr className="text-muted-foreground">
                <th className="text-left pr-4 py-1">Component</th>
                <th className="text-right pr-4 py-1">Raw</th>
                <th className="text-right pr-4 py-1">Weight</th>
                <th className="text-right py-1">Value</th>
              </tr>
            </thead>
            <tbody>
              {state.components.map((c) => (
                <tr key={c.name}>
                  <td className="pr-4 py-0.5">{c.name}</td>
                  <td className="text-right pr-4 py-0.5">{c.raw.toFixed(2)}</td>
                  <td className="text-right pr-4 py-0.5">{c.weight}</td>
                  <td className="text-right py-0.5">{c.value.toFixed(2)}</td>
                </tr>
              ))}
              <tr className="border-t border-border font-semibold">
                <td className="pr-4 py-0.5">Score</td>
                <td colSpan={2} />
                <td className="text-right py-0.5">{state.score?.toFixed(2)}</td>
              </tr>
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function StatusBadge({ status }: { status: number }) {
  let cls = 'text-xs font-mono font-semibold px-1.5 py-0.5 rounded ';
  if (status >= 200 && status < 300) cls += 'bg-green-500/15 text-green-600 dark:text-green-400';
  else if (status >= 400 && status < 500) cls += 'bg-yellow-500/15 text-yellow-600 dark:text-yellow-400';
  else cls += 'bg-red-500/15 text-red-600 dark:text-red-400';
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
    <div className="relative w-full">
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

// FilterField adds a small label above a filter control - the bare inputs
// (esp. the two datetime pickers and the tri-state selects) were impossible
// to tell apart at a glance without one.
function FilterField({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <label className="block text-xs font-medium text-muted-foreground mb-1">{label}</label>
      {children}
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

  // P41 routing explainability: which row (if any) has its breakdown
  // expanded, and a per-id cache of the fetched/looked-up decision so
  // re-expanding a row already viewed this session doesn't re-fetch.
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [explainData, setExplainData] = useState<Record<string, RoutingDecision | 'loading' | 'error'>>({});

  function toggleExplain(id: string) {
    if (expandedId === id) {
      setExpandedId(null);
      return;
    }
    setExpandedId(id);
    if (explainData[id]) return;
    setExplainData((prev) => ({ ...prev, [id]: 'loading' }));
    if (isDemoMode) {
      const decision = mockExplainForRequest(id);
      setExplainData((prev) => ({ ...prev, [id]: decision ?? 'error' }));
      return;
    }
    fetchRequestExplain(id)
      .then((decision) => setExplainData((prev) => ({ ...prev, [id]: decision })))
      .catch(() => setExplainData((prev) => ({ ...prev, [id]: 'error' })));
  }

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
      // No pagination UI on this page yet, so request the server's max page
      // size (admin.go caps at 1000) rather than silently truncating to the
      // client default of 50 - "all requests" means all within that cap.
      limit: 1000,
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
            {isDemoMode ? (
              <span className="flex items-center gap-1.5 text-xs font-medium text-amber-600 dark:text-amber-400 bg-amber-500/10 px-2 py-0.5 rounded-full">
                <span className="w-1.5 h-1.5 rounded-full bg-amber-500 dark:bg-amber-400" />
                Demo Mode
              </span>
            ) : (
              <span className="flex items-center gap-1.5 text-xs font-medium text-green-600 dark:text-green-400 bg-green-500/10 px-2 py-0.5 rounded-full">
                <span className="w-1.5 h-1.5 rounded-full bg-green-500 dark:bg-green-400 animate-pulse" />
                Live
              </span>
            )}
          </div>
          <p className="text-sm text-muted-foreground mt-1">
            {isDemoMode ? 'Showing static sample data' : 'Server-side filtered - auto-refreshes every 3s'}
          </p>
        </div>
      </div>

      {/* Filter bar */}
      <div className="bg-card border border-border rounded-lg p-3 sm:p-4 space-y-3">
        <div className="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-4 gap-3">
          <FilterField label="Model">
            <ClearableInput
              type="text"
              placeholder="e.g. llama3"
              value={modelInput}
              onChange={(e) => setModelInput(e.target.value)}
              onClear={() => setModelInput('')}
              className="px-3 py-2 text-sm rounded-md border border-border bg-secondary/30 text-foreground placeholder:text-muted-foreground focus:outline-none focus:ring-2 focus:ring-primary/40"
            />
          </FilterField>
          <FilterField label="Key name">
            <CustomCombobox
              value={keyInput}
              onChange={setKeyInput}
              options={keyOptions}
              placeholder="Any key"
            />
          </FilterField>
          <FilterField label="Node">
            <CustomCombobox
              value={nodeInput}
              onChange={setNodeInput}
              options={nodeOptions}
              placeholder="Any node"
            />
          </FilterField>
          <FilterField label="Local / cloud">
            <CustomSelect
              value={cloudFilter}
              onChange={(val) => setCloudFilter(val as CloudFilter)}
              options={[
                { value: 'all', label: 'All (local + cloud)' },
                { value: 'local', label: 'Local only' },
                { value: 'cloud', label: 'Cloud only' },
              ]}
            />
          </FilterField>
          <FilterField label="Status">
            <CustomSelect
              value={statusFilter}
              onChange={(val) => setStatusFilter(val as StatusFilter)}
              options={[
                { value: 'all', label: 'All statuses' },
                { value: 'success', label: 'Success (2xx)' },
                { value: 'client_error', label: 'Client error (4xx)' },
                { value: 'server_error', label: 'Server error (5xx)' },
              ]}
            />
          </FilterField>
          <FilterField label="Quick range">
            <CustomSelect
              value={sincePreset}
              onChange={(val) => {
                setSincePreset(val as SincePreset);
                setSinceInput(''); // a quick preset always wins over a stale custom "From" value
              }}
              disabled={!!sinceInput}
              placeholder={sinceInput ? 'Clear custom date to use preset' : 'Any time'}
              options={[
                { value: 'all', label: 'Any time' },
                { value: '15m', label: 'Last 15 min' },
                { value: '1h', label: 'Last hour' },
                { value: '24h', label: 'Last 24h' },
              ]}
            />
          </FilterField>
          <FilterField label="From (custom)">
            <CustomDateTimePicker
              value={sinceInput}
              onChange={(val) => {
                setSinceInput(val);
                if (val) setSincePreset('all'); // custom "From" wins over a stale preset
              }}
              placeholder="Any start time"
            />
          </FilterField>
          <FilterField label="Until">
            <CustomDateTimePicker
              value={untilInput}
              onChange={setUntilInput}
              placeholder="Any end time"
            />
          </FilterField>
        </div>
        {hasActiveFilter && (
          <div className="flex justify-end">
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
              className="text-xs text-muted-foreground hover:text-foreground transition-colors px-2 py-1"
            >
              Clear all filters
            </button>
          </div>
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

      {/* Table (md and up) */}
      <div className="hidden md:block">
        <div className="bg-card border border-border rounded-lg overflow-hidden">
          <div className="overflow-x-auto">
            {/*
              Fixed pixel column widths (not %) on purpose: this table polls
              every 3s, so widths must be stable across refreshes rather than
              content-fitted (auto layout would jitter as row content
              changes), and pixel widths - unlike the % widths this replaced -
              don't balloon on a wide viewport (App.tsx caps content at
              1600px; % columns turned into hundreds of px of dead padding
              around 15-char values there). The table intentionally has no
              w-full: table-fixed columns count as SUM=~1085px here, with the
              rest resting as gutter inside the card is preferable to every
              column stretching to fill an ultrawide window.
            */}
            <table className="text-sm table-fixed">
              <colgroup>
                <col className="w-[90px]" />
                <col className="w-[170px]" />
                <col className="w-[160px]" />
                <col className="w-[200px]" />
                <col className="w-[140px]" />
                <col className="w-[75px]" />
                <col className="w-[90px]" />
                <col className="w-[160px]" />
              </colgroup>
              <thead>
                <tr className="border-b border-border bg-secondary/30">
                  <th className="px-4 py-3 text-left text-xs font-medium text-muted-foreground uppercase tracking-wider">Time</th>
                  <th className="px-4 py-3 text-left text-xs font-medium text-muted-foreground uppercase tracking-wider">Request ID</th>
                  <th className="px-4 py-3 text-left text-xs font-medium text-muted-foreground uppercase tracking-wider">Key</th>
                  <th className="px-4 py-3 text-left text-xs font-medium text-muted-foreground uppercase tracking-wider">Model</th>
                  <th className="px-4 py-3 text-left text-xs font-medium text-muted-foreground uppercase tracking-wider">Node</th>
                  <th className="px-4 py-3 text-left text-xs font-medium text-muted-foreground uppercase tracking-wider">Status</th>
                  <th className="px-4 py-3 text-right text-xs font-medium text-muted-foreground uppercase tracking-wider">Latency</th>
                  <th className="px-4 py-3 text-left text-xs font-medium text-muted-foreground uppercase tracking-wider">Why</th>
                </tr>
              </thead>
              <tbody>
                {loading ? (
                  [...Array(5)].map((_, i) => <SkeletonRow key={i} />)
                ) : filtered.length === 0 ? (
                  <tr>
                    <td colSpan={8} className="px-4 py-16 text-center text-muted-foreground text-sm">
                      {hasActiveFilter
                        ? 'No requests match your filter.'
                        : 'No requests yet. Send a request through the proxy to see it here.'}
                    </td>
                  </tr>
                ) : (
                  filtered.map((entry) => (
                    <Fragment key={entry.id}>
                    <tr
                      className="border-b border-border last:border-0 hover:bg-secondary/50 transition-colors"
                    >
                      <td className="px-4 py-3 text-muted-foreground whitespace-nowrap">
                        {formatRelativeTime(entry.time)}
                      </td>
                      <td className="px-4 py-3 max-w-0">
                        <span
                          className="block truncate font-mono text-xs text-foreground"
                          title={entry.id}
                        >
                          {entry.id}
                        </span>
                      </td>
                      <td className="px-4 py-3 max-w-0">
                        {entry.key_name ? (
                          <span
                            className="block truncate font-mono text-xs bg-primary/10 text-primary px-1.5 py-0.5 rounded"
                            title={entry.key_name}
                          >
                            {entry.key_name}
                          </span>
                        ) : entry.source_ip ? (
                          <span className="block truncate font-mono text-xs text-muted-foreground" title="No API key - showing source IP">
                            {entry.source_ip}
                          </span>
                        ) : (
                          <span className="text-muted-foreground/40 text-xs">-</span>
                        )}
                      </td>
                      <td className="px-4 py-3 max-w-0">
                        <span className="block truncate font-mono text-xs text-foreground" title={entry.model}>
                          {entry.model}
                        </span>
                      </td>
                      <td className="px-4 py-3 max-w-0">
                        {entry.cloud ? (
                          <span
                            className="inline-block max-w-full truncate align-middle text-xs font-medium px-2 py-0.5 rounded-full bg-amber-500/15 text-amber-600 dark:text-amber-400"
                            title={`cloud:${entry.node.replace('cloud:', '')}`}
                          >
                            cloud:{entry.node.replace('cloud:', '')}
                          </span>
                        ) : (
                          <span className="block truncate text-foreground" title={entry.node || '-'}>
                            {entry.node || '-'}
                          </span>
                        )}
                      </td>
                      <td className="px-4 py-3">
                        <StatusBadge status={entry.status} />
                      </td>
                      <td className="px-4 py-3 text-right font-mono text-xs text-muted-foreground">
                        {entry.latency_ms} ms
                      </td>
                      <td className="px-4 py-3">
                        <button
                          type="button"
                          onClick={() => toggleExplain(entry.id)}
                          className="flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground transition-colors"
                        >
                          {expandedId === entry.id ? <ChevronDown className="w-3.5 h-3.5" /> : <ChevronRight className="w-3.5 h-3.5" />}
                          <ReasonBadge reason={entry.routingReason} />
                        </button>
                      </td>
                    </tr>
                    {expandedId === entry.id && (
                      <tr className="border-b border-border last:border-0 bg-secondary/20">
                        <td colSpan={8} className="px-4 py-3">
                          <ExplainPanel state={explainData[entry.id]} />
                        </td>
                      </tr>
                    )}
                    </Fragment>
                  ))
                )}
              </tbody>
            </table>
          </div>
        </div>
      </div>

      {/* Card list (below md) */}
      <div className="md:hidden space-y-3">
        {loading ? (
          [...Array(5)].map((_, i) => (
            <div
              key={i}
              className="bg-card/50 backdrop-blur-sm border border-border/60 rounded-xl p-4 space-y-2"
            >
              {[...Array(4)].map((_, j) => (
                <div
                  key={j}
                  className="h-4 bg-secondary rounded animate-pulse"
                  style={{ width: `${40 + (j * 17) % 50}%` }}
                />
              ))}
            </div>
          ))
        ) : filtered.length === 0 ? (
          <div className="bg-card/50 backdrop-blur-sm border border-border/60 rounded-xl p-8 text-center text-muted-foreground text-sm">
            {hasActiveFilter
              ? 'No requests match your filter.'
              : 'No requests yet. Send a request through the proxy to see it here.'}
          </div>
        ) : (
          filtered.map((entry) => (
            <div
              key={entry.id}
              className="bg-card/50 backdrop-blur-sm border border-border/60 rounded-xl p-4 space-y-3 active:bg-secondary/50 transition-colors"
            >
              <div className="flex items-center justify-between gap-2">
                <span className="font-mono text-xs text-foreground truncate" title={entry.id}>
                  {entry.id}
                </span>
                <StatusBadge status={entry.status} />
              </div>
              <div className="grid grid-cols-2 gap-x-3 gap-y-2">
                <div>
                  <div className="text-[10px] uppercase tracking-wider text-muted-foreground">Time</div>
                  <div className="text-sm text-foreground">{formatRelativeTime(entry.time)}</div>
                </div>
                <div>
                  <div className="text-[10px] uppercase tracking-wider text-muted-foreground">Latency</div>
                  <div className="text-sm text-foreground font-mono">{entry.latency_ms} ms</div>
                </div>
                <div>
                  <div className="text-[10px] uppercase tracking-wider text-muted-foreground">Model</div>
                  <div className="text-sm text-foreground font-mono truncate">{entry.model}</div>
                </div>
                <div>
                  <div className="text-[10px] uppercase tracking-wider text-muted-foreground">Node</div>
                  <div className="text-sm text-foreground">
                    {entry.cloud ? (
                      <span className="text-xs font-medium px-2 py-0.5 rounded-full bg-amber-500/15 text-amber-600 dark:text-amber-400">
                        cloud:{entry.node.replace('cloud:', '')}
                      </span>
                    ) : (
                      entry.node || '-'
                    )}
                  </div>
                </div>
                <div className="col-span-2">
                  <div className="text-[10px] uppercase tracking-wider text-muted-foreground">Key</div>
                  <div className="text-sm text-foreground">
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
                  </div>
                </div>
              </div>
              <button
                type="button"
                onClick={() => toggleExplain(entry.id)}
                className="flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground transition-colors pt-1 border-t border-border/60 w-full"
              >
                {expandedId === entry.id ? <ChevronDown className="w-3.5 h-3.5" /> : <ChevronRight className="w-3.5 h-3.5" />}
                <span>Why this node?</span>
                <ReasonBadge reason={entry.routingReason} />
              </button>
              {expandedId === entry.id && (
                <div className="pt-2">
                  <ExplainPanel state={explainData[entry.id]} />
                </div>
              )}
            </div>
          ))
        )}
      </div>
    </div>
  );
}
