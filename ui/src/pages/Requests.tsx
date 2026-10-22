import { useState, useEffect, useRef, useCallback } from 'react';
import { useLocation, useSearchParams } from 'react-router-dom';
import { Inbox, X } from 'lucide-react';
import { CustomSelect, CustomCombobox } from '../components/Select';
import { CustomDateTimePicker } from '../components/DateTimePicker';
import { ClearableInput, FilterField, FilterBar, FilterBarGrid, FilterBarClear } from '../components/FilterField';
import { RequestsPager } from '../components/RequestsPager';
import { RequestRow, RequestCard } from '../components/RequestRows';
import { COLUMNS, SkeletonRow } from '../components/RequestLogParts';
import { RequestEntry, RoutingDecision } from '../types';
import { fetchAuditPage, fetchNodes, fetchKeys, fetchRequestExplain } from '../lib/api';
import { useDemoMode, currentAppPath } from '../hooks/useDemoMode';
import { useMediaQuery } from '../hooks/useMediaQuery';
import { pageMockRequests, mockGPUNodes, mockAPIKeys, mockExplainForRequest } from '../lib/mockData';
import { useTimezone } from '../hooks/useTimezone';
import { wallDateTimeToUtcIso } from '../lib/time';
import { EmptyState } from '../components/EmptyState';
import {
  parseRequestsViewNotes,
  serializeRequestsView,
  filtersKey,
  resolveCursor,
  classifyUrlChange,
  shouldGoLive,
  pollIntervalMs,
  cursorParams,
  removeChipFromView,
  clearedView,
  emptyStateKind,
  activeFilterChips,
  loadedSummary,
  type CloudFilter,
  type StatusFilter,
  type SincePreset,
  type PageCursor,
  type RequestsView,
  type FilterChip,
} from '../lib/requestsView';

// One fetched page of the request log: the rows plus the cursors the server
// returned. A cursor is present only when a matching newer / older row exists.
interface PageData {
  entries: RequestEntry[];
  nextBeforeId?: number;
  prevAfterId?: number;
}

// The bottom pager saves scrolling back up on a long page.
const BOTTOM_PAGER_MIN_ROWS = 25;
// Relative times ("3m ago") are re-rendered on this cadence even when a
// refresh returns the same rows.
const TIME_TICK_MS = 30_000;

// Quick-range presets map to a lookback window; 'all' sends no since param.
function sinceIso(preset: SincePreset): string | undefined {
  if (preset === 'all' || preset === 'custom') return undefined;
  const ms = { '15m': 15 * 60_000, '1h': 60 * 60_000, '24h': 24 * 60 * 60_000 }[preset];
  return new Date(Date.now() - ms).toISOString();
}

// Keep the previous row objects when a refreshed row is unchanged, so memoized
// rows skip re-rendering, and keep the whole previous page when nothing moved.
function reusePage(prev: PageData | null, next: PageData): PageData {
  if (prev === null) return next;
  const old = new Map(prev.entries.map((e) => [e.id, e]));
  let allSame =
    prev.entries.length === next.entries.length &&
    prev.nextBeforeId === next.nextBeforeId &&
    prev.prevAfterId === next.prevAfterId;
  const entries = next.entries.map((e, i) => {
    const before = old.get(e.id);
    const same = before !== undefined && JSON.stringify(before) === JSON.stringify(e);
    if (!same || prev.entries[i]?.id !== e.id) allSame = false;
    return same ? before : e;
  });
  return allSame ? prev : { ...next, entries };
}

// The notice shown when a pasted or old link carried values the page could not
// use, so a shared link never silently shows a different view.
function ignoredNotice(ignored: string[]): string | null {
  if (ignored.length === 0) return null;
  return `This link had values the page could not use (${[...new Set(ignored)].join(', ')}); defaults are shown for them.`;
}

