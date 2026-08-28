import { useState, useEffect } from 'react';
import { useLocation } from 'react-router-dom';
import { TrendingUp, DollarSign, Server, Cloud, Download } from 'lucide-react';
import {
  AreaChart,
  Area,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  ResponsiveContainer,
  Legend,
} from 'recharts';
import { fetchAnalytics, analyticsExportUrl, getSpillCounters } from '../lib/api';
import { mockAnalytics, mockSpillCounters } from '../lib/mockData';
import { useDemoMode, currentAppPath } from '../hooks/useDemoMode';
import { useTimezone } from '../hooks/useTimezone';
import { formatHourLabelInTimezone } from '../lib/time';
import type { Analytics, HourlyBucket, ModelStat, SpillCounterRow } from '../types';

function formatHourLabel(hour: string, tz: string): string {
  return formatHourLabelInTimezone(hour, tz);
}

function StatCard({
  title,
  value,
  sub,
  icon,
  accent,
}: {
  title: string;
  value: string;
  sub?: string;
  icon: React.ReactNode;
  accent: 'success' | 'primary' | 'amber';
}) {
  const colors = {
    success: { bg: 'bg-success/10', text: 'text-success', icon: 'text-success' },
    primary: { bg: 'bg-primary/10', text: 'text-primary', icon: 'text-primary' },
    amber: { bg: 'bg-amber-500/10', text: 'text-amber-600 dark:text-amber-400', icon: 'text-amber-700 dark:text-amber-400' },
  };
  const c = colors[accent];
  return (
    <div className="glass-panel rounded-xl p-5 hover:border-primary/50 transition-colors">
      <div className="flex items-start justify-between">
        <div>
          <p className="text-sm font-medium text-muted-foreground mb-1">{title}</p>
          <p className={`text-2xl font-bold ${c.text}`}>{value}</p>
          {sub && <p className="text-xs font-medium text-muted-foreground mt-0.5">{sub}</p>}
        </div>
        <div className={`p-2 ${c.bg} rounded-lg ${c.icon}`}>{icon}</div>
      </div>
    </div>
  );
}

function ExportButton({ type, label }: { type: 'hourly' | 'models'; label: string }) {
  return (
    <a
      href={analyticsExportUrl(type)}
      className="inline-flex items-center gap-1.5 px-3 py-1.5 text-xs font-medium rounded-lg border border-border bg-secondary/50 text-muted-foreground hover:text-foreground hover:border-primary/50 transition-colors"
    >
      <Download className="w-3.5 h-3.5" />
      {label}
    </a>
  );
}

