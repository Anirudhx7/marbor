import { useState, useEffect } from 'react';
import { useLocation } from 'react-router-dom';
import { Download, BarChart3, ChevronDown, ChevronUp } from 'lucide-react';
import {
  LineChart,
  Line,
  BarChart,
  Bar,
  PieChart,
  Pie,
  Cell,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  ResponsiveContainer,
  Legend,
} from 'recharts';
import { mockAnalytics } from '../lib/mockData';
import { fetchAnalytics } from '../lib/api';
import type { Analytics, HourlyBucket, ModelStat } from '../types';
import { useDemoMode, currentAppPath } from '../hooks/useDemoMode';

const COLORS = ['#10b981', '#3b82f6', '#f59e0b', '#ef4444', '#8b5cf6', '#ec4899', '#06b6d4', '#84cc16', '#f97316', '#6366f1'];

function exportToCSV(data: Record<string, unknown>[], filename: string) {
  if (data.length === 0) return;
  const headers = Object.keys(data[0]).join(',');
  const rows = data.map(row => Object.values(row).join(','));
  const csv = [headers, ...rows].join('\n');
  const blob = new Blob([csv], { type: 'text/csv' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  URL.revokeObjectURL(url);
}

interface ChartCardProps {
  title: string;
  children: React.ReactNode;
  onExport: () => void;
}

function ChartCard({ title, children, onExport }: ChartCardProps) {
  return (
    <div className="bg-card border border-border shadow-sm rounded-xl p-5">
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2 mb-4">
        <h3 className="text-sm font-semibold text-foreground">{title}</h3>
        <button
          onClick={onExport}
          className="flex items-center gap-1.5 px-2.5 py-1.5 text-xs text-muted-foreground hover:text-foreground bg-secondary hover:bg-secondary/80 rounded-lg transition-colors self-start sm:self-auto"
        >
          <Download className="w-3.5 h-3.5" />
          Export CSV
        </button>
      </div>
      {children}
    </div>
  );
}

interface TooltipPayloadEntry {
  color: string;
  name: string;
  dataKey: string;
  value: number;
}

const CustomTooltip = ({ active, payload, label }: { active?: boolean; payload?: TooltipPayloadEntry[]; label?: string }) => {
  if (active && payload && payload.length) {
    return (
      <div className="bg-card border border-border rounded-lg p-3 shadow-xl">
        {label && <p className="text-xs text-muted-foreground mb-1">{label}</p>}
        {payload.map((entry, index) => (
          <p key={index} className="text-sm" style={{ color: entry.color }}>
            <span className="font-medium">{entry.name || entry.dataKey}:</span>{' '}
            <span className="font-mono">{entry.value?.toLocaleString()}</span>
          </p>
        ))}
      </div>
    );
  }
  return null;
};

function formatHour(hour: string): string {
  const parts = hour.split('T');
  if (parts.length === 2) return `${parts[1]}:00`;
  return hour;
}

function EmptyChart({ message }: { message: string }) {
  return (
    <div className="flex items-center justify-center h-full text-sm text-muted-foreground text-center px-4">
      {message}
    </div>
  );
}

function LoadingSkeleton() {
  return (
    <div className="space-y-6 animate-pulse">
      <div className="grid grid-cols-1 md:grid-cols-4 gap-4">
        {[...Array(4)].map((_, i) => (
          <div key={i} className="bg-card border border-border rounded-xl p-4 h-20" />
        ))}
      </div>
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
        {[...Array(4)].map((_, i) => (
          <div key={i} className="bg-card border border-border rounded-xl p-5 h-80" />
        ))}
      </div>
    </div>
  );
}

export function Metrics() {
  const { demoMode } = useDemoMode();
  const location = useLocation();
  const [analytics, setAnalytics] = useState<Analytics | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [advancedOpen, setAdvancedOpen] = useState(false);

  useEffect(() => {
    if (currentAppPath() !== '/metrics') return;
    if (demoMode) {
      setAnalytics(mockAnalytics);
      setLoading(false);
      return;
    }
    let active = true;
    const load = () => {
      if (active && currentAppPath() === '/metrics') {
        setLoading(true);
        setError(null);
      }
      fetchAnalytics()
        .then(data => {
          if (!active || currentAppPath() !== '/metrics') return;
          setAnalytics(data);
          setLoading(false);
        })
        .catch(err => {
          if (!active || currentAppPath() !== '/metrics') return;
          setError(err instanceof Error ? err.message : 'Failed to load analytics');
          setLoading(false);
        });
    };
    load();
    const interval = setInterval(load, 30000);
    return () => {
      active = false;
      clearInterval(interval);
    };
  }, [demoMode, location.pathname]);

  const hourlyData = (analytics?.hourly ?? []).map((b: HourlyBucket) => ({
    hour: formatHour(b.hour),
    Local: b.local,
    Cloud: b.cloud,
  }));

  const modelRequestData = (analytics?.by_model ?? []).map((m: ModelStat) => ({
    model: m.model,
    Local: m.local,
    Cloud: m.cloud,
  }));

  const modelPieData = (analytics?.by_model ?? []).map((m: ModelStat) => ({
    name: m.model,
    requests: m.local + m.cloud,
  }));

  const savingsData = (analytics?.hourly ?? []).map((b: HourlyBucket) => ({
    hour: formatHour(b.hour),
    'Saved ($)': b.saved_usd,
    'Spent ($)': b.spent_usd,
  }));

  const totalRequests = analytics ? analytics.local_requests + analytics.cloud_requests : null;
  const localPct =
    totalRequests && totalRequests > 0
      ? ((analytics!.local_requests / totalRequests) * 100).toFixed(1)
      : null;

  return (
    <div className="space-y-6 animate-fade-in max-w-7xl mx-auto">
      {/* Header */}
      <div>
        <h1 className="text-2xl font-bold text-foreground">Metrics</h1>
        <p className="text-sm text-muted-foreground mt-1">
          Performance metrics and usage statistics for your Ollama deployment
        </p>
      </div>

      {loading && <LoadingSkeleton />}

      {!loading && error && (
        <div className="p-4 bg-destructive/10 border border-destructive/30 rounded-xl">
          <p className="text-sm font-semibold text-destructive">Failed to load analytics</p>
          <p className="text-xs text-muted-foreground mt-1">{error}</p>
        </div>
      )}

      {!loading && !error && !analytics && (
        <div className="text-center py-16 bg-card border border-border rounded-xl shadow-sm">
          <BarChart3 className="w-12 h-12 text-muted-foreground/30 mx-auto mb-4" />
          <p className="text-muted-foreground font-medium">No analytics data available yet.</p>
          <p className="text-xs text-muted-foreground mt-1">Requests will appear here as traffic flows through the proxy.</p>
        </div>
      )}

      {!loading && !error && analytics && (
        <>
          {/* Summary Stats */}
          <div className="grid grid-cols-1 md:grid-cols-4 gap-4">
            <div className="bg-card border border-border shadow-sm rounded-xl p-4">
              <div className="flex items-center gap-3">
                <div className="p-2 bg-primary/10 rounded-lg">
                  <BarChart3 className="w-5 h-5 text-primary" />
                </div>
                <div>
                  <p className="text-xs font-medium text-muted-foreground">Total Requests</p>
                  <p className="text-xl font-bold text-foreground font-mono">
                    {totalRequests != null ? totalRequests.toLocaleString() : '-'}
                  </p>
                </div>
              </div>
            </div>

            <div className="bg-card border border-border shadow-sm rounded-xl p-4">
              <div className="flex items-center gap-3">
                <div className="p-2 bg-emerald-500/10 rounded-lg">
                  <BarChart3 className="w-5 h-5 text-emerald-600 dark:text-emerald-400" />
                </div>
                <div>
                  <p className="text-xs font-medium text-muted-foreground">Local Routing</p>
                  <p className="text-xl font-bold text-foreground font-mono">
                    {localPct != null ? `${localPct}%` : '-'}
                  </p>
                </div>
              </div>
            </div>

            <div className="bg-card border border-border shadow-sm rounded-xl p-4">
              <div className="flex items-center gap-3">
                <div className="p-2 bg-amber-500/10 rounded-lg">
                  <BarChart3 className="w-5 h-5 text-amber-600 dark:text-amber-400" />
                </div>
                <div>
                  <p className="text-xs font-medium text-muted-foreground">Cloud Spend</p>
                  <p className="text-xl font-bold text-foreground font-mono">
                    {analytics.total_spent_usd != null
                      ? `$${analytics.total_spent_usd.toFixed(4)}`
                      : '-'}
                  </p>
                </div>
              </div>
            </div>

            <div className="bg-card border border-border shadow-sm rounded-xl p-4">
              <div className="flex items-center gap-3">
                <div className="p-2 bg-blue-500/10 rounded-lg">
                  <BarChart3 className="w-5 h-5 text-blue-600 dark:text-blue-400" />
                </div>
                <div>
                  <p className="text-xs font-medium text-muted-foreground">Estimated Savings</p>
                  <p className="text-xl font-bold text-foreground font-mono">
                    {analytics.total_saved_usd != null
                      ? `$${analytics.total_saved_usd.toFixed(4)}`
                      : '-'}
                  </p>
                </div>
              </div>
            </div>
          </div>

          {/* Charts Grid */}
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
            {/* Requests Per Hour */}
            <ChartCard
              title="Requests Per Hour (Last 24h)"
              onExport={() =>
                exportToCSV(
                  hourlyData as unknown as Record<string, unknown>[],
                  'requests-hourly.csv'
                )
              }
            >
              <div className="h-64">
                {hourlyData.length === 0 ? (
                  <EmptyChart message="No data yet - requests will appear here as traffic flows through the proxy." />
                ) : (
                  <ResponsiveContainer width="100%" height="100%">
                    <LineChart data={hourlyData}>
                      <CartesianGrid strokeDasharray="3 3" stroke="currentColor" className="text-border" vertical={false} />
                      <XAxis
                        dataKey="hour"
                        stroke="currentColor"
                        className="text-muted-foreground"
                        fontSize={10}
                        tickLine={false}
                        axisLine={false}
                        interval="preserveStartEnd"
                      />
                      <YAxis
                        stroke="currentColor"
                        className="text-muted-foreground"
                        fontSize={10}
                        tickLine={false}
                        axisLine={false}
                        allowDecimals={false}
                      />
                      <Tooltip content={<CustomTooltip />} />
                      <Legend iconType="circle" wrapperStyle={{ fontSize: '11px' }} />
                      <Line type="monotone" dataKey="Local" stroke="#10b981" strokeWidth={2} dot={false} />
                      <Line type="monotone" dataKey="Cloud" stroke="#3b82f6" strokeWidth={2} dot={false} />
                    </LineChart>
                  </ResponsiveContainer>
                )}
              </div>
            </ChartCard>

            {/* Requests by Model */}
            <ChartCard
              title="Requests by Model"
              onExport={() =>
                exportToCSV(
                  modelRequestData as unknown as Record<string, unknown>[],
                  'requests-by-model.csv'
                )
              }
            >
              <div className="h-64">
                {modelRequestData.length === 0 ? (
                  <EmptyChart message="No model traffic recorded yet." />
                ) : (
                  <ResponsiveContainer width="100%" height="100%">
                    <BarChart data={modelRequestData} layout="vertical">
                      <CartesianGrid strokeDasharray="3 3" stroke="currentColor" className="text-border" horizontal={false} />
                      <XAxis
                        type="number"
                        stroke="currentColor"
                        className="text-muted-foreground"
                        fontSize={10}
                        tickLine={false}
                        axisLine={false}
                        allowDecimals={false}
                      />
                      <YAxis
                        type="category"
                        dataKey="model"
                        stroke="currentColor"
                        className="text-muted-foreground"
                        fontSize={10}
                        tickLine={false}
                        axisLine={false}
                        width={100}
                      />
                      <Tooltip content={<CustomTooltip />} />
                      <Legend iconType="circle" wrapperStyle={{ fontSize: '11px' }} />
                      <Bar dataKey="Local" fill="#10b981" radius={[0, 4, 4, 0]} stackId="a" />
                      <Bar dataKey="Cloud" fill="#3b82f6" radius={[0, 4, 4, 0]} stackId="a" />
                    </BarChart>
                  </ResponsiveContainer>
                )}
              </div>
            </ChartCard>

            {/* Request Distribution Pie */}
            <ChartCard
              title="Request Distribution by Model"
              onExport={() =>
                exportToCSV(
                  modelPieData as unknown as Record<string, unknown>[],
                  'request-distribution.csv'
                )
              }
            >
              <div className="h-64">
                {modelPieData.length === 0 ? (
                  <EmptyChart message="No model traffic recorded yet." />
                ) : (
                  <ResponsiveContainer width="100%" height="100%">
                    <PieChart>
                      <Pie
                        data={modelPieData}
                        cx="50%"
                        cy="50%"
                        innerRadius={60}
                        outerRadius={80}
                        paddingAngle={5}
                        dataKey="requests"
                        nameKey="name"
                      >
                        {modelPieData.map((_, index) => (
                          <Cell key={`cell-${index}`} fill={COLORS[index % COLORS.length]} />
                        ))}
                      </Pie>
                      <Tooltip content={<CustomTooltip />} />
                      <Legend
                        verticalAlign="middle"
                        align="right"
                        layout="vertical"
                        iconType="circle"
                        wrapperStyle={{ fontSize: '11px', color: 'hsl(var(--muted-foreground))' }}
                      />
                    </PieChart>
                  </ResponsiveContainer>
                )}
              </div>
            </ChartCard>

            {/* Savings vs Spend */}
            <ChartCard
              title="Savings vs Spend per Hour (Last 24h)"
              onExport={() =>
                exportToCSV(
                  savingsData as unknown as Record<string, unknown>[],
                  'savings-per-hour.csv'
                )
              }
            >
              <div className="h-64">
                {savingsData.length === 0 ? (
                  <EmptyChart message="No data yet." />
                ) : (
                  <ResponsiveContainer width="100%" height="100%">
                    <BarChart data={savingsData}>
                      <CartesianGrid strokeDasharray="3 3" stroke="currentColor" className="text-border" vertical={false} />
                      <XAxis
                        dataKey="hour"
                        stroke="currentColor"
                        className="text-muted-foreground"
                        fontSize={10}
                        tickLine={false}
                        axisLine={false}
                        interval="preserveStartEnd"
                      />
                      <YAxis
                        stroke="currentColor"
                        className="text-muted-foreground"
                        fontSize={10}
                        tickLine={false}
                        axisLine={false}
                        tickFormatter={(v: number) => `$${v.toFixed(2)}`}
                      />
                      <Tooltip content={<CustomTooltip />} />
                      <Legend iconType="circle" wrapperStyle={{ fontSize: '11px' }} />
                      <Bar dataKey="Saved ($)" fill="#10b981" radius={[4, 4, 0, 0]} />
                      <Bar dataKey="Spent ($)" fill="#f59e0b" radius={[4, 4, 0, 0]} />
                    </BarChart>
                  </ResponsiveContainer>
                )}
              </div>
            </ChartCard>
          </div>

          {/* Advanced monitoring - collapsible */}
          <div className="bg-card border border-border shadow-sm rounded-xl overflow-hidden">
            <button
              className="w-full flex items-center justify-between p-4 text-sm font-medium text-foreground hover:bg-secondary/50 transition-colors"
              onClick={() => setAdvancedOpen(v => !v)}
            >
              <span>Advanced monitoring</span>
              {advancedOpen
                ? <ChevronUp className="w-4 h-4 text-muted-foreground" />
                : <ChevronDown className="w-4 h-4 text-muted-foreground" />}
            </button>
            {advancedOpen && (
              <div className="px-4 pb-4 space-y-3 border-t border-border pt-4">
                <div>
                  <p className="text-xs font-medium text-muted-foreground mb-1">Prometheus scrape endpoint</p>
                  <code className="block bg-secondary text-xs px-3 py-2 rounded-lg border border-border font-mono break-all">
                    {`http://${window.location.hostname}:9090/metrics`}
                  </code>
                </div>
                <div>
                  <p className="text-xs font-medium text-muted-foreground mb-1">Grafana dashboard</p>
                  <a
                    href={`${import.meta.env.BASE_URL}grafana/marbor.json`}
                    download
                    className="inline-flex items-center gap-1.5 px-3 py-1.5 text-xs font-medium text-primary border border-primary/30 rounded-lg hover:bg-primary/5 transition-colors"
                  >
                    <Download className="w-3.5 h-3.5" />
                    Download marbor.json
                  </a>
                </div>
              </div>
            )}
          </div>
        </>
      )}
    </div>
  );
}
