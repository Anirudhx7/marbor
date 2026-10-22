import type { ReactNode } from 'react';
import type { RoutingDecision } from '../types';

// One entry per table column: the header, its sizing and its alignment all
// come from here, and the full-width rows use COLUMNS.length, so the column
// count cannot drift between <th> and those rows.
//
// Sizing is content-driven, not a fixed share of the card. "fit" columns
// (short, predictable values) shrink to their content and never wrap, with a
// min-width so a slightly longer value on a later poll does not make the
// table jump. "flex" columns (long, variable values: key, model, node) split
// whatever width is left, weighted by the percentage, and scroll inside their
// cell when a value is longer than the space (their cells carry max-w-0). So
// a wide card gives the spare room to the columns that need it, and a narrow
// one squeezes only those, instead of every column keeping a fixed slice.
const FIT = 'w-px whitespace-nowrap';
export const COLUMNS: { label: string; size: string; align: 'left' | 'right' }[] = [
  { label: 'Time', size: `${FIT} min-w-[88px]`, align: 'left' },
  { label: 'Request ID', size: 'w-[16%]', align: 'left' },
  { label: 'Key', size: 'w-[18%]', align: 'left' },
  { label: 'Model', size: 'w-[24%]', align: 'left' },
  { label: 'Node', size: 'w-[18%]', align: 'left' },
  { label: 'Status', size: `${FIT} min-w-[80px]`, align: 'left' },
  { label: 'Latency', size: `${FIT} min-w-[90px]`, align: 'right' },
  { label: 'Prefill', size: `${FIT} min-w-[84px]`, align: 'right' },
  { label: 'Why', size: `${FIT} min-w-[156px]`, align: 'left' },
];

const REASON_LABELS: Record<string, string> = {
  session_affinity: 'Session affinity',
  pinned_warm: 'Pinned + warm',
  score_based: 'Score-based',
};

// ScrollableValue backs every cell that can legitimately hold a value longer
// than its column (request IDs, key names, model paths). Instead of
// truncating with an ellipsis (which hides the rest of the value with no way
// to read it short of a hover tooltip - useless on touch, and this app's
// mobile dashboard is a real, actively-used surface) the value scrolls in
// place: the cell's width stays fixed, but dragging/swiping inside it reveals
// the rest of the text, like a horizontally-scrollable code line. `title`
// stays as a hover bonus for desktop mouse users. .no-scrollbar (index.css)
// keeps the row height uniform regardless of whether a given cell's content
// happens to overflow.
//
// The outer div is a plain, unstyled scroll viewport - it's deliberately kept
// free of background/padding classes. valueClassName (badge background,
// padding, rounded corners, color) goes on the inner inline-block span
// instead, which sizes to its own text rather than stretching to fill the
// cell: a `bg-primary/10` pill on the outer (full-cell-width) div would
// otherwise paint the entire column, not just the text it's meant to badge.
export function ScrollableValue({ value, valueClassName }: { value: string; valueClassName: string }) {
  return (
    <div className="overflow-x-auto whitespace-nowrap no-scrollbar" title={value}>
      <span className={`inline-block ${valueClassName}`}>{value}</span>
    </div>
  );
}

// EXCLUDED_REASON_LABELS translates ExcludedCandidate.reason (a stable,
// machine-readable identifier from the Admin API) into display text - kept
// as a local mapping rather than trusting the wire value to already be a
// sentence. The nine identifiers (unhealthy, draining, runtime_mismatch,
// ineligible_model, over_capacity, insufficient_gpu_group, replica_worker,
// replica_unresolved, replica_member_unreachable) mirror the ExcludeReason
// constants in internal/router/explain.go.
const EXCLUDED_REASON_LABELS: Record<string, string> = {
  unhealthy: 'node is unhealthy',
  draining: 'node is draining',
  runtime_mismatch: 'runtime does not match the request',
  ineligible_model: 'model not loaded on this node',
  over_capacity: 'over the per-node request cap',
  insufficient_gpu_group: "insufficient GPUs for this model's parallelism requirement",
  replica_worker: 'non-head member of a multi-host replica; requests go to its head',
  replica_unresolved: 'replica declaration conflicts with another node and needs reconciling',
  replica_member_unreachable: "a replica member's host agent is not answering (the worker itself may still be up)",
};

