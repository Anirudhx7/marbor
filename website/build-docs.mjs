#!/usr/bin/env node
/**
 * build-docs.mjs -- renders docs/*.md into website/docs/*.html
 *
 * Why this approach:
 *   The website must be plain static HTML with NO external CDN/JS. So instead of
 *   shipping a client-side markdown renderer, we render at build time with a small,
 *   dependency-free markdown -> HTML function and drop the result into a shared
 *   template that matches the landing page (same nav, theme toggle, footer).
 *
 * Source of truth stays in docs/*.md. This script never modifies them.
 *
 * Usage:
 *   node website/build-docs.mjs
 *   (CI runs this, then a sed pass injects {{VERSION}})
 */
import { readFileSync, writeFileSync, mkdirSync, readdirSync, statSync, existsSync } from "node:fs";
import { join, dirname, relative, basename } from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = dirname(fileURLToPath(import.meta.url));
const ROOT = join(__dirname, "..");
const DOCS_SRC = join(ROOT, "docs");
const DOCS_OUT = join(__dirname, "docs");

/* ---------- tiny markdown renderer (no deps) ---------- */
function escapeHtml(s) {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}
function inline(s) {
  // code spans first (protect their contents)
  const codes = [];
  s = s.replace(/`([^`]+)`/g, (_, c) => {
    codes.push(`<code>${escapeHtml(c)}</code>`);
    return `__CODE_PLACEHOLDER_${codes.length - 1}__`;
  });
  s = escapeHtml(s);
  // links [text](url)
  s = s.replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, (_, t, u) => {
    let href = u;
    // rewrite intra-doc .md links to .html (skip external links)
    if (!/^https?:/.test(href) && (/\.md(#|$)/.test(href) || href.startsWith("../") || href.startsWith("./"))) {
      href = href.replace(/\.md(#|$)/, ".html$1");
    }
    const ext = /^https?:/.test(href) ? ' target="_blank" rel="noopener"' : "";
    return `<a href="${href}"${ext}>${t}</a>`;
  });
  s = s.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
  s = s.replace(/(^|[^*])\*([^*]+)\*/g, "$1<em>$2</em>");
  // restore code spans
  s = s.replace(/__CODE_PLACEHOLDER_(\d+)__/g, (_, i) => codes[+i]);
  return s;
}
function slugify(s) {
  return s.toLowerCase().replace(/[^\w\s-]/g, "").trim().replace(/\s+/g, "-");
}

function renderMarkdown(md) {
  const lines = md.replace(/\r\n/g, "\n").split("\n");
  let html = "";
  const headings = [];
  let i = 0;
  while (i < lines.length) {
    let line = lines[i];

    // fenced code
    if (/^```/.test(line)) {
      const lang = line.slice(3).trim();
      const buf = [];
      i++;
      while (i < lines.length && !/^```/.test(lines[i])) { buf.push(lines[i]); i++; }
      i++; // closing fence
      html += `<pre class="code"${lang ? ` data-lang="${lang}"` : ""}><code>${escapeHtml(buf.join("\n"))}</code></pre>\n`;
      continue;
    }

    // headings
    const h = line.match(/^(#{1,6})\s+(.*)$/);
    if (h) {
      const level = h[1].length;
      const text = h[2].trim();
      const id = slugify(text);
      if (level === 2 || level === 3) headings.push({ level, text, id });
      html += `<h${level} id="${id}">${inline(text)}<a class="anchor" href="#${id}" aria-label="Link to section">#</a></h${level}>\n`;
      i++; continue;
    }

    // blockquote
    if (/^>\s?/.test(line)) {
      const buf = [];
      while (i < lines.length && /^>\s?/.test(lines[i])) { buf.push(lines[i].replace(/^>\s?/, "")); i++; }
      html += `<blockquote>${inline(buf.join(" "))}</blockquote>\n`;
      continue;
    }

    // table
    if (/^\|.*\|$/.test(line) && i + 1 < lines.length && /^\|[\s:|-]+\|$/.test(lines[i + 1])) {
      const head = line.split("|").slice(1, -1).map((c) => c.trim());
      i += 2;
      const rows = [];
      while (i < lines.length && /^\|.*\|$/.test(lines[i])) {
        rows.push(lines[i].split("|").slice(1, -1).map((c) => c.trim()));
        i++;
      }
      html += `<div class="table-scroll"><table>\n<thead><tr>${head.map((c) => `<th>${inline(c)}</th>`).join("")}</tr></thead>\n<tbody>${rows.map((r) => `<tr>${r.map((c) => `<td>${inline(c)}</td>`).join("")}</tr>`).join("\n")}</tbody>\n</table></div>\n`;
      continue;
    }

    // lists (unordered + ordered)
    if (/^\s*([-*]|\d+\.)\s+/.test(line)) {
      const ordered = /^\s*\d+\.\s+/.test(line);
      const buf = [];
      while (i < lines.length && /^\s*([-*]|\d+\.)\s+/.test(lines[i])) {
        buf.push(lines[i].replace(/^\s*([-*]|\d+\.)\s+/, ""));
        i++;
      }
      html += `<${ordered ? "ol" : "ul"}>${buf.map((b) => `<li>${inline(b)}</li>`).join("")}</${ordered ? "ol" : "ul"}>\n`;
      continue;
    }

    // hr
    if (/^---+$/.test(line.trim())) { html += "<hr/>\n"; i++; continue; }

    // blank
    if (line.trim() === "") { i++; continue; }

    // paragraph (collect until blank or block start)
    const buf = [line];
    i++;
    while (i < lines.length && lines[i].trim() !== "" &&
      !/^(#{1,6}\s|```|>|\s*([-*]|\d+\.)\s|\|.*\||---+$)/.test(lines[i])) {
      buf.push(lines[i]); i++;
    }
    html += `<p>${inline(buf.join(" "))}</p>\n`;
  }
  return { html, headings };
}

