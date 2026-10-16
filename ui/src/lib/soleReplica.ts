import type { SoleReplicaBody, SoleReplicaImpact, SoleReplicaModel, UnknownReplicaItem } from './api';

// Pure helpers behind the "other replicas" check inside the Drain and Restart
// dialogs. Kept free of React so they can be tested directly.

export type SoleImpactState =
  | { status: 'idle' }
  | { status: 'loading' }
  | { status: 'ready'; impact: SoleReplicaImpact }
  | { status: 'error'; message?: string };

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

// impactSummary is the one-sentence description the confirm button points at
// (aria-describedby), or null when the notice shows nothing visible. The
// notice and the button both use it, so the button never references a missing
// element.
export function impactSummary(s: SoleImpactState): string | null {
  switch (s.status) {
    case 'idle': return null;
    case 'loading': return 'Checking other replicas.';
    case 'error': return 'Could not check other replicas.';
    case 'ready': {
      const { models, cold_load_only: cold, unknown } = s.impact;
      if (models.length > 0) return `Only this node can serve ${models.length === 1 ? '1 model' : `${models.length} models`}; no other node can serve them.`;
      if (cold.length > 0) return `${coldLoadSummary(cold.length)}.`;
      if (unknown.length > 0) return 'Could not verify other replicas.';
      return null;
    }
  }
}

export function impactVisible(s: SoleImpactState): boolean {
  return impactSummary(s) !== null;
}

// The runtime actions the sole-replica check applies to or skips.
export type SoleAction = 'drain' | 'start' | 'stop' | 'restart';

// shouldForce decides whether a confirmed action is sent with force=true: only a
// drain or restart, and only when the dialog listed models that only this node
// can serve. A failed, loading or empty check never sends it.
export function shouldForce(action: SoleAction, s: SoleImpactState): boolean {
  return (action === 'drain' || action === 'restart') && hasSoleModels(s);
}

// runtimeActionQuery builds the query string for a drain/stop/restart request:
// '' when nothing is set, otherwise '?a=true&b=true'. Only the exact value true
// is ever sent.
export function runtimeActionQuery(opts: { acknowledgeReplica?: boolean; force?: boolean }): string {
  const params = [opts.acknowledgeReplica ? 'acknowledge_replica=true' : '', opts.force ? 'force=true' : ''].filter(Boolean);
  return params.length > 0 ? `?${params.join('&')}` : '';
}

export interface RuntimeActionFailureInfo { message: string; code?: string }

// runtimeActionFailureInfo maps a non-2xx runtime action response to the
// message the dashboard shows. replica_member: the node became part of a
// multi-host replica after the dialog opened, so the operator is asked to review
// the impact in the reopened dialog (stop and restart reopen it; a drain never
// receives this code). sole_replica: the server's text names flags the
// dashboard does not have, and the dialog may or may not have listed models
// (the check can have failed), so the wording stays neutral.
export function runtimeActionFailureInfo(
  status: number,
  body: { error?: string; code?: string; sole_replica?: SoleReplicaBody },
  fallback: string,
): RuntimeActionFailureInfo {
  if (body.code === 'replica_member') {
    return { message: 'This node is now part of a multi-host replica. Review the impact below and confirm again.', code: body.code };
  }
  if (body.code === 'sole_replica') {
    return { message: 'This node is now the only server of one or more models. Review the list below and confirm again.', code: body.code };
  }
  return { message: body.error || `${fallback} (HTTP ${status})`, code: body.code };
}

// soleCheckErrorMessage is the text kept in the dialog state when the check
// request fails, so the operator can see why it did.
export function soleCheckErrorMessage(e: unknown): string {
  if (e instanceof Error) {
    if (e.name === 'AbortError') return 'Timed out checking other replicas';
    if (e.message) return e.message;
  }
  return 'Could not check other replicas';
}