export function Requests() {
  const tz = useTimezone();
  const { demoMode: isDemoMode } = useDemoMode();
  const location = useLocation();
  const [searchParams, setSearchParams] = useSearchParams();
  // The URL seeds the view on mount and whenever it changes from outside (a
  // pasted link, the sidebar link, history); the page's own changes are
  // written back with replace so each keystroke is not a history entry.
  const [parsed] = useState(() => parseRequestsViewNotes(searchParams));
  const initial = parsed.view;
  const [notice, setNotice] = useState<string | null>(() => ignoredNotice(parsed.ignored));
  const [page, setPage] = useState<PageData | null>(null);
  const entries = page?.entries ?? [];
  const [loading, setLoading] = useState(true);
  const [fetchError, setFetchError] = useState<string | null>(null);
  const [retryTick, setRetryTick] = useState(0);
  const [timeTick, setTimeTick] = useState(0);
  const topRef = useRef<HTMLDivElement>(null);
  const focusAfterLoad = useRef(false);
  const keepPage = useRef(false);
  // Render one layout, not both: with a 500-row page the hidden twin would
  // double the DOM.
  const isDesktop = useMediaQuery('(min-width: 768px)', true);

  // Routing explainability: which row (if any) has its breakdown expanded,
  // and a per-id cache of the fetched/looked-up decision so re-expanding a row
  // already viewed this session doesn't re-fetch.
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [explainData, setExplainData] = useState<Record<string, RoutingDecision | 'loading' | 'error'>>({});
  const explainRequested = useRef(new Set<string>());

  const toggleExplain = useCallback(
    (id: string) => {
      setExpandedId((cur) => (cur === id ? null : id));
      if (explainRequested.current.has(id)) return;
      explainRequested.current.add(id);
      setExplainData((prev) => ({ ...prev, [id]: 'loading' }));
      if (isDemoMode) {
        const decision = mockExplainForRequest(id);
        setExplainData((prev) => ({ ...prev, [id]: decision ?? 'error' }));
        return;
      }
      fetchRequestExplain(id)
        .then((decision) => setExplainData((prev) => ({ ...prev, [id]: decision })))
        .catch(() => {
          explainRequested.current.delete(id);
          setExplainData((prev) => ({ ...prev, [id]: 'error' }));
        });
    },
    [isDemoMode],
  );

  // Raw text inputs (debounced before becoming active filters).
  const [modelInput, setModelInput] = useState(initial.model);
  const [keyInput, setKeyInput] = useState(initial.key);
  const [nodeInput, setNodeInput] = useState(initial.node);
  const [cloudFilter, setCloudFilter] = useState<CloudFilter>(initial.cloud);
  const [statusFilter, setStatusFilter] = useState<StatusFilter>(initial.status);
  const [sincePreset, setSincePreset] = useState<SincePreset>(initial.range);
  const [sinceInput, setSinceInput] = useState(initial.from); // datetime-local value, overrides sincePreset
  const [untilInput, setUntilInput] = useState(initial.until); // datetime-local value
  const [pageSize, setPageSize] = useState(initial.size);

  // Debounced text filters actually sent as query params.
  const [modelFilter, setModelFilter] = useState(initial.model);
  const [keyFilter, setKeyFilter] = useState(initial.key);
  const [nodeFilter, setNodeFilter] = useState(initial.node);

  // The page cursor belongs to one filter set. It is stored with the key of
  // the filters it was issued under; when the key changes the stored cursor is
  // replaced during render, so changing a filter or the page size and then
  // changing it back cannot bring the old page back.
  const filterKey = filtersKey({
    model: modelFilter, key: keyFilter, node: nodeFilter, cloud: cloudFilter, status: statusFilter,
    range: sincePreset, from: sinceInput, until: untilInput, size: pageSize,
  });
  const [cursorState, setCursorState] = useState<{ key: string; cursor: PageCursor | null }>(() => ({
    key: filtersKey(initial),
    cursor: initial.cursor,
  }));
  if (cursorState.key !== filterKey) setCursorState({ key: filterKey, cursor: null });
  const cursor = resolveCursor(cursorState, filterKey);
  const setCursor = (next: PageCursor | null) => setCursorState({ key: filterKey, cursor: next });

  // Set every part of the view at once (URL adoption, chip removal, Clear all).
  // The debounced filters are set too, so nothing waits out the typing delay.
  const applyView = useCallback((next: RequestsView) => {
    setModelInput(next.model);
    setKeyInput(next.key);
    setNodeInput(next.node);
    setModelFilter(next.model);
    setKeyFilter(next.key);
    setNodeFilter(next.node);
    setCloudFilter(next.cloud);
    setStatusFilter(next.status);
    setSincePreset(next.range);
    setSinceInput(next.from);
    setUntilInput(next.until);
    setPageSize(next.size);
    setCursorState({ key: filtersKey(next), cursor: next.cursor });
  }, []);

  // Known node names / key names for this marbor's current config, so the
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
      .catch((err) => {
        // Non-fatal: the filter bar still works as free text.
        console.warn('Requests: could not load node and key suggestions', err);
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

  useEffect(() => {
    const id = setInterval(() => setTimeTick((n) => n + 1), TIME_TICK_MS);
    return () => clearInterval(id);
  }, []);

  // Deliberately NOT memoized: a "since" quick-range preset (e.g. "Last
  // hour") must mean a rolling window computed at request time, not a value
  // frozen once and reused on every 3s poll tick - see the poll() call site
  // below, which calls this fresh on every tick instead of closing over a
  // stale memoized object.
  const buildActiveFilters = () => {
    // Wall strings "YYYY-MM-DDTHH:MM" are in `tz` wall time (via the picker),
    // not browser local - convert to UTC via zone-aware helper so the server's
    // `time.Parse(RFC3339)` query sees the correct instant.
    let sinceIsoUtc: string | undefined;
    if (sinceInput) {
      const conv = wallDateTimeToUtcIso(sinceInput, tz);
      sinceIsoUtc = conv ?? new Date(sinceInput).toISOString();
    } else {
      sinceIsoUtc = sinceIso(sincePreset);
    }
    let untilIsoUtc: string | undefined;
    if (untilInput) {
      const conv = wallDateTimeToUtcIso(untilInput, tz);
      untilIsoUtc = conv ?? new Date(untilInput).toISOString();
    }
    return {
      model: modelFilter || undefined,
      key: keyFilter || undefined,
      node: nodeFilter || undefined,
      status: statusFilter === 'all' ? undefined : statusFilter,
      cloud: cloudFilter === 'all' ? undefined : cloudFilter === 'cloud',
      since: sinceIsoUtc,
      until: untilIsoUtc,
    };
  };

  // Paging: the newest page (no cursor) is live and polls; any page reached
  // with Older, or a Newer that has not yet reached the top, is a fixed view
  // that is fetched once and never refreshed, so a refresh cannot move the
  // operator off the rows they are reading. Resume returns to the newest page.
  // A new effect run means a new view (filters, size, cursor, retry), so the
  // previous rows are dropped rather than shown under the wrong heading,
  // except when Newer has just reached the top and the page is going live.
  useEffect(() => {
    if (currentAppPath() !== '/requests') return;
    const params = cursorParams(cursor);
    setFetchError(null);
    if (keepPage.current) keepPage.current = false;
    else setPage(null);

    if (isDemoMode) {
      const demoPage = pageMockRequests(buildActiveFilters(), { limit: pageSize, ...params });
      if (shouldGoLive(cursor, demoPage)) {
        keepPage.current = true;
        setPage(demoPage);
        setCursor(null);
        return;
      }
      setPage(demoPage);
      setLoading(false);
      return;
    }

    let cancelled = false;
    let inFlight = false;

    async function poll() {
      if (inFlight) return;
      inFlight = true;
      try {
        const data = await fetchAuditPage({ ...buildActiveFilters(), limit: pageSize, ...params });
        if (cancelled || currentAppPath() !== '/requests') return;
        setFetchError(null);
        setLoading(false);
        if (shouldGoLive(cursor, data)) {
          // Newer reached the top: show what was fetched and go live.
          keepPage.current = true;
          setPage(data);
          setCursor(null);
          return;
        }
        setPage((prev) => reusePage(prev, data));
      } catch (err: unknown) {
        if (!cancelled && currentAppPath() === '/requests') {
          setFetchError(err instanceof Error && err.message ? err.message : 'Failed to load requests');
          setLoading(false);
        }
      } finally {
        inFlight = false;
      }
    }

    setLoading(true);
    poll();
    const everyMs = pollIntervalMs(cursor);
    const interval = everyMs === null ? null : setInterval(poll, everyMs);
    return () => {
      cancelled = true;
      if (interval !== null) clearInterval(interval);
    };
    // filterKey stands for every filter and the page size; buildActiveFilters,
    // setCursor and the cursor object are rebuilt each render but are fully
    // determined by it plus the cursor's direction and id.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isDemoMode, filterKey, cursor?.dir, cursor?.id, location.pathname, tz, retryTick]);

  const view: RequestsView = {
    model: modelFilter,
    key: keyFilter,
    node: nodeFilter,
    cloud: cloudFilter,
    status: statusFilter,
    range: sincePreset,
    from: sinceInput,
    until: untilInput,
    size: pageSize,
    cursor,
  };
  const viewQuery = serializeRequestsView(view).toString();

  // The URL and the view stay in step both ways. lastSynced is the query the
  // two last agreed on. A view change not yet in the URL is written out so the
  // page can be shared or bookmarked; `written` remembers those writes so a
  // late router update for an earlier one is not mistaken for a new link. A URL
  // the page did not write (a pasted link, the sidebar link, history) is
  // adopted as the new view; unusable values in it are named in a notice and
  // the address bar is rewritten to the view actually shown.
  const lastSynced = useRef(viewQuery);
  const written = useRef(new Set<string>());
  useEffect(() => {
    if (currentAppPath() !== '/requests' || viewQuery === lastSynced.current) return;
    lastSynced.current = viewQuery;
    written.current.add(viewQuery);
    // A view change the user made makes the "link had unusable values" notice
    // out of date.
    setNotice(null);
    setSearchParams(new URLSearchParams(viewQuery), { replace: true });
  }, [viewQuery, setSearchParams]);

  useEffect(() => {
    if (currentAppPath() !== '/requests') return;
    const raw = searchParams.toString();
    if (classifyUrlChange(raw, lastSynced.current, written.current) !== 'external') return;
    const { view: next, ignored } = parseRequestsViewNotes(searchParams);
    const canonical = serializeRequestsView(next).toString();
    setNotice(ignoredNotice(ignored));
    if (canonical !== lastSynced.current) {
      lastSynced.current = canonical;
      applyView(next);
    }
    if (raw !== canonical) setSearchParams(new URLSearchParams(canonical), { replace: true });
  }, [searchParams, applyView, setSearchParams]);

  const chips = activeFilterChips(view);
  const hasActiveFilter = chips.length > 0;
  const hasOlder = page !== null && page.nextBeforeId !== undefined;
  const hasNewer = page !== null && page.prevAfterId !== undefined;
  const livePaused = cursor !== null;
  const inputsPending = modelInput.trim() !== modelFilter || keyInput.trim() !== keyFilter || nodeInput.trim() !== nodeFilter;
  const pagerBusy = loading || inputsPending;
  const showSkeleton = page === null && !fetchError;
  const showEmpty = page !== null && entries.length === 0;
  const summary = page === null ? (fetchError ? '' : 'Loading requests') : loadedSummary(entries.length, hasOlder);

  // After a user-driven page change, bring the top of the list into view and,
  // once the new rows are in, put focus on the list top so keyboard and
  // screen-reader users are not left on a control that just changed.
  const afterPageChange = () => {
    focusAfterLoad.current = true;
    topRef.current?.scrollIntoView?.({ block: 'start' });
  };
  useEffect(() => {
    if (!focusAfterLoad.current || page === null) return;
    focusAfterLoad.current = false;
    topRef.current?.focus({ preventScroll: true });
  }, [page]);

  const goOlder = () => {
    if (pagerBusy || page?.nextBeforeId === undefined) return;
    setCursor({ dir: 'before', id: page.nextBeforeId });
    afterPageChange();
  };
  const goNewer = () => {
    if (pagerBusy || page?.prevAfterId === undefined) return;
    setCursor({ dir: 'after', id: page.prevAfterId });
    afterPageChange();
  };
  const resumeLive = () => {
    setCursor(null);
    afterPageChange();
  };
  const removeChip = (id: FilterChip['id']) => {
    applyView(removeChipFromView(view, id));
    afterPageChange();
  };
  const clearAllFilters = () => {
    applyView(clearedView(view));
    afterPageChange();
  };

  // The empty state says what is filtering, in the same words as the chips.
  const emptyState = (compact: boolean) => {
    const kind = emptyStateKind(cursor, hasActiveFilter);
    const title = kind === 'pastEnd' ? 'No more requests here' : kind === 'filtered' ? 'No requests match your filters' : 'No requests yet';
    const copy =
      kind === 'pastEnd'
        ? 'This page is past the end of the log. Go back to the newest requests.'
        : kind === 'filtered'
          ? `Nothing matches ${chips.map((c) => c.label).join(', ')}. Remove a filter or widen the time range.`
          : 'Send a request through the proxy to see it here.';
    const actionLabel = kind === 'pastEnd' ? 'Back to newest' : kind === 'filtered' ? 'Clear all filters' : null;
    return (
      <EmptyState
        compact={compact}
        icon={Inbox}
        title={title}
        copy={copy}
        action={
          actionLabel ? (
            <button
              type="button"
              onClick={kind === 'pastEnd' ? resumeLive : clearAllFilters}
              className="min-h-[40px] px-2 text-sm font-medium text-primary hover:underline"
            >
              {actionLabel}
            </button>
          ) : undefined
        }
      />
    );
  };

  const localCount = entries.filter((e) => !e.cloud).length;
  const cloudCount = entries.filter((e) => e.cloud).length;
  const avgLatency = entries.length > 0 ? `${Math.round(entries.reduce((sum, e) => sum + e.latency_ms, 0) / entries.length)} ms` : '-';
  const stat = (n: number) => (page === null ? '-' : n);

  const pagerProps = {
    summary,
    hasNewer,
    hasOlder,
    onNewer: goNewer,
    onOlder: goOlder,
    busy: pagerBusy,
  };

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex items-start justify-between">
        <div>
          <div className="flex items-center gap-2">
            <h1 className="text-2xl font-semibold tracking-tight text-foreground">Request log</h1>
            {isDemoMode ? (
              <span className="flex items-center gap-1.5 text-xs font-medium text-amber-600 dark:text-amber-400 bg-amber-500/10 px-2 py-0.5 rounded-md">
                <span className="w-1.5 h-1.5 rounded-full bg-amber-500 dark:bg-amber-400" />
                Demo Mode
              </span>
            ) : livePaused ? (
              <span className="flex items-center gap-1.5 text-xs font-medium text-amber-600 dark:text-amber-400 bg-amber-500/10 px-2 py-0.5 rounded-md">
                <span className="w-1.5 h-1.5 rounded-full bg-amber-500 dark:bg-amber-400" />
                Paused
              </span>
            ) : (
              <span className="flex items-center gap-1.5 text-xs font-medium text-green-600 dark:text-green-400 bg-green-500/10 px-2 py-0.5 rounded-md">
                <span className="w-1.5 h-1.5 rounded-full bg-green-500 dark:bg-green-400 animate-pulse" />
                Live
              </span>
            )}
          </div>
          <p className="text-sm text-muted-foreground mt-1">
            {isDemoMode
              ? 'Showing static sample data'
              : livePaused
                ? 'Viewing an earlier page - live refresh is paused'
                : 'Newest requests - server-side filtered, auto-refreshes every 3s'}
          </p>
        </div>
      </div>

      {notice && (
        <div role="status" className="flex items-start justify-between gap-3 p-3 bg-amber-500/10 border border-amber-500/20 rounded-xl text-sm text-amber-700 dark:text-amber-300">
          <span>{notice}</span>
          <button type="button" onClick={() => setNotice(null)} aria-label="Dismiss notice" className="shrink-0 min-h-[40px] min-w-[40px] grid place-items-center">
            <X className="w-4 h-4" aria-hidden="true" />
          </button>
        </div>
      )}

      {/* Filter bar - uses FilterBar/FilterField shared primitives (parity with Activity) */}
      <FilterBar>
        <FilterBarGrid>
          <FilterField label="Model">
            <ClearableInput
              type="text"
              placeholder="e.g. llama3"
              value={modelInput}
              onChange={(e) => setModelInput(e.target.value)}
              onClear={() => setModelInput('')}
            />
          </FilterField>
          <FilterField label="Key name">
            <CustomCombobox value={keyInput} onChange={setKeyInput} options={keyOptions} placeholder="Any key" />
          </FilterField>
          <FilterField label="Node">
            <CustomCombobox value={nodeInput} onChange={setNodeInput} options={nodeOptions} placeholder="Any node" />
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
              disabled={sincePreset === 'custom'}
              placeholder="Clear custom date to use preset"
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
                setSincePreset(val ? 'custom' : 'all'); // custom "From" wins over a stale preset, clearing it falls back to "Any time"
              }}
              placeholder="Any start time"
            />
          </FilterField>
          <FilterField label="Until">
            <CustomDateTimePicker value={untilInput} onChange={setUntilInput} placeholder="Any end time" />
          </FilterField>
        </FilterBarGrid>
        {hasActiveFilter && (
          <>
            <ul aria-label="Active filters" className="flex flex-wrap gap-2">
              {chips.map((chip) => (
                <li key={chip.id} className="min-w-0 max-w-full">
                  <button
                    type="button"
                    onClick={() => removeChip(chip.id)}
                    aria-label={`Remove filter ${chip.label}`}
                    className="inline-flex items-center gap-1.5 min-h-[40px] max-w-full px-3 py-1 rounded-full border border-border bg-secondary/50 text-xs text-foreground hover:bg-secondary transition-colors"
                  >
                    <span className="truncate">{chip.label}</span>
                    <X className="w-3 h-3 shrink-0 text-muted-foreground" aria-hidden="true" />
                  </button>
                </li>
              ))}
            </ul>
            <FilterBarClear countText={`${chips.length} active ${chips.length === 1 ? 'filter' : 'filters'}`} onClear={clearAllFilters} />
          </>
        )}
      </FilterBar>

      <div
        ref={topRef}
        tabIndex={-1}
        role="group"
        aria-label="Request log pages"
        className="scroll-mt-4 rounded-lg outline-none focus-visible:ring-2 focus-visible:ring-primary/50"
      >
        <RequestsPager
          {...pagerProps}
          label="Request log pages, top"
          announce={livePaused}
          size={pageSize}
          onSize={setPageSize}
          livePaused={livePaused && !isDemoMode}
          onResume={resumeLive}
        />
      </div>

      {/* Stats chips - computed over the rows on this page */}
      <div className="flex flex-wrap gap-3">
        {[
          { label: 'On this page', value: stat(entries.length) },
          { label: 'Local', value: stat(localCount) },
          { label: 'Cloud', value: stat(cloudCount) },
          { label: 'Avg latency', value: avgLatency },
        ].map((s) => (
          <div key={s.label} className="flex items-center gap-2 bg-card border border-border rounded-lg px-4 py-2 text-sm">
            <span className="text-muted-foreground">{s.label}</span>
            <span className="font-semibold text-foreground">{s.value}</span>
          </div>
        ))}
      </div>

      {fetchError && (
        <div role="alert" className="flex flex-wrap items-center justify-between gap-3 p-4 bg-destructive/10 border border-destructive/20 rounded-xl text-destructive text-sm font-medium">
          <span>{fetchError}</span>
          <button
            type="button"
            onClick={() => {
              setRetryTick((n) => n + 1);
              afterPageChange();
            }}
            className="min-h-[40px] px-3 rounded-lg border border-destructive/30 hover:bg-destructive/10 transition-colors"
          >
            Retry
          </button>
        </div>
      )}

      {isDesktop ? (
        <div className="bg-card border border-border rounded-xl overflow-hidden shadow-sm">
          <div className="overflow-x-auto scroll-region" tabIndex={0} role="region" aria-label="Request log table: scroll horizontally for more columns">
            {/*
              The table always fills its card (w-full); column sizing is in
              COLUMNS. min-w stops the flex columns collapsing on a narrow
              card - below it the region scrolls instead of colliding headers.
            */}
            <table className="w-full min-w-[960px] text-sm">
              <thead>
                <tr className="border-b border-border bg-secondary/30">
                  {COLUMNS.map((c) => (
                    <th
                      key={c.label}
                      className={`px-4 py-3 text-xs font-medium text-muted-foreground uppercase tracking-wider whitespace-nowrap ${c.size} ${c.align === 'right' ? 'text-right' : 'text-left'}`}
                    >
                      {c.label}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {showSkeleton ? (
                  [...Array(5)].map((_, i) => <SkeletonRow key={i} />)
                ) : showEmpty ? (
                  <tr>
                    <td colSpan={COLUMNS.length} className="px-4">
                      {emptyState(true)}
                    </td>
                  </tr>
                ) : (
                  entries.map((entry) => (
                    <RequestRow
                      key={entry.id}
                      entry={entry}
                      expanded={expandedId === entry.id}
                      explain={explainData[entry.id]}
                      onToggle={toggleExplain}
                      timeTick={timeTick}
                    />
                  ))
                )}
              </tbody>
            </table>
          </div>
        </div>
      ) : (
        <div className="space-y-3">
          {showSkeleton ? (
            [...Array(5)].map((_, i) => (
              <div key={i} className="bg-card/50 backdrop-blur-sm border border-border/60 rounded-xl p-4 space-y-2">
                {[...Array(4)].map((_, j) => (
                  <div key={j} className="h-4 bg-secondary rounded animate-pulse" style={{ width: `${40 + (j * 17) % 50}%` }} />
                ))}
              </div>
            ))
          ) : showEmpty ? (
            <div className="bg-card/50 backdrop-blur-sm border border-border/60 rounded-xl px-4">{emptyState(false)}</div>
          ) : (
            entries.map((entry) => (
              <RequestCard
                key={entry.id}
                entry={entry}
                expanded={expandedId === entry.id}
                explain={explainData[entry.id]}
                onToggle={toggleExplain}
                timeTick={timeTick}
              />
            ))
          )}
        </div>
      )}

      {(entries.length >= BOTTOM_PAGER_MIN_ROWS || hasNewer || hasOlder) && (
        <RequestsPager {...pagerProps} label="Request log pages, bottom" />
      )}
    </div>
  );
}
