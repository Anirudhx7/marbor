import { useState, useEffect } from 'react';
import { fetchCloudBudgetStatus } from '../lib/api';
import { useDemoMode } from '../hooks/useDemoMode';
import { useCurrency } from '../hooks/useCurrency';
import type { BudgetEntry, CloudBudgetStatus } from '../types';

const POLL_MS = 30_000;

function worstEntry(entries: BudgetEntry[]): { entry: BudgetEntry; pct: number; period: 'daily' | 'monthly' } | null {
  let best: { entry: BudgetEntry; pct: number; period: 'daily' | 'monthly' } | null = null;
  for (const e of entries) {
    if (e.dailyCap > 0 && (!best || e.dailyPct > best.pct)) best = { entry: e, pct: e.dailyPct, period: 'daily' };
    if (e.monthlyCap > 0 && (!best || e.monthlyPct > best.pct)) best = { entry: e, pct: e.monthlyPct, period: 'monthly' };
  }
  return best;
}

export function BudgetBanner() {
  const { demoMode } = useDemoMode();
  const { currency, toDisplay } = useCurrency();
  const [status, setStatus] = useState<CloudBudgetStatus | null>(null);

  useEffect(() => {
    let active = true;
    const load = () => {
      fetchCloudBudgetStatus()
        .then(s => { if (active) setStatus(s); })
        .catch(() => { if (active) setStatus(null); });
    };
    load();
    const id = setInterval(load, POLL_MS);
    return () => { active = false; clearInterval(id); };
  }, [demoMode]);

  if (!status || status.softBudgetPct <= 0) return null;

  const worst = worstEntry([status.global, ...status.perKey]);
  if (!worst || worst.pct < status.softBudgetPct) return null;

  const { entry, pct, period } = worst;
  const spent = period === 'daily' ? entry.dailySpent : entry.monthlySpent;
  const cap = period === 'daily' ? entry.dailyCap : entry.monthlyCap;
  const scope = entry.name ? `key "${entry.name}"` : 'global cloud budget';

  return (
    <div className="bg-orange-500 text-black text-sm font-medium text-center py-1.5 px-4">
      Cloud spend warning: {scope} at {Math.round(pct * 100)}% of its {period} cap
      ({currency.symbol}{toDisplay(spent).toFixed(2)} / {currency.symbol}{toDisplay(cap).toFixed(2)}).
    </div>
  );
}
