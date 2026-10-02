import { useState } from 'react';
import { Layers, ChevronRight } from 'lucide-react';
import { Modal } from './Modal';
import { confirmReplicaSuggestion, dismissReplicaSuggestion, restoreReplicaSuggestion } from '../lib/api';
import type { ReplicaSuggestion, ReplicaSuggestionsResponse, TopologyCoverage } from '../lib/api';

interface ReplicaSuggestionStripProps {
  data: ReplicaSuggestionsResponse | null;
  demo: boolean;
  // Called after a confirm, dismiss or restore so the page refetches.
  onChanged: () => Promise<void>;
  // Opens the existing Edit panel for a node.
  onEditNode: (name: string) => void;
}

function launcherLabel(s: ReplicaSuggestion): string {
  if (s.launcher === 'vllm-mp') return 'vLLM multi-host';
  if (s.launcher === 'llamacpp-rpc') return 'llama.cpp RPC';
  return 'multi-host';
}

function workersOf(s: ReplicaSuggestion): string[] {
  return s.members.filter(m => m !== s.head);
}

function coverageLine(c: TopologyCoverage): string {
  switch (c.state) {
    case 'agent_update_needed':
      return `${c.node}: agent update needed to detect multi-host launches`;
    case 'no_agent':
      return `${c.node}: no agent enabled, so multi-host launches cannot be seen`;
    case 'env_unreadable':
      return `${c.node}: the agent cannot read this runtime's environment, so some launch details are unknown`;
    default:
      return `${c.node}: -`;
  }
}

const buttonBase = 'px-3 py-2 min-h-[40px] text-sm font-medium rounded-lg transition-colors disabled:opacity-50 disabled:cursor-not-allowed w-full sm:w-auto';
const primaryButton = `${buttonBase} bg-primary hover:bg-primary/90 text-primary-foreground shadow-sm`;
const quietButton = `${buttonBase} border border-border text-muted-foreground hover:text-foreground hover:bg-secondary`;

