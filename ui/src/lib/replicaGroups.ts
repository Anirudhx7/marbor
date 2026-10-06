// Pure derivations for the GPU nodes "Replica groups" tab. Everything here is
// typed and side-effect free (apart from the two small localStorage helpers at
// the bottom) so `tsc -b` is the check for the logic. Nothing is invented: every
// value comes from the node payload or the suggestions payload, and a missing
// value is `-`.

import type { GPUNode } from '../types';
import type { ReplicaSuggestion, ReplicaSuggestionDeclared, ReplicaSuggestionsResponse, TopologyCoverage } from './api';

export function launcherLabel(s: ReplicaSuggestion): string {
  if (s.launcher === 'vllm-mp') return 'vLLM multi-host';
  if (s.launcher === 'llamacpp-rpc') return 'llama.cpp RPC';
  return 'multi-host';
}

export function workersOf(s: ReplicaSuggestion): string[] {
  return s.members.filter(m => m !== s.head);
}

// --- Suggestion buckets ---

export interface SuggestionBuckets {
  conflicts: ReplicaSuggestion[];
  ready: ReplicaSuggestion[];
  waiting: ReplicaSuggestion[];
  dismissed: ReplicaSuggestion[];
}

export function bucketSuggestions(list: readonly ReplicaSuggestion[]): SuggestionBuckets {
  const out: SuggestionBuckets = { conflicts: [], ready: [], waiting: [], dismissed: [] };
  for (const s of list) {
    if (s.dismissed) out.dismissed.push(s);
    else if (s.state === 'contradicts_declared') out.conflicts.push(s);
    else if (s.state === 'complete') out.ready.push(s);
    else out.waiting.push(s);
  }
  return out;
}

export function actionableCount(b: SuggestionBuckets): number {
  return b.conflicts.length + b.ready.length;
}

// The tab exists when there is a group to list or any suggestion (dismissed
// ones included, so Restore stays reachable). Coverage gaps alone never show it.
export function hasReplicaContent(nodes: readonly GPUNode[], data: ReplicaSuggestionsResponse | null): boolean {
  if ((data?.suggestions.length ?? 0) > 0) return true;
  return nodes.some(n => (n.replicaPeers?.members.length ?? 0) > 0);
}

// --- Status chips ---

export type ChipTone = 'success' | 'info' | 'warn' | 'danger' | 'muted';

export interface StatusChip {
  label: string;
  tone: ChipTone;
}

const RANK_MISSING = /^rank (\d+) has not been seen/i;

// waitingChip reads the literal rank numbers out of the missing[] text. If any
// item is not in that exact form it falls back to a generic label rather than
// guessing.
export function waitingChip(s: ReplicaSuggestion): StatusChip {
  if (s.state === 'conflicting') return { label: 'Details conflict', tone: 'warn' };
  const ranks: string[] = [];
  for (const m of s.missing) {
    const hit = RANK_MISSING.exec(m.trim());
    if (!hit) return { label: 'Waiting for more details', tone: 'muted' };
    ranks.push(hit[1]);
  }
  if (ranks.length === 0) return { label: 'Waiting for more details', tone: 'muted' };
  return { label: `Waiting on rank${ranks.length === 1 ? '' : 's'} ${ranks.join(', ')}`, tone: 'muted' };
}

export function suggestionChip(s: ReplicaSuggestion): StatusChip {
  switch (s.state) {
    case 'complete': return { label: 'Ready to confirm', tone: 'success' };
    case 'contradicts_declared': return { label: 'Differs from declared', tone: 'danger' };
    default: return waitingChip(s);
  }
}

// reasonToShow keeps the reason from being printed twice: when missing[] has
// items it already says what is needed, so the reason is dropped.
export function reasonToShow(s: ReplicaSuggestion): string {
  if (!s.reason || s.missing.length > 0) return '';
  return s.reason;
}

// --- Drift (contradicts_declared) ---

export type NodeRole = 'head' | 'worker' | 'standalone';

export interface DriftRow {
  node: string;
  before: string;
  after: string;
  beforeRole: NodeRole;
  afterRole: NodeRole;
}

function membersText(members: readonly string[]): string {
  return members.length > 0 ? members.join(', ') : '-';
}

// driftRows lists every node the adopt would write: each detected member plus
// each node whose declaration the suggestion carries. "nothing declared" only
// appears when declared[] has no entry for the node.
export function driftRows(s: ReplicaSuggestion): DriftRow[] {
  const names: string[] = [];
  for (const n of [...s.members, ...s.declared.map(d => d.node)]) {
    if (!names.includes(n)) names.push(n);
  }
  return names.map(node => {
    const d = s.declared.find(x => x.node === node);
    const beforeRole: NodeRole = !d ? 'standalone' : d.head === node ? 'head' : d.head ? 'worker' : 'standalone';
    const inGroup = s.members.includes(node);
    const afterRole: NodeRole = !inGroup ? beforeRole : node === s.head ? 'head' : 'worker';
    const before = d ? `head ${d.head || '-'}, members ${membersText(d.members)}` : 'nothing declared';
    const after = inGroup ? `head ${s.head || '-'}, members ${membersText(s.members)}` : before;
    return { node, before, after, beforeRole, afterRole };
  });
}

