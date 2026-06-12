import { useState, useEffect } from 'react';
import { Download, BarChart3, ExternalLink } from 'lucide-react';
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
import {
  generateRequestsPerMinuteData,
  generateTokenUsageData,
  generateNodeLatencyData,
  generateRequestDistributionData,
} from '../lib/mockData';
import { getAnalytics } from '../lib/api';
import type { Analytics } from '../types';

const COLORS = ['#10b981', '#3b82f6', '#f59e0b', '#ef4444', '#8b5cf6', '#ec4899', '#06b6d4', '#84cc16', '#f97316', '#6366f1'];

function exportToCSV(data: any[], filename: string) {
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
      <div className="flex items-center justify-between mb-4">
        <h3 className="text-sm font-semibold text-foreground">{title}</h3>
        <button
          onClick={onExport}
          className="flex items-center gap-1.5 px-2.5 py-1.5 text-xs text-muted-foreground hover:text-foreground bg-secondary hover:bg-secondary/80 rounded-lg transition-colors"
        >
          <Download className="w-3.5 h-3.5" />
          Export CSV
        </button>
      </div>
      {children}
    </div>
  );
}

const CustomTooltip = ({ active, payload, label }: any) => {
  if (active && payload && payload.length) {
    return (
      <div className="bg-card border border-border rounded-lg p-3 shadow-xl">
        {label && <p className="text-xs text-muted-foreground mb-1">{label}</p>}
        {payload.map((entry: any, index: number) => (
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

import { useDemoMode } from '../hooks/useDemoMode';

export function Metrics() {
  const { demoMode } = useDemoMode();

  const [requestsData] = useState(() => demoMode ? generateRequestsPerMinuteData() : []);
  const [tokenUsageData] = useState(() => demoMode ? generateTokenUsageData() : []);
  const [latencyData] = useState(() => demoMode ? generateNodeLatencyData() : []);
  const [distributionData] = useState(() => demoMode ? generateRequestDistributionData() : []);
  const [analytics, setAnalytics] = useState<Analytics | null>(null);

  useEffect(() => {
    if (demoMode) return;
    getAnalytics().then(setAnalytics).catch(() => setAnalytics(null));
  }, [demoMode]);

  return (
    <div className="space-y-6 animate-fade-in max-w-7xl mx-auto">
      {/* Header */}
      <div>
        <h1 className="text-2xl font-bold text-foreground">Metrics</h1>
        <p className="text-sm text-muted-foreground mt-1">
          Performance metrics and usage statistics for your Ollama deployment
        </p>
      </div>

      {!demoMode && (
        <div className="p-4 bg-secondary border border-border shadow-sm rounded-xl flex items-center justify-between">
          <div>
            <p className="text-sm font-semibold text-foreground">Live Metrics Require Grafana</p>
            <p className="text-xs text-muted-foreground mt-1">
              Historical chart data is not stored in the proxy. Configure Prometheus to scrape <code className="bg-background px-1 py-0.5 rounded border border-border">/metrics</code> and view dashboards in Grafana.
            </p>
          </div>
        </div>
      )}

      {/* Grafana CTA (live mode only) */}
      {!demoMode && (
        <div className="p-5 bg-card border border-border shadow-sm rounded-xl flex items-center justify-between gap-4">
          <div className="flex items-center gap-3">
            <div className="p-2 bg-primary/10 rounded-lg">
              <BarChart3 className="w-5 h-5 text-primary" />
            </div>
            <div>
              <p className="text-sm font-semibold text-foreground">Charts available in Grafana</p>
              <p className="text-xs text-muted-foreground mt-0.5">
                Import the dashboard from <code className="bg-secondary px-1 py-0.5 rounded border border-border">/grafana/ollama-mesh.json</code> to visualize time-series data.
              </p>
            </div>
          </div>
          <a
            href="/grafana/ollama-mesh.json"
            download
            className="flex items-center gap-1.5 px-3 py-1.5 text-xs font-medium text-primary border border-primary/30 rounded-lg hover:bg-primary/5 transition-colors whitespace-nowrap"
          >
            <ExternalLink className="w-3.5 h-3.5" />
            Download JSON
          </a>
        </div>
      )}

      {/* Charts Grid */}
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
        {/* Requests Per Minute */}
        <ChartCard
          title="Requests Per Minute (Last 24 Hours)"
          onExport={() => exportToCSV(requestsData, 'requests-per-minute.csv')}
        >
          <div className="h-64">
            <ResponsiveContainer width="100%" height="100%">
              <LineChart data={requestsData}>
                <defs>
                  <linearGradient id="requestsGradient" x1="0" y1="0" x2="0" y2="1">
                    <stop offset="5%" stopColor="#10b981" stopOpacity={0.3} />
                    <stop offset="95%" stopColor="#10b981" stopOpacity={0} />
                  </linearGradient>
                </defs>
                <CartesianGrid strokeDasharray="3 3" stroke="currentColor" className="text-border" vertical={false} />
                <XAxis 
                  dataKey="timestamp" 
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
                  tickFormatter={(value: number) => value.toLocaleString()}
                />
                <Tooltip content={<CustomTooltip />} />
                <Line
                  type="monotone"
                  dataKey="value"
                  name="Requests"
                  stroke="#10b981"
                  strokeWidth={2}
                  dot={false}
                  fillOpacity={1}
                  fill="url(#requestsGradient)"
                />
              </LineChart>
            </ResponsiveContainer>
          </div>
        </ChartCard>

        {/* Token Usage Per API Key */}
        <ChartCard
          title="Token Usage by API Key (Top 10)"
          onExport={() => exportToCSV(tokenUsageData, 'token-usage.csv')}
        >
          <div className="h-64">
            <ResponsiveContainer width="100%" height="100%">
              <BarChart data={tokenUsageData} layout="vertical">
                <CartesianGrid strokeDasharray="3 3" stroke="currentColor" className="text-border" horizontal={true} vertical={false} />
                <XAxis 
                  type="number" 
                  stroke="currentColor" 
                  className="text-muted-foreground"
                  fontSize={10}
                  tickLine={false}
                  axisLine={false}
                  tickFormatter={(value: number) => `${(value / 1000000).toFixed(1)}M`}
                />
                <YAxis 
                  type="category" 
                  dataKey="keyName" 
                  stroke="currentColor" 
                  className="text-muted-foreground"
                  fontSize={10}
                  tickLine={false}
                  axisLine={false}
                  width={100}
                />
                <Tooltip content={<CustomTooltip />} />
                <Bar dataKey="tokens" name="Tokens" radius={[0, 4, 4, 0]}>
                  {tokenUsageData.map((_, index) => (
                    <Cell key={`cell-${index}`} fill={COLORS[index % COLORS.length]} />
                  ))}
                </Bar>
              </BarChart>
            </ResponsiveContainer>
          </div>
        </ChartCard>

        {/* Average Latency */}
        <ChartCard
          title="Average Latency Per Node (Last 24 Hours)"
          onExport={() => exportToCSV(latencyData, 'latency-data.csv')}
        >
          <div className="h-64">
            <ResponsiveContainer width="100%" height="100%">
              <LineChart data={latencyData}>
                <CartesianGrid strokeDasharray="3 3" stroke="currentColor" className="text-border" vertical={false} />
                <XAxis 
                  dataKey="timestamp" 
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
                  tickFormatter={(value: number) => `${value}ms`}
                />
                <Tooltip content={<CustomTooltip />} />
                <Line
                  type="monotone"
                  dataKey="value"
                  name="Latency"
                  stroke="#3b82f6"
                  strokeWidth={2}
                  dot={false}
                />
              </LineChart>
            </ResponsiveContainer>
          </div>
        </ChartCard>

        {/* Request Distribution */}
        <ChartCard
          title="Request Distribution Across GPU Nodes"
          onExport={() => exportToCSV(distributionData, 'request-distribution.csv')}
        >
          <div className="h-64">
            <ResponsiveContainer width="100%" height="100%">
              <PieChart>
                <Pie
                  data={distributionData}
                  cx="50%"
                  cy="50%"
                  innerRadius={60}
                  outerRadius={80}
                  paddingAngle={5}
                  dataKey="requests"
                  nameKey="nodeName"
                >
                  {distributionData.map((_, index) => (
                    <Cell key={`cell-${index}`} fill={COLORS[index % COLORS.length]} />
                  ))}
                </Pie>
                <Tooltip content={<CustomTooltip />} />
                <Legend 
                  verticalAlign="middle" 
                  align="right" 
                  layout="vertical"
                  iconType="circle"
                  wrapperStyle={{ fontSize: '11px', color: 'currentColor' }}
                  className="text-muted-foreground"
                />
              </PieChart>
            </ResponsiveContainer>
          </div>
        </ChartCard>
      </div>

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
                {demoMode
                  ? '116,708'
                  : analytics
                    ? (analytics.local_requests + analytics.cloud_requests).toLocaleString()
                    : '—'}
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
              <p className="text-xs font-medium text-muted-foreground">Total Spend</p>
              <p className="text-xl font-bold text-foreground font-mono">
                {demoMode
                  ? '4.2M'
                  : analytics
                    ? analytics.total_spent_usd != null
                      ? `$${analytics.total_spent_usd.toFixed(4)}`
                      : '—'
                    : '—'}
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
              <p className="text-xs font-medium text-muted-foreground">Avg Response Time</p>
              <p className="text-xl font-bold text-foreground font-mono">
                {demoMode ? '45ms' : '—'}
              </p>
            </div>
          </div>
        </div>
        <div className="bg-card border border-border shadow-sm rounded-xl p-4">
          <div className="flex items-center gap-3">
            <div className="p-2 bg-purple-500/10 rounded-lg">
              <BarChart3 className="w-5 h-5 text-purple-600 dark:text-purple-400" />
            </div>
            <div>
              <p className="text-xs font-medium text-muted-foreground">Cloud Requests</p>
              <p className="text-xl font-bold text-foreground font-mono">
                {demoMode
                  ? '0.02%'
                  : analytics
                    ? analytics.cloud_requests.toLocaleString()
                    : '—'}
              </p>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
