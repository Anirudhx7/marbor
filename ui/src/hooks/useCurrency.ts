import { useState, useEffect } from 'react';

const STORAGE_KEY = 'marbor-currency';
const CHANGE_EVENT = 'marbor-currency-change';

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
    // readCurrencyPref() returns a new object every call, so setting it
    // unconditionally would re-trigger the write effect above even when the
    // value hasn't changed - every other mounted useCurrency() instance
    // would then re-dispatch CHANGE_EVENT in response, and this listener
    // would fire again, forever (two instances ping-ponging the event with
    // no way to settle). Bail out to the same object reference when the
    // value is unchanged so the write effect's [pref] dependency doesn't
    // see a change and the loop terminates - same fix as LESSONS.md L13.
    const sync = () => {
      const next = readCurrencyPref();
      setPref((prev) =>
        prev.code === next.code && prev.symbol === next.symbol && prev.fxRate === next.fxRate
          ? prev
          : next
      );
    };
    window.addEventListener(CHANGE_EVENT, sync);
    return () => window.removeEventListener(CHANGE_EVENT, sync);
  }, []);

  const toDisplay = (usd: number): number => usd * pref.fxRate;
  const toUSD = (displayAmount: number): number => displayAmount / pref.fxRate;

  return { currency: pref, setCurrency: setPref, toDisplay, toUSD };
}
