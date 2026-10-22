import { memo } from 'react';
import { ChevronDown, ChevronRight } from 'lucide-react';
import type { RequestEntry, RoutingDecision } from '../types';
import { formatRelativeTime } from '../lib/time';
import { COLUMNS, ExplainPanel, ReasonBadge, ScrollableValue, StatusBadge } from './RequestLogParts';

export type ExplainState = RoutingDecision | 'loading' | 'error' | undefined;

interface RowProps {
  entry: RequestEntry;
  expanded: boolean;
  explain: ExplainState;
  onToggle: (id: string) => void;
  // Bumped by the page every 30s; changing it re-renders the memoized row so
  // its relative time ("3m ago") stays current when the row itself is unchanged.
  timeTick?: number;
}

// RequestRow and RequestCard are memoized so a 3s refresh that returns the
// same rows does not re-render up to 500 of them; relative times refresh with
// the next page that changes.
export const RequestRow = memo(function RequestRow({ entry, expanded, explain, onToggle }: RowProps) {
  return (
    <>
      <tr className="border-b border-border last:border-0 hover:bg-secondary/30 transition-colors">
        <td className="px-4 py-3 text-muted-foreground whitespace-nowrap">{formatRelativeTime(entry.time)}</td>
        <td className="px-4 py-3 max-w-0">
          <ScrollableValue value={entry.id} valueClassName="font-mono text-xs text-foreground" />
        </td>
        <td className="px-4 py-3 max-w-0">
          {entry.key_name ? (
            <ScrollableValue
              value={entry.key_name}
              valueClassName="font-mono text-xs bg-primary/10 text-primary px-1.5 py-0.5 rounded"
            />
          ) : entry.source_ip ? (
            <span className="block truncate font-mono text-xs text-muted-foreground" title="No API key - showing source IP">
              {entry.source_ip}
            </span>
          ) : (
            <span className="text-muted-foreground/40 text-xs">-</span>
          )}
        </td>
        <td className="px-4 py-3 max-w-0">
          <ScrollableValue value={entry.model} valueClassName="font-mono text-xs text-foreground" />
        </td>
        <td className="px-4 py-3 max-w-0">
          {entry.cloud ? (
            <span
              className="inline-block max-w-full truncate align-middle text-xs font-medium px-2 py-0.5 rounded-md border bg-amber-500/15 text-amber-600 dark:text-amber-400 border-amber-500/20"
              title={`cloud:${(entry.node || '').replace('cloud:', '')}`}
            >
              cloud:{(entry.node || '').replace('cloud:', '')}
            </span>
          ) : (
            <span className="block truncate text-foreground" title={entry.node || '-'}>
              {entry.node || '-'}
            </span>
          )}
        </td>
        <td className="px-4 py-3 whitespace-nowrap">
          <StatusBadge status={entry.status} />
        </td>
        <td className="px-4 py-3 whitespace-nowrap text-right font-mono text-xs text-muted-foreground">{entry.latency_ms} ms</td>
        <td className="px-4 py-3 whitespace-nowrap text-right font-mono text-xs text-muted-foreground">
          {entry.prefillMs ? `${entry.prefillMs} ms` : '-'}
        </td>
        <td className="px-4 py-3 whitespace-nowrap">
          <button
            type="button"
            onClick={() => onToggle(entry.id)}
            aria-expanded={expanded}
            className="flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground transition-colors"
          >
            {expanded ? <ChevronDown className="w-3.5 h-3.5" /> : <ChevronRight className="w-3.5 h-3.5" />}
            <ReasonBadge reason={entry.routingReason} />
          </button>
        </td>
      </tr>
      {expanded && (
        <tr className="border-b border-border last:border-0 bg-secondary/20">
          <td colSpan={COLUMNS.length} className="px-4 py-3">
            <ExplainPanel state={explain} />
          </td>
        </tr>
      )}
    </>
  );
});

export const RequestCard = memo(function RequestCard({ entry, expanded, explain, onToggle }: RowProps) {
  return (
    <div className="bg-card/50 backdrop-blur-sm border border-border/60 rounded-xl p-4 space-y-3 active:bg-secondary/50 transition-colors">
      <div className="flex items-center justify-between gap-2">
        <span className="font-mono text-xs text-foreground truncate" title={entry.id}>
          {entry.id}
        </span>
        <StatusBadge status={entry.status} />
      </div>
      <div className="grid grid-cols-2 gap-x-3 gap-y-2">
        <div>
          <div className="text-[10px] uppercase tracking-wider text-muted-foreground">Time</div>
          <div className="text-sm text-foreground">{formatRelativeTime(entry.time)}</div>
        </div>
        <div>
          <div className="text-[10px] uppercase tracking-wider text-muted-foreground">Latency</div>
          <div className="text-sm text-foreground font-mono">{entry.latency_ms} ms</div>
        </div>
        <div>
          <div className="text-[10px] uppercase tracking-wider text-muted-foreground">Prefill</div>
          <div className="text-sm text-foreground font-mono">{entry.prefillMs ? `${entry.prefillMs} ms` : '-'}</div>
        </div>
        <div>
          <div className="text-[10px] uppercase tracking-wider text-muted-foreground">Model</div>
          <div className="text-sm text-foreground font-mono truncate">{entry.model}</div>
        </div>
        <div>
          <div className="text-[10px] uppercase tracking-wider text-muted-foreground">Node</div>
          <div className="text-sm text-foreground">
            {entry.cloud ? (
              <span className="text-xs font-medium px-2 py-0.5 rounded-md border bg-amber-500/15 text-amber-600 dark:text-amber-400 border-amber-500/20">
                cloud:{(entry.node || '').replace('cloud:', '')}
              </span>
            ) : (
              entry.node || '-'
            )}
          </div>
        </div>
        <div className="col-span-2">
          <div className="text-[10px] uppercase tracking-wider text-muted-foreground">Key</div>
          <div className="text-sm text-foreground">
            {entry.key_name ? (
              <span className="font-mono text-xs bg-primary/10 text-primary px-1.5 py-0.5 rounded-md border border-primary/20">
                {entry.key_name}
              </span>
            ) : entry.source_ip ? (
              <span className="font-mono text-xs text-muted-foreground" title="No API key - showing source IP">
                {entry.source_ip}
              </span>
            ) : (
              <span className="text-muted-foreground/40 text-xs">-</span>
            )}
          </div>
        </div>
      </div>
      <button
        type="button"
        onClick={() => onToggle(entry.id)}
        aria-expanded={expanded}
        className="flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground transition-colors pt-1 border-t border-border/60 w-full min-h-[40px]"
      >
        {expanded ? <ChevronDown className="w-3.5 h-3.5" /> : <ChevronRight className="w-3.5 h-3.5" />}
        <span>Why this node?</span>
        <ReasonBadge reason={entry.routingReason} />
      </button>
      {expanded && (
        <div className="pt-2">
          <ExplainPanel state={explain} />
        </div>
      )}
    </div>
  );
});
