import type { ReplicaSuggestion } from '../lib/api';
import type { GPUNode } from '../types';
import { driftRows, keepDeclaredCopy, launcherLabel, reasonToShow, suggestionChip, workersOf } from '../lib/replicaGroups';
import type { ChipTone, StatusChip } from '../lib/replicaGroups';

const toneClass: Record<ChipTone, string> = {
  success: 'bg-success/10 text-success border-success/30',
  info: 'bg-info/15 text-info border-info/30',
  warn: 'bg-amber-500/10 text-amber-700 dark:text-amber-400 border-amber-500/30',
  danger: 'bg-destructive/10 text-destructive dark:text-red-400 border-destructive/30',
  muted: 'bg-secondary text-muted-foreground border-border',
};

export function ChipBadge({ chip }: { chip: StatusChip }) {
  return (
    <span className={`text-xs font-medium px-1.5 py-0.5 rounded border whitespace-nowrap shrink-0 ${toneClass[chip.tone]}`}>
      {chip.label}
    </span>
  );
}

const buttonBase = 'px-3 py-2 min-h-[40px] text-sm font-medium rounded-lg transition-colors disabled:opacity-50 disabled:cursor-not-allowed w-full sm:w-auto';
export const primaryButton = `${buttonBase} bg-primary hover:bg-primary/90 text-primary-foreground shadow-sm`;
export const quietButton = `${buttonBase} border border-border text-muted-foreground hover:text-foreground hover:bg-secondary`;

function titleFor(s: ReplicaSuggestion): string {
  const label = launcherLabel(s);
  switch (s.state) {
    case 'complete': return `Detected: ${label} group`;
    case 'contradicts_declared': return `Detected ${label} group differs from what is declared`;
    case 'incomplete': return `Possible ${label} group, not complete yet`;
    default: return `Possible ${label} group, details conflict`;
  }
}

function memberLine(s: ReplicaSuggestion) {
  const workers = workersOf(s);
  if (!s.head) return <>{s.members.join(', ') || '-'}</>;
  return <><span className="text-foreground font-medium">{s.head}</span> (head){workers.length > 0 ? `, ${workers.join(', ')}` : ''}</>;
}

interface ReplicaSuggestionCardProps {
  suggestion: ReplicaSuggestion;
  nodes: readonly GPUNode[];
  // True while any replica-group request is running: every action locks.
  disabled: boolean;
  onReview: (s: ReplicaSuggestion) => void;
  onAdopt: (s: ReplicaSuggestion) => void;
  onDismiss: (s: ReplicaSuggestion) => void;
  onEditNode: (name: string) => void;
}

export function ReplicaSuggestionCard({ suggestion: s, nodes, disabled, onReview, onAdopt, onDismiss, onEditNode }: ReplicaSuggestionCardProps) {
  const reason = reasonToShow(s);
  const dismissButton = (label: string) => (
    <button onClick={() => onDismiss(s)} disabled={disabled} className={quietButton}>{label}</button>
  );

  return (
    <div className="p-4 bg-card border border-border rounded-xl space-y-3">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <p className="text-sm font-semibold text-foreground break-words">{titleFor(s)}</p>
          <p className="text-sm text-muted-foreground mt-0.5 break-words">{memberLine(s)}</p>
        </div>
        <ChipBadge chip={suggestionChip(s)} />
      </div>

      {s.state === 'complete' && (
        <div className="flex flex-col sm:flex-row gap-2">
          <button onClick={() => onReview(s)} disabled={disabled} className={primaryButton}>Review and confirm</button>
          {dismissButton('Dismiss')}
        </div>
      )}

      {(s.state === 'incomplete' || s.state === 'conflicting') && (
        <>
          {reason && <p className="text-sm text-muted-foreground break-words">{reason}</p>}
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-3 text-sm">
            <div className="p-3 rounded-lg bg-secondary/50 min-w-0">
              <p className="text-xs font-medium text-muted-foreground uppercase tracking-wide">Seen so far</p>
              {s.evidence.length === 0 ? (
                <p className="mt-1 text-muted-foreground">-</p>
              ) : (
                <ul className="mt-1 space-y-1">
                  {s.evidence.map((e, i) => (
                    <li key={`${e.node}-${i}`} className="break-words"><span className="font-medium text-foreground">{e.node}</span>: {e.detail || '-'} ({e.source || '-'})</li>
                  ))}
                </ul>
              )}
            </div>
            <div className="p-3 rounded-lg bg-secondary/50 min-w-0">
              <p className="text-xs font-medium text-muted-foreground uppercase tracking-wide">Still needed</p>
              {s.missing.length === 0 ? (
                <p className="mt-1 text-muted-foreground">
                  {s.state === 'conflicting' ? 'Fix the conflict on the hosts above; this card updates on its own.' : '-'}
                </p>
              ) : (
                <ul className="mt-1 space-y-1">
                  {s.missing.map((m, i) => <li key={`${m}-${i}`} className="break-words">{m}</li>)}
                </ul>
              )}
            </div>
          </div>
          <div className="flex flex-col sm:flex-row gap-2">{dismissButton('Dismiss')}</div>
        </>
      )}

      {s.state === 'contradicts_declared' && (
        <>
          <p className="text-sm text-muted-foreground">Nothing is changed until you choose.</p>
          <div className="space-y-2 text-sm">
            {driftRows(s).map(row => (
              <div key={row.node} className="p-3 rounded-lg bg-secondary/50 min-w-0">
                <p className="font-medium text-foreground break-words">{row.node}</p>
                <p className="mt-1 text-muted-foreground break-words"><span className="text-xs uppercase tracking-wide">Declared now</span> {row.before}</p>
                <p className="mt-0.5 text-muted-foreground break-words"><span className="text-xs uppercase tracking-wide">Detected</span> {row.after}</p>
              </div>
            ))}
          </div>
          <div className="flex flex-col sm:flex-row gap-2">
            <button onClick={() => onAdopt(s)} disabled={disabled} className={primaryButton}>Adopt detected</button>
            {dismissButton('Keep declared')}
          </div>
          <p className="text-xs text-muted-foreground">{keepDeclaredCopy(s, nodes)}</p>
          <div className="flex flex-wrap gap-x-1">
            {s.declared.map(d => (
              <button key={d.node} onClick={() => onEditNode(d.node)} disabled={disabled} className="px-2 min-h-[40px] text-xs text-muted-foreground hover:text-foreground underline disabled:opacity-50 disabled:cursor-not-allowed">
                Edit {d.node}
              </button>
            ))}
          </div>
        </>
      )}
    </div>
  );
}