export function Analytics() {
  const tz = useTimezone();
  const { demoMode } = useDemoMode();
  const location = useLocation();
  const [data, setData] = useState<Analytics | null>(demoMode ? mockAnalytics : null);
  const [loading, setLoading] = useState(!demoMode);
  const [error, setError] = useState<string | null>(null);
  const [spillRows, setSpillRows] = useState<SpillCounterRow[]>(demoMode ? mockSpillCounters : []);
  const [spillLoading, setSpillLoading] = useState(!demoMode);

  useEffect(() => {
    if (currentAppPath() !== '/analytics') return;
    if (demoMode) {
      setData(mockAnalytics);
      setLoading(false);
      return;
    }
    let active = true;
    const load = async () => {
      try {
        const d = await fetchAnalytics();
        if (!active || currentAppPath() !== '/analytics') return;
        setData(d);
        setError(null);
      } catch (e: unknown) {
        if (!active || currentAppPath() !== '/analytics') return;
        setError(e instanceof Error ? e.message : 'Failed to load analytics');
      } finally {
        if (active && currentAppPath() === '/analytics') {
          setLoading(false);
        }
      }
    };
    load();
    const id = setInterval(load, 10000);
    return () => {
      active = false;
      clearInterval(id);
    };
  }, [demoMode, location.pathname]);

  useEffect(() => {
    if (currentAppPath() !== '/analytics') return;
    if (demoMode) {
      setSpillRows(mockSpillCounters);
      setSpillLoading(false);
      return;
    }
    let active = true;
    const loadSpill = async () => {
      try {
        const rows = await getSpillCounters();
        if (!active || currentAppPath() !== '/analytics') return;
        setSpillRows(rows || []);
      } catch {
        if (!active || currentAppPath() !== '/analytics') return;
        setSpillRows([]);
      } finally {
        if (active && currentAppPath() === '/analytics') {
          setSpillLoading(false);
        }
      }
    };
    loadSpill();
    const id = setInterval(loadSpill, 10000);
    return () => {
      active = false;
      clearInterval(id);
    };
  }, [demoMode, location.pathname]);

  const chartData = (data?.hourly ?? []).map((b: HourlyBucket) => ({
    hour: formatHourLabel(b.hour, tz),
    Local: b.local,
    Cloud: b.cloud,
    saved: b.saved_usd,
  }));

  const totalRequests = (data?.local_requests ?? 0) + (data?.cloud_requests ?? 0);
  const localPct =
    totalRequests > 0 ? Math.round(((data?.local_requests ?? 0) / totalRequests) * 100) : 0;

  return (
    <div className="space-y-6 animate-fade-in max-w-7xl mx-auto">
      {/* Header */}
      <div className="border-b border-border pb-6">
        <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
          <div className="flex items-center gap-3">
            <div className="p-2 bg-success/10 rounded-lg">
              <TrendingUp className="w-5 h-5 text-success" />
            </div>
            <div>
              <h1 className="text-2xl font-bold text-foreground">Analytics</h1>
              <p className="text-sm text-muted-foreground mt-0.5">
                Cost savings and routing breakdown - last 24 hours
              </p>
            </div>
          </div>
          {!demoMode && (
            <div className="flex items-center gap-2">
              <ExportButton type="hourly" label="Export Hourly CSV" />
              <ExportButton type="models" label="Export Models CSV" />
            </div>
          )}
        </div>
      </div>

      {error && (
        <div className="p-4 bg-destructive/10 border border-destructive/20 rounded-xl text-destructive text-sm font-medium">
          {error}
        </div>
      )}

      {/* Hero Stats */}
      <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
        <StatCard
          title="Total Saved vs Cloud"
          value={loading ? '--' : data?.total_saved_usd != null ? `$${data.total_saved_usd.toFixed(2)}` : '-'}
          sub={`${localPct}% requests served locally`}
          icon={<DollarSign className="w-5 h-5" />}
          accent="success"
        />
        <StatCard
          title="Local Requests"
          value={loading ? '--' : (data?.local_requests ?? 0).toLocaleString()}
          sub="served by your GPU nodes"
          icon={<Server className="w-5 h-5" />}
          accent="primary"
        />
        <StatCard
          title="Cloud Spend"
          value={loading ? '--' : data?.total_spent_usd != null ? `$${data.total_spent_usd.toFixed(4)}` : '-'}
          sub={`${(data?.cloud_requests ?? 0).toLocaleString()} cloud fallback requests`}
          icon={<Cloud className="w-5 h-5" />}
          accent="amber"
        />
      </div>

      {/* 24h Chart */}
      <div className="glass-panel rounded-xl p-6">
        <h3 className="text-sm font-semibold text-foreground mb-6">
          Requests per Hour - Local vs Cloud (24h)
        </h3>
        {loading ? (
          <div className="h-64 bg-secondary/30 rounded-lg animate-pulse" />
        ) : (
          <ResponsiveContainer width="100%" height={260}>
            <AreaChart data={chartData} margin={{ top: 4, right: 4, left: 0, bottom: 0 }}>
              <defs>
                <linearGradient id="colorLocal" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="5%" stopColor="hsl(var(--success))" stopOpacity={0.3} />
                  <stop offset="95%" stopColor="hsl(var(--success))" stopOpacity={0} />
                </linearGradient>
                <linearGradient id="colorCloud" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="5%" stopColor="#f59e0b" stopOpacity={0.3} />
                  <stop offset="95%" stopColor="#f59e0b" stopOpacity={0} />
                </linearGradient>
              </defs>
              <CartesianGrid strokeDasharray="3 3" stroke="hsl(var(--border))" />
              <XAxis
                dataKey="hour"
                tick={{ fontSize: 11, fill: 'hsl(var(--muted-foreground))' }}
                tickLine={false}
                axisLine={false}
                interval={3}
              />
              <YAxis
                tick={{ fontSize: 11, fill: 'hsl(var(--muted-foreground))' }}
                tickLine={false}
                axisLine={false}
                allowDecimals={false}
              />
              <Tooltip
                contentStyle={{
                  background: 'hsl(var(--card))',
                  border: '1px solid hsl(var(--border))',
                  borderRadius: '8px',
                  fontSize: '12px',
                }}
                formatter={(value, name) => [
                  typeof value === 'number' ? value.toLocaleString() : String(value),
                  String(name),
                ]}
              />
              <Legend
                wrapperStyle={{ fontSize: '12px', paddingTop: '16px' }}
                formatter={(value) => <span className="text-muted-foreground">{value}</span>}
              />
              <Area
                type="monotone"
                dataKey="Local"
                stroke="hsl(var(--success))"
                strokeWidth={2}
                fill="url(#colorLocal)"
              />
              <Area
                type="monotone"
                dataKey="Cloud"
                stroke="#f59e0b"
                strokeWidth={2}
                fill="url(#colorCloud)"
              />
            </AreaChart>
          </ResponsiveContainer>
        )}
      </div>

      {/* Model Breakdown */}
      <div className="bg-card border border-border shadow-sm rounded-xl overflow-hidden">
        <div className="px-6 py-4 border-b border-border bg-secondary/30">
          <h3 className="text-sm font-semibold text-foreground">Requests by Model</h3>
        </div>
        {loading ? (
          <div className="divide-y divide-border">
            {[1, 2, 3].map((i) => (
              <div key={i} className="px-6 py-4 flex gap-4 animate-pulse">
                <div className="h-4 bg-secondary rounded w-1/3" />
                <div className="h-4 bg-secondary rounded w-16 ml-auto" />
                <div className="h-4 bg-secondary rounded w-16" />
                <div className="h-4 bg-secondary rounded w-16" />
              </div>
            ))}
          </div>
        ) : !data?.by_model?.length ? (
          <div className="px-6 py-12 text-center text-sm text-muted-foreground font-medium">
            No request data yet. Start sending requests through the proxy.
          </div>
        ) : (
          <>
            <div className="hidden md:block overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="bg-secondary/50 text-muted-foreground">
                    <th className="px-6 py-3 text-left font-medium">Model</th>
                    <th className="px-6 py-3 text-right font-medium">Local</th>
                    <th className="px-6 py-3 text-right font-medium">Cloud</th>
                    <th className="px-6 py-3 text-right font-medium">Local %</th>
                    <th className="px-6 py-3 text-right font-medium">Saved ($)</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-border">
                  {data.by_model.slice(0, 10).map((m: ModelStat) => {
                    const total = m.local + m.cloud;
                    const pct = total > 0 ? Math.round((m.local / total) * 100) : 0;
                    return (
                      <tr key={m.model} className="hover:bg-secondary/50 transition-colors">
                        <td className="px-6 py-3 font-mono font-medium text-foreground">
                          {m.model}
                        </td>
                        <td className="px-6 py-3 text-right text-success font-medium">
                          {m.local.toLocaleString()}
                        </td>
                        <td className="px-6 py-3 text-right text-amber-700 dark:text-amber-400 font-medium">
                          {m.cloud.toLocaleString()}
                        </td>
                        <td className="px-6 py-3 text-right">
                          <span className={`font-semibold ${pct >= 90 ? 'text-success' : pct >= 70 ? 'text-primary' : 'text-amber-700 dark:text-amber-400'}`}>
                            {pct}%
                          </span>
                        </td>
                        <td className="px-6 py-3 text-right font-mono font-medium text-success">
                          ${(m.saved_usd ?? 0).toFixed(4)}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>

            <div className="md:hidden space-y-3 p-4">
              {data.by_model.slice(0, 10).map((m: ModelStat) => {
                const total = m.local + m.cloud;
                const pct = total > 0 ? Math.round((m.local / total) * 100) : 0;
                return (
                  <div
                    key={m.model}
                    className="bg-card/50 backdrop-blur-sm border border-border/60 rounded-xl p-4"
                  >
                    <p className="font-mono font-medium text-foreground text-sm mb-3 break-all">
                      {m.model}
                    </p>
                    <div className="grid grid-cols-2 gap-3">
                      <div>
                        <p className="text-[10px] uppercase tracking-wider text-muted-foreground">
                          Local
                        </p>
                        <p className="text-sm text-success font-medium">
                          {m.local.toLocaleString()}
                        </p>
                      </div>
                      <div>
                        <p className="text-[10px] uppercase tracking-wider text-muted-foreground">
                          Cloud
                        </p>
                        <p className="text-sm text-amber-700 dark:text-amber-400 font-medium">
                          {m.cloud.toLocaleString()}
                        </p>
                      </div>
                      <div>
                        <p className="text-[10px] uppercase tracking-wider text-muted-foreground">
                          Local %
                        </p>
                        <p
                          className={`text-sm font-semibold ${pct >= 90 ? 'text-success' : pct >= 70 ? 'text-primary' : 'text-amber-700 dark:text-amber-400'}`}
                        >
                          {pct}%
                        </p>
                      </div>
                      <div>
                        <p className="text-[10px] uppercase tracking-wider text-muted-foreground">
                          Saved ($)
                        </p>
                        <p className="text-sm font-mono font-medium text-success">
                          ${(m.saved_usd ?? 0).toFixed(4)}
                        </p>
                      </div>
                    </div>
                  </div>
                );
              })}
            </div>
          </>
        )}
      </div>

      {/* Cloud Spill - per-key, per-provider local-vs-cloud request counts (P66) */}
      <div className="bg-card border border-border shadow-sm rounded-xl overflow-hidden">
        <div className="px-6 py-4 border-b border-border bg-secondary/30">
          <h3 className="text-sm font-semibold text-foreground">Cloud Spill</h3>
          <p className="text-xs text-muted-foreground mt-0.5">
            Requests served locally, by a cloud provider, or blocked by a key's local-only policy.
          </p>
        </div>
        {spillLoading ? (
          <div className="divide-y divide-border">
            {[1, 2, 3].map((i) => (
              <div key={i} className="px-6 py-4 flex gap-4 animate-pulse">
                <div className="h-4 bg-secondary rounded w-1/3" />
                <div className="h-4 bg-secondary rounded w-24 ml-auto" />
                <div className="h-4 bg-secondary rounded w-16" />
              </div>
            ))}
          </div>
        ) : !spillRows.length ? (
          <div className="px-6 py-12 text-center text-sm text-muted-foreground font-medium">
            No spill data yet. Send requests through the proxy to populate this table.
          </div>
        ) : (
          <>
            <div className="hidden md:block overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="bg-secondary/50 text-muted-foreground">
                    <th className="px-6 py-3 text-left font-medium">Key</th>
                    <th className="px-6 py-3 text-left font-medium">Served By</th>
                    <th className="px-6 py-3 text-right font-medium">Requests</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-border">
                  {spillRows.map((row) => (
                    <tr key={`${row.key_name}-${row.served_by}`} className="hover:bg-secondary/50 transition-colors">
                      <td className="px-6 py-3 font-mono font-medium text-foreground">{row.key_name || '-'}</td>
                      <td className="px-6 py-3">
                        <span className={
                          row.served_by === 'local' ? 'text-success font-medium' :
                          row.served_by === 'blocked' ? 'text-destructive dark:text-red-400 font-medium' :
                          row.served_by ? 'text-amber-600 dark:text-amber-400 font-medium' : 'text-muted-foreground'
                        }>
                          {row.served_by || '-'}
                        </span>
                      </td>
                      <td className="px-6 py-3 text-right font-mono font-medium text-foreground">
                        {row.requests != null ? row.requests.toLocaleString() : '-'}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>

            <div className="md:hidden space-y-3 p-4">
              {spillRows.map((row) => (
                <div
                  key={`${row.key_name}-${row.served_by}`}
                  className="bg-card/50 backdrop-blur-sm border border-border/60 rounded-xl p-4"
                >
                  <p className="font-mono font-medium text-foreground text-sm mb-3 break-all">
                    {row.key_name || '-'}
                  </p>
                  <div className="grid grid-cols-2 gap-3">
                    <div>
                      <p className="text-[10px] uppercase tracking-wider text-muted-foreground">Served By</p>
                      <p className={
                        row.served_by === 'local' ? 'text-sm text-success font-medium' :
                        row.served_by === 'blocked' ? 'text-sm text-destructive dark:text-red-400 font-medium' :
                        row.served_by ? 'text-sm text-amber-600 dark:text-amber-400 font-medium' : 'text-sm text-muted-foreground'
                      }>
                        {row.served_by || '-'}
                      </p>
                    </div>
                    <div>
                      <p className="text-[10px] uppercase tracking-wider text-muted-foreground">Requests</p>
                      <p className="text-sm font-mono font-medium text-foreground">
                        {row.requests != null ? row.requests.toLocaleString() : '-'}
                      </p>
                    </div>
                  </div>
                </div>
              ))}
            </div>
          </>
        )}
      </div>
    </div>
  );
}