export function ReplicaSuggestionStrip({ data, demo, onChanged, onEditNode }: ReplicaSuggestionStripProps) {
  const [target, setTarget] = useState<ReplicaSuggestion | null>(null);
  const [busy, setBusy] = useState(false);
  const [rowBusy, setRowBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string[] | null>(null);
  const [showDismissed, setShowDismissed] = useState(false);
  const [showCoverage, setShowCoverage] = useState(false);

  const all = data?.suggestions ?? [];
  const visible = all.filter(s => !s.dismissed);
  const dismissed = all.filter(s => s.dismissed);
  const gaps = (data?.coverage ?? []).filter(c => c.state !== 'reporting');

  if (visible.length === 0 && dismissed.length === 0 && gaps.length === 0 && !notice) return null;

  const closeConfirm = () => {
    if (busy) return; // a slow confirm must not be dismissed and re-submitted
    setTarget(null);
    setError(null);
  };

  const handleConfirm = async () => {
    if (!target || busy) return;
    setBusy(true);
    setError(null);
    try {
      const result = await confirmReplicaSuggestion(target.fingerprint, demo);
      setNotice(result.roles.map(r => (r.role === 'head' ? `${r.node} is now the head` : `${r.node} is now a worker of ${r.head}`)));
      setTarget(null);
      await onChanged();
    } catch (e: any) {
      setError(e?.message || 'Could not confirm the suggestion. Nothing was changed.');
      await onChanged().catch(() => undefined);
    } finally {
      setBusy(false);
    }
  };

  const handleToggleDismiss = async (s: ReplicaSuggestion, dismiss: boolean) => {
    if (rowBusy) return;
    setRowBusy(s.fingerprint);
    setError(null);
    try {
      if (dismiss) await dismissReplicaSuggestion(s.fingerprint, demo);
      else await restoreReplicaSuggestion(s.fingerprint, demo);
      await onChanged();
    } catch (e: any) {
      setError(e?.message || (dismiss ? 'Could not dismiss the suggestion.' : 'Could not restore the suggestion.'));
    } finally {
      setRowBusy(null);
    }
  };

  const dismissButton = (s: ReplicaSuggestion) => (
    <button onClick={() => handleToggleDismiss(s, true)} disabled={rowBusy !== null} className={quietButton}>
      Dismiss
    </button>
  );

  const renderCard = (s: ReplicaSuggestion) => {
    const label = launcherLabel(s);
    const workers = workersOf(s);
    return (
      <div key={s.fingerprint} className="p-4 bg-card border border-border rounded-xl space-y-3">
        {s.state === 'complete' && (
          <>
            <div className="min-w-0">
              <p className="text-sm font-semibold text-foreground">Detected: {label} group</p>
              <p className="text-sm text-muted-foreground mt-0.5 break-words">
                <span className="text-foreground font-medium">{s.head}</span> (head), {workers.join(', ')}
              </p>
            </div>
            <div className="flex flex-col sm:flex-row gap-2">
              <button onClick={() => { setError(null); setTarget(s); }} className={primaryButton}>Review and confirm</button>
              {dismissButton(s)}
            </div>
          </>
        )}
        {(s.state === 'incomplete' || s.state === 'conflicting') && (
          <>
            <div className="min-w-0">
              <p className="text-sm font-semibold text-foreground">
                {s.state === 'incomplete' ? `Possible ${label} group, not complete yet` : `Possible ${label} group, details conflict`}
              </p>
              <p className="text-sm text-muted-foreground mt-0.5 break-words">{s.reason || '-'}</p>
            </div>
            <div className="flex flex-col sm:flex-row gap-2">{dismissButton(s)}</div>
          </>
        )}
        {s.state === 'contradicts_declared' && (
          <>
            <div className="min-w-0">
              <p className="text-sm font-semibold text-foreground">Detected {label} group differs from what is declared</p>
              <p className="text-sm text-muted-foreground mt-0.5">Nothing is changed. Fix the declaration, then the group can be confirmed.</p>
            </div>
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-3 text-sm">
              <div className="p-3 rounded-lg bg-secondary/50 min-w-0">
                <p className="text-xs font-medium text-muted-foreground uppercase tracking-wide">Detected</p>
                <p className="mt-1 break-words"><span className="font-medium text-foreground">{s.head || '-'}</span> (head), {workersOf(s).join(', ') || '-'}</p>
              </div>
              <div className="p-3 rounded-lg bg-secondary/50 min-w-0">
                <p className="text-xs font-medium text-muted-foreground uppercase tracking-wide">Declared</p>
                <ul className="mt-1 space-y-1">
                  {s.declared.map(d => (
                    <li key={d.node} className="break-words">
                      <span className="font-medium text-foreground">{d.node}</span>: head {d.head || '-'}, members {d.members.join(', ') || '-'}
                    </li>
                  ))}
                </ul>
              </div>
            </div>
            <div className="flex flex-col sm:flex-row gap-2">
              {s.declared.map(d => (
                <button key={d.node} onClick={() => onEditNode(d.node)} className={quietButton}>Edit {d.node}</button>
              ))}
              {dismissButton(s)}
            </div>
          </>
        )}
      </div>
    );
  };

  return (
    <div className="space-y-3">
      {notice && (
        <div className="p-4 bg-success/10 border border-success/20 rounded-xl text-sm flex items-start justify-between gap-3">
          <div className="min-w-0">
            <p className="font-medium text-success">Replica group declared</p>
            <ul className="text-muted-foreground mt-1">
              {notice.map(line => <li key={line} className="break-words">{line}</li>)}
            </ul>
          </div>
          <button onClick={() => setNotice(null)} className="text-xs text-muted-foreground hover:text-foreground shrink-0 min-h-[40px] px-2">Close</button>
        </div>
      )}

      {error && !target && (
        <div className="p-4 bg-destructive/10 border border-destructive/20 rounded-xl text-destructive text-sm font-medium">{error}</div>
      )}

      {visible.length > 0 && (
        <div className="space-y-3">
          <div className="flex items-center gap-2 text-xs font-medium text-muted-foreground uppercase tracking-wide">
            <Layers className="w-4 h-4" />
            Multi-host groups detected by agents
          </div>
          {visible.map(renderCard)}
        </div>
      )}

      {dismissed.length > 0 && (
        <div className="space-y-2">
          <button onClick={() => setShowDismissed(v => !v)} className="text-xs text-muted-foreground hover:text-foreground min-h-[40px] flex items-center gap-1">
            <ChevronRight className={`w-3.5 h-3.5 transition-transform ${showDismissed ? 'rotate-90' : ''}`} />
            {dismissed.length} dismissed - {showDismissed ? 'Hide' : 'Show'}
          </button>
          {showDismissed && dismissed.map(s => (
            <div key={s.fingerprint} className="p-3 border border-border rounded-xl flex flex-col sm:flex-row sm:items-center justify-between gap-2">
              <p className="text-sm text-muted-foreground break-words min-w-0">
                {launcherLabel(s)} group: {s.head || '-'}{workersOf(s).length > 0 ? `, ${workersOf(s).join(', ')}` : ''}
              </p>
              <button onClick={() => handleToggleDismiss(s, false)} disabled={rowBusy !== null} className={quietButton}>Restore</button>
            </div>
          ))}
        </div>
      )}

      {gaps.length > 0 && (
        <div>
          <button onClick={() => setShowCoverage(v => !v)} className="text-xs text-muted-foreground hover:text-foreground min-h-[40px] flex items-center gap-1 text-left">
            <ChevronRight className={`w-3.5 h-3.5 shrink-0 transition-transform ${showCoverage ? 'rotate-90' : ''}`} />
            Multi-host detection cannot see {gaps.length} {gaps.length === 1 ? 'node' : 'nodes'} - {showCoverage ? 'Hide' : 'Show'}
          </button>
          {showCoverage && (
            <ul className="text-xs text-muted-foreground space-y-1 pl-5">
              {gaps.map(c => <li key={c.node} className="break-words">{coverageLine(c)}</li>)}
            </ul>
          )}
        </div>
      )}

      <Modal isOpen={target !== null} onClose={closeConfirm} title="Declare this replica group?" maxWidth="md">
        {target && (
          <div className="space-y-4">
            <p className="text-sm text-muted-foreground">
              This declares one {launcherLabel(target)} group: <span className="text-foreground font-semibold">{target.head}</span> as the head, with{' '}
              <span className="text-foreground font-semibold">{workersOf(target).join(', ')}</span> as {workersOf(target).length === 1 ? 'its worker' : 'its workers'}.
            </p>
            <p className="text-xs text-muted-foreground">
              {workersOf(target).join(', ')} will stop receiving requests directly. The head {target.head} serves the whole group. Nothing is restarted and in-flight requests are unaffected.
            </p>
            <p className="text-xs text-muted-foreground">
              This is reversible: clear the replica membership in each node's Edit panel, or run <code className="text-foreground">marbor nodes patch NODE --replica-members ""</code>.
            </p>
            {target.evidence.length > 0 && (
              <div className="text-xs text-muted-foreground">
                <p className="font-medium text-foreground mb-1">What the agents reported</p>
                <ul className="space-y-1">
                  {target.evidence.map((e, i) => (
                    <li key={`${e.node}-${i}`} className="break-words"><span className="text-foreground">{e.node}</span>: {e.detail || '-'} ({e.source || '-'})</li>
                  ))}
                </ul>
              </div>
            )}
            {error && <p className="text-sm text-destructive">{error}</p>}
            <div className="flex flex-col-reverse sm:flex-row sm:justify-end gap-3 pt-4 border-t border-border">
              <button onClick={closeConfirm} disabled={busy} className="px-4 py-2 min-h-[40px] text-sm font-medium text-muted-foreground hover:text-foreground transition-colors disabled:opacity-50 disabled:cursor-not-allowed">
                Cancel
              </button>
              <button onClick={handleConfirm} disabled={busy} className="px-4 py-2 min-h-[40px] bg-primary hover:bg-primary/90 disabled:opacity-50 disabled:cursor-not-allowed text-primary-foreground font-medium rounded-lg text-sm transition-colors shadow-sm">
                {busy ? 'Declaring...' : 'Declare group'}
              </button>
            </div>
          </div>
        )}
      </Modal>
    </div>
  );
}
