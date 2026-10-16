// Run with: npm test. Guards the status/chart color tokens: no raw palette
// colors on the status surfaces, the activity kinds map to tokens, and every
// token keeps readable contrast in both themes.
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { getActivityKindColor } from '../src/lib/activityKind.ts';

const SRC = path.join(path.dirname(fileURLToPath(import.meta.url)), '..', 'src');
const read = rel => fs.readFileSync(path.join(SRC, rel), 'utf8');
const css = read('index.css');

const RAW = /\b(?:bg|text|border|ring|fill|stroke|from|to|via)-(?:red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose|slate|gray|zinc|neutral|stone)-\d{2,3}\b/;

// Lines that intentionally keep raw palette classes.
const ALLOWED_RAW = [
  /text-\[9px\]/, // tiny provenance label, left as is
  /<Star\b/, // star rating icons stay amber
  /dark:text-red-400/, // destructive is too dark on dark surfaces; see contrast test
];

function rawHits(rel) {
  return read(rel).split('\n')
    .map((line, i) => ({ line, n: i + 1 }))
    .filter(({ line }) => RAW.test(line) && !ALLOWED_RAW.some(re => re.test(line)));
}

test('status surfaces use semantic tokens, not raw palette classes', () => {
  for (const rel of ['lib/activityKind.ts', 'components/Sidebar.tsx', 'pages/ModelAdvisor.tsx']) {
    const hits = rawHits(rel);
    assert.deepEqual(hits, [], `${rel} has raw palette classes: ${hits.map(h => h.n).join(', ')}`);
  }
});

test('the red dark-theme exception is only paired with the destructive token', () => {
  for (const { line, n } of read('pages/ModelAdvisor.tsx').split('\n').map((line, i) => ({ line, n: i + 1 }))) {
    if (line.includes('dark:text-red-400')) {
      assert.match(line, /text-destructive dark:text-red-400/, `ModelAdvisor.tsx:${n}`);
    }
  }
});

test('charts use the chart tokens, not hex colors', () => {
  for (const rel of ['pages/Analytics.tsx', 'pages/Metrics.tsx']) {
    const bad = read(rel).split('\n').filter(l => /\b(stroke|fill|stopColor)=["']#[0-9a-fA-F]{3,8}["']/.test(l));
    assert.deepEqual(bad, [], `${rel} has hex chart colors`);
  }
  assert.match(read('pages/Analytics.tsx'), /hsl\(var\(--chart-1\)\)/);
  assert.match(read('pages/Metrics.tsx'), /hsl\(var\(--chart-2\)\)/);
});

test('activity kinds map to token classes', () => {
  const expected = {
    drain: 'warning', agent: 'info', runtime: 'primary', node: 'success',
    warmup: 'chart-1', schedule: 'chart-2', predictive: 'chart-2',
  };
  for (const [kind, tok] of Object.entries(expected)) {
    assert.equal(getActivityKindColor(kind), `bg-${tok}/15 text-${tok} border-${tok}/20`, kind);
  }
  assert.equal(getActivityKindColor('config'), 'bg-muted text-muted-foreground border-border');
  for (const kind of [...Object.keys(expected), 'config']) {
    assert.doesNotMatch(getActivityKindColor(kind), RAW, kind);
  }
});

// ---- tokens and contrast ----

const rootBlock = css.slice(css.indexOf(':root {'), css.indexOf('.dark {'));
const darkBlock = css.slice(css.indexOf('.dark {'));
const themes = { light: rootBlock, dark: darkBlock };

function hsl(block, name) {
  const m = block.match(new RegExp(String.raw`--${name}:\s*([\d.]+)\s+([\d.]+)%\s+([\d.]+)%`));
  assert.ok(m, `--${name} missing`);
  return [Number(m[1]), Number(m[2]), Number(m[3])];
}

function toRgb([h, s, l]) {
  s /= 100; l /= 100;
  const k = n => (n + h / 30) % 12;
  const a = s * Math.min(l, 1 - l);
  const f = n => l - a * Math.max(-1, Math.min(k(n) - 3, Math.min(9 - k(n), 1)));
  return [f(0), f(8), f(4)];
}

const lum = rgb => {
  const [r, g, b] = rgb.map(c => (c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4));
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
};
const ratio = (a, b) => {
  const [hi, lo] = [lum(a), lum(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
};
const over = (fg, bg, alpha) => fg.map((c, i) => c * alpha + bg[i] * (1 - alpha));

test('status and chart tokens exist in both themes', () => {
  for (const [theme, block] of Object.entries(themes)) {
    for (const name of ['success', 'warning', 'info', 'chart-1', 'chart-2']) {
      assert.doesNotThrow(() => hsl(block, name), `${theme} --${name}`);
    }
  }
  assert.match(css, /--color-chart-1:\s*hsl\(var\(--chart-1\)\)/);
  assert.match(css, /--color-chart-2:\s*hsl\(var\(--chart-2\)\)/);
});

// destructive is excluded: its dark value is below AA on dark surfaces by
// design, and call sites pair it with dark:text-red-400 instead.
const TEXT_TOKENS = ['success', 'warning', 'info', 'primary', 'chart-1', 'chart-2'];

for (const [theme, block] of Object.entries(themes)) {
  const card = toRgb(hsl(block, 'card'));
  for (const name of TEXT_TOKENS) {
    const tok = toRgb(hsl(block, name));
    test(`${theme}: ${name} text on its 15% tint is at least 4.5:1`, () => {
      const r = ratio(tok, over(tok, card, 0.15));
      assert.ok(r >= 4.5, `${name} ${r.toFixed(2)}`);
    });
    test(`${theme}: ${name} against the card is at least 3:1`, () => {
      const r = ratio(tok, card);
      assert.ok(r >= 3, `${name} ${r.toFixed(2)}`);
    });
  }
  test(`${theme}: muted text on the muted fill is at least 4.5:1`, () => {
    const r = ratio(toRgb(hsl(block, 'muted-foreground')), toRgb(hsl(block, 'muted')));
    assert.ok(r >= 4.5, `muted ${r.toFixed(2)}`);
  });
}
