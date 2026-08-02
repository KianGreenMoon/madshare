// shell.js — the persistent app shell + client router for the listening pages.
//
// Phase 1 step 2 of docs/ui/shells.md. The server still
// renders complete pages; this intercepts same-origin nav clicks, fetches the
// target, and swaps only the page-content slots (<main>, the header-insert
// region, the active nav link, <title>) — the header, player-bar and <audio>
// outside <main> are never torn down, so playback survives navigation.
//
// Each page ships `<body data-page=… data-module=/static/js/x.js>`; the shell
// imports that module and runs its exported init()/teardown(). Anything that is
// not shell-native (no data-module) falls back to a full browser navigation.
import { initAuth, openLoginModal, applyNavPermissions } from './auth.js';
import { getController } from './player-controller.js';
import { initQueuePanel } from './queue-panel.js';
import { ensureLiked, isLiked, toggleLike, trackKey, onLikedChange } from './favorites.js';
import { initAboutMenu } from './about-menu.js';
import { initNavMenu } from './nav-menu.js';
import { showToast } from './toast.js';
import { applyTheme, currentTheme } from './theme.js';

// ── Theme ────────────────────────────────────────────────────────────────────
// Theme is single-sourced in theme.js and CHANGED on the Settings page
// (settings.js) — the header no longer carries the switcher dots. The inline
// <head> guard already set data-theme before first paint; this re-applies the
// saved value defensively on boot.
function wireTheme() {
  applyTheme(currentTheme());
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
  // The server may forward the request rather than answer it — "/" is a front
  // door, not a page (webui.homeHandler). fetch follows that hop transparently,
  // so adopt the URL it settled on: pushing the one we asked for would leave the
  // address bar naming a page we are not showing, and a reload would bounce again.
  if (res.redirected) {
    const final = new URL(res.url);
    if (final.origin === location.origin) url = final.pathname + final.search;
  }

  const doc = new DOMParser().parseFromString(await res.text(), 'text/html');
  if (!shellNative(doc)) { location.assign(url); return; } // not a shell page

  teardownCurrent();
  ensureStylesheets(doc);
  swapMain(doc);
  swapHeaderInsert(doc);
  // The freshly-swapped <main> carries the server-rendered subtab bar with every
  // tab present; re-gate it for the current identity (e.g. drop Playlists for a
  // principal without content.access). initAuth ran this once at boot for the
  // first page; each swap needs it again.
  applyNavPermissions();
  document.title = doc.title;
  document.body.dataset.page    = doc.body.dataset.page    || '';
  document.body.dataset.module  = doc.body.dataset.module  || '';
  document.body.dataset.section = doc.body.dataset.section || '';
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

// A header tab can stand for a whole section (e.g. "Library" covers the Music and
// Playlists subtabs): a link carrying data-section is active whenever the current
// page's section (body[data-section], copied from the swapped doc) matches it,
// regardless of the exact subtab path. Other links fall back to an exact path match.
// Covers both the left main-nav and the right-side username link (.user-area, which
// points to /settings) — all .nav-link anchors in the header.
function setActiveNav(pathname) {
  const section = document.body.dataset.section || '';
  document.querySelectorAll('header .nav-link').forEach(a => {
    const linkSection = a.dataset.section || '';
    const match = linkSection
      ? linkSection === section
      : new URL(a.href, location.origin).pathname === pathname;
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
  // Admin pages are never shell-native; navigate() would fetch the document
  // only to fall back to a full load. Skip interception and let the browser
  // navigate directly (this is where the same-tab admin entry happens).
  if (url.pathname === '/admin' || url.pathname.startsWith('/admin/')) return;
  // Let true in-page anchors (same path + a #hash) behave normally; a same-path
  // link without a hash (e.g. the active "Library" nav) still re-swaps the page.
  if (url.hash && url.pathname === location.pathname && url.search === location.search) return;
  e.preventDefault();
  navigate(url.pathname + url.search);
});

window.addEventListener('popstate', () => {
  navigate(location.pathname + location.search, { push: false });
});

// ── Toasts ───────────────────────────────────────────────────────────────────
// The implementation lives in toast.js (the one shared system). Re-export it so
// existing `import { showToast } from './shell.js'` callers (app.js) keep working
// unchanged; shell features below (queue replace-with-undo) use it directly.
export { showToast } from './toast.js';

// ── Player / queue (shell-owned: survives every swap) ────────────────────────
// The controller singleton is created here so the queue, the panel, and the
// localStorage resume work on every listening page — not just the library.
function wirePlayer() {
  const controller = getController();
  // Session expired mid-playback: the <audio> fetch bypasses the router, so the
  // controller probes and reports it here — surface the login modal.
  controller.on('autherror', openLoginModal);
  // A manually edited queue was replaced by a track click (Decision §5): a brief
  // notice. The un-replace also lives on as the queue panel's Restore button
  // (controller.canRestore/restoreQueue), so this toast's lifetime doesn't gate
  // it — but offer a one-click restore inline too. restoreQueue() no-ops safely
  // if the stash was already cleared (e.g. the new queue was edited meanwhile).
  controller.on('queuereplaced', () => {
    showToast('Queue replaced — restore from the queue panel,', {
      actionLabel: 'or click here',
      onAction: () => controller.restoreQueue(),
    });
  });
  initQueuePanel(controller, showToast);
  wireLikeButton(controller);
}

// The player-bar heart (playlists.md Decision §8): likes the CURRENT track,
// synced with the row hearts through the shared liked-set in favorites.js.
function wireLikeButton(controller) {
  const btn = document.getElementById('btnLike');
  if (!btn) return; // page without the listening player chrome
  const sync = () => {
    const cur = controller.current();
    const key = cur ? trackKey(cur.track) : null;
    const on = isLiked(key);
    btn.disabled = !key;
    btn.classList.toggle('liked', on);
    btn.setAttribute('aria-pressed', String(on));
    const label = on ? 'Remove from Favorites' : 'Add to Favorites';
    btn.setAttribute('aria-label', label);
    btn.title = label;
  };
  controller.on('trackchange', sync);
  onLikedChange(sync);
  btn.addEventListener('click', () => {
    const cur = controller.current();
    if (cur) toggleLike(trackKey(cur.track), cur.track.remoteLike); // sync runs via onLikedChange
  });
  ensureLiked(); // resolves to an empty set for anonymous users — no prompt
  sync();
}

// ── Boot ─────────────────────────────────────────────────────────────────────
(async function boot() {
  wireTheme();
  initAboutMenu();         // persistent header — wired once for the document
  initNavMenu();           // responsive ☰ overflow menu (same persistent header)
  wirePlayer();
  await initAuth();        // once for the document's lifetime
  await runModule();       // the server already rendered this page; just init it
})();