/* ---------- shared chrome (matches index.html) ---------- */
const LOGO_HTML = `<div style="width:32px;height:32px;background:#1a1714;border-radius:8px;display:flex;align-items:center;justify-content:center;flex-shrink:0"><svg width="22" height="22" viewBox="0 0 100 100" fill="none" aria-hidden="true"><path d="M30 35 L30 65 M30 50 L50 35 L50 65 M50 50 L70 35 L70 65" stroke="var(--accent)" stroke-width="8" stroke-linecap="round" stroke-linejoin="round"/><circle cx="75" cy="75" r="8" fill="#a87f3a"/></svg></div>`;
const GH_SVG = `<svg width="15" height="15" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0016 8c0-4.42-3.58-8-8-8z"/></svg>`;

// All docs in nav order, grouped.
const DOC_GROUPS = [
  { title: "Guides", items: [
    { slug: "INTEGRATIONS", label: "Integrations" },
    { slug: "LIMITATIONS", label: "Known limitations" },
    { slug: "PRODUCTION", label: "Production" },
    { slug: "SAVINGS-MATH", label: "Savings math" },
    { slug: "USE-CASES", label: "Use cases" },
  ]},
  { title: "Deploy", items: [
    { slug: "deploy/aws-ec2", label: "AWS EC2" },
  ]},
  { title: "Integrations", items: [
    { slug: "integrations/continue", label: "Continue" },
    { slug: "integrations/librechat", label: "LibreChat" },
    { slug: "integrations/litellm", label: "LiteLLM" },
    { slug: "integrations/open-webui", label: "Open WebUI" },
  ]},
];

function relRoot(slug) {
  const depth = slug.split("/").length - 1;
  return depth === 0 ? "../" : "../".repeat(depth + 1);
}

function docSidebar(currentSlug) {
  const r = relRoot(currentSlug);
  return DOC_GROUPS.map((g) => `
    <div class="nav-group">
      <div class="nav-group-title">${g.title}</div>
      ${g.items.map((it) => {
        const active = it.slug === currentSlug ? " active" : "";
        return `<a class="doc-nav-link${active}" href="${r}docs/${it.slug}.html">${it.label}</a>`;
      }).join("")}
    </div>`).join("");
}

function tocHtml(headings) {
  if (!headings.length) return `<p class="toc-empty">No sections.</p>`;
  return `<ul class="toc">${headings.map((h) =>
    `<li class="lvl-${h.level}"><a href="#${h.id}">${escapeHtml(h.text)}</a></li>`
  ).join("")}</ul>`;
}

function breadcrumb(slug, title) {
  const r = relRoot(slug);
  const parts = slug.split("/");
  const crumbs = [`<a href="${r}index.html">Home</a>`, `<a href="${r}docs/index.html">Docs</a>`];
  if (parts.length > 1) crumbs.push(`<span>${parts[0]}</span>`);
  crumbs.push(`<span class="current">${escapeHtml(title)}</span>`);
  return crumbs.join('<span class="sep">/</span>');
}

const DOC_CSS = readFileSync(join(__dirname, "docs.css"), "utf8");

