// shell.js — the persistent app shell + client router for the listening pages.
//
// Phase 1 step 2 of docs/plans/persistent-shell-playback.md. The server still
// renders complete pages; this intercepts same-origin nav clicks, fetches the
// target, and swaps only the page-content slots (<main>, the header-insert
// region, the active nav link, <title>) — the header, player-bar and <audio>
// outside <main> are never torn down, so playback survives navigation.
//
// Each page ships `<body data-page=… data-module=/static/js/x.js>`; the shell
// imports that module and runs its exported init()/teardown(). Anything that is
// not shell-native (no data-module) falls back to a full browser navigation.
import { initAuth } from './auth.js';

// ── Theme (persistent header — applied once for every shell page) ────────────
const VALID_THEMES = new Set(['dark', 'light', 'ocean', 'sunset']);
function applyTheme(name) {
  if (!VALID_THEMES.has(name)) name = 'dark';
  document.documentElement.dataset.theme = name;
  localStorage.setItem('madshare-theme', name);
  document.querySelectorAll('.theme-dot').forEach(d => {
    const on = d.dataset.theme === name;
    d.classList.toggle('active', on);
    d.setAttribute('aria-pressed', String(on));
  });
}
function wireTheme() {
  applyTheme(localStorage.getItem('madshare-theme') || 'dark');
  // The theme switcher lives in the persistent header, so wire it once.
  document.querySelectorAll('.theme-dot').forEach(dot =>
    dot.addEventListener('click', () => applyTheme(dot.dataset.theme)));
}

// ── Page-module lifecycle ────────────────────────────────────────────────────
let current = null; // { module }

async function runModule() {
  const src = document.body.dataset.module;
  if (!src) return;
  const mod = await import(src);   // ES modules are cached: re-visits reuse it
  current = { module: mod };
  await mod.init?.();
}
function teardownCurrent() {
  try { current?.module.teardown?.(); } catch (e) { console.error('teardown:', e); }
  current = null;
}

// ── Navigation / swap ────────────────────────────────────────────────────────
const shellNative = doc => !!doc.body?.dataset.module;

async function navigate(url, { push = true } = {}) {
  let res;
  try { res = await fetch(url); }
  catch { location.assign(url); return; }              // network error → full nav
  if (!res.ok) { location.assign(url); return; }

  const doc = new DOMParser().parseFromString(await res.text(), 'text/html');
  if (!shellNative(doc)) { location.assign(url); return; } // not a shell page

  teardownCurrent();
  ensureStylesheets(doc);
  swapMain(doc);
  swapHeaderInsert(doc);
  document.title = doc.title;
  document.body.dataset.page   = doc.body.dataset.page   || '';
  document.body.dataset.module = doc.body.dataset.module || '';
  setActiveNav(new URL(url, location.origin).pathname);
  if (push) {
    // Re-navigating to the same URL (e.g. clicking the active nav link) shouldn't
    // pile up duplicate history entries.
    if (new URL(url, location.origin).href === location.href) history.replaceState({}, '', url);
    else history.pushState({}, '', url);
  }
  focusMain();
  await runModule();
}

// inject-and-keep: add the target page's stylesheets if absent, never remove.
function ensureStylesheets(doc) {
  const have = new Set([...document.querySelectorAll('head link[rel="stylesheet"]')]
    .map(l => l.getAttribute('href')));
  doc.querySelectorAll('head link[rel="stylesheet"]').forEach(link => {
    const href = link.getAttribute('href');
    if (href && !have.has(href)) {
      const l = document.createElement('link');
      l.rel = 'stylesheet';
      l.href = href;
      document.head.appendChild(l);
    }
  });
}

function swapMain(doc) {
  const cur = document.querySelector('main');
  const next = doc.querySelector('main');
  if (cur && next) cur.replaceWith(next);
}

// The header-insert region (e.g. the library search bar) is page-specific. Its
// wrapper #header-insert is `display:contents`, so swapping it doesn't disturb
// the header layout.
function swapHeaderInsert(doc) {
  const cur = document.getElementById('header-insert');
  const next = doc.getElementById('header-insert');
  if (cur && next) cur.replaceWith(next);
  else if (cur) cur.replaceChildren();
}

function setActiveNav(pathname) {
  document.querySelectorAll('.main-nav .nav-link').forEach(a => {
    const match = new URL(a.href, location.origin).pathname === pathname;
    a.classList.toggle('nav-link--active', match);
    if (match) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  });
}

// Screen readers don't announce a JS swap, so move focus to the new <main> (it
// gets a -1 tabindex) to mirror a real navigation.
function focusMain() {
  const main = document.querySelector('main');
  if (!main) return;
  main.setAttribute('tabindex', '-1');
  main.focus({ preventScroll: true });
}

// ── Link interception ────────────────────────────────────────────────────────
document.addEventListener('click', e => {
  if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  const a = e.target.closest('a');
  if (!a || a.target === '_blank' || a.hasAttribute('download')) return;
  const url = new URL(a.href, location.origin);
  if (url.origin !== location.origin) return;
  // Let true in-page anchors (same path + a #hash) behave normally; a same-path
  // link without a hash (e.g. the active "Library" nav) still re-swaps the page.
  if (url.hash && url.pathname === location.pathname && url.search === location.search) return;
  e.preventDefault();
  navigate(url.pathname + url.search);
});

window.addEventListener('popstate', () => {
  navigate(location.pathname + location.search, { push: false });
});

// ── Boot ─────────────────────────────────────────────────────────────────────
(async function boot() {
  wireTheme();
  await initAuth();        // once for the document's lifetime
  await runModule();       // the server already rendered this page; just init it
})();
