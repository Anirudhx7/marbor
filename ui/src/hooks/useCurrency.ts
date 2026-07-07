import { useState, useEffect } from 'react';

const STORAGE_KEY = 'ollama-mesh-currency';
const CHANGE_EVENT = 'ollama-mesh-currency-change';

export interface CurrencyPref {
  code: string;
  symbol: string;
  fxRate: number; // 1 USD = fxRate <code>
}

const DEFAULT_PREF: CurrencyPref = { code: 'USD', symbol: '$', fxRate: 1 };

export const CURRENCY_PRESETS: Array<{ code: string; symbol: string }> = [
  { code: 'USD', symbol: '$' },
  { code: 'EUR', symbol: '€' },
  { code: 'GBP', symbol: '£' },
  { code: 'INR', symbol: '₹' },
  { code: 'JPY', symbol: '¥' },
  { code: 'AUD', symbol: 'A$' },
  { code: 'CAD', symbol: 'C$' },
];

function readCurrencyPref(): CurrencyPref {
  const raw = localStorage.getItem(STORAGE_KEY);
  if (!raw) return DEFAULT_PREF;
  try {
    const parsed = JSON.parse(raw);
    if (parsed && typeof parsed.code === 'string' && typeof parsed.fxRate === 'number' && parsed.fxRate > 0) {
      return { code: parsed.code, symbol: parsed.symbol || DEFAULT_PREF.symbol, fxRate: parsed.fxRate };
    }
  } catch {
    // fall through to default
  }
  return DEFAULT_PREF;
}

// Client-only display preference, mirroring useDemoMode.ts. The backend
// always stores/enforces USD - this hook only converts for display and
// for input fields, never touches any API payload's units directly.
export function useCurrency() {
  const [pref, setPref] = useState<CurrencyPref>(readCurrencyPref);

  useEffect(() => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(pref));
    window.dispatchEvent(new Event(CHANGE_EVENT));
  }, [pref]);

  useEffect(() => {
    const sync = () => setPref(readCurrencyPref());
    window.addEventListener(CHANGE_EVENT, sync);
    return () => window.removeEventListener(CHANGE_EVENT, sync);
  }, []);

  const toDisplay = (usd: number): number => usd * pref.fxRate;
  const toUSD = (displayAmount: number): number => displayAmount / pref.fxRate;

  return { currency: pref, setCurrency: setPref, toDisplay, toUSD };
}
