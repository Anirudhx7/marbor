// Run with: npm test. Checks the shared button classes and that the Button
// wrapper stays a plain passthrough.
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { buttonClasses } from '../src/lib/buttonClasses.ts';

const SRC = path.join(path.dirname(fileURLToPath(import.meta.url)), '..', 'src');

function walk(dir) {
  return fs.readdirSync(dir, { withFileTypes: true }).flatMap(e =>
    e.isDirectory() ? walk(path.join(dir, e.name)) : /\.(tsx?|css)$/.test(e.name) ? [path.join(dir, e.name)] : []);
}
const files = walk(SRC).filter(f => !f.endsWith('mockData.ts'));
const read = f => fs.readFileSync(f, 'utf8');
const tokens = s => s.split(/\s+/).filter(Boolean).sort();

const EXPECTED = {
  primary: {
    md: 'px-4 py-2 bg-primary hover:bg-primary/90 text-primary-foreground text-sm font-medium rounded-lg transition-colors shadow-sm disabled:opacity-50 disabled:cursor-not-allowed',
    sm: 'px-4 py-2 bg-primary hover:bg-primary/90 text-primary-foreground text-xs font-medium rounded-lg transition-colors shadow-sm disabled:opacity-50 disabled:cursor-not-allowed',
  },
  secondary: {
    md: 'px-4 py-2 bg-secondary hover:bg-secondary/80 text-foreground text-sm font-medium rounded-lg transition-colors shadow-sm disabled:opacity-50 disabled:cursor-not-allowed',
    sm: 'px-4 py-2 bg-secondary hover:bg-secondary/80 text-foreground text-xs font-medium rounded-lg transition-colors shadow-sm disabled:opacity-50 disabled:cursor-not-allowed',
  },
  ghost: {
    md: 'px-4 py-2 text-muted-foreground hover:text-foreground text-sm font-medium transition-colors disabled:opacity-50 disabled:cursor-not-allowed',
    sm: 'px-4 py-2 text-muted-foreground hover:text-foreground text-xs font-medium transition-colors disabled:opacity-50 disabled:cursor-not-allowed',
  },
  destructive: {
    md: 'px-4 py-2 bg-destructive hover:bg-destructive/90 text-destructive-foreground text-sm font-medium rounded-lg transition-colors shadow-sm disabled:opacity-50 disabled:cursor-not-allowed',
    sm: 'px-4 py-2 bg-destructive hover:bg-destructive/90 text-destructive-foreground text-xs font-medium rounded-lg transition-colors shadow-sm disabled:opacity-50 disabled:cursor-not-allowed',
  },
};

test('each variant and size returns its exact class string', () => {
  for (const [variant, sizes] of Object.entries(EXPECTED)) {
    for (const [size, expected] of Object.entries(sizes)) {
      assert.equal(buttonClasses(variant, size), expected, `${variant}/${size}`);
    }
  }
});

test('defaults are primary and md', () => {
  assert.equal(buttonClasses(), EXPECTED.primary.md);
});

// The primary string as the app wrote it before the shared button existed.
const PRIMARY_BEFORE = 'px-4 py-2 bg-primary hover:bg-primary/90 disabled:opacity-50 disabled:cursor-not-allowed text-primary-foreground font-medium rounded-lg text-sm transition-colors shadow-sm';

test('primary matches the string the app used before the shared button', () => {
  assert.deepEqual(tokens(buttonClasses('primary', 'md')), tokens(PRIMARY_BEFORE));
});

// The small primary as Warmup wrote it before the shared button existed.
const PRIMARY_SM_BEFORE = 'px-4 py-2 bg-primary hover:bg-primary/90 disabled:opacity-50 disabled:cursor-not-allowed text-primary-foreground font-medium rounded-lg text-xs transition-colors shadow-sm';

test('small primary matches the string Warmup used before', () => {
  assert.deepEqual(tokens(buttonClasses('primary', 'sm')), tokens(PRIMARY_SM_BEFORE));
});

test('secondary, ghost and destructive each match a class string still in the app', () => {
  const literals = files.flatMap(f => [...read(f).matchAll(/["'`]([^"'`\n]{20,})["'`]/g)].map(m => tokens(m[1]).join(' ')));
  for (const variant of ['secondary', 'ghost', 'destructive']) {
    const want = tokens(buttonClasses(variant, 'md')).join(' ');
    assert.ok(literals.includes(want), `${variant}: no real site has this exact class set`);
  }
});

test('buttonClasses is consumed by the Button component', () => {
  const src = read(path.join(SRC, 'components', 'Button.tsx'));
  assert.match(src, /buttonClasses\(/);
});

test('Button.tsx has no onClick token', () => {
  assert.equal(/onClick/.test(read(path.join(SRC, 'components', 'Button.tsx'))), false);
});

test('Button.tsx does not force a type', () => {
  assert.equal(/type=\{?['"](button|submit)/.test(read(path.join(SRC, 'components', 'Button.tsx'))), false);
});

test('className is appended after the variant classes', () => {
  const src = read(path.join(SRC, 'components', 'Button.tsx'));
  assert.match(src, /\$\{buttonClasses\(variant, size\)\} \$\{className\}/);
});

test('no <Button className> sets color, spacing, size, weight or radius', () => {
  const banned = /(^|\s)(bg-|px-|py-|text-sm|font-|rounded)/;
  for (const f of files.filter(f => f.endsWith('.tsx'))) {
    const src = read(f);
    for (const m of src.matchAll(/<Button/g)) {
      // The opening tag runs to the first ">" that is not an arrow "=>" and not inside braces.
      let depth = 0, end = m.index;
      for (let i = m.index; i < src.length; i++) {
        const c = src[i];
        if (c === '{') depth++;
        else if (c === '}') depth--;
        else if (c === '>' && depth === 0 && src[i - 1] !== '=') { end = i; break; }
      }
      const tag = src.slice(m.index, end);
      const cn = tag.match(/className=("[^"]*"|\{[^}]*\})/s);
      if (cn) assert.equal(banned.test(cn[1]), false, `${path.relative(SRC, f)}: <Button className=${cn[1]}>`);
    }
  }
});

test('no rounded-2xl or rounded-3xl anywhere in ui/src', () => {
  for (const f of files) {
    assert.equal(/rounded-[23]xl/.test(read(f)), false, path.relative(SRC, f));
  }
});

test('the radius scale defines --radius-xl', () => {
  assert.match(read(path.join(SRC, 'index.css')), /--radius-xl:\s*0\.75rem;/);
});
