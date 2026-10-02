import { useState } from 'react';
import type { ReactNode } from 'react';
import { ChevronRight, Layers } from 'lucide-react';
import { ReplicaSuggestionCard, ChipBadge, quietButton } from './ReplicaSuggestionCard';
import { ReplicaConfirmModal } from './ReplicaConfirmModal';
import type { ReplicaModalTarget } from './ReplicaConfirmModal';
import { confirmReplicaSuggestion, dismissReplicaSuggestion, restoreReplicaSuggestion } from '../lib/api';
import type { ReplicaSuggestion, ReplicaSuggestionsResponse } from '../lib/api';
import type { GPUNode } from '../types';
import {
  bucketSuggestions, confirmedChip, coverageLine, coverageSummary, deriveConfirmedGroups, detectedLaunchText,
  gpusText, launcherLabel, sameDeclared, workersOf,
} from '../lib/replicaGroups';
import type { ConfirmedGroup } from '../lib/replicaGroups';

interface ReplicaGroupsTabProps {
  data: ReplicaSuggestionsResponse | null;
  // True when the last suggestions refresh failed. With data, results are
  // stale; without data, there is nothing to show but the error.
  loadError: boolean;
  nodes: GPUNode[];
  demo: boolean;
  // Refetches nodes and suggestions after an action.
  onChanged: () => Promise<void>;
  // Opens the existing Edit Node modal for a node.
  onEditNode: (name: string) => void;
  // Scrolls to and highlights a node on the Nodes tab.
  onGoToNode: (name: string) => void;
}

interface Notice {
  title: string;
  lines: string[];
}

const sectionHeading = 'flex items-center gap-2 text-xs font-medium text-muted-foreground uppercase tracking-wide';
const subHeading = 'text-xs font-medium text-muted-foreground';

// modalStaleNote says why an open dialog no longer applies. Checked only while
// nothing is in flight, so a poll can never pull a dialog out from under a
// request that is running.
function modalStaleNote(modal: ReplicaModalTarget, data: ReplicaSuggestionsResponse | null): string | null {
  if (!data) return null;
  const live = data.suggestions.find(x => x.fingerprint === modal.suggestion.fingerprint);
  const wanted = modal.mode === 'adopt' ? 'contradicts_declared' : 'complete';
  if (live && live.state === wanted && (modal.mode !== 'adopt' || sameDeclared(live.declared, modal.suggestion.declared))) return null;
  if (!live) return 'This group is no longer a suggestion, so nothing was changed. The list has been refreshed.';
  if (live.state === 'complete') return 'This group is now complete and ready to confirm. Nothing was changed. Review it below.';
  return 'This group changed while you were reviewing it. Nothing was changed. Review it again below.';
}

function NodeLink({ name, onGoToNode }: { name: string; onGoToNode: (name: string) => void }) {
  return (
    <button onClick={() => onGoToNode(name)} className="text-foreground font-medium underline-offset-2 hover:underline min-h-[40px] sm:min-h-0 pr-2 sm:pr-0 text-left">
      {name}
    </button>
  );
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <p className="text-xs font-medium text-muted-foreground uppercase tracking-wide">{label}</p>
      <div className="mt-0.5 text-sm text-foreground break-words">{children}</div>
    </div>
  );
}

function ConfirmedCard({ group, onEditNode, onGoToNode }: { group: ConfirmedGroup; onEditNode: (name: string) => void; onGoToNode: (name: string) => void }) {
  const headNode = group.nodes.find(n => n.name === group.head);
  const workers = group.members.filter(m => m !== group.head);
  return (
    <div className="p-4 bg-card border border-border rounded-xl space-y-3">
      <div className="flex items-start justify-between gap-3">
        <p className="text-sm font-semibold text-foreground break-words min-w-0">
          {group.resolved ? `Replica group headed by ${group.head}` : 'Replica declarations do not agree'}
        </p>
        <ChipBadge chip={confirmedChip(group)} />
      </div>
      {group.resolved ? (
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
          <Field label="Head"><NodeLink name={group.head} onGoToNode={onGoToNode} /></Field>
          <Field label="Workers">
            {workers.length === 0 ? '-' : <span className="flex flex-wrap gap-x-3">{workers.map(w => <NodeLink key={w} name={w} onGoToNode={onGoToNode} />)}</span>}
          </Field>
          <Field label="Runtime">{headNode?.runtime || '-'}</Field>
          <Field label="GPUs">{gpusText(headNode)}</Field>
          <Field label="Detected launch">{detectedLaunchText(headNode)}</Field>
        </div>
      ) : (
        <>
          <p className="text-sm text-muted-foreground">
            These nodes declare different groups, so none of them can be scheduled until the declarations match: {group.members.join(', ')}.
          </p>
          <div className="flex flex-wrap gap-x-1">
            {group.nodes.map(n => (
              <button key={n.name} onClick={() => onEditNode(n.name)} className="px-2 min-h-[40px] text-sm text-foreground underline">Edit {n.name}</button>
            ))}
          </div>
        </>
      )}
    </div>
  );
}

