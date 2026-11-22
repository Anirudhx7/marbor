import { useSyncExternalStore } from 'react';

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

// Pure helpers for the Settings currency controls, kept here so the rules are
// testable without rendering the page.

// Picking a currency changes the code and symbol. USD is the base unit, so its
// rate is always 1; any other currency keeps the rate already entered.
export function applyCurrencyCode(pref: CurrencyPref, code: string): CurrencyPref {
  const preset = CURRENCY_PRESETS.find((c) => c.code === code);
  return {
    ...pref,
    code,
    symbol: preset?.symbol || pref.symbol,
    fxRate: code === 'USD' ? 1 : pref.fxRate,
  };
}

// Returns the rate when the text is a finite number above zero, else null.
export function parseFxRate(text: string): number | null {
  const rate = parseFloat(text);
  return Number.isFinite(rate) && rate > 0 ? rate : null;
}

function parsePref(raw: string | null): CurrencyPref {
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

// One shared store over localStorage. Every useCurrency() caller reads the same
// snapshot, so there is a single writer. The earlier version kept a copy per
// caller and each copy wrote itself back on change; with two mounted (Settings
// and the budget banner) a lagging copy re-wrote the previous rate over a newer
// one while the FX field was being typed into, leaving 8 where 83 was entered.
let cachedRaw: string | null | undefined;
let cachedPref: CurrencyPref = DEFAULT_PREF;

function getSnapshot(): CurrencyPref {
  const raw = localStorage.getItem(STORAGE_KEY);
  if (raw !== cachedRaw) {
    cachedRaw = raw;
    cachedPref = parsePref(raw);
  }
  return cachedPref;
}

function subscribe(listener: () => void): () => void {
  window.addEventListener(CHANGE_EVENT, listener);
  window.addEventListener('storage', listener);
  return () => {
    window.removeEventListener(CHANGE_EVENT, listener);
    window.removeEventListener('storage', listener);
  };
}

function setCurrency(next: CurrencyPref | ((prev: CurrencyPref) => CurrencyPref)) {
  const value = typeof next === 'function' ? next(getSnapshot()) : next;
  localStorage.setItem(STORAGE_KEY, JSON.stringify(value));
  window.dispatchEvent(new Event(CHANGE_EVENT));
}

// Client-only display preference, mirroring useDemoMode.ts. The backend
// always stores/enforces USD - this hook only converts for display and
// for input fields, never touches any API payload's units directly.
export function useCurrency() {
  const pref = useSyncExternalStore(subscribe, getSnapshot, () => DEFAULT_PREF);
  const toDisplay = (usd: number): number => usd * pref.fxRate;
  const toUSD = (displayAmount: number): number => displayAmount / pref.fxRate;
  return { currency: pref, setCurrency, toDisplay, toUSD };
}
