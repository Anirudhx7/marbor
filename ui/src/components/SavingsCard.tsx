import { DollarSign } from 'lucide-react';
import { Savings } from '../types';
import { CardKicker } from './CardKicker';

interface SavingsCardProps {
  savings: Savings | null;
  loading: boolean;
}

// Saved-vs-Cloud - the fleet ROI figure: every locally served token counts as
// avoided cloud spend at the configured reference rate, so even a pure local
// fleet shows real value. Rendered on the dashboard (paired with Fleet
// Capacity, above the fold) and on Routing (next to the strategy that drives
// the local/cloud split). One shared component so the two surfaces never
// drift. saved_usd is null until real token counts exist - the card shows "-"
// then, never a guess, and the caption always says "estimated": the
// figure is token counts priced at a reference rate, not a billed amount.
export function SavingsCard({ savings, loading }: SavingsCardProps) {
  const localPct = savings && savings.total_requests > 0
    ? Math.round((savings.local_requests / savings.total_requests) * 100)
    : 0;
  const cloudPct = 100 - localPct;

  return (
    <div className="glass-panel rounded-xl p-5 hover:border-primary/50 transition-colors h-full min-w-0">
      <div className="flex items-start justify-between gap-2">
        <div className="min-w-0">
          <CardKicker className="mb-1">Saved vs cloud</CardKicker>
          {loading ? (
            <div aria-hidden="true" className="animate-pulse">
              <div className="h-8 w-32 max-w-full bg-secondary rounded mb-1.5" />
              <div className="h-3 w-40 max-w-full bg-secondary rounded" />
            </div>
          ) : (
            <div className="flex items-baseline gap-1">
              {savings ? (
                <span className="text-2xl font-bold text-success">
                  {savings.saved_usd !== null ? `$${savings.saved_usd.toFixed(2)}` : '-'}
                </span>
              ) : (
                <span className="text-2xl font-bold text-muted-foreground">--</span>
              )}
            </div>
          )}
          {loading ? (
            <div aria-hidden="true" className="h-3 w-36 max-w-full bg-secondary rounded mt-2 animate-pulse" />
          ) : savings ? (
            <p className="text-xs font-medium text-muted-foreground mt-0.5 flex flex-wrap items-center gap-x-1 leading-snug">
              <span className="whitespace-nowrap">{savings.local_requests.toLocaleString('en-US')} local</span>
              <span>/</span>
              <span className="whitespace-nowrap">{savings.cloud_requests.toLocaleString('en-US')} cloud</span>
            </p>
          ) : (
            <p className="text-xs font-medium text-muted-foreground mt-0.5">local / cloud requests</p>
          )}
        </div>
        <div className="p-2 bg-success/10 rounded-lg text-success shrink-0">
          <DollarSign className="w-5 h-5" />
        </div>
      </div>
      {loading ? (
        <div aria-hidden="true" className="mt-3 h-4 w-40 max-w-full bg-secondary rounded animate-pulse" />
      ) : (
        <div className="mt-3 flex items-center flex-wrap gap-x-1.5 text-xs font-medium">
          <span className="text-success whitespace-nowrap">
            Local {!savings ? '--' : `${localPct}%`}
          </span>
          <span className="text-muted-foreground">/</span>
          <span className="text-amber-700 dark:text-amber-400 whitespace-nowrap">
            Cloud {!savings ? '--' : `${cloudPct}%`}
          </span>
        </div>
      )}
      <p className="mt-2 text-[10px] text-muted-foreground/70 leading-snug">
        estimated: parsed token counts priced at the configured cloud reference rate
      </p>
    </div>
  );
}
