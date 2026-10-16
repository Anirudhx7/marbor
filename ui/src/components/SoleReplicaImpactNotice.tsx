import {
  COLD_COLLAPSE_AFTER, coldLoadSummary, formatModel, impactSummary, unknownLine, type SoleImpactState,
} from '../lib/soleReplica';

// SoleReplicaImpactNotice shows, inside the Drain and Restart dialogs, what the
// action does to model availability: models only this node can serve (amber,
// the action needs an explicit "anyway"), models an Ollama peer would have to
// cold-load (neutral note), and places replica data could not be verified. The
// outer role="status" region is always mounted, so a result that arrives after
// the dialog opens is announced. The confirm button points at the short summary
// (aria-describedby, via the caller's useId value), not the whole list. Lists
// scroll inside a capped height so the dialog stays usable at 375px;
// break-words keeps long model names inside it.
export function SoleReplicaImpactNotice({ id, state }: { id: string; state: SoleImpactState }) {
  return (
    <div role="status" aria-live="polite" className="empty:hidden">
      <NoticeBody id={id} state={state} />
    </div>
  );
}

function NoticeBody({ id, state }: { id: string; state: SoleImpactState }) {
  const summary = impactSummary(state);
  if (summary === null || state.status === 'idle') return null;
  if (state.status === 'loading') {
    return <p className="text-xs text-muted-foreground"><span id={id}>Checking other replicas...</span></p>;
  }
  if (state.status === 'error') {
    return (
      <p className="text-xs text-muted-foreground rounded-lg border border-border bg-secondary/40 p-3">
        <span id={id}>{state.message ?? 'Could not check other replicas'}.</span> You can still continue; marbor checks again when you confirm.
      </p>
    );
  }
  const { models, cold_load_only: cold, unknown } = state.impact;
  const listClass = 'mt-1 max-h-28 overflow-y-auto space-y-0.5 font-mono';
  return (
    <div role="note" className="space-y-2 text-xs break-words">
      <span id={id} className="sr-only">{summary}</span>
      {models.length > 0 && (
        <div className="rounded-lg border border-amber-500/40 bg-amber-500/10 text-foreground p-3">
          <p className="font-semibold">Only this node can serve:</p>
          <ul className={listClass}>
            {models.map(m => <li key={`${m.runtime}/${m.model}`}>{formatModel(m)}</li>)}
          </ul>
          <p className="mt-1">No other node can serve these.</p>
        </div>
      )}
      {cold.length > 0 && (
        <div className="rounded-lg border border-border bg-secondary/40 text-muted-foreground p-3">
          {cold.length > COLD_COLLAPSE_AFTER ? (
            <details>
              <summary className="cursor-pointer">{coldLoadSummary(cold.length)} (not warm there and may not be installed; fit not checked)</summary>
              <ul className={listClass}>
                {cold.map(c => <li key={`${c.runtime}/${c.model}`}>{c.model} on {c.peers.join(', ')}</li>)}
              </ul>
            </details>
          ) : (
            <>
              <p>{coldLoadSummary(cold.length)} (not warm there and may not be installed; fit not checked):</p>
              <ul className={listClass}>
                {cold.map(c => <li key={`${c.runtime}/${c.model}`}>{c.model} on {c.peers.join(', ')}</li>)}
              </ul>
            </>
          )}
        </div>
      )}
      {unknown.length > 0 && (
        <div className="rounded-lg border border-border bg-secondary/40 text-muted-foreground p-3">
          <p>Could not verify other replicas:</p>
          <ul className={listClass}>
            {unknown.map((u, i) => <li key={`${u.node}/${u.model ?? ''}/${i}`}>{unknownLine(u)}</li>)}
          </ul>
        </div>
      )}
      <p className="text-muted-foreground">This check covers drains and restarts done through marbor only.</p>
    </div>
  );
}
