import { useState, useEffect } from 'react';
import { 
  Activity, 
  Clock, 
  Zap, 
  Server,
  ArrowRight,
  Shield,
  Layers,
  Database,
  CheckCircle,
  AlertCircle,
  XCircle
} from 'lucide-react';
import { StatusDot } from '../components/StatusDot';
import { VramBar } from '../components/VramBar';
import { Badge } from '../components/Badge';
import { useLiveRequests } from '../hooks/useLiveRequests';
import { mockGPUNodes } from '../lib/mockData';
import { fetchNodes, fetchSummary } from '../lib/api';
import { GPUNode } from '../types';

interface MetricCardProps {
  title: string;
  value: string;
  unit?: string;
  icon: React.ReactNode;
  trend?: string;
  trendUp?: boolean;
}

function MetricCard({ title, value, unit, icon, trend, trendUp }: MetricCardProps) {
  return (
    <div className="glass-panel rounded-xl p-5 hover:border-primary/50 transition-colors">
      <div className="flex items-start justify-between">
        <div>
          <p className="text-sm font-medium text-muted-foreground mb-1">{title}</p>
          <div className="flex items-baseline gap-1">
            <span className="text-2xl font-bold text-foreground">{value}</span>
            {unit && <span className="text-sm font-medium text-muted-foreground ml-1">{unit}</span>}
          </div>
        </div>
        <div className="p-2 bg-primary/10 rounded-lg text-primary">
          {icon}
        </div>
      </div>
      {trend && (
        <div className="mt-3 flex items-center gap-1.5 text-xs font-medium">
          <span className={trendUp ? 'text-success' : 'text-destructive'}>
            {trendUp ? '↑' : '↓'} {trend}
          </span>
          <span className="text-muted-foreground">vs last hour</span>
        </div>
      )}
    </div>
  );
}