function page({ slug, title, contentHtml, headings }) {
  const r = relRoot(slug);
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="UTF-8" />
<meta name="viewport" content="width=device-width, initial-scale=1.0" />
<script>
  (function () { try { var s = localStorage.getItem("om-theme"); if (s === "light" || (!s && window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches)) document.documentElement.classList.add("light"); } catch (e) {} })();
</script>
<title>${escapeHtml(title)} · ollama-mesh docs</title>
<meta name="description" content="ollama-mesh documentation: ${escapeHtml(title)}." />
<link rel="icon" type="image/svg+xml" href="${r}favicon.svg" />
<meta property="og:type" content="article" />
<meta property="og:site_name" content="ollama-mesh" />
<meta property="og:title" content="${escapeHtml(title)} · ollama-mesh docs" />
<meta property="og:description" content="ollama-mesh documentation: ${escapeHtml(title)}." />
<meta property="og:image" content="https://anirudh.social/ollama-mesh/screenshots/dashboard.png" />
<meta property="og:image:alt" content="ollama-mesh admin dashboard" />
<meta name="twitter:card" content="summary_large_image" />
<meta name="twitter:title" content="${escapeHtml(title)} · ollama-mesh docs" />
<meta name="twitter:image" content="https://anirudh.social/ollama-mesh/screenshots/dashboard.png" />
<style>${DOC_CSS}</style>
</head>
<body>
<nav>
  <div class="page">
    <a href="${r}index.html" class="brand" aria-label="ollama-mesh home">${LOGO_HTML}<span class="name">ollama<span style="color:var(--accent)">-mesh</span></span><span class="ver">{{VERSION}}</span><span class="brand-dot" title="Active"></span></a>
    <div class="nav-right">
      <div class="nav-links">
        <a class="link" href="${r}index.html#features">Features</a>
        <a class="link" href="${r}index.html#how">How it works</a>
        <a class="link" href="${r}index.html#compare">Compare</a>
        <a class="link active" href="${r}docs/index.html">Docs</a>
        <a class="link" href="https://github.com/Anirudhx7/ollama-mesh" target="_blank" rel="noopener noreferrer">GitHub&nbsp;↗</a>
        <a class="link" href="https://anirudh.social/ollama-mesh/demo/" target="_blank" rel="noopener noreferrer" style="color:var(--accent2);font-weight:500;">Demo&nbsp;↗</a>
      </div>
      <button class="icon-btn theme-toggle" id="themeToggle" aria-label="Toggle dark and light mode">
        <svg class="sun" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M6.34 17.66l-1.41 1.41M19.07 4.93l-1.41 1.41"/></svg>
        <svg class="moon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12.79A9 9 0 1111.21 3 7 7 0 0021 12.79z"/></svg>
      </button>
      <button class="icon-btn hamburger" id="sidebarToggle" aria-label="Open docs menu" aria-expanded="false">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M3 6h18M3 12h18M3 18h18"/></svg>
      </button>
    </div>
  </div>
</nav>

<div class="doc-shell">
  <aside class="doc-sidebar" id="docSidebar" aria-label="Documentation navigation">
    <a class="doc-nav-link home" href="${r}docs/index.html">← Docs home</a>
    <a class="doc-nav-link" href="https://anirudh.social/ollama-mesh/demo/" target="_blank" rel="noopener" style="display:flex;align-items:center;gap:6px;color:var(--accent2);margin-bottom:18px;"><span class="brand-dot" style="flex-shrink:0"></span>Live demo ↗</a>
    ${docSidebar(slug)}
  </aside>

  <main class="doc-main">
    <div class="breadcrumb" aria-label="Breadcrumb">${breadcrumb(slug, title)}</div>
    <article class="doc-content">
      ${contentHtml}
      <div class="doc-foot-edit">
        <a href="https://github.com/Anirudhx7/ollama-mesh/blob/main/docs/${slug}.md" target="_blank" rel="noopener">View on GitHub →</a>
      </div>
    </article>
  </main>

  <aside class="doc-toc" aria-label="On this page">
    <div class="toc-title">On this page</div>
    ${tocHtml(headings)}
  </aside>
</div>

<footer class="foot">
  <div class="foot-bottom">
    <span>© <span id="year">2026</span> ollama-mesh contributors · Apache-2.0</span>
    <span>ollama-mesh <span style="color:var(--accent)">{{VERSION}}</span></span>
  </div>
</footer>

<script>
(function(){"use strict";var root=document.documentElement;
try{var s=localStorage.getItem("om-theme");if(s==="light")root.classList.add("light");else if(!s&&matchMedia&&matchMedia("(prefers-color-scheme: light)").matches)root.classList.add("light");}catch(e){}
var t=document.getElementById("themeToggle");if(t)t.addEventListener("click",function(){root.classList.toggle("light");try{localStorage.setItem("om-theme",root.classList.contains("light")?"light":"dark");}catch(e){}});
var sb=document.getElementById("sidebarToggle"),side=document.getElementById("docSidebar");
if(sb&&side)sb.addEventListener("click",function(){var o=side.classList.toggle("open");sb.setAttribute("aria-expanded",String(o));});
var y=document.getElementById("year");if(y)y.textContent=new Date().getFullYear();
// active TOC on scroll
var links=[].slice.call(document.querySelectorAll(".toc a"));
var ids=links.map(function(a){return a.getAttribute("href").slice(1);});
var heads=ids.map(function(id){return document.getElementById(id);}).filter(Boolean);
function onScroll(){var top=window.scrollY+120,cur=heads[0];for(var i=0;i<heads.length;i++){if(heads[i].offsetTop<=top)cur=heads[i];}links.forEach(function(a){a.classList.toggle("active",a.getAttribute("href")==="#"+(cur&&cur.id));});}
window.addEventListener("scroll",onScroll,{passive:true});onScroll();
})();
</script>
</body>
</html>`;
}

/* ---------- docs index page ---------- */
function docsIndexPage() {
  const slug = "index-placeholder"; // depth 0 for relRoot via custom
  const r = "../";
  const cards = DOC_GROUPS.flatMap((g) => g.items.map((it) => {
    const md = readFileSync(join(DOCS_SRC, it.slug + ".md"), "utf8");
    const firstPara = (md.split("\n").find((l, idx) => l.trim() && !l.startsWith("#") && !l.startsWith(">")) || "").trim();
    return `<a class="index-card" href="${it.slug}.html"><h3>${it.label}</h3><p>${escapeHtml(firstPara.replace(/[*`\[\]]/g, "").slice(0, 130))}...</p><span class="arrow">Read →</span></a>`;
  })).join("");

  return `<!doctype html>
<html lang="en">
<head>
<meta charset="UTF-8" />
<meta name="viewport" content="width=device-width, initial-scale=1.0" />
<script>
  (function () { try { var s = localStorage.getItem("om-theme"); if (s === "light" || (!s && window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches)) document.documentElement.classList.add("light"); } catch (e) {} })();
</script>
<title>Documentation · ollama-mesh</title>
<meta name="description" content="ollama-mesh documentation -- integrations, production deployment, savings math, and use cases." />
<link rel="icon" type="image/svg+xml" href="${r}favicon.svg" />
<style>${DOC_CSS}</style>
</head>
<body>
<nav>
  <div class="page">
    <a href="${r}index.html" class="brand" aria-label="ollama-mesh home">${LOGO_HTML}<span class="name">ollama<span style="color:var(--accent)">-mesh</span></span><span class="ver">{{VERSION}}</span><span class="brand-dot" title="Active"></span></a>
    <div class="nav-right">
      <div class="nav-links">
        <a class="link" href="${r}index.html#features">Features</a>
        <a class="link" href="${r}index.html#how">How it works</a>
        <a class="link" href="${r}index.html#compare">Compare</a>
        <a class="link active" href="index.html">Docs</a>
        <a class="link" href="https://github.com/Anirudhx7/ollama-mesh" target="_blank" rel="noopener noreferrer">GitHub&nbsp;↗</a>
        <a class="link" href="https://anirudh.social/ollama-mesh/demo/" target="_blank" rel="noopener noreferrer" style="color:var(--accent2);font-weight:500;">Demo&nbsp;↗</a>
      </div>
      <button class="icon-btn theme-toggle" id="themeToggle" aria-label="Toggle dark and light mode">
        <svg class="sun" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M6.34 17.66l-1.41 1.41M19.07 4.93l-1.41 1.41"/></svg>
        <svg class="moon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12.79A9 9 0 1111.21 3 7 7 0 0021 12.79z"/></svg>
      </button>
      <button class="icon-btn hamburger" id="hamburger" aria-label="Open menu" aria-expanded="false">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M3 6h18M3 12h18M3 18h18"/></svg>
      </button>
    </div>
  </div>
</nav>
<div class="mobile-menu" id="mobileMenu" aria-hidden="true">
  <a href="${r}index.html#features">Features</a>
  <a href="${r}index.html#how">How it works</a>
  <a href="${r}index.html#compare">Compare</a>
  <a href="index.html">Docs</a>
  <a href="https://anirudh.social/ollama-mesh/demo/" target="_blank" rel="noopener noreferrer">Live demo</a>
  <a href="https://github.com/Anirudhx7/ollama-mesh" target="_blank" rel="noopener noreferrer">GitHub ↗</a>
</div>

<main class="doc-index">
  <p class="eyebrow">Documentation</p>
  <h1>Run it, route it, read the numbers.</h1>
  <p class="lead">Everything you need to put ollama-mesh in front of your cluster -- connect your tools, ship to production, and understand exactly what it's saving you.</p>
  ${DOC_GROUPS.map((g) => `
    <section class="index-section">
      <h2>${g.title}</h2>
      <div class="index-grid">
        ${g.items.map((it) => {
          const md = readFileSync(join(DOCS_SRC, it.slug + ".md"), "utf8");
          const firstPara = (md.split("\n").find((l) => l.trim() && !l.startsWith("#") && !l.startsWith(">")) || "").trim();
          return `<a class="index-card" href="${it.slug}.html"><h3>${it.label}</h3><p>${escapeHtml(firstPara.replace(/[*\`\[\]()]/g, "").replace(/https?:\S+/g,"").slice(0, 120))}...</p><span class="arrow">Read →</span></a>`;
        }).join("")}
      </div>
    </section>`).join("")}
</main>

<footer class="foot">
  <div class="foot-bottom">
    <span>© <span id="year">2026</span> ollama-mesh contributors · Apache-2.0</span>
    <span>ollama-mesh <span style="color:var(--accent)">{{VERSION}}</span></span>
  </div>
</footer>
<script>
(function(){
  var root=document.documentElement;
  try{
    var s=localStorage.getItem("om-theme");
    if(s==="light")root.classList.add("light");
    else if(!s&&matchMedia&&matchMedia("(prefers-color-scheme: light)").matches)root.classList.add("light");
  }catch(e){}
  var t=document.getElementById("themeToggle");
  if(t)t.addEventListener("click",function(){
    root.classList.toggle("light");
    try{localStorage.setItem("om-theme",root.classList.contains("light")?"light":"dark");}catch(e){}
  });
  var burger=document.getElementById("hamburger");
  var menu=document.getElementById("mobileMenu");
  function closeMenu() { menu.classList.remove("open"); burger.setAttribute("aria-expanded", "false"); menu.setAttribute("aria-hidden", "true"); }
  if(burger&&menu) {
    burger.addEventListener("click",function(){
      var open=menu.classList.toggle("open");
      burger.setAttribute("aria-expanded",String(open));
      menu.setAttribute("aria-hidden",String(!open));
    });
    menu.querySelectorAll("a").forEach(function(a){a.addEventListener("click",closeMenu);});
  }
  var y=document.getElementById("year");
  if(y)y.textContent=new Date().getFullYear();
})();
</script>
</body>
</html>`;
}

/* ---------- walk docs ---------- */
function listMd(dir, base = "") {
  const out = [];
  for (const name of readdirSync(dir)) {
    const full = join(dir, name);
    const rel = base ? base + "/" + name : name;
    if (statSync(full).isDirectory()) out.push(...listMd(full, rel));
    else if (name.endsWith(".md")) out.push(rel.replace(/\.md$/, ""));
  }
  return out;
}

function titleFromMd(md, slug) {
  const h1 = md.split("\n").find((l) => /^#\s+/.test(l));
  return h1 ? h1.replace(/^#\s+/, "").trim() : basename(slug);
}

function main() {
  // Exclude internal/design docs from the public site - these stay local (never published).
  const slugs = listMd(DOCS_SRC).filter((s) => s !== "prometheus-alerts" && !s.startsWith("design/"));
  let count = 0;
  for (const slug of slugs) {
    const md = readFileSync(join(DOCS_SRC, slug + ".md"), "utf8");
    const { html, headings } = renderMarkdown(md);
    const title = titleFromMd(md, slug);
    const outFile = join(DOCS_OUT, slug + ".html");
    mkdirSync(dirname(outFile), { recursive: true });
    writeFileSync(outFile, page({ slug, title, contentHtml: html, headings }));
    count++;
    console.log("  rendered", "docs/" + slug + ".html");
  }
  // index
  mkdirSync(DOCS_OUT, { recursive: true });
  writeFileSync(join(DOCS_OUT, "index.html"), docsIndexPage());
  console.log("  rendered docs/index.html");
  console.log(`\n built ${count} doc page(s) + index into website/docs/`);
}
main();
