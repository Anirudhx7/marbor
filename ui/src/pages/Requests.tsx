import { useState, useEffect } from 'react';
import { RequestEntry } from '../types';
import { getRequests } from '../lib/api';
import { useDemoMode } from '../hooks/useDemoMode';
import { mockRequests } from '../lib/mockData';

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

export function Requests() {
  const { demoMode: isDemoMode } = useDemoMode();
  const [entries, setEntries] = useState<RequestEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [filter, setFilter] = useState('');

  useEffect(() => {
    if (isDemoMode) {
      setEntries(mockRequests);
      setLoading(false);
      return;
    }

    let cancelled = false;

    async function poll() {
      try {
        const data = await getRequests();
        if (!cancelled) {
          setEntries(data);
          setLoading(false);
        }
      } catch {
        if (!cancelled) setLoading(false);
      }
    }

    poll();
    const interval = setInterval(poll, 3000);
    return () => {
      cancelled = true;
      clearInterval(interval);
    };
  }, [isDemoMode]);

  const filtered = filter.trim()
    ? entries.filter(
        (e) =>
          e.model.toLowerCase().includes(filter.toLowerCase()) ||
          e.node.toLowerCase().includes(filter.toLowerCase()) ||
          e.key_name.toLowerCase().includes(filter.toLowerCase())
      )
    : entries;

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
            Last 50 requests - auto-refreshes every 3s
          </p>
        </div>
      </div>

      {/* Filter bar */}
      <div className="flex items-center gap-3">
        <input
          type="text"
          placeholder="Filter by model, node, or key..."
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          className="flex-1 max-w-sm px-3 py-2 text-sm rounded-md border border-border bg-card text-foreground placeholder:text-muted-foreground focus:outline-none focus:ring-2 focus:ring-primary/40"
        />
        {filter && (
          <button
            onClick={() => setFilter('')}
            className="text-xs text-muted-foreground hover:text-foreground transition-colors"
          >
            Clear
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
                    {entries.length === 0
                      ? 'No requests yet. Send a request through the proxy to see it here.'
                      : 'No requests match your filter.'}
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
                        {entry.id.slice(0, 8)}
                      </span>
                    </td>
                    <td className="px-4 py-3 text-foreground whitespace-nowrap">
                      {entry.key_name}
                    </td>
                    <td className="px-4 py-3">
                      <span className="font-mono text-xs text-foreground">{entry.model}</span>
                    </td>
                    <td className="px-4 py-3 whitespace-nowrap">
                      {entry.cloud ? (
                        <span className="text-xs font-medium px-2 py-0.5 rounded-full bg-amber-500/15 text-amber-400">
                          cloud
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
