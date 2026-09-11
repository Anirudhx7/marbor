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
import { readFileSync, writeFileSync, mkdirSync, readdirSync, statSync } from "node:fs";
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
function escapeAttr(s) {
  return escapeHtml(s).replace(/"/g, "&quot;");
}
// Allowlist of schemes safe to emit as an href/src on the static docs site -
// javascript:/data: etc. in a community-authored docs PR must never become a
// clickable/renderable anchor (P386). A value with no scheme prefix at all
// (relative path or #fragment) is always safe.
function isSafeUrl(url) {
  if (/^[a-zA-Z][a-zA-Z0-9+.-]*:/.test(url)) {
    return /^https:/i.test(url) || /^mailto:/i.test(url);
  }
  return true;
}
function inline(s) {
  // code spans first (protect their contents)
  const codes = [];
  s = s.replace(/`([^`]+)`/g, (_, c) => {
    codes.push(`<code>${escapeHtml(c)}</code>`);
    return `__CODE_PLACEHOLDER_${codes.length - 1}__`;
  });
  s = escapeHtml(s);
  // images ![alt](url)
  s = s.replace(/!\[([^\]]*)\]\(([^)\s]+)\)/g, (_, alt, src) =>
    `<img src="${escapeHtml(isSafeUrl(src) ? src : "#")}" alt="${escapeHtml(alt)}" loading="lazy">`
  );
  // links [text](url)
  s = s.replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, (_, t, u) => {
    let href = u;
    // rewrite intra-doc .md links to .html (skip external links)
    if (!/^https?:/.test(href) && (/\.md(#|$)/.test(href) || href.startsWith("../") || href.startsWith("./"))) {
      href = href.replace(/\.md(#|$)/, ".html$1");
    }
    if (!isSafeUrl(href)) href = "#";
    const ext = /^https?:/.test(href) ? ' target="_blank" rel="noopener"' : "";
    return `<a href="${href}"${ext}>${t}</a>`;
  });
  s = s.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
  s = s.replace(/(^|[^*])\*([^*]+)\*/g, "$1<em>$2</em>");
  // restore code spans
  s = s.replace(/__CODE_PLACEHOLDER_(\d+)__/g, (_, i) => codes[+i]);
  return s;
}

const usedIds = new Map();
function slugify(s) {
  let id = s.toLowerCase().replace(/[^\w\s-]/g, "").trim().replace(/\s+/g, "-");
  const count = usedIds.get(id) || 0;
  usedIds.set(id, count + 1);
  return count ? `${id}-${count}` : id;
}

function renderMarkdown(md) {
  usedIds.clear();
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
      const raw = buf.join("\n");
      html += `<div class="code-wrap"><pre class="code"${lang ? ` data-lang="${lang}"` : ""}><code>${escapeHtml(raw)}</code></pre><button class="copy-btn" aria-label="Copy code" data-copy="${escapeAttr(raw)}"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 01-2-2V4a2 2 0 012-2h9a2 2 0 012 2v1"/></svg><span>copy</span></button></div>\n`;
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
      html += `<div class="table-scroll" tabindex="0" role="region" aria-label="Data table: scroll horizontally on small screens"><table>\n<thead><tr>${head.map((c) => `<th>${inline(c)}</th>`).join("")}</tr></thead>\n<tbody>${rows.map((r) => `<tr>${r.map((c) => `<td>${inline(c)}</td>`).join("")}</tr>`).join("\n")}</tbody>\n</table></div>\n`;
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
const LOGO_HTML = `<span class="brand-chip" aria-hidden="true"><svg width="22" height="22" viewBox="0 0 100 100" fill="none"><path d="M30 35 L30 65 M30 50 L50 35 L50 65 M50 50 L70 35 L70 65" stroke="currentColor" stroke-width="8" stroke-linecap="round" stroke-linejoin="round"/><circle cx="75" cy="75" r="8" fill="currentColor" opacity=".65"/></svg></span>`;
const BRAND_HTML = (r) => `<a href="${r}index.html" class="brand" aria-label="Marbor home">${LOGO_HTML}<span class="brand-name">Marbor</span><span class="brand-ver">{{VERSION}}</span></a>`;
const GH_SVG = `<svg width="15" height="15" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0016 8c0-4.42-3.58-8-8-8z"/></svg>`;
const EXT_IC = `<svg class="ext-ic" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M7 17L17 7M7 7h10v10"/></svg>`;

// Word-boundary excerpt for cards: never cut mid-word, no trailing "..." when short.
function excerpt(s, n) {
  const t = String(s).replace(/\s+/g, " ").trim();
  if (t.length <= n) return t;
  const cut = t.slice(0, n);
  const at = cut.lastIndexOf(" ");
  return (at > 1 ? cut.slice(0, at) : cut).trimEnd() + "...";
}

// Shared footer - identical to the landing page footer (paths relative via r).
function siteFooter(r) {
  return `<footer><div class="page foot-grid"><div>${BRAND_HTML(r)}<div style="margin-top:10px">© <span class="yr">2026</span> Anirudh Mehandru · Apache-2.0</div></div><div class="foot-links"><a href="${r}index.html#install">Install</a><a href="${r}index.html#features">Features</a><a href="${r}index.html#how">How</a><a href="${r}index.html#compare">Compare</a><a href="${r}docs/index.html">Docs</a><a href="https://marbor.in/demo/" target="_blank" rel="noopener">Demo</a><a href="https://github.com/Anirudhx7/marbor" target="_blank" rel="noopener">GitHub</a></div></div></footer>`;
}

// All docs in nav order, grouped.
const DOC_GROUPS = [
  { title: "Guides", items: [
    { slug: "INTEGRATIONS", label: "Integrations" },
    { slug: "LIMITATIONS", label: "Known limitations" },
    { slug: "SAVINGS-MATH", label: "Savings math" },
    { slug: "USE-CASES", label: "Use cases" },
    { slug: "backup", label: "Backup & Restore" },
  ]},
  { title: "Deployment", items: [
    { slug: "PRODUCTION", label: "Production" },
    { slug: "deploy/aws-ec2", label: "AWS EC2" },
    { slug: "deploy/gpu-node-registration", label: "GPU node registration" },
    { slug: "deploy/marbor-agent-enrollment", label: "marbor agent enrollment" },
  ]},
  { title: "Integrations", items: [
    { slug: "integrations/continue", label: "Continue" },
    { slug: "integrations/librechat", label: "LibreChat" },
    { slug: "integrations/litellm", label: "LiteLLM" },
    { slug: "integrations/open-webui", label: "Open WebUI" },
  ]},
  { title: "Reference", items: [
    { slug: "cli", label: "CLI reference" },
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
        const active = it.slug === currentSlug;
        return `<a class="doc-nav-link${active ? " active" : ""}"${active ? ' aria-current="page"' : ""} href="${r}docs/${it.slug}.html">${it.label}</a>`;
      }).join("")}
    </div>`).join("");
}

function tocHtml(headings) {
  if (!headings.length) return `<p class="toc-empty">No sections.</p>`;
  return `<ul class="toc">${headings.map((h) =>
    `<li class="lvl-${h.level}"><a href="#${h.id}">${escapeHtml(h.text)}</a></li>`
  ).join("")}</ul>`;
}

// Prev/next pager across all grouped pages in nav order, plus a demoted
// centered source link underneath.
function pagerHtml(slug) {
  const flat = DOC_GROUPS.flatMap((g) => g.items);
  const i = flat.findIndex((it) => it.slug === slug);
  if (i < 0) return "";
  const r = relRoot(slug);
  const card = (it, dir) => it
    ? `<a class="page-card ${dir}" rel="${dir}" href="${r}docs/${it.slug}.html"><span class="dir">${dir === "prev" ? "← Previous" : "Next →"}</span><span class="lbl">${escapeHtml(it.label)}</span></a>`
    : `<span class="page-card missing" aria-hidden="true"></span>`;
  return `<nav class="pager" aria-label="Documentation pages">${card(flat[i - 1], "prev")}${card(flat[i + 1], "next")}</nav>
      <div class="edit-link"><a href="https://github.com/Anirudhx7/marbor/blob/main/docs/${slug}.md" target="_blank" rel="noopener">View on GitHub →</a></div>`;
}

function breadcrumb(slug, title) {
  const r = relRoot(slug);
  const parts = slug.split("/");
  const crumbs = [`<li><a href="${r}index.html">Home</a></li>`, `<li><a href="${r}docs/index.html">Docs</a></li>`];
  if (parts.length > 1) crumbs.push(`<li><span>${parts[0]}</span></li>`);
  crumbs.push(`<li><span class="current" aria-current="page">${escapeHtml(title)}</span></li>`);
  return crumbs.join('<li aria-hidden="true"><span class="sep">/</span></li>');
}

const DOC_CSS = readFileSync(join(__dirname, "docs.css"), "utf8");

function page({ slug, title, contentHtml, headings }) {
  const r = relRoot(slug);
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="UTF-8" />
<meta name="viewport" content="width=device-width, initial-scale=1.0, viewport-fit=cover" />
<meta name="color-scheme" content="dark light" />
<script defer data-domain="marbor.in,anirudh.social" src="https://plausible.io/js/script.tagged-events.js"><\/script>
<script>
  (function () { try { var s = localStorage.getItem("marbor-theme")||localStorage.getItem("om-theme"); if (s === "light" || (!s && window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches)) document.documentElement.classList.add("light"); } catch (e) {} })();
</script>
<title>${escapeHtml(title)} · Marbor docs</title>
<meta name="description" content="Marbor documentation: ${escapeHtml(title)}." />
<link rel="icon" type="image/svg+xml" href="${r}favicon.svg" />
<meta property="og:type" content="article" />
<meta property="og:site_name" content="Marbor" />
<meta property="og:title" content="${escapeHtml(title)} · Marbor docs" />
<meta property="og:description" content="Marbor documentation: ${escapeHtml(title)}." />
<meta property="og:image" content="https://marbor.in/screenshots/dashboard.png" />
<meta property="og:image:alt" content="marbor admin dashboard" />
<meta name="twitter:card" content="summary_large_image" />
<meta name="twitter:title" content="${escapeHtml(title)} · Marbor docs" />
<meta name="twitter:image" content="https://marbor.in/screenshots/dashboard.png" />
<link rel="preconnect" href="https://fonts.googleapis.com" />
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin />
<link href="https://fonts.googleapis.com/css2?family=Plus+Jakarta+Sans:wght@400..700&family=Playfair+Display:ital,wght@0,400..800;1,400..800&family=JetBrains+Mono:wght@400..700&display=swap" rel="stylesheet" />
<meta name="theme-color" content="#d4a853" />
<style>${DOC_CSS}</style>
</head>
<body>
<a class="skip" href="#main">Skip to content</a>
<header class="nav"><div class="nav-inner">
${BRAND_HTML(r)}
<nav class="nav-links" aria-label="Primary"><a class="nl" href="${r}index.html#features">Features</a><a class="nl" href="${r}index.html#how">How it works</a><a class="nl" href="${r}index.html#compare">Compare</a><a class="nl on" href="${r}docs/index.html">Docs</a><a class="btn btn-gold btn-sm nav-cta" href="https://marbor.in/demo/" target="_blank" rel="noopener">Live demo →</a><button type="button" class="icon-btn" id="themeBtn" aria-label="Toggle theme" aria-pressed="false">◐</button><button type="button" class="icon-btn hamb" id="hamb" aria-label="Menu" aria-expanded="false" aria-controls="mmenu">☰</button></nav>
</div><nav class="mobile-menu" id="mmenu" aria-label="Mobile"><a href="${r}index.html#features">Features</a><a href="${r}index.html#how">How it works</a><a href="${r}index.html#compare">Compare</a><a href="${r}docs/index.html">Docs</a><a href="https://marbor.in/demo/" target="_blank" rel="noopener">Live demo</a><a href="https://github.com/Anirudhx7/marbor">GitHub</a></nav></header>

<div class="doc-shell">
<nav class="doc-sidebar" id="docSidebar" aria-label="Documentation navigation">
<div class="doc-side-head">Docs · <b>index</b></div>
<div class="doc-side-body">
    <a class="doc-nav-link home" href="${r}docs/index.html">← Docs home</a>
    ${docSidebar(slug)}
</div>
</nav>

  <main class="doc-main" id="main">
    <article class="doc-content doc-card"><div class="doc-card-body">
      <nav class="breadcrumb" aria-label="Breadcrumb"><ol>${breadcrumb(slug, title)}</ol></nav>
      ${contentHtml}
      <div class="doc-foot-edit">
        ${pagerHtml(slug)}
      </div>
    </div></article>
  </main>

  <div class="doc-toc-wrap"><aside class="toc-card" aria-label="On this page">
    <p class="toc-title">On this page</p>
    ${tocHtml(headings)}
  </aside></div>
</div>

${siteFooter(r)}

<script>
(function(){var b=document.getElementById('themeBtn');function syncT(){if(!b)return;var l=document.documentElement.classList.contains('light');b.setAttribute('aria-pressed',l?'true':'false');b.textContent=l?'☀':'◐';syncMetaTheme();}function syncMetaTheme(){try{var cs=getComputedStyle(document.documentElement);var m=document.querySelector('meta[name="theme-color"]');if(m){var bg=cs.getPropertyValue('--bg').trim();if(bg)m.setAttribute('content',bg);}}catch(e){}}if(b)b.addEventListener('click',function(){var h=document.documentElement;h.classList.toggle('light');try{localStorage.setItem('marbor-theme',h.classList.contains('light')?'light':'dark');}catch(e){}syncT();});syncT();
var hb=document.getElementById('hamb'),mm=document.getElementById('mmenu');if(hb&&mm){function setMenu(o){mm.classList.toggle('open',o);hb.setAttribute('aria-expanded',o?'true':'false');hb.textContent=o?'✕':'☰';}hb.addEventListener('click',function(){setMenu(!mm.classList.contains('open'));});mm.querySelectorAll('a').forEach(function(a){a.addEventListener('click',function(){setMenu(false);});});document.addEventListener('keydown',function(ev){if(ev.key==='Escape'&&mm.classList.contains('open')){setMenu(false);hb.focus();}});window.addEventListener('resize',function(){if(window.innerWidth>640)setMenu(false);});}
document.querySelectorAll('.yr').forEach(function(y){y.textContent=new Date().getFullYear();});
var io;try{io=new IntersectionObserver(function(es){es.forEach(function(e){if(e.isIntersecting){e.target.classList.add('in');io.unobserve(e.target);}});},{threshold:.12});document.querySelectorAll('[data-reveal]').forEach(function(el){io.observe(el);});}catch(e){}
var nv=document.querySelector('.nav');if(nv){var tick=false;function paintNav(){nv.classList.toggle('scrolled',window.scrollY>120);}function onScrollNav(){if(tick)return;tick=true;requestAnimationFrame(function(){paintNav();tick=false;});}window.addEventListener('scroll',onScrollNav,{passive:true});paintNav();var navInner=nv.querySelector('.nav-inner');try{nv.style.transition='none';if(navInner)navInner.style.transition='none';}catch(e){}function enableNavTrans(){try{nv.style.transition='';if(navInner)navInner.style.transition='';}catch(e){}}window.addEventListener('load',function(){setTimeout(function(){paintNav();enableNavTrans();},80);});setTimeout(enableNavTrans,2500);}
var dmq=window.matchMedia?window.matchMedia('(prefers-reduced-motion: reduce)'):null;function dmReduced(){return !!(dmq&&dmq.matches);}if(!dmReduced()){try{document.documentElement.style.scrollBehavior='auto';}catch(e){}}try{document.querySelectorAll('a[href^="#"]').forEach(function(a){var id=a.getAttribute('href');if(!id||id.length<2)return;var t=document.querySelector(id);if(!t)return;a.addEventListener('click',function(ev){ev.preventDefault();var y=t.getBoundingClientRect().top+window.scrollY-96;function focusT(){try{t.setAttribute('tabindex','-1');t.focus({preventScroll:true});}catch(e){}}if(dmReduced()){window.scrollTo(0,y);focusT();return;}var from=window.scrollY,d=y-from;if(Math.abs(d)<4){focusT();return;}var start=null;function step(ts){if(!start)start=ts;var p=Math.min((ts-start)/900,1);var e=p<.5?4*p*p*p:1-Math.pow(-2*p+2,3)/2;try{window.scrollTo(0,from+d*e);}catch(e){window.scrollTo(0,y);focusT();return;}if(p<1){requestAnimationFrame(step);}else{focusT();}}requestAnimationFrame(step);});});}catch(e){}
document.querySelectorAll(".copy-btn").forEach(function(btn){btn.addEventListener("click",function(){var text=btn.dataset.copy||"";var done=function(){if(btn.dataset.done)return;btn.dataset.done="1";var old=btn.innerHTML;btn.classList.add("copied");try{btn.setAttribute("aria-label","Copied!");}catch(e){}btn.innerHTML='<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><path d="M20 6L9 17l-5-5"/></svg><span>copied!</span>';setTimeout(function(){btn.classList.remove("copied");btn.innerHTML=old;try{btn.setAttribute("aria-label","Copy code");}catch(e){}delete btn.dataset.done;},1600);};if(navigator.clipboard&&navigator.clipboard.writeText)navigator.clipboard.writeText(text).then(done).catch(done);else{var ta=document.createElement("textarea");ta.value=text;document.body.appendChild(ta);ta.select();try{document.execCommand("copy");}catch(e){}document.body.removeChild(ta);done();}});});
// active TOC on scroll
var links=[].slice.call(document.querySelectorAll(".toc a"));
var ids=links.map(function(a){return a.getAttribute("href").slice(1);});
var heads=ids.map(function(id){return document.getElementById(id);}).filter(Boolean);
function onScroll(){var top=window.scrollY+140,cur=heads[0];for(var i=0;i<heads.length;i++){if(heads[i].offsetTop<=top)cur=heads[i];}links.forEach(function(a){var on=a.getAttribute("href")==="#"+(cur&&cur.id);a.classList.toggle("active",on);if(on)a.setAttribute("aria-current","true");else a.removeAttribute("aria-current");});}
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
  const total = DOC_GROUPS.reduce((a, g) => a + g.items.length, 0);

  return `<!doctype html>
<html lang="en">
<head>
<meta charset="UTF-8" />
<meta name="viewport" content="width=device-width, initial-scale=1.0, viewport-fit=cover" />
<meta name="color-scheme" content="dark light" />
<script defer data-domain="marbor.in,anirudh.social" src="https://plausible.io/js/script.tagged-events.js"><\/script>
<script>
  (function () { try { var s = localStorage.getItem("marbor-theme")||localStorage.getItem("om-theme"); if (s === "light" || (!s && window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches)) document.documentElement.classList.add("light"); } catch (e) {} })();
</script>
<title>Documentation · Marbor</title>
<meta name="description" content="Marbor documentation -- integrations, production deployment, savings math, and use cases." />
<link rel="icon" type="image/svg+xml" href="${r}favicon.svg" />
<link rel="preconnect" href="https://fonts.googleapis.com" />
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin />
<link href="https://fonts.googleapis.com/css2?family=Plus+Jakarta+Sans:wght@400..700&family=Playfair+Display:ital,wght@0,400..800;1,400..800&family=JetBrains+Mono:wght@400..700&display=swap" rel="stylesheet" />
<meta name="theme-color" content="#d4a853" />
<style>${DOC_CSS}</style>
</head>
<body>
<a class="skip" href="#main">Skip to content</a>
<header class="nav"><div class="nav-inner">
${BRAND_HTML(r)}
<nav class="nav-links" aria-label="Primary"><a class="nl" href="${r}index.html#features">Features</a><a class="nl" href="${r}index.html#how">How it works</a><a class="nl" href="${r}index.html#compare">Compare</a><a class="nl on" href="index.html">Docs</a><a class="btn btn-gold btn-sm nav-cta" href="https://marbor.in/demo/" target="_blank" rel="noopener">Live demo →</a><button type="button" class="icon-btn" id="themeBtn" aria-label="Toggle theme" aria-pressed="false">◐</button><button type="button" class="icon-btn hamb" id="hamb" aria-label="Menu" aria-expanded="false" aria-controls="mmenu">☰</button></nav>
</div><nav class="mobile-menu" id="mmenu" aria-label="Mobile"><a href="${r}index.html#install">Install</a><a href="${r}index.html#features">Features</a><a href="${r}index.html#how">How it works</a><a href="${r}index.html#compare">Compare</a><a href="index.html">Docs</a><a href="https://marbor.in/demo/" target="_blank" rel="noopener">Live demo</a><a href="https://github.com/Anirudhx7/marbor" target="_blank" rel="noopener">GitHub</a></nav></header>

<main class="doc-index" id="main">
<div class="hero" data-reveal>
  <h1>Run it, <em>route it</em>, read the numbers.</h1>
  <p class="lede">Everything you need to put Marbor in front of your cluster -- connect your tools, ship to production, and understand exactly what it's saving you.</p>
  <nav class="breadcrumb" aria-label="Breadcrumb"><ol><li><a href="../index.html">Home</a></li><li aria-hidden="true"><span class="sep">/</span></li><li><span class="current" aria-current="page">Docs</span></li></ol></nav>
</div>
<div class="doc-shell two-col">
<nav class="doc-sidebar" aria-label="Documentation navigation" data-reveal>
<div class="doc-side-head">Docs · <b>${total} pages</b></div>
<div class="doc-side-body">
${DOC_GROUPS.map((g) => `
    <div class="nav-group"><p class="nav-group-title">${g.title}</p>${g.items.map((it) => `<a class="doc-nav-link" href="${it.slug}.html">${it.label}</a>`).join("")}</div>`).join("")}
</div>
</nav>
<div class="doc-main">
  ${DOC_GROUPS.map((g) => `
    <section class="doc-card" data-reveal>
      <div class="doc-card-head"><span class="t">${g.title} · <b>${g.items.length} page${g.items.length === 1 ? "" : "s"}</b></span></div>
      <div class="doc-card-body"><div class="index-grid">
        ${g.items.map((it) => {
          const md = readFileSync(join(DOCS_SRC, it.slug + ".md"), "utf8");
          const firstPara = (md.split("\n").find((l) => l.trim() && !l.startsWith("#") && !l.startsWith(">")) || "").trim();
        return `<a class="index-card" href="${it.slug}.html"><div class="index-card-top"><span class="index-card-count">${g.title}</span></div><h3>${it.label}</h3><p>${escapeHtml(excerpt(firstPara.replace(/[*\`\[\]()]/g, "").replace(/https?:\S+/g,""), 120))}</p><span class="arrow">Read →</span></a>`;
      }).join("")}
      </div></div>
    </section>`).join("")}
  <div class="cta-band" data-reveal><div><h2>Find out what your fleet is actually doing.</h2><p>Install in one command. Point your OpenAI client at Marbor. Watch the first placed request.</p></div><div class="cta-act"><a class="btn btn-gold" href="${r}index.html#install">↓ Install Marbor</a><a class="docs-link" href="${r}index.html#features">Explore features →</a></div></div>
</div>
</div>
</main>

${siteFooter(r)}
<script>
(function(){var b=document.getElementById('themeBtn');function syncT(){if(!b)return;var l=document.documentElement.classList.contains('light');b.setAttribute('aria-pressed',l?'true':'false');b.textContent=l?'☀':'◐';syncMetaTheme();}function syncMetaTheme(){try{var cs=getComputedStyle(document.documentElement);var m=document.querySelector('meta[name="theme-color"]');if(m){var bg=cs.getPropertyValue('--bg').trim();if(bg)m.setAttribute('content',bg);}}catch(e){}}if(b)b.addEventListener('click',function(){var h=document.documentElement;h.classList.toggle('light');try{localStorage.setItem('marbor-theme',h.classList.contains('light')?'light':'dark');}catch(e){}syncT();});syncT();
var hb=document.getElementById('hamb'),mm=document.getElementById('mmenu');if(hb&&mm){function setMenu(o){mm.classList.toggle('open',o);hb.setAttribute('aria-expanded',o?'true':'false');hb.textContent=o?'✕':'☰';}hb.addEventListener('click',function(){setMenu(!mm.classList.contains('open'));});mm.querySelectorAll('a').forEach(function(a){a.addEventListener('click',function(){setMenu(false);});});document.addEventListener('keydown',function(ev){if(ev.key==='Escape'&&mm.classList.contains('open')){setMenu(false);hb.focus();}});window.addEventListener('resize',function(){if(window.innerWidth>640)setMenu(false);});}
document.querySelectorAll('.yr').forEach(function(y){y.textContent=new Date().getFullYear();});
var io;try{io=new IntersectionObserver(function(es){es.forEach(function(e){if(e.isIntersecting){e.target.classList.add('in');io.unobserve(e.target);}});},{threshold:.12});document.querySelectorAll('[data-reveal]').forEach(function(el){el.classList.add('in');});}catch(e){}
var nv=document.querySelector('.nav');if(nv){var tick=false;function paintNav(){nv.classList.toggle('scrolled',window.scrollY>120);}function onScrollNav(){if(tick)return;tick=true;requestAnimationFrame(function(){paintNav();tick=false;});}window.addEventListener('scroll',onScrollNav,{passive:true});paintNav();var navInner=nv.querySelector('.nav-inner');try{nv.style.transition='none';if(navInner)navInner.style.transition='none';}catch(e){}function enableNavTrans(){try{nv.style.transition='';if(navInner)navInner.style.transition='';}catch(e){}}window.addEventListener('load',function(){setTimeout(function(){paintNav();enableNavTrans();},80);});setTimeout(enableNavTrans,2500);}
var dmq=window.matchMedia?window.matchMedia('(prefers-reduced-motion: reduce)'):null;function dmReduced(){return !!(dmq&&dmq.matches);}if(!dmReduced()){try{document.documentElement.style.scrollBehavior='auto';}catch(e){}}try{document.querySelectorAll('a[href^="#"]').forEach(function(a){var id=a.getAttribute('href');if(!id||id.length<2)return;var t=document.querySelector(id);if(!t)return;a.addEventListener('click',function(ev){ev.preventDefault();var y=t.getBoundingClientRect().top+window.scrollY-96;function focusT(){try{t.setAttribute('tabindex','-1');t.focus({preventScroll:true});}catch(e){}}if(dmReduced()){window.scrollTo(0,y);focusT();return;}var from=window.scrollY,d=y-from;if(Math.abs(d)<4){focusT();return;}var start=null;function step(ts){if(!start)start=ts;var p=Math.min((ts-start)/900,1);var e=p<.5?4*p*p*p:1-Math.pow(-2*p+2,3)/2;try{window.scrollTo(0,from+d*e);}catch(e){window.scrollTo(0,y);focusT();return;}if(p<1){requestAnimationFrame(step);}else{focusT();}}requestAnimationFrame(step);});});}catch(e){}
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
  // Every rendered page must be linked from the index + sidebar (both render
  // from DOC_GROUPS). Fail loudly so an ungrouped page can never silently
  // ship unlinked again (this script runs in pages.yml CI).
  const grouped = new Set(DOC_GROUPS.flatMap((g) => g.items.map((it) => it.slug)));
  const orphans = slugs.filter((s) => !grouped.has(s));
  if (orphans.length) {
    console.error(`build-docs: ${orphans.length} rendered page(s) missing from DOC_GROUPS (unlinked from index + sidebar): ${orphans.join(", ")}`);
    process.exit(1);
  }
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
