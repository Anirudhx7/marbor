import test from 'node:test';
import assert from 'node:assert/strict';
import { applyCurrencyCode, parseFxRate } from '../src/hooks/useCurrency.ts';

const usd = { code: 'USD', symbol: '$', fxRate: 1 };

test('parseFxRate accepts rates below 1 and rejects zero, negatives and junk', () => {
  assert.equal(parseFxRate('0.92'), 0.92);
  assert.equal(parseFxRate('83'), 83);
  assert.equal(parseFxRate('0'), null);
  assert.equal(parseFxRate('-1'), null);
  assert.equal(parseFxRate(''), null);
  assert.equal(parseFxRate('abc'), null);
});

test('choosing a non-USD currency keeps the entered rate and sets the symbol', () => {
  const eur = applyCurrencyCode({ ...usd, fxRate: 0.92 }, 'EUR');
  assert.deepEqual(eur, { code: 'EUR', symbol: '€', fxRate: 0.92 });
});

test('choosing USD always resets the rate to 1', () => {
  const back = applyCurrencyCode({ code: 'INR', symbol: '₹', fxRate: 83 }, 'USD');
  assert.deepEqual(back, usd);
});

test('USD to EUR to USD round trip leaves the base value unchanged', () => {
  const usdCap = 25;
  const eur = { ...applyCurrencyCode(usd, 'EUR'), fxRate: 0.92 };
  const display = usdCap * eur.fxRate;
  assert.equal(display, 23);
  const restored = applyCurrencyCode(eur, 'USD');
  assert.equal(usdCap * restored.fxRate, 25);
  assert.equal(display / eur.fxRate, usdCap);
});

test('an unknown code keeps the previous symbol', () => {
  assert.equal(applyCurrencyCode({ code: 'EUR', symbol: '€', fxRate: 2 }, 'XYZ').symbol, '€');
});
