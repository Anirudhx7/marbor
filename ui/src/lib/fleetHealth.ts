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
  // workersFailing counts headless workers with a failing probe. They are muted
  // on purpose: a headless worker is expected to fail its probe.
  workersFailing: number;
  staleAgents: GPUNode[];
  vramPressure: GPUNode[];
}

// computeFleetHealth derives every figure from the live node list the
// dashboard already polls (/admin/v1/nodes) - counts of real states, never
// estimates. Which nodes are healthy, degraded, draining, down or under VRAM
// pressure is the server's call (node.signals, the same definition behind
// "marbor nodes --signal"); this only counts them. A node flagged
// headlessWorker lands in the workers figure only, never also in another
// bucket, and the flag is read as sent, never recomputed from role and health.
export function computeFleetHealth(nodes: GPUNode[]): FleetHealth {
  const counted = nodes.filter(n => !n.headlessWorker);
  const withSignal = (id: NodeSignalId) => counted.filter(n => (n.signals ?? []).includes(id));
  return {
    total: counted.length,
    healthy: withSignal('healthy').length,
    degraded: withSignal('degraded').length,
    draining: withSignal('draining').length,
    downNodes: withSignal('down'),
    workersFailing: nodes.length - counted.length,
    // agentStale means an enabled agent IS configured for that host but
    // stopped answering past the poll-failure threshold - deliberately not
    // set for hosts that simply run agentless (see NodeState.AgentStale).
    staleAgents: nodes.filter(n => !!n.agentStale),
    vramPressure: withSignal('pressure'),
  };
}

// workersText is the strip's worker segment, also used as its tooltip and
// aria-label so all three say the same thing. Empty when no worker is failing.
export function workersText(count: number): string {
  if (count <= 0) return '';
  return count === 1 ? '1 worker probe failing' : `${count} workers probe failing`;
}
