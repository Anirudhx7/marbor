import {
  COLD_COLLAPSE_AFTER, coldLoadSummary, formatModel, unknownLine, type SoleImpactState,
} from '../lib/soleReplica';

// SoleReplicaImpactNotice shows, inside the Drain and Restart dialogs, what the
// action does to model availability: models only this node can serve (amber,
// the action needs an explicit "anyway"), models an Ollama peer would have to
// cold-load (neutral note), and places replica data could not be verified. The
// confirm button points at it with aria-describedby (the caller passes a useId
// value). Lists scroll inside a capped height so the dialog stays usable at
// 375px; break-words keeps long model names inside it.
export function SoleReplicaImpactNotice({ id, state }: { id: string; state: SoleImpactState }) {
  if (state.status === 'idle') return null;
  if (state.status === 'loading') {
    return <p id={id} role="status" className="text-xs text-muted-foreground">Checking other replicas...</p>;
  }
  if (state.status === 'error') {
    return (
      <p id={id} role="note" className="text-xs text-muted-foreground rounded-lg border border-border bg-secondary/40 p-3">
        Could not check other replicas. You can still continue; marbor checks again when you confirm.
      </p>
    );
  }
  const { models, cold_load_only: cold, unknown } = state.impact;
  if (models.length === 0 && cold.length === 0 && unknown.length === 0) return null;
  const listClass = 'mt-1 max-h-28 overflow-y-auto space-y-0.5 font-mono';
  return (
    <div id={id} role="note" className="space-y-2 text-xs break-words">
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
