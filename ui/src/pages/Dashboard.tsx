import { useState, useEffect } from 'react';
import { Link, useLocation } from 'react-router-dom';
import {
  Activity,
  Clock,
  Zap,
  Server,
  ArrowRight,
  Flame,
  AlertTriangle,
  MemoryStick
} from 'lucide-react';
import { StatusDot } from '../components/StatusDot';
import { VramBar } from '../components/VramBar';
import { Badge } from '../components/Badge';
import { SavingsCard } from '../components/SavingsCard';
import { useLiveRequests } from '../hooks/useLiveRequests';
import { useDemoMode, currentAppPath } from '../hooks/useDemoMode';
import { mockGPUNodes, mockSavings } from '../lib/mockData';
import { fetchNodes, fetchSummary, fetchSavings, fetchHealth } from '../lib/api';
import { GPUNode, Savings } from '../types';

interface MetricCardProps {
  title: string;
  value: string;
  unit?: string;
  icon: React.ReactNode;
  trend?: string;
  trendUp?: boolean;
  highlight?: 'warning' | 'danger';
  // compact renders the demoted traffic-metric card: tighter padding and a
  // smaller headline number, since fleet health/capacity now own the top of
  // the page and these are secondary signals.
  compact?: boolean;
}

function MetricCard({ title, value, unit, icon, trend, trendUp, highlight, compact }: MetricCardProps) {
  const iconBg = highlight === 'danger' ? 'bg-destructive/10 text-destructive'
    : highlight === 'warning' ? 'bg-warning/10 text-warning'
    : 'bg-primary/10 text-primary';
  return (
    <div className={`glass-panel rounded-xl hover:border-primary/50 transition-colors h-full min-w-0 ${compact ? 'p-3.5' : 'p-5'}`}>
      <div className="flex items-start justify-between gap-2">
        <div className="min-w-0">
          <p className={`font-medium text-muted-foreground mb-1 ${compact ? 'text-xs' : 'text-sm'}`}>{title}</p>
          <div className="flex items-baseline gap-1">
            <span className={`font-bold text-foreground ${compact ? 'text-lg' : 'text-2xl'}`}>{value}</span>
            {unit && <span className="text-sm font-medium text-muted-foreground ml-1">{unit}</span>}
          </div>
        </div>
        <div className={`rounded-lg shrink-0 ${compact ? 'p-1.5' : 'p-2'} ${iconBg}`}>
          {icon}
        </div>
      </div>
      {trend && (
        <div className="mt-3 flex items-center gap-1.5 text-xs font-medium">
          <span className={trendUp ? 'text-success' : 'text-destructive'}>
            {trendUp ? 'Γåæ' : 'Γåô'} {trend}
          </span>
          <span className="text-muted-foreground">vs last hour</span>
        </div>
      )}
    </div>
  );
}

// VRAM_PRESSURE_THRESHOLD matches VramBar's red band (>90%): a node whose
// real measured used/total ratio crosses it is flagged here too.
const VRAM_PRESSURE_THRESHOLD = 0.9;

// MAX_DOWN_BADGES caps per-node down badges in the strip so a large outage
// degrades to "+N more" instead of wrapping the strip into a wall of red.
const MAX_DOWN_BADGES = 3;

interface FleetHealth {
  total: number;
  healthy: number;
  degraded: number;
  draining: number;
  downNodes: GPUNode[];
  staleAgents: GPUNode[];
  vramPressure: GPUNode[];
}

// computeFleetHealth derives every figure from the live node list the
// dashboard already polls (/admin/v1/nodes) - counts of real states, never
// estimates (R1).
function computeFleetHealth(nodes: GPUNode[]): FleetHealth {
  const downNodes = nodes.filter(n => n.health === 'down');
  return {
    total: nodes.length,
    healthy: nodes.filter(n => n.health === 'healthy' && !n.draining).length,
    degraded: nodes.filter(n => n.health === 'degraded' && !n.draining).length,
    draining: nodes.filter(n => n.draining).length,
    downNodes,
    // agentStale means an enabled agent IS configured for that host but
    // stopped answering past the poll-failure threshold - deliberately not
    // set for hosts that simply run agentless (see NodeState.AgentStale).
    staleAgents: nodes.filter(n => !!n.agentStale),
    vramPressure: nodes.filter(n =>
      n.vramSource !== 'none' && n.vramTotalMB > 0 &&
      n.vramUsedMB / n.vramTotalMB >= VRAM_PRESSURE_THRESHOLD
    ),
  };
}

