import { useState, useEffect } from 'react';
import { fetchCloudBudgetStatus, fetchSettings } from '../lib/api';
import { useDemoMode, forcedDemo } from '../hooks/useDemoMode';
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
  const [dismissed, setDismissed] = useState(false);
  const [hidden, setHidden] = useState(() => {
    try {
      const stored = localStorage.getItem('demo_settings');
      if (stored) {
        const s = JSON.parse(stored);
        return !!s.hide_budget_banner;
      }
    } catch {}
    return false;
  });

  useEffect(() => {
    const load = () => {
      fetchSettings()
        .then(s => setHidden(!!s.hide_budget_banner))
        .catch(() => {});
    };
    load();
    window.addEventListener('marbor-settings-change', load);
    return () => window.removeEventListener('marbor-settings-change', load);
  }, [demoMode]);

  useEffect(() => {
    let active = true;
    const load = () => {
      fetchCloudBudgetStatus()
        .then(s => {
          if (!active) return;
          // Skip the setState entirely when the payload is unchanged - an
          // unnecessary re-render here is exactly the kind of update that
          // can interrupt a pending route transition (see LESSONS.md L11).
          setStatus(prev => JSON.stringify(prev) === JSON.stringify(s) ? prev : s);
        })
        .catch(() => { if (active) setStatus(prev => prev === null ? prev : null); });
    };
    load();
    const id = setInterval(load, POLL_MS);
    return () => { active = false; clearInterval(id); };
  }, [demoMode]);

  if (hidden || dismissed) return null;
  if (!status || status.softBudgetPct <= 0) return null;

  const worst = worstEntry([status.global, ...status.perKey]);
  if (!worst || worst.pct < status.softBudgetPct) return null;

  const { entry, pct, period } = worst;
  const spent = period === 'daily' ? entry.dailySpent : entry.monthlySpent;
  const cap = period === 'daily' ? entry.dailyCap : entry.monthlyCap;
  const scope = entry.name ? `key "${entry.name}"` : 'global cloud budget';

  return (
    <div className="relative bg-orange-500 text-black text-sm font-medium text-center py-1.5 px-9">
      Cloud spend warning: {scope} at {Math.round(pct * 100)}% of its {period} cap
      ({currency.symbol}{toDisplay(spent).toFixed(2)} / {currency.symbol}{toDisplay(cap).toFixed(2)}).
      <button
        onClick={() => setDismissed(true)}
        aria-label="Dismiss warning"
        className="absolute right-2 top-1/2 -translate-y-1/2 text-black/70 hover:text-black text-lg leading-none px-1"
      >
        &times;
      </button>
    </div>
  );
}
