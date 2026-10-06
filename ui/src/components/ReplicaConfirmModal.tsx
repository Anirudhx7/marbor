import { useState } from 'react';
import { Modal } from './Modal';
import type { ReplicaSuggestion } from '../lib/api';
import type { GPUNode } from '../types';
import { driftRows, launcherLabel, outsideDeclarers, roleChangeLine, workersOf } from '../lib/replicaGroups';

export type ReplicaModalMode = 'confirm' | 'adopt';

export interface ReplicaModalTarget {
  mode: ReplicaModalMode;
  // The suggestion exactly as the operator saw it when the dialog opened.
  suggestion: ReplicaSuggestion;
}

interface ReplicaConfirmModalProps {
  target: ReplicaModalTarget | null;
  nodes: readonly GPUNode[];
  busy: boolean;
  error: string | null;
  onClose: () => void;
  onSubmit: () => void;
  onEditNode: (name: string) => void;
}

const cancelClass = 'px-4 py-2 min-h-[40px] text-sm font-medium text-muted-foreground hover:text-foreground transition-colors disabled:opacity-50 disabled:cursor-not-allowed';
const submitClass = 'px-4 py-2 min-h-[40px] bg-primary hover:bg-primary/90 disabled:opacity-50 disabled:cursor-not-allowed text-primary-foreground font-medium rounded-lg text-sm transition-colors shadow-sm';

function ConfirmBody({ s }: { s: ReplicaSuggestion }) {
  const workers = workersOf(s);
  return (
    <>
      <p className="text-sm text-muted-foreground">
        This declares one {launcherLabel(s)} group: <span className="text-foreground font-semibold">{s.head}</span> as the head, with{' '}
        <span className="text-foreground font-semibold">{workers.join(', ')}</span> as {workers.length === 1 ? 'its worker' : 'its workers'}.
      </p>
      <p className="text-xs text-muted-foreground">
        {workers.join(', ')} will stop receiving requests directly. The head {s.head} serves the whole group. Nothing is restarted and in-flight requests are unaffected.
      </p>
      <p className="text-xs text-muted-foreground">
        This is reversible: clear the replica membership in each node&apos;s Edit panel, or run <code className="text-foreground">marbor nodes patch NODE --replica-members ""</code>.
      </p>
      {s.evidence.length > 0 && (
        <div className="text-xs text-muted-foreground">
          <p className="font-medium text-foreground mb-1">What the agents reported</p>
          <ul className="space-y-1">
            {s.evidence.map((e, i) => (
              <li key={`${e.node}-${i}`} className="break-words"><span className="text-foreground">{e.node}</span>: {e.detail || '-'} ({e.source || '-'})</li>
            ))}
          </ul>
        </div>
      )}
    </>
  );
}

function AdoptBody({ s }: { s: ReplicaSuggestion }) {
  const rows = driftRows(s);
  return (
    <>
      <p className="text-sm text-muted-foreground">
        This replaces what these nodes declare today with the group the agents detected: <span className="text-foreground font-semibold">{s.head}</span> as
        the head of <span className="text-foreground font-semibold">{s.members.join(', ')}</span>.
      </p>
      <div className="space-y-2 text-sm">
        {rows.map(row => (
          <div key={row.node} className="p-3 rounded-lg bg-secondary/50 min-w-0">
            <p className="font-medium text-foreground break-words">{row.node}</p>
            <p className="mt-1 text-muted-foreground break-words"><span className="text-xs uppercase tracking-wide">Before</span> {row.before}</p>
            <p className="mt-0.5 text-muted-foreground break-words"><span className="text-xs uppercase tracking-wide">After</span> {row.after}</p>
          </div>
        ))}
      </div>
      <ul className="text-xs text-muted-foreground space-y-1">
        {rows.map(row => {
          const line = roleChangeLine(row, s.head);
          return line ? <li key={row.node} className="break-words">{line}</li> : null;
        })}
      </ul>
      <p className="text-xs text-muted-foreground">Nothing is restarted and in-flight requests are unaffected.</p>
      <p className="text-xs text-muted-foreground">
        This is reversible: re-enter the previous values shown above in each node&apos;s Edit panel, or run <code className="text-foreground">marbor nodes patch NODE --replica-members ...</code> for each node.
      </p>
    </>
  );
}

export function ReplicaConfirmModal({ target: liveTarget, nodes, busy, error, onClose, onSubmit, onEditNode }: ReplicaConfirmModalProps) {
  // Keep showing the last target while the modal animates out, so the title and
  // body do not blank for a frame on close.
  const [lastTarget, setLastTarget] = useState(liveTarget);
  if (liveTarget && liveTarget !== lastTarget) setLastTarget(liveTarget);
  const target = liveTarget ?? lastTarget;
  const adopt = target?.mode === 'adopt';
  const title = !target
    ? ''
    : adopt
      ? `Overwrite declarations on ${driftRows(target.suggestion).length} nodes?`
      : 'Declare this replica group?';
  const closeIfIdle = () => { if (!busy) onClose(); }; // a slow request must not be dismissed and re-submitted
  const outside = target && adopt && error ? outsideDeclarers(target.suggestion, nodes, error) : [];

  return (
    <Modal isOpen={liveTarget !== null} onClose={closeIfIdle} title={title} maxWidth={adopt ? 'lg' : 'md'}>
      {target && (
        <div className="space-y-4">
          {adopt ? <AdoptBody s={target.suggestion} /> : <ConfirmBody s={target.suggestion} />}
          {error && <p role="alert" className="text-sm text-destructive break-words">{error}</p>}
          {outside.length > 0 && (
            <div className="text-sm">
              <p className="text-muted-foreground">Fix these nodes first, then try again:</p>
              <div className="flex flex-wrap gap-x-1">
                {outside.map(name => (
                  <button key={name} onClick={() => onEditNode(name)} disabled={busy} className="px-2 min-h-[40px] text-sm text-foreground underline disabled:opacity-50 disabled:cursor-not-allowed">
                    Edit {name}
                  </button>
                ))}
              </div>
            </div>
          )}
          <div className="flex flex-col-reverse sm:flex-row sm:justify-end gap-3 pt-4 border-t border-border">
            <button onClick={closeIfIdle} disabled={busy} className={cancelClass}>Cancel</button>
            <button onClick={onSubmit} disabled={busy} className={submitClass}>
              {busy ? (adopt ? 'Overwriting...' : 'Declaring...') : (adopt ? 'Overwrite and declare group' : 'Declare group')}
            </button>
          </div>
        </div>
      )}
    </Modal>
  );
}