// ExplainTable is the shared table shell for the explain panel's score
// breakdown and excluded-candidates blocks - same scroll-region wrapper,
// header styling, and cell padding, so a future visual tweak to one applies
// to both instead of drifting between two hand-copied markup blocks.
function ExplainTable({
  ariaLabel,
  columns,
  children,
}: {
  ariaLabel: string;
  columns: { label: string; align?: 'right' }[];
  children: ReactNode;
}) {
  return (
    <div className="overflow-x-auto scroll-region" tabIndex={0} role="region" aria-label={ariaLabel}>
      <table className="text-xs font-mono">
        <thead>
          <tr className="text-muted-foreground">
            {columns.map((col, i) => (
              <th
                key={col.label}
                className={`${col.align === 'right' ? 'text-right' : 'text-left'} ${i < columns.length - 1 ? 'pr-4' : ''} py-1`}
              >
                {col.label}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>{children}</tbody>
      </table>
    </div>
  );
}

export function ReasonBadge({ reason }: { reason?: string }) {
  if (!reason) return <span className="text-muted-foreground/40 text-xs">-</span>;
  return (
    <span className="inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium border bg-secondary text-foreground/80 border-border">
      {REASON_LABELS[reason] ?? reason}
    </span>
  );
}

// ExplainPanel renders the routing-decision breakdown for one request,
// lazily fetched (or looked up from static mock data in demo mode) on first
// expand. State is one of: undefined (not yet fetched), 'loading', 'error',
// or the resolved RoutingDecision.
export function ExplainPanel({ state }: { state: RoutingDecision | 'loading' | 'error' | undefined }) {
  if (state === 'loading' || state === undefined) {
    return <div className="text-sm text-muted-foreground">Loading routing explanation...</div>;
  }
  if (state === 'error') {
    return <div className="text-sm text-muted-foreground">No routing decision recorded for this request.</div>;
  }
  return (
    <div className="space-y-2 text-sm">
      {/* Decision strip - landing-page trace language: where it landed and why */}
      <div className="flex flex-wrap items-center gap-2">
        <span className="sr-only">Decision: </span>
        <span className="font-mono text-xs text-muted-foreground" aria-hidden="true">Decision →</span>
        <span className="font-mono text-xs font-semibold text-foreground">{state.node}</span>
        <ReasonBadge reason={state.reason} />
        {typeof state.score === 'number' && (
          <span className="font-mono text-xs text-muted-foreground tabular-nums">
            score {state.score.toFixed(2)}
          </span>
        )}
        {state.affinityLost && (
          <span className="text-xs font-medium px-1.5 py-0.5 rounded-full bg-amber-500/15 text-amber-600 dark:text-amber-400 border border-amber-500/30">
            affinity lost
          </span>
        )}
      </div>
      {state.detail && <div className="text-muted-foreground text-xs">{state.detail}</div>}
      {state.components && state.components.length > 0 && (
        <ExplainTable
          ariaLabel="Score components table: scroll horizontally for more columns"
          columns={[
            { label: 'Component' },
            { label: 'Raw', align: 'right' },
            { label: 'Weight', align: 'right' },
            { label: 'Value', align: 'right' },
            { label: 'Phase' },
          ]}
        >
          {state.components.map((c) => (
            <tr key={c.name}>
              <td className="pr-4 py-0.5">{c.name}</td>
              <td className="text-right pr-4 py-0.5">{c.raw.toFixed(2)}</td>
              <td className="text-right pr-4 py-0.5">{c.weight}</td>
              <td className="text-right pr-4 py-0.5">{c.value.toFixed(2)}</td>
              <td className="py-0.5">{c.phase}</td>
            </tr>
          ))}
          <tr className="border-t border-border font-semibold">
            <td className="pr-4 py-0.5">Score</td>
            <td colSpan={2} />
            <td className="text-right pr-4 py-0.5">{state.score?.toFixed(2)}</td>
            <td />
          </tr>
        </ExplainTable>
      )}
      {state.excluded && state.excluded.length > 0 && (
        <div>
          <div className="text-xs font-mono text-muted-foreground pt-1">Excluded</div>
          <ExplainTable
            ariaLabel="Excluded candidates table: scroll horizontally for more columns"
            columns={[{ label: 'Node' }, { label: 'Reason' }]}
          >
            {state.excluded.map((e) => (
              <tr key={e.node}>
                <td className="pr-4 py-0.5">{e.node}</td>
                <td className="py-0.5">{EXCLUDED_REASON_LABELS[e.reason] ?? e.reason}</td>
              </tr>
            ))}
          </ExplainTable>
          {typeof state.excludedTotal === 'number' && state.excludedTotal > state.excluded.length && (
            <div className="text-xs text-muted-foreground pt-1">
              ...and {state.excludedTotal - state.excluded.length} more
            </div>
          )}
        </div>
      )}
    </div>
  );
}

export function StatusBadge({ status }: { status: number }) {
  let cls = 'text-xs font-mono font-semibold px-1.5 py-0.5 rounded-md border ';
  if (status >= 200 && status < 300) cls += 'bg-green-500/15 text-green-600 dark:text-green-400 border-green-500/20';
  else if (status >= 400 && status < 500) cls += 'bg-yellow-500/15 text-yellow-600 dark:text-yellow-400 border-yellow-500/20';
  else cls += 'bg-red-500/15 text-red-600 dark:text-red-400 border-red-500/20';
  return <span className={cls}>{status}</span>;
}

export function SkeletonRow() {
  return (
    <tr className="border-b border-border">
      {[...Array(7)].map((_, i) => (
        <td key={i} className="px-4 py-3">
          <div className="h-4 bg-secondary rounded animate-pulse" style={{ width: `${40 + (i * 17) % 50}%` }} />
        </td>
      ))}
    </tr>
  );
}