export function roleChangeLine(row: DriftRow, head: string): string {
  if (row.beforeRole === row.afterRole) return '';
  if (row.afterRole === 'worker') {
    return `${row.node} becomes a worker: it stops receiving requests directly and its own keep-warm config stops applying. Requests route to ${head}.`;
  }
  if (row.afterRole === 'head') return `${row.node} becomes the head and serves the whole group.`;
  return '';
}

// mentionsNode reports whether text names the node as a whole word, so
// "gpu-node-1" is not found inside "gpu-node-10".
export function mentionsNode(text: string, name: string): boolean {
  if (!name) return false;
  const isNameChar = (c: string | undefined) => c !== undefined && /[A-Za-z0-9._-]/.test(c);
  let from = 0;
  for (;;) {
    const i = text.indexOf(name, from);
    if (i < 0) return false;
    const end = i + name.length;
    // A trailing "." ends a sentence unless a name character follows it (gpu-1.lan).
    const afterIsName = text[end] === '.' ? isNameChar(text[end + 1]) : isNameChar(text[end]);
    if (!isNameChar(text[i - 1]) && !afterIsName) return true;
    from = i + 1;
  }
}

// outsideDeclarers finds nodes outside the group whose own declaration names a
// group member. Adopt refuses while any exist, so they are listed for fixing.
export function outsideDeclarers(s: ReplicaSuggestion, nodes: readonly GPUNode[], serverText = ''): string[] {
  const inGroup = new Set<string>([...s.members, ...s.declared.map(d => d.node)]);
  const found: string[] = [];
  for (const n of nodes) {
    if (inGroup.has(n.name)) continue;
    const names = n.replicaPeers?.members ?? [];
    const mentioned = serverText !== '' && mentionsNode(serverText, n.name);
    if (mentioned || names.some(m => s.members.includes(m))) found.push(n.name);
  }
  return found;
}

// sameDeclared compares two declared[] lists ignoring entry order, member order
// and JSON key order, the way the server compares an adopt snapshot.
export function sameDeclared(a: readonly ReplicaSuggestionDeclared[], b: readonly ReplicaSuggestionDeclared[]): boolean {
  if (a.length !== b.length) return false;
  const byNode = new Map(b.map(d => [d.node, d] as const));
  if (byNode.size !== b.length) return false;
  return a.every(x => {
    const y = byNode.get(x.node);
    if (!y || x.head !== y.head) return false;
    const xm = [...new Set(x.members)].sort();
    const ym = [...new Set(y.members)].sort();
    return xm.length === ym.length && xm.every((m, i) => m === ym[i]);
  });
}

export function keepDeclaredCopy(s: ReplicaSuggestion, nodes: readonly GPUNode[]): string {
  const unresolved = s.members.some(m => nodes.find(n => n.name === m)?.schedulingRole === 'unresolved');
  return unresolved
    ? 'Your declaration stays unresolved. This hides the suggestion until the group state changes.'
    : 'Keeps your declaration and hides this suggestion until the group state changes.';
}

// --- Confirmed groups (derived from node payload only) ---

export interface ConfirmedGroup {
  key: string;
  resolved: boolean;
  head: string;
  // Declared member names (resolved: head first, then workers sorted).
  members: string[];
  nodes: GPUNode[];
}

export function deriveConfirmedGroups(nodes: readonly GPUNode[]): ConfirmedGroup[] {
  const byName = new Map(nodes.map(n => [n.name, n] as const));
  const groups: ConfirmedGroup[] = [];
  for (const h of nodes.filter(n => n.schedulingRole === 'head')) {
    const workers = nodes.filter(n => n.schedulingRole === 'worker' && n.replicaHead === h.name).sort((a, b) => a.name.localeCompare(b.name));
    groups.push({ key: `head:${h.name}`, resolved: true, head: h.name, members: [h.name, ...workers.map(w => w.name)], nodes: [h, ...workers] });
  }
  const seen = new Set<string>();
  for (const n of nodes.filter(x => x.schedulingRole === 'unresolved')) {
    const declared = [...(n.replicaPeers?.members ?? [n.name])].sort();
    const key = `unresolved:${declared.join(',')}`;
    if (seen.has(key)) continue;
    seen.add(key);
    const present = declared.map(m => byName.get(m)).filter((x): x is GPUNode => x !== undefined);
    groups.push({ key, resolved: false, head: '-', members: declared, nodes: present });
  }
  return groups;
}

const HEALTH_RANK: Record<GPUNode['health'], number> = { healthy: 0, degraded: 1, down: 2 };

export function confirmedChip(g: ConfirmedGroup): StatusChip {
  if (!g.resolved) return { label: 'Unresolved', tone: 'danger' };
  let worst: GPUNode['health'] = 'healthy';
  for (const n of g.nodes) if (HEALTH_RANK[n.health] > HEALTH_RANK[worst]) worst = n.health;
  if (worst === 'down') return { label: 'Down', tone: 'danger' };
  if (worst === 'degraded') return { label: 'Degraded', tone: 'warn' };
  return { label: 'Healthy', tone: 'success' };
}