function HealthSegment({ color, children }: { color: string; children: React.ReactNode }) {
  return (
    <span className="flex items-center gap-1.5 whitespace-nowrap">
      <span className={`w-2 h-2 rounded-full shrink-0 ${color}`} />
      <span className="text-muted-foreground">{children}</span>
    </span>
  );
}

function FleetHealthStrip({ nodes }: { nodes: GPUNode[] }) {
  const f = computeFleetHealth(nodes);
  const shownDown = f.downNodes.slice(0, MAX_DOWN_BADGES);
  const extraDown = f.downNodes.length - shownDown.length;

  return (
    <div className="glass-panel rounded-xl px-5 py-4">
      <div className="flex flex-col lg:flex-row lg:items-center justify-between gap-3 min-w-0">
        <div className="flex flex-wrap items-center gap-x-5 gap-y-2 min-w-0">
          <span className="text-[10px] font-semibold uppercase tracking-wider text-muted-foreground shrink-0">
            Fleet Health
          </span>
          {f.total === 0 ? (
            <span className="text-sm text-muted-foreground">No nodes registered yet.</span>
          ) : (
            <>
              <HealthSegment color="bg-success">
                <span className="text-foreground font-semibold font-mono">{f.healthy}/{f.total}</span> healthy
              </HealthSegment>
              {f.degraded > 0 && (
                <HealthSegment color="bg-warning">
                  <span className="text-foreground font-semibold font-mono">{f.degraded}</span> degraded
                </HealthSegment>
              )}
              <HealthSegment color={f.draining > 0 ? 'bg-amber-500' : 'bg-muted-foreground/30'}>
                <span className="text-foreground font-semibold font-mono">{f.draining}</span> draining
              </HealthSegment>
              <HealthSegment color={f.downNodes.length > 0 ? 'bg-destructive' : 'bg-muted-foreground/30'}>
                <span className="text-foreground font-semibold font-mono">{f.downNodes.length}</span> down
              </HealthSegment>
            </>
          )}
        </div>

        {f.total > 0 && (f.downNodes.length > 0 || f.staleAgents.length > 0 || f.vramPressure.length > 0) && (
          <div className="flex flex-wrap items-center gap-1.5 min-w-0">
            {shownDown.map(n => (
              <Badge key={n.id} variant="destructive" size="sm" className="max-w-full">
                <AlertTriangle className="w-3 h-3 mr-1 shrink-0" />
                <span className="truncate">{n.name} down</span>
              </Badge>
            ))}
            {extraDown > 0 && (
              <Badge variant="destructive" size="sm">+{extraDown} more down</Badge>
            )}
            {f.staleAgents.length > 0 && (
              <Badge variant="warning" size="sm">
                <AlertTriangle className="w-3 h-3 mr-1 shrink-0" />
                {f.staleAgents.length} agent{f.staleAgents.length > 1 ? 's' : ''} stale
              </Badge>
            )}
            {f.vramPressure.length > 0 && (
              <Badge variant="warning" size="sm">
                <AlertTriangle className="w-3 h-3 mr-1 shrink-0" />
                {f.vramPressure.length} VRAM pressure
              </Badge>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

function FleetCapacityCard({ nodes }: { nodes: GPUNode[] }) {
  const reporting = nodes.filter(n => n.vramTotalMB > 0);
  const usedKnown = nodes.filter(n => n.vramSource !== 'none');
  // Cluster sums over real per-node readings only (R1): used VRAM is each
  // node's own live measurement, capacity comes from nodes that report a
  // total. Nodes missing either figure are excluded from that sum and the
  // caption says how many report - never padded with an estimate.
  const usedGB = usedKnown.reduce((s, n) => s + n.vramUsedMB, 0) / 1024;
  const totalGB = reporting.reduce((s, n) => s + n.vramTotalMB, 0) / 1024;
  const freeGB = totalGB - usedGB;

  const nameCounts = new Map<string, number>();
  for (const n of nodes) {
    for (const m of n.loadedModels ?? []) {
      nameCounts.set(m.name, (nameCounts.get(m.name) ?? 0) + 1);
    }
  }
  const uniqueWarm = nameCounts.size;
  const warmInstances = [...nameCounts.values()].reduce((s, c) => s + c, 0);
  const duplicated = [...nameCounts.values()].filter(c => c > 1).length;

  const capacityPartial = nodes.length > 0 && reporting.length < nodes.length;

  return (
    <div className="glass-panel rounded-xl p-5">
      <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1 mb-4">
        <h3 className="text-sm font-semibold text-foreground flex items-center gap-2">
          <MemoryStick className="w-4 h-4 text-primary" />
          Fleet Capacity
        </h3>
        <span className="text-[10px] text-muted-foreground/70 font-medium">
          {nodes.length === 0
            ? 'no nodes'
            : capacityPartial
              ? `summed across ${reporting.length}/${nodes.length} nodes reporting capacity`
              : `summed across ${nodes.length} node${nodes.length > 1 ? 's' : ''}`}
        </span>
      </div>

      {nodes.length === 0 ? (
        <p className="text-sm text-muted-foreground py-2">Register a GPU node to see cluster capacity.</p>
      ) : (
        <div className="grid grid-cols-1 md:grid-cols-3 gap-6">
          <div className="min-w-0">
            <VramBar used={usedGB} total={totalGB} size="md" />
            <div className="flex items-center justify-between text-xs mt-2">
              <span className="text-muted-foreground font-medium">Free VRAM</span>
              <span className="text-success font-mono font-semibold">
                {totalGB > 0 ? `${freeGB.toFixed(1)} GB` : '-'}
              </span>
            </div>
          </div>
          <div className="min-w-0">
            <p className="text-xs font-medium text-muted-foreground mb-1">Warm Models</p>
            <div className="flex items-baseline gap-2">
              <span className="text-2xl font-bold text-foreground font-mono">
                {uniqueWarm > 0 || warmInstances > 0 ? uniqueWarm : '-'}
              </span>
              <span className="text-xs text-muted-foreground">
                unique · {warmInstances} resident
              </span>
            </div>
            <p className="text-[10px] text-muted-foreground/70 mt-1">
              loaded in VRAM across the fleet right now
            </p>
          </div>
          <div className="min-w-0">
            <p className="text-xs font-medium text-muted-foreground mb-1">Duplicated Models</p>
            <div className="flex items-baseline gap-2">
              <span className={`text-2xl font-bold font-mono ${duplicated > 0 ? 'text-amber-600 dark:text-amber-400' : 'text-foreground'}`}>
                {nodes.some(n => (n.loadedModels ?? []).length > 0) ? duplicated : '-'}
              </span>
              <span className="text-xs text-muted-foreground">
                on multiple nodes
              </span>
            </div>
            <p className="text-[10px] text-muted-foreground/70 mt-1">
              the same model warm twice costs VRAM another model could use
            </p>
          </div>
        </div>
      )}
    </div>
  );
}

export function Dashboard() {
  const { demoMode } = useDemoMode();
  const location = useLocation();
  const { requests, newRequestId, isLive: requestsLive } = useLiveRequests(10);
  const [nodes, setNodes] = useState<GPUNode[]>(demoMode ? mockGPUNodes : []);
  const [summary, setSummary] = useState(demoMode ? {
        activeRequests: 1,
        avgLatency: 145.2,
        tokensPerMin: 12450,
        coldStarts: 19,
        queueDepth: 0,
        nodesOnline: 4,
        nodesDraining: 1,
        totalNodes: 6,
        warmHitRatio: 0.94,
  } : {
    activeRequests: 0,
    avgLatency: 0,
    tokensPerMin: 0,
    coldStarts: 0,
    queueDepth: 0,
    nodesOnline: 0,
    nodesDraining: 0,
    totalNodes: 0,
    warmHitRatio: 0,
  });
  const [isLive, setIsLive] = useState(!demoMode);
  const [error, setError] = useState<string | null>(null);
  const [savings, setSavings] = useState<Savings | null>(demoMode ? mockSavings : null);
  const [savingsLoading, setSavingsLoading] = useState(!demoMode);
  const [proxyPort, setProxyPort] = useState<number>(11434);
  const [version, setVersion] = useState<string>('');

  useEffect(() => {
    if (currentAppPath() !== '/') return;
    let active = true;
    fetchHealth().then(h => {
      if (!active || currentAppPath() !== '/') return;
      if (h.proxy_port) setProxyPort(h.proxy_port);
      if (h.version) setVersion(h.version);
    }).catch(() => {});
    return () => { active = false; };
  }, [location.pathname]);

  useEffect(() => {
    if (currentAppPath() !== '/') return;
    let active = true;
    const loadData = async () => {
      if (demoMode) {
        if (!active || currentAppPath() !== '/') return;
        setNodes(mockGPUNodes);
        setSummary({
    activeRequests: 1,
    avgLatency: 145.2,
    tokensPerMin: 12450,
    coldStarts: 19,
    queueDepth: 0,
    nodesOnline: 4,
    nodesDraining: 1,
    totalNodes: 6,
    warmHitRatio: 0.94,
  });
        setIsLive(false);
        setError(null);
        return;
      }
      try {
        const [nodesData, summaryData] = await Promise.all([
          fetchNodes(),
          fetchSummary()
        ]);
        if (!active || currentAppPath() !== '/') return;
        setNodes(nodesData || []);
        setSummary(summaryData || summary);
        setIsLive(true);
        setError(null);
      } catch (err: any) {
        if (!active || currentAppPath() !== '/') return;
        setIsLive(false);
        setNodes([]);
        setError(err.message || 'Failed to fetch data');
      }
    };
    loadData();
    if (demoMode) return () => { active = false; };
    const interval = setInterval(loadData, 10000);
    return () => {
      active = false;
      clearInterval(interval);
    };
  }, [demoMode, location.pathname]);

  useEffect(() => {
    if (currentAppPath() !== '/') return;
    let active = true;
    // Tracked locally (not read from the savings state directly) since this
    // effect's deps don't include savings - reading React state here would
    // be a stale closure over whatever savings was when the effect mounted,
    // never reflecting a later successful load within the same interval.
    let hasData = savings !== null;
    const loadSavings = async () => {
      if (demoMode) return;
      if (active && currentAppPath() === '/' && !hasData) {
        setSavingsLoading(true);
      }
      try {
        const data = await fetchSavings();
        if (!active || currentAppPath() !== '/') return;
        setSavings(data);
        hasData = true;
      } catch {
        if (!active || currentAppPath() !== '/') return;
        setSavings(null);
      } finally {
        if (active && currentAppPath() === '/') {
          setSavingsLoading(false);
        }
      }
    };
    loadSavings();
    if (demoMode) return () => { active = false; };
    const interval = setInterval(loadSavings, 5000);
    return () => {
      active = false;
      clearInterval(interval);
    };
  }, [demoMode, location.pathname]);

  const activeFromRequests = requests.filter(r => r.status === 'loading').length;
  const displayActive = (isLive || demoMode) ? summary.activeRequests : activeFromRequests;
  const displayLatency = (isLive || demoMode) ? summary.avgLatency : 0;
  const displayTokens = (isLive || demoMode) ? summary.tokensPerMin : "--";
  const displayColdStarts = (isLive || demoMode) ? summary.coldStarts : '--';
  const displayQueue = (isLive || demoMode) ? summary.queueDepth : 0;
  const displayWarmHitRatio = (isLive || demoMode) ? summary.warmHitRatio : 0;

  return (
    <div className="space-y-6 animate-fade-in max-w-7xl mx-auto">
      {/* Top Bar */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4 border-b border-border pb-6">
        <div className="flex flex-col sm:flex-row sm:items-center gap-4 sm:gap-8">
          <div className="flex items-center gap-2">
            <StatusDot status="online" pulse />
            <span className="text-sm font-semibold text-foreground">System Status</span>
          </div>
          <div className="flex flex-wrap items-center gap-4 text-sm">
            <div className="flex items-center gap-2">
              <span className="text-muted-foreground">Port:</span>
              <span className="text-foreground font-medium font-mono">{proxyPort}</span>
            </div>
            <div className="flex items-center gap-2">
              <span className="text-muted-foreground">Version:</span>
              <Badge variant="muted" size="sm">{version ? `v${version}` : `v${__APP_VERSION__}`}</Badge>
            </div>
            <div className="flex items-center gap-2 sm:px-3 sm:border-l sm:border-border">
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

      {/* Fleet Health strip - first operational answer: is my fleet OK? */}
      <FleetHealthStrip nodes={nodes} />

      {/* Fleet Capacity + fleet ROI, one screen: is my fleet healthy, do I
          need another GPU or am I placing badly, and what is it saving me.
          Capacity owns two-thirds; the savings figure sits beside it above
          the fold (it also lives on Routing next to the strategy picker). */}
      <div className="grid grid-cols-1 lg:grid-cols-3 gap-4 items-stretch">
        <div className="lg:col-span-2 min-w-0">
          <FleetCapacityCard nodes={nodes} />
        </div>
        <div className="min-w-0">
          <SavingsCard savings={savings} loading={savingsLoading} />
        </div>
      </div>

      {/* Traffic metrics - demoted to a compact secondary row below the fleet views */}
      <div className="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-6 gap-3">
        <MetricCard
          compact
          title="Active Requests"
          value={displayActive.toString()}
          icon={<Activity className="w-4 h-4" />}
        />
        <MetricCard
          compact
          title="Queued"
          value={displayQueue.toString()}
          unit="waiting"
          icon={<Clock className="w-4 h-4" />}
          highlight={displayQueue > 0 ? 'warning' : undefined}
        />
        <MetricCard
          compact
          title="Avg Latency"
          value={isLive || demoMode ? displayLatency.toFixed(0) : '--'}
          unit={isLive || demoMode ? 'ms' : undefined}
          icon={<Clock className="w-4 h-4" />}
        />
        <MetricCard
          compact
          title="Tokens/min"
          value={displayTokens.toString()}
          icon={<Zap className="w-4 h-4" />}
        />
        <MetricCard
          compact
          title="Warm Hit Ratio"
          value={isLive || demoMode ? `${(displayWarmHitRatio * 100).toFixed(0)}%` : '--'}
          icon={<Flame className="w-4 h-4 text-orange-600 dark:text-orange-400" />}
        />
        <MetricCard
          compact
          title="Cold Starts"
          value={displayColdStarts.toString()}
          unit="events"
          icon={<Server className="w-4 h-4" />}
        />
      </div>

      {/* GPU Nodes Panel */}
      <div className="glass-panel rounded-xl p-6">
        <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 mb-6">
          <h3 className="text-sm font-semibold text-foreground">GPU Nodes Status</h3>
          <div className="flex flex-wrap items-center gap-x-4 gap-y-1.5 text-xs font-medium">
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
            {summary.nodesDraining > 0 && (
              <div className="flex items-center gap-1.5">
                <span className="w-2 h-2 rounded-full bg-amber-500" />
                <span className="text-amber-700 dark:text-amber-400 font-semibold">{summary.nodesDraining} Draining</span>
              </div>
            )}
          </div>
        </div>
        
        <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
          {nodes.length === 0 && !demoMode ? (
            <div className="col-span-2 py-10 text-center text-sm text-muted-foreground">
              {isLive ? 'No nodes connected.' : 'No nodes available - backend disconnected'}
            </div>
          ) : (
            nodes.map((node) => (
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

                <div className="mb-3">
                  <VramBar
                    used={node.vramUsedMB / 1024}
                    total={node.vramTotalMB / 1024}
                    size="sm"
                    pending={(node.pendingPrewarmMB ?? 0) / 1024}
                  />
                </div>

                {/* Node telemetry metrics */}
                <div className="grid grid-cols-3 gap-2 mb-4 text-[11px] border-t border-border/40 pt-3">
                  <div>
                    <span className="text-muted-foreground block text-[9px] uppercase font-semibold tracking-wider">Warm Hit</span>
                    <span className="font-semibold text-foreground font-mono mt-0.5 block">
                      {node.warmHitRatio !== undefined ? `${(node.warmHitRatio * 100).toFixed(0)}%` : '--'}
                    </span>
                  </div>
                  <div>
                    <span className="text-muted-foreground block text-[9px] uppercase font-semibold tracking-wider">Cold Starts</span>
                    <span className="font-semibold text-foreground font-mono mt-0.5 block">
                      {node.coldStarts !== undefined ? node.coldStarts : '--'}
                    </span>
                  </div>
                  <div>
                    <span className="text-muted-foreground block text-[9px] uppercase font-semibold tracking-wider">Avg Latency</span>
                    <span className="font-semibold text-foreground font-mono mt-0.5 block">
                      {node.avgLatencyMs !== undefined && node.avgLatencyMs > 0 ? `${node.avgLatencyMs.toFixed(0)}ms` : '--'}
                    </span>
                  </div>
                </div>

                <div className="flex flex-wrap gap-1.5">
                  {(node.loadedModels || []).map((model) => (
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
            ))
          )}
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
          <div className="flex items-center gap-4">
            <span className="text-xs font-medium text-muted-foreground">Auto-refresh: 2s</span>
            <Link
              to="/requests"
              className="text-xs font-medium text-primary hover:underline flex items-center gap-1"
            >
              View All Requests
              <ArrowRight className="w-3 h-3" />
            </Link>
          </div>
        </div>
        
        <div className="hidden md:block overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="bg-secondary/50 border-b border-border text-muted-foreground">
                <th className="px-6 py-3 text-left font-medium">API Key</th>
                <th className="px-6 py-3 text-left font-medium">Model</th>
                <th className="px-6 py-3 text-left font-medium">Routed To</th>
                <th className="px-6 py-3 text-left font-medium">Status</th>
                <th className="px-6 py-3 text-right font-medium">Tokens</th>
                <th className="px-6 py-3 text-right font-medium">tok/s</th>
                <th className="px-6 py-3 text-right font-medium">Latency</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-border">
              {requests.length === 0 && requestsLive && (
                <tr>
                  <td colSpan={7} className="px-6 py-10 text-center text-sm text-muted-foreground">
                    No requests yet. Send a request to your proxy endpoint to see live traffic here.
                  </td>
                </tr>
              )}
              {requests.slice(0, 10).map((req) => (
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
                  <td className="px-6 py-3 text-right font-mono text-muted-foreground">
                    {req.tokens > 0 ? req.tokens : '-'}
                  </td>
                  <td className="px-6 py-3 text-right font-mono text-muted-foreground">
                    {req.tokensPerSec > 0 ? req.tokensPerSec.toFixed(1) : '-'}
                  </td>
                  <td className="px-6 py-3 text-right font-medium font-mono">
                    <span className={req.latency > 1000 ? 'text-amber-700 dark:text-amber-400' : 'text-primary'}>
                      {req.latency}ms
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>

        <div className="md:hidden space-y-3 p-4">
          {requests.length === 0 && requestsLive && (
            <div className="text-center text-sm text-muted-foreground py-6">
              No requests yet. Send a request to your proxy endpoint to see live traffic here.
            </div>
          )}
          {requests.slice(0, 10).map((req) => (
            <div
              key={req.id}
              className={`bg-card/50 backdrop-blur-sm border border-border/60 rounded-xl p-4 transition-colors duration-200 ${
                newRequestId === req.id ? 'bg-primary/5' : ''
              }`}
            >
              <div className="flex items-center justify-between mb-3">
                <span className="text-sm font-medium text-foreground">{req.model}</span>
                <Badge variant={req.status === 'warm' ? 'success' : 'warning'} size="sm">
                  {req.status === 'warm' ? 'Warm' : 'Loading'}
                </Badge>
              </div>
              <div className="grid grid-cols-2 gap-3">
                <div>
                  <div className="text-[10px] uppercase tracking-wider text-muted-foreground">API Key</div>
                  <div className="text-sm text-foreground font-mono truncate">{req.apiKey}</div>
                </div>
                <div>
                  <div className="text-[10px] uppercase tracking-wider text-muted-foreground">Routed To</div>
                  <div className="text-sm text-foreground">{req.routedTo}</div>
                </div>
                <div>
                  <div className="text-[10px] uppercase tracking-wider text-muted-foreground">Tokens</div>
                  <div className="text-sm text-foreground font-mono">{req.tokens > 0 ? req.tokens : '-'}</div>
                </div>
                <div>
                  <div className="text-[10px] uppercase tracking-wider text-muted-foreground">tok/s</div>
                  <div className="text-sm text-foreground font-mono">
                    {req.tokensPerSec > 0 ? req.tokensPerSec.toFixed(1) : '-'}
                  </div>
                </div>
                <div>
                  <div className="text-[10px] uppercase tracking-wider text-muted-foreground">Latency</div>
                  <div className={`text-sm font-mono font-medium ${req.latency > 1000 ? 'text-amber-700 dark:text-amber-400' : 'text-primary'}`}>
                    {req.latency}ms
                  </div>
                </div>
              </div>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
