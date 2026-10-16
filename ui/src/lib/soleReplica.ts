import type { SoleReplicaImpact, SoleReplicaModel, UnknownReplicaItem } from './api';

// Pure helpers behind the "other replicas" check inside the Drain and Restart
// dialogs. Kept free of React so they can be tested directly.

export type SoleImpactState =
  | { status: 'idle' }
  | { status: 'loading' }
  | { status: 'ready'; impact: SoleReplicaImpact }
  | { status: 'error' };

// A stored result is only valid for the node it was fetched for; anything else
// (a previous dialog's result) counts as still checking, so the confirm button
// is never enabled from stale data during the render before the new fetch starts.
export function resolveImpactState(
  stored: { node: string | null } & SoleImpactState,
  nodeName: string | null,
): SoleImpactState {
  if (!nodeName) return { status: 'idle' };
  if (stored.node !== nodeName) return { status: 'loading' };
  return stored;
}

export function hasSoleModels(s: SoleImpactState): boolean {
  return s.status === 'ready' && s.impact.models.length > 0;
}

export function isChecking(s: SoleImpactState): boolean {
  return s.status === 'loading';
}

export function confirmLabel(verb: 'Drain' | 'Restart', busy: boolean, s: SoleImpactState): string {
  if (busy) return verb === 'Drain' ? 'Draining...' : 'Restarting...';
  if (hasSoleModels(s)) return `${verb} anyway`;
  return verb === 'Drain' ? 'Drain node' : 'Restart';
}

export function formatModel(m: SoleReplicaModel): string {
  return m.runtime ? `${m.model} (${m.runtime})` : m.model;
}

export function unknownLine(u: UnknownReplicaItem): string {
  return `${u.node}${u.model ? `, ${u.model}` : ''}: ${u.reason}`;
}

// More cold-load models than this collapse to a one-line count.
export const COLD_COLLAPSE_AFTER = 3;

export function coldLoadSummary(count: number): string {
  return count === 1
    ? '1 model would have to cold-load on another node'
    : `${count} models would have to cold-load on other nodes`;
}