export function parallelismText(n: GPUNode | undefined): string {
  if (!n || !n.parallelismType) return '-';
  const type = n.parallelismType.toUpperCase();
  return n.parallelismWidth && n.parallelismWidth > 0 ? `${type} ${n.parallelismWidth}` : type;
}

export function gpusText(n: GPUNode | undefined): string {
  const par = parallelismText(n);
  const model = n?.gpuModel ?? '';
  if (par !== '-' && model) return `${par} on ${model}`;
  if (par !== '-') return par;
  return model || '-';
}

// detectedLaunchText shows the node-level detected fields verbatim, or `-`.
export function detectedLaunchText(n: GPUNode | undefined): string {
  if (!n) return '-';
  const shape = n.detectedParallelismType
    ? `${n.detectedParallelismType.toUpperCase()}${n.detectedParallelismWidth ? ` ${n.detectedParallelismWidth}` : ''}`
    : '';
  const main = [n.detectedRuntime, shape].filter(Boolean).join(', ');
  if (!main && !n.detectedSource) return '-';
  return n.detectedSource ? `${main || 'detected'} (${n.detectedSource})` : main;
}

// --- Detection coverage ---

export interface CoverageSummary {
  listed: TopologyCoverage[];
  // Only nodes whose limit the operator can act on count toward the header.
  // no_agent is listed but not counted: an agent-less fleet is a choice.
  limitedCount: number;
}

export function coverageSummary(cov: readonly TopologyCoverage[]): CoverageSummary {
  const listed = cov.filter(c => c.state !== 'reporting');
  const limitedCount = listed.filter(c => c.state === 'agent_update_needed' || c.state === 'env_unreadable').length;
  return { listed, limitedCount };
}

export function coverageLine(c: TopologyCoverage): string {
  switch (c.state) {
    case 'agent_update_needed': return `${c.node}: agent update needed to detect multi-host launches`;
    case 'no_agent': return `${c.node}: no agent data, so multi-host launches cannot be seen`;
    case 'env_unreadable': return `${c.node}: the agent cannot read this runtime's environment, so some launch details are unknown`;
    default: return `${c.node}: -`;
  }
}

// --- Per-node chips on the Nodes tab ---

export interface NodeChip {
  key: 'detected-head' | 'no-agent';
  label: string;
  title: string;
}

// chipsByNode: the head of a detected (ready or conflicting-with-declared)
// group, and a no-agent chip on a node that sits in an active suggestion but
// has no agent to report for itself. Nothing else.
export function chipsByNode(data: ReplicaSuggestionsResponse | null): Record<string, NodeChip[]> {
  const out: Record<string, NodeChip[]> = {};
  if (!data) return out;
  const push = (node: string, chip: NodeChip) => {
    if (!out[node]) out[node] = [];
    if (!out[node].some(c => c.key === chip.key)) out[node].push(chip);
  };
  const noAgent = new Set(data.coverage.filter(c => c.state === 'no_agent').map(c => c.node));
  for (const s of data.suggestions) {
    if (s.dismissed) continue;
    if ((s.state === 'complete' || s.state === 'contradicts_declared') && s.head) {
      push(s.head, { key: 'detected-head', label: 'Detected group head', title: 'Agents report this node as the head of a multi-host group. Open the Replica groups tab to review it.' });
    }
    for (const m of s.members) {
      if (noAgent.has(m)) push(m, { key: 'no-agent', label: 'No agent data', title: 'No marbor agent is enabled on this host, so it cannot report its own launch details.' });
    }
  }
  return out;
}

// --- Slim Nodes-tab line dismissal ---

const READY_LINE_KEY = 'marbor.replicaReadyLineDismissed';

export function readyFingerprints(b: SuggestionBuckets): string[] {
  return b.ready.map(s => s.fingerprint).sort();
}

export function readDismissedReady(): string[] {
  try {
    const raw = localStorage.getItem(READY_LINE_KEY);
    const parsed: unknown = raw ? JSON.parse(raw) : [];
    return Array.isArray(parsed) ? parsed.filter((x): x is string => typeof x === 'string') : [];
  } catch {
    return [];
  }
}

export function writeDismissedReady(fingerprints: readonly string[]): void {
  try {
    localStorage.setItem(READY_LINE_KEY, JSON.stringify([...fingerprints].sort()));
  } catch {
    // storage unavailable: the line simply reappears on the next load
  }
}

// pruneDismissedReady drops dismissals for groups that no longer exist, so the
// stored list cannot grow forever. A fingerprint still present in the current
// suggestions (in any state) is kept.
export function pruneDismissedReady(dismissed: readonly string[], known: readonly string[]): string[] {
  const live = new Set(known);
  return dismissed.filter(f => live.has(f));
}

// The line stays hidden while every ready fingerprint was already dismissed;
// a new ready group brings it back.
export function readyLineVisible(ready: readonly string[], dismissed: readonly string[]): boolean {
  return ready.length > 0 && ready.some(f => !dismissed.includes(f));
}