function ArchitectureDiagram() {
  const steps = [
    { icon: <Layers className="w-5 h-5" />, label: 'Clients', sublabel: 'API Requests' },
    { icon: <Shield className="w-5 h-5" />, label: 'Auth Layer', sublabel: 'API Key Validation' },
    { icon: <ArrowRight className="w-4 h-4 text-muted-foreground/50" /> },
    { icon: <Database className="w-5 h-5" />, label: 'LiteLLM', sublabel: 'Optional', dashed: true },
    { icon: <ArrowRight className="w-4 h-4 text-muted-foreground/50" /> },
    { icon: <Server className="w-5 h-5" />, label: 'Mesh Router', sublabel: 'Load Balancer' },
    { icon: <ArrowRight className="w-4 h-4 text-muted-foreground/50" /> },
    { icon: <Zap className="w-5 h-5" />, label: 'GPU Nodes', sublabel: 'Ollama Instances' },
  ];

  return (
    <div className="glass-panel rounded-xl p-6">
      <h3 className="text-sm font-semibold text-foreground mb-6">Architecture Flow</h3>
      <div className="flex items-center justify-between gap-2 overflow-x-auto pb-2">
        {steps.map((step, index) => (
          <div key={index} className="flex items-center gap-2">
            {'icon' in step && !('label' in step) ? (
              step.icon
            ) : (
              <div className={`flex flex-col items-center p-3 rounded-lg border ${
                'dashed' in step && step.dashed
                  ? 'border-dashed border-border bg-secondary/30'
                  : 'border-border bg-secondary'
              } min-w-[120px]`}>
                <div className="text-primary mb-2">{step.icon}</div>
                <span className="text-xs font-semibold text-foreground">{step.label}</span>
                {'sublabel' in step && (
                  <span className="text-[10px] font-medium text-muted-foreground">{step.sublabel}</span>
                )}
              </div>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}

import { useDemoMode } from '../hooks/useDemoMode';

export function Dashboard() {
  const { demoMode } = useDemoMode();
  const { requests, newRequestId, isLive: requestsLive } = useLiveRequests();
  const [nodes, setNodes] = useState<GPUNode[]>(demoMode ? mockGPUNodes : []);
  const [summary, setSummary] = useState({
    activeRequests: 0,
    avgLatency: 0,
    tokensPerMin: 0,
    coldStarts: 0
  });
  const [isLive, setIsLive] = useState(!demoMode);
  const [error, setError] = useState<string | null>(null);
  const [prometheusStatus] = useState('connected');
  const [grafanaStatus] = useState('connected');

  useEffect(() => {
    const loadData = async () => {
      if (demoMode) {
        setNodes(mockGPUNodes);
        setIsLive(false);
        setError(null);
        return;
      }
      try {
        const [nodesData, summaryData] = await Promise.all([
          fetchNodes(),
          fetchSummary()
        ]);
        setNodes(nodesData || []);
        setSummary(summaryData || summary);
        setIsLive(true);
        setError(null);
      } catch (err: any) {
        setIsLive(false);
        setNodes([]);
        setError(err.message || 'Failed to fetch data');
      }
    };
    loadData();
    const interval = setInterval(loadData, 10000);
    return () => clearInterval(interval);
  }, [demoMode]);

  const activeFromRequests = requests.filter(r => r.status === 'loading').length;
  const displayActive = isLive ? summary.activeRequests : activeFromRequests;
  const displayLatency = isLive ? summary.avgLatency : 45;
  const displayTokens = isLive ? summary.tokensPerMin : "24.5K";
  const displayColdStarts = isLive ? summary.coldStarts : 18;

  return (
    <div className="space-y-6 animate-fade-in max-w-7xl mx-auto">
      {/* Top Bar */}
      <div className="flex items-center justify-between border-b border-border pb-6">
        <div className="flex items-center gap-8">
          <div className="flex items-center gap-2">
            <StatusDot status="online" pulse />
            <span className="text-sm font-semibold text-foreground">System Status</span>
          </div>
          <div className="flex items-center gap-4 text-sm">
            <div className="flex items-center gap-2">
              <span className="text-muted-foreground">Port:</span>
              <span className="text-foreground font-medium font-mono">11434</span>
            </div>
            <div className="flex items-center gap-2">
              <span className="text-muted-foreground">Version:</span>
              <Badge variant="muted" size="sm">v0.1.0</Badge>
            </div>
            <div className="flex items-center gap-2 px-3 border-l border-border">
              <div className={`w-2 h-2 rounded-full ${isLive || requestsLive ? 'bg-success' : 'bg-amber-500'}`} />
              <span className={`font-medium ${isLive || requestsLive ? 'text-success' : 'text-amber-600 dark:text-amber-400'}`}>
                {demoMode ? 'Demo Mode' : (isLive || requestsLive ? 'Live Data' : 'Disconnected')}
              </span>
            </div>
          </div>
        </div>
      </div>

      {error && !demoMode && (
        <div className="p-4 bg-destructive/10 border border-destructive/20 rounded-xl text-destructive text-sm font-medium">
          {error}
        </div>
      )}

      {/* Metric Cards */}
      <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-4">
        <MetricCard
          title="Active Requests"
          value={displayActive.toString()}
          icon={<Activity className="w-5 h-5" />}
          trend="12.4%"
          trendUp={false}
        />
        <MetricCard
          title="Avg Latency"
          value={displayLatency.toString()}
          unit="ms"
          icon={<Clock className="w-5 h-5" />}
          trend="8.1%"
          trendUp={true}
        />
        <MetricCard
          title="Tokens/min"
          value={displayTokens.toString()}
          icon={<Zap className="w-5 h-5" />}
          trend="23.5%"
          trendUp={true}
        />
        <MetricCard
          title="Cold Starts"
          value={displayColdStarts.toString()}
          unit="events"
          icon={<Server className="w-5 h-5" />}
          trend="5.2%"
          trendUp={false}
        />
      </div>

      <ArchitectureDiagram />

      {/* GPU Nodes Panel */}
      <div className="glass-panel rounded-xl p-6">
        <div className="flex items-center justify-between mb-6">
          <h3 className="text-sm font-semibold text-foreground">GPU Nodes Status</h3>
          <div className="flex items-center gap-6 text-xs font-medium">
            <div className="flex items-center gap-1.5">
              <span className="w-2 h-2 rounded-full bg-success" />
              <span className="text-muted-foreground">Healthy</span>
            </div>
            <div className="flex items-center gap-1.5">
              <span className="w-2 h-2 rounded-full bg-warning" />
              <span className="text-muted-foreground">Degraded</span>
            </div>
            <div className="flex items-center gap-1.5">
              <span className="w-2 h-2 rounded-full bg-destructive" />
              <span className="text-muted-foreground">Down</span>
            </div>
          </div>
        </div>
        
        <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
          {(isLive ? nodes : mockGPUNodes).map((node) => (
            <div key={node.id} className="bg-secondary/50 rounded-xl p-5 border border-border hover:border-primary/40 transition-colors">
              <div className="flex items-start justify-between mb-4">
                <div>
                  <div className="flex items-center gap-2">
                    <StatusDot status={node.health} size="sm" />
                    <span className="font-semibold text-foreground text-sm">{node.name}</span>
                  </div>
                  <p className="text-xs font-medium text-muted-foreground mt-1">{node.gpuModel}</p>
                </div>
                <div className="text-right">
                  <span className="text-[10px] font-medium text-muted-foreground uppercase tracking-wider block">Port</span>
                  <p className="text-sm text-foreground font-mono font-medium">{node.port}</p>
                </div>
              </div>
              
              <div className="mb-4">
                <VramBar used={node.vramUsedMB / 1024} total={node.vramTotalMB / 1024} size="sm" />
              </div>
              
              <div className="flex flex-wrap gap-1.5">
                {node.loadedModels.map((model) => (
                  <Badge
                    key={model.name}
                    variant="success"
                    size="sm"
                  >
                    {model.name}
                  </Badge>
                ))}
              </div>
            </div>
          ))}
        </div>
      </div>

      {/* Prometheus Status Bar */}
      <div className="flex items-center justify-between px-6 py-4 bg-card border border-border rounded-xl shadow-sm">
        <div className="flex items-center gap-10">
          <div className="flex items-center gap-3">
            <CheckCircle className="w-4 h-4 text-primary" />
            <div className="flex flex-col">
              <span className="text-sm font-medium text-foreground">Prometheus Database</span>
              <span className="text-xs text-muted-foreground font-mono">Connected @ :9090/metrics</span>
            </div>
          </div>
          <div className="flex items-center gap-3 border-l border-border pl-10">
            <CheckCircle className="w-4 h-4 text-primary" />
            <div className="flex flex-col">
              <span className="text-sm font-medium text-foreground">Grafana Dashboards</span>
              <span className="text-xs text-muted-foreground">Tunnel established</span>
            </div>
          </div>
        </div>
      </div>

      {/* Live Requests Table */}
      <div className="bg-card border border-border shadow-sm rounded-xl overflow-hidden">
        <div className="px-6 py-4 border-b border-border flex items-center justify-between bg-secondary/30">
          <div className="flex items-center gap-3">
            <h3 className="text-sm font-semibold text-foreground">Live Requests</h3>
            <div className="flex items-center gap-1.5">
              <div className="w-1.5 h-1.5 rounded-full bg-primary animate-pulse" />
              <span className="text-[10px] font-medium text-primary uppercase tracking-wider">Live</span>
            </div>
          </div>
          <span className="text-xs font-medium text-muted-foreground">Auto-refresh: 2s</span>
        </div>
        
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="bg-secondary/50 border-b border-border text-muted-foreground">
                <th className="px-6 py-3 text-left font-medium">API Key</th>
                <th className="px-6 py-3 text-left font-medium">Model</th>
                <th className="px-6 py-3 text-left font-medium">Routed To</th>
                <th className="px-6 py-3 text-left font-medium">Status</th>
                <th className="px-6 py-3 text-right font-medium">Latency</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-border">
              {requests.map((req) => (
                <tr 
                  key={req.id} 
                  className={`transition-colors duration-200 ${
                    newRequestId === req.id ? 'bg-primary/5' : 'hover:bg-secondary/50'
                  }`}
                >
                  <td className="px-6 py-3">
                    <span className="text-muted-foreground font-mono text-xs">{req.apiKey}</span>
                  </td>
                  <td className="px-6 py-3 text-foreground font-medium">
                    {req.model}
                  </td>
                  <td className="px-6 py-3">
                    <span className="text-muted-foreground">{req.routedTo}</span>
                  </td>
                  <td className="px-6 py-3">
                    <Badge variant={req.status === 'warm' ? 'success' : 'warning'} size="sm">
                      {req.status === 'warm' ? 'Warm' : 'Loading'}
                    </Badge>
                  </td>
                  <td className="px-6 py-3 text-right font-medium font-mono">
                    <span className={req.latency > 1000 ? 'text-amber-500' : 'text-primary'}>
                      {req.latency}ms
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  );
}
