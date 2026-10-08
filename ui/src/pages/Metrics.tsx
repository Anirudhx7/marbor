import { useState, useEffect } from 'react';
import { useLocation } from 'react-router-dom';
import { Download, BarChart3, ChevronDown, ChevronUp } from 'lucide-react';
import {
  LineChart,
  Line,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  ResponsiveContainer,
} from 'recharts';
import { mockAnalytics } from '../lib/mockData';
import { fetchAnalytics } from '../lib/api';
import type { Analytics } from '../types';
import { useDemoMode, currentAppPath } from '../hooks/useDemoMode';
import { useTimezone } from '../hooks/useTimezone';
import { formatHourLabelInTimezone } from '../lib/time';
import { toTpsSeries } from '../lib/tokensPerSec';

interface TooltipPayloadEntry {
  color: string;
  name: string;
  dataKey: string;
  value: number | null;
}

const CustomTooltip = ({ active, payload, label }: { active?: boolean; payload?: TooltipPayloadEntry[]; label?: string }) => {
  if (active && payload && payload.length) {
    return (
      <div className="bg-card border border-border rounded-lg p-3 shadow-xl">
        {label && <p className="text-xs text-muted-foreground mb-1">{label}</p>}
        {payload.map((entry, index) => (
          <p key={index} className="text-sm" style={{ color: entry.color }}>
            <span className="font-medium">{entry.name || entry.dataKey}:</span>{' '}
            <span className="font-mono">
              {entry.value != null ? `${entry.value.toFixed(1)} tok/s` : '-'}
            </span>
          </p>
        ))}
      </div>
    );
  }
  return null;
};

function LoadingSkeleton() {
  return (
    <div className="space-y-6 animate-pulse">
      <div className="bg-card border border-border rounded-xl p-5 h-80" />
    </div>
  );
}

export function Metrics() {
  const tz = useTimezone();
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
    let hasData = analytics !== null;
    const load = () => {
      if (active && currentAppPath() === '/metrics') {
        if (!hasData) setLoading(true);
      }
      fetchAnalytics()
        .then(data => {
          if (!active || currentAppPath() !== '/metrics') return;
          setError(null);
          setAnalytics(data);
          hasData = true;
          setLoading(false);
        })
        .catch(err => {
          if (!active || currentAppPath() !== '/metrics') return;
          setError(err instanceof Error ? err.message : 'Failed to load metrics');
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

  const tpsSeries = toTpsSeries(analytics?.hourly ?? []);
  const tpsData = tpsSeries.map(p => ({
    hour: formatHourLabelInTimezone(p.hour, tz),
    'Tokens per second': p.tps,
  }));
  const hasMeasured = tpsSeries.some(p => p.tps !== null);

  return (
    <div className="space-y-6 animate-fade-in max-w-7xl mx-auto">
      {/* Header */}
      <div>
        <h1 className="text-2xl font-bold tracking-tight text-foreground">Metrics</h1>
        <p className="text-sm text-muted-foreground mt-1">
          Generation speed over the last 24 hours. Cost and routing live under Analytics.
        </p>
      </div>

      {loading && <LoadingSkeleton />}

      {!loading && error && (
        <div className="p-4 bg-destructive/10 border border-destructive/30 rounded-xl">
          <p className="text-sm font-semibold text-destructive">Failed to load metrics</p>
          <p className="text-xs text-muted-foreground mt-1">{error}</p>
        </div>
      )}

      {!loading && analytics && (
        <div className="bg-card border border-border shadow-sm rounded-xl p-5">
          <h2 className="text-sm font-semibold text-foreground">Tokens per second per hour (last 24h)</h2>
          <p className="text-xs text-muted-foreground mt-1 mb-4">
            Ollama-native requests only (other runtimes report no generation timing)
          </p>
          <div className="h-64">
            {!hasMeasured ? (
              <div className="flex flex-col items-center justify-center h-full text-center px-4">
                <BarChart3 className="w-10 h-10 text-muted-foreground/30 mb-3" />
                <p className="text-sm font-medium text-muted-foreground">No generation timing in this window</p>
                <p className="text-xs text-muted-foreground mt-1">
                  Hours without timing show as gaps, not zero. Timing is not kept across a marbor restart, so a restart clears this chart.
                </p>
              </div>
            ) : (
              <ResponsiveContainer width="100%" height="100%">
                <LineChart data={tpsData}>
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
                    tickFormatter={(v: number) => `${v} tok/s`}
                    width={64}
                  />
                  <Tooltip content={<CustomTooltip />} />
                  <Line
                    type="monotone"
                    dataKey="Tokens per second"
                    stroke="#10b981"
                    strokeWidth={2}
                    dot={{ r: 2 }}
                    connectNulls={false}
                  />
                </LineChart>
              </ResponsiveContainer>
            )}
          </div>
        </div>
      )}

      {/* Advanced monitoring - collapsible */}
      <div className="bg-card border border-border shadow-sm rounded-xl overflow-hidden">
        <button
          className="w-full flex items-center justify-between p-4 text-sm font-medium text-foreground hover:bg-secondary/30 transition-colors"
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
    </div>
  );
}
