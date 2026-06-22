import { useState, useEffect } from 'react';
import { TrendingUp, DollarSign, Server, Cloud } from 'lucide-react';
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
import { fetchAnalytics } from '../lib/api';
import { mockAnalytics } from '../lib/mockData';
import { useDemoMode } from '../hooks/useDemoMode';
import type { Analytics, HourlyBucket, ModelStat } from '../types';

function formatHourLabel(hour: string): string {
  // "2026-05-23T14" -> "14:00"
  const parts = hour.split('T');
  if (parts.length === 2) return `${parts[1]}:00`;
  return hour;
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
    amber: { bg: 'bg-amber-500/10', text: 'text-amber-600 dark:text-amber-400', icon: 'text-amber-500' },
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

export function Analytics() {
  const { demoMode } = useDemoMode();
  const [data, setData] = useState<Analytics | null>(demoMode ? mockAnalytics : null);
  const [loading, setLoading] = useState(!demoMode);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const load = async () => {
      if (demoMode) {
        setData(mockAnalytics);
        setLoading(false);
        return;
      }
      try {
        const d = await fetchAnalytics();
        setData(d);
        setError(null);
      } catch (e: unknown) {
        setError(e instanceof Error ? e.message : 'Failed to load analytics');
      } finally {
        setLoading(false);
      }
    };
    load();
    const id = setInterval(load, 10000);
    return () => clearInterval(id);
  }, [demoMode]);

  const chartData = (data?.hourly ?? []).map((b: HourlyBucket) => ({
    hour: formatHourLabel(b.hour),
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
          <div className="overflow-x-auto">
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
                      <td className="px-6 py-3 text-right text-amber-500 font-medium">
                        {m.cloud.toLocaleString()}
                      </td>
                      <td className="px-6 py-3 text-right">
                        <span className={`font-semibold ${pct >= 90 ? 'text-success' : pct >= 70 ? 'text-primary' : 'text-amber-500'}`}>
                          {pct}%
                        </span>
                      </td>
                      <td className="px-6 py-3 text-right font-mono font-medium text-success">
                        ${m.saved_usd.toFixed(4)}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  );
}
