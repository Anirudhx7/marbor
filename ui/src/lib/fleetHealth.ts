// Fleet-health figures for the dashboard strip. Pure and side-effect free so
// the node:test suite can import it; the dashboard page only renders them.

import type { GPUNode, NodeSignalId } from '../types';

export interface FleetHealth {
  // total is every node except a replica worker whose probe is failing, the
  // same population the health endpoint counts, so the strip and /health agree.
  total: number;
  healthy: number;
  degraded: number;
  draining: number;
  downNodes: GPUNode[];
  // workerNodes are headless workers with a failing probe. They are muted on
  // purpose: a headless worker is expected to fail its probe.
  workerNodes: GPUNode[];
  workersFailing: number;
  staleAgents: GPUNode[];
  vramPressure: GPUNode[];
}

// computeFleetHealth derives every figure from the live node list the
// dashboard already polls (/admin/v1/nodes) - counts of real states, never
// estimates. Which nodes are healthy, degraded, draining, down or under VRAM
// pressure is the server's call (node.signals, the same definition behind
// "marbor nodes --signal"); this only counts them.
//
// A node flagged headlessWorker is read as sent, never recomputed from role and
// health. It lands in the workers figure and is left out of the total and of
// healthy, degraded and down, exactly as the health endpoint leaves it out. The
// server keeps the draining, agent and pressure signals on such a worker, so
// draining, stale agents and VRAM pressure are counted over every node: muting
// a failing probe must never hide a drain, a silent agent or a full GPU.
export function computeFleetHealth(nodes: GPUNode[]): FleetHealth {
  const counted = nodes.filter(n => !n.headlessWorker);
  const workerNodes = nodes.filter(n => !!n.headlessWorker);
  const has = (list: GPUNode[], id: NodeSignalId) => list.filter(n => (n.signals ?? []).includes(id));
  return {
    total: counted.length,
    healthy: has(counted, 'healthy').length,
    degraded: has(counted, 'degraded').length,
    draining: has(nodes, 'draining').length,
    downNodes: has(counted, 'down'),
    workerNodes,
    workersFailing: workerNodes.length,
    // agentStale means an enabled agent IS configured for that host but
    // stopped answering past the poll-failure threshold - deliberately not
    // set for hosts that simply run agentless (see NodeState.AgentStale).
    staleAgents: nodes.filter(n => !!n.agentStale),
    vramPressure: has(nodes, 'pressure'),
  };
}

// workersLabel is the words after the count in the strip's worker segment.
export function workersLabel(count: number): string {
  return count === 1 ? 'worker probe failing' : 'workers probe failing';
}

// workersText is the whole segment as one string, used for the tooltip. Empty
// when no worker is failing.
export function workersText(count: number): string {
  return count > 0 ? `${count} ${workersLabel(count)}` : '';
}

// nodeDotStatus picks the status dot for a node card: a failing headless
// worker is a muted worker, not red; everything else shows its raw health.
export function nodeDotStatus(n: Pick<GPUNode, 'health' | 'headlessWorker'>): 'worker' | GPUNode['health'] {
  return n.headlessWorker ? 'worker' : n.health;
}

// WORKER_DOT_TITLE is the muted worker dot's tooltip.
export const WORKER_DOT_TITLE = 'Worker host: probe failing is expected for a headless worker. Not counted in fleet health.';

// nodeDotTitle is the dot's tooltip. A draining worker keeps both facts; an
// undefined result lets the dot fall back to its own label.
export function nodeDotTitle(n: Pick<GPUNode, 'draining' | 'headlessWorker'>): string | undefined {
  if (n.draining && n.headlessWorker) return `Draining. ${WORKER_DOT_TITLE}`;
  return n.draining ? 'Draining' : undefined;
}
