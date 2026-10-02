// Loads every dashboard route of the static demo build (ui/dist, built with
// VITE_FORCE_DEMO=true) in headless Chrome and fails if a page makes a failed
// backend request, throws, logs a console error, or renders blank.
//
// Usage: npm run demo:crawl   (after: VITE_FORCE_DEMO=true npm run build)
// Browser: playwright-core drives an installed Chrome (nothing is downloaded).
// Set DEMO_CRAWL_CHANNEL=msedge, or DEMO_CRAWL_CHROME=/path/to/chrome, when
// Google Chrome is not installed.
import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import { readFileSync } from 'node:fs';
import { extname, join, normalize, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright-core';

const root = resolve(fileURLToPath(new URL('..', import.meta.url)));
const distDir = join(root, 'dist');
const routes = JSON.parse(readFileSync(join(root, 'src', 'routes.json'), 'utf8')).map(r => r.path);

const CONCURRENCY = 5;
const PAGE_TIMEOUT_MS = 5000;
// Paths that belong to the real backend. The demo never talks to one, so any
// request to these is a gap in the demo data path.
const BACKEND_PREFIXES = ['/admin/', '/api/', '/v1/', '/login', '/health'];
// console.error messages that are acceptable, each with a written reason.
// Empty on purpose: add { pattern: /.../, reason: '...' } only for a real,
// understood false positive.
const CONSOLE_ALLOWLIST = [];
// Text the app's error boundary shows when a page throws while rendering.
const ERROR_BOUNDARY_TEXT = ['Page failed to load', 'New application version available'];

const MIME = {
  '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.json': 'application/json',
  '.svg': 'image/svg+xml', '.png': 'image/png', '.ico': 'image/x-icon', '.woff2': 'font/woff2',
  '.woff': 'font/woff', '.txt': 'text/plain', '.map': 'application/json',
};

function startServer() {
  const server = createServer(async (req, res) => {
    const urlPath = decodeURIComponent(new URL(req.url, 'http://x').pathname);
    const file = normalize(join(distDir, urlPath === '/' ? 'index.html' : urlPath));
    if (!file.startsWith(distDir + sep)) {
      res.writeHead(403).end();
      return;
    }
    try {
      const body = await readFile(file);
      res.writeHead(200, { 'content-type': MIME[extname(file)] || 'application/octet-stream' }).end(body);
    } catch {
      res.writeHead(404).end('not found');
    }
  });
  return new Promise(ok => server.listen(0, '127.0.0.1', () => ok(server)));
}

async function crawlRoute(context, baseUrl, route) {
  const problems = [];
  const page = await context.newPage();
  page.on('pageerror', err => problems.push(`pageerror: ${err.message.split('\n')[0]}`));
  page.on('console', msg => {
    if (msg.type() !== 'error') return;
    const text = msg.text();
    if (CONSOLE_ALLOWLIST.some(a => a.pattern.test(text))) return;
    const where = msg.location().url ? ` (${new URL(msg.location().url).pathname})` : '';
    problems.push(`console.error: ${text.split('\n')[0]}${where}`);
  });
  page.on('response', resp => {
    const { pathname } = new URL(resp.url());
    if (resp.status() >= 400 && BACKEND_PREFIXES.some(p => pathname.startsWith(p))) {
      problems.push(`${resp.status()} ${pathname}`);
    }
  });
  page.on('requestfailed', req => {
    const { pathname } = new URL(req.url());
    if (BACKEND_PREFIXES.some(p => pathname.startsWith(p))) problems.push(`failed ${pathname}`);
  });
  try {
    // The demo uses a HashRouter, so the route lives after the '#'.
    await page.goto(`${baseUrl}/#${route}`, { waitUntil: 'networkidle', timeout: PAGE_TIMEOUT_MS });
    const { rootChars, bodyText } = await page.evaluate(() => ({
      rootChars: (document.getElementById('root')?.innerText ?? '').trim().length,
      bodyText: document.body.innerText,
    }));
    if (rootChars === 0) problems.push('blank: app root rendered empty');
    for (const t of ERROR_BOUNDARY_TEXT) {
      if (bodyText.includes(t)) problems.push(`error boundary: "${t}"`);
    }
  } catch (err) {
    problems.push(`load: ${err.message.split('\n')[0]}`);
  } finally {
    await page.close();
  }
  return { route, problems: [...new Set(problems)] };
}

async function main() {
  const started = Date.now();
  const server = await startServer();
  const baseUrl = `http://127.0.0.1:${server.address().port}`;
  const launchOpts = process.env.DEMO_CRAWL_CHROME
    ? { executablePath: process.env.DEMO_CRAWL_CHROME }
    : { channel: process.env.DEMO_CRAWL_CHANNEL || 'chrome' };
  const browser = await chromium.launch(launchOpts);
  try {
    const context = await browser.newContext();
    const queue = [...routes];
    const results = [];
    const worker = async () => {
      for (let route = queue.shift(); route !== undefined; route = queue.shift()) {
        results.push(await crawlRoute(context, baseUrl, route));
      }
    };
    await Promise.all(Array.from({ length: Math.min(CONCURRENCY, routes.length) }, worker));
    results.sort((a, b) => routes.indexOf(a.route) - routes.indexOf(b.route));

    const width = Math.max(...routes.map(r => r.length));
    for (const { route, problems } of results) {
      console.log(`${route.padEnd(width)}  ${problems.length ? 'FAIL' : 'ok'}`);
      for (const p of problems) console.log(`${' '.repeat(width)}    - ${p}`);
    }
    const failed = results.filter(r => r.problems.length).length;
    console.log(`\n${results.length} routes, ${failed} failing, ${((Date.now() - started) / 1000).toFixed(1)}s`);
    process.exitCode = failed ? 1 : 0;
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(err => {
  console.error(err);
  process.exit(1);
});