export function ReplicaGroupsTab({ data, loadError, nodes, demo, onChanged, onEditNode, onGoToNode }: ReplicaGroupsTabProps) {
  const [modal, setModal] = useState<ReplicaModalTarget | null>(null);
  // One flag for every request here: dismiss, restore, confirm and adopt all
  // lock while any of them runs.
  const [inFlight, setInFlight] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<Notice | null>(null);
  const [showDismissed, setShowDismissed] = useState(false);
  const [showCoverage, setShowCoverage] = useState(false);

  if (!data) {
    if (loadError) {
      return (
        <div className="p-4 bg-destructive/10 border border-destructive/20 rounded-xl text-sm flex flex-col sm:flex-row sm:items-center justify-between gap-3">
          <span className="text-destructive font-medium">Could not load replica groups.</span>
          <button onClick={() => { void onChanged(); }} className={quietButton}>Retry</button>
        </div>
      );
    }
    return <div className="h-32 rounded-xl bg-secondary/50 animate-pulse" aria-label="Loading replica groups" />;
  }

  const buckets = bucketSuggestions(data.suggestions);
  const groups = deriveConfirmedGroups(nodes);
  const coverage = coverageSummary(data.coverage);
  const needsAttention = buckets.conflicts.length + buckets.ready.length + buckets.waiting.length;
  const staleNote = modal && !inFlight ? modalStaleNote(modal, data) : null;
  const openModal = modal && !staleNote ? modal : null;

  const run = async (action: () => Promise<void>) => {
    if (inFlight) return;
    setInFlight(true);
    setError(null);
    try {
      await action();
    } finally {
      setInFlight(false);
    }
  };

  const handleDismiss = (s: ReplicaSuggestion) => run(async () => {
    try {
      await dismissReplicaSuggestion(s.fingerprint, demo, s.state);
      await onChanged();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Could not dismiss the suggestion.');
    }
  });

  const handleRestore = (s: ReplicaSuggestion) => run(async () => {
    try {
      await restoreReplicaSuggestion(s.fingerprint, demo);
      await onChanged();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Could not restore the suggestion.');
    }
  });

  const handleSubmit = () => {
    if (!modal) return;
    const { mode, suggestion: s } = modal;
    void run(async () => {
      try {
        const result = await confirmReplicaSuggestion(s.fingerprint, demo, mode === 'adopt' ? { adopt: true, declaredSnapshot: s.declared } : undefined);
        setNotice({
          title: 'Replica group declared',
          lines: result.roles.map(r => (r.role === 'head' ? `${r.node} is now the head` : `${r.node} is now a worker of ${r.head}`)),
        });
        setModal(null);
        await onChanged();
      } catch (e: unknown) {
        setError(e instanceof Error ? e.message : 'Could not confirm the suggestion. Nothing was changed.');
        await onChanged().catch(() => undefined);
      }
    });
  };

  const openFor = (mode: ReplicaModalTarget['mode']) => (s: ReplicaSuggestion) => {
    setError(null);
    setModal({ mode, suggestion: s });
  };

  const editFromModal = (name: string) => {
    setModal(null);
    setError(null);
    onEditNode(name);
  };

  const cards = (list: ReplicaSuggestion[], label: string) => list.length > 0 && (
    <div className="space-y-3">
      <p className={subHeading}>{label}</p>
      {list.map(s => (
        <ReplicaSuggestionCard
          key={s.fingerprint}
          suggestion={s}
          nodes={nodes}
          disabled={inFlight}
          onReview={openFor('confirm')}
          onAdopt={openFor('adopt')}
          onDismiss={handleDismiss}
          onEditNode={onEditNode}
        />
      ))}
    </div>
  );

  return (
    <div className="space-y-6">
      {loadError && (
        <div className="p-3 bg-amber-500/10 border border-amber-500/30 rounded-xl text-sm text-amber-700 dark:text-amber-400">
          Could not refresh replica groups. Showing the last result.
        </div>
      )}

      {notice && (
        <div className="p-4 bg-success/10 border border-success/20 rounded-xl text-sm flex items-start justify-between gap-3">
          <div className="min-w-0">
            <p className="font-medium text-success">{notice.title}</p>
            <ul className="text-muted-foreground mt-1">
              {notice.lines.map(line => <li key={line} className="break-words">{line}</li>)}
            </ul>
          </div>
          <button onClick={() => setNotice(null)} className="text-xs text-muted-foreground hover:text-foreground shrink-0 min-h-[40px] px-2">Close</button>
        </div>
      )}

      {staleNote && (
        <div className="p-4 bg-amber-500/10 border border-amber-500/30 rounded-xl text-sm flex items-start justify-between gap-3">
          <p className="text-amber-700 dark:text-amber-400 break-words min-w-0">{staleNote}</p>
          <button onClick={() => setModal(null)} className="text-xs text-muted-foreground hover:text-foreground shrink-0 min-h-[40px] px-2">Close</button>
        </div>
      )}

      {error && !openModal && (
        <div className="p-4 bg-destructive/10 border border-destructive/20 rounded-xl text-destructive text-sm font-medium">{error}</div>
      )}

      <section className="space-y-3" aria-label="Needs attention">
        <div className={sectionHeading}>
          <Layers className="w-4 h-4" />
          Needs attention
        </div>
        {needsAttention === 0 && (
          <p className="text-sm text-muted-foreground">Nothing to decide.</p>
        )}
        {cards(buckets.conflicts, 'Differs from what is declared')}
        {cards(buckets.ready, 'Ready to confirm')}
        {cards(buckets.waiting, 'Waiting for more details')}
      </section>

      {groups.length > 0 && (
        <section className="space-y-3" aria-label="Confirmed replica groups">
          <div className={sectionHeading}>Confirmed replica groups</div>
          {groups.map(g => <ConfirmedCard key={g.key} group={g} onEditNode={onEditNode} onGoToNode={onGoToNode} />)}
        </section>
      )}

      {buckets.dismissed.length > 0 && (
        <div className="space-y-2">
          <button onClick={() => setShowDismissed(v => !v)} aria-expanded={showDismissed} className="text-xs text-muted-foreground hover:text-foreground min-h-[40px] flex items-center gap-1">
            <ChevronRight className={`w-3.5 h-3.5 transition-transform ${showDismissed ? 'rotate-90' : ''}`} />
            {buckets.dismissed.length} dismissed - {showDismissed ? 'Hide' : 'Show'}
          </button>
          {showDismissed && buckets.dismissed.map(s => (
            <div key={s.fingerprint} className="p-3 border border-border rounded-xl flex flex-col sm:flex-row sm:items-center justify-between gap-2">
              <p className="text-sm text-muted-foreground break-words min-w-0">
                {launcherLabel(s)} group: {s.head || s.members.join(', ') || '-'}{s.head && workersOf(s).length > 0 ? `, ${workersOf(s).join(', ')}` : ''}
              </p>
              <button onClick={() => handleRestore(s)} disabled={inFlight} className={quietButton}>Restore</button>
            </div>
          ))}
        </div>
      )}

      {coverage.listed.length > 0 && (
        <div>
          <button onClick={() => setShowCoverage(v => !v)} aria-expanded={showCoverage} className="text-xs text-muted-foreground hover:text-foreground min-h-[40px] flex items-center gap-1 text-left">
            <ChevronRight className={`w-3.5 h-3.5 shrink-0 transition-transform ${showCoverage ? 'rotate-90' : ''}`} />
            {coverage.limitedCount > 0
              ? `Detection coverage: ${coverage.limitedCount} ${coverage.limitedCount === 1 ? 'node' : 'nodes'} limited`
              : 'Detection coverage'}
            {' '}- {showCoverage ? 'Hide' : 'Show'}
          </button>
          {showCoverage && (
            <ul className="text-xs text-muted-foreground space-y-1 pl-5">
              {coverage.listed.map(c => <li key={c.node} className="break-words">{coverageLine(c)}</li>)}
            </ul>
          )}
        </div>
      )}

      <ReplicaConfirmModal
        target={openModal}
        nodes={nodes}
        busy={inFlight}
        error={error}
        onClose={() => { setModal(null); setError(null); }}
        onSubmit={handleSubmit}
        onEditNode={editFromModal}
      />
    </div>
  );
}
