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
// notice and the button both use it (the notice also shows it as the error
// text), so the button never references a missing element.
// endSentence adds a full stop unless the text already ends in sentence punctuation.
export function endSentence(text: string): string {
  const t = text.trim();
  return /[.!?]$/.test(t) ? t : `${t}.`;
}

export function impactSummary(s: SoleImpactState): string | null {
  switch (s.status) {
    case 'idle': return null;
    case 'loading': return 'Checking other replicas.';
    case 'error': return endSentence(s.message || 'Could not check other replicas');
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

// readSoleReplicaImpact turns the sole-replica endpoint's response into an
// impact. A failed status, or a body without a sole_replica object, throws
// instead of reading as "nothing to report": an unreadable answer is not a safe
// one.
export async function readSoleReplicaImpact(res: Response): Promise<SoleReplicaImpact> {
  if (!res.ok) throw new Error(`Failed to check other replicas (HTTP ${res.status})`);
  const j = (await res.json().catch(() => ({}))) as { sole_replica?: Partial<SoleReplicaImpact> };
  const r = j.sole_replica;
  if (!r || typeof r !== 'object') throw new Error('Unexpected response while checking other replicas');
  return { models: r.models ?? [], cold_load_only: r.cold_load_only ?? [], unknown: r.unknown ?? [] };
}

export type StoredImpact = { node: string | null } & SoleImpactState;

// A check that has not answered by then stops waiting, so the confirm button is
// never held by a hung request (the action is still checked by the server).
export const CHECK_TIMEOUT_MS = 10_000;

// createImpactChecker runs the check for one node at a time and reports each
// state through onState. A newer load or a cancel aborts the previous request
// and its result is discarded, so a slow answer for an earlier node can never
// overwrite a later one; a request still running after timeoutMs is aborted
// and reported as an error.
export function createImpactChecker(
  fetchImpact: (name: string, signal: AbortSignal) => Promise<SoleReplicaImpact>,
  onState: (s: StoredImpact) => void,
  timeoutMs: number = CHECK_TIMEOUT_MS,
) {
  let seq = 0;
  let abort: AbortController | null = null;
  return {
    async load(name: string): Promise<void> {
      const id = ++seq;
      abort?.abort();
      const controller = new AbortController();
      abort = controller;
      const timer = setTimeout(() => controller.abort(), timeoutMs);
      onState({ node: name, status: 'loading' });
      try {
        const impact = await fetchImpact(name, controller.signal);
        if (id === seq) onState({ node: name, status: 'ready', impact });
      } catch (e) {
        if (id === seq) onState({ node: name, status: 'error', message: soleCheckErrorMessage(e) });
      } finally {
        clearTimeout(timer);
      }
    },
    cancel(): void {
      seq++;
      abort?.abort();
      abort = null;
    },
  };
}
