import { useState, useEffect } from 'react';
import { fetchCloudBudgetStatus, fetchSettings, updateSettings } from '../lib/api';
import { useDemoMode, forcedDemo } from '../hooks/useDemoMode';
import { useCurrency } from '../hooks/useCurrency';
import type { BudgetEntry, CloudBudgetStatus } from '../types';
import { X, AlertTriangle, TrendingUp } from 'lucide-react';

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
        // A failed fetch must not leave a stale localStorage-derived hidden
        // value in place - fail toward visibility, not suppression, so a
        // real overspend warning can't be hidden by a transient fetch error.
        .catch(() => setHidden(false));
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

  const handleDismiss = () => {
    setDismissed(true);
    fetchSettings().then(s => {
      const updated = { ...s, hide_budget_banner: true };
      return updateSettings(updated);
    }).then(() => {
      window.dispatchEvent(new CustomEvent('marbor-settings-change'));
    }).catch(() => {});
  };

  return (
    <div className="relative animate-fade-in min-w-0">
      <div className="bg-warning/10 border border-warning/30 rounded-lg px-3 sm:px-4 py-3 flex items-center justify-between gap-3 sm:gap-4 min-w-0">
        <div className="flex items-center gap-3 min-w-0 flex-1">
          <div className="flex-shrink-0 w-8 h-8 rounded-full bg-warning/20 flex items-center justify-center">
            <AlertTriangle className="w-4 h-4 text-warning" />
          </div>
          <div className="min-w-0">
            <p className="text-sm font-medium text-foreground">
              Cloud spend warning
            </p>
            <p className="text-xs text-muted-foreground leading-snug break-words">
              {scope} at {Math.round(pct * 100)}% of its {period} cap
              ({currency.symbol}{toDisplay(spent).toFixed(2)} / {currency.symbol}{toDisplay(cap).toFixed(2)})
            </p>
          </div>
        </div>
        <div className="flex items-center gap-2 shrink-0">
          <TrendingUp className="w-4 h-4 text-warning/80 hidden sm:block" />
          <button
            onClick={handleDismiss}
            aria-label="Dismiss warning"
            className="p-1 rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors min-w-[40px] min-h-[40px] sm:min-w-0 sm:min-h-0 flex items-center justify-center"
          >
            <X className="w-4 h-4" />
          </button>
        </div>
      </div>
    </div>
  );
}
