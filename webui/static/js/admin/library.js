// Admin · Library — the unified file-management page. One page, one scope switch
// (All files · Review · Trash), one shared preview player. Each scope is a
// factory over the shared component (file-list.js); this module owns the page:
// it boots auth once, creates the shared player, builds the available scopes,
// and swaps panels in place (no reload). My uploads stays on /upload.
//
// Design: docs/architecture/file-management-view.md (Hybrid).
import { bootAdmin, API, toast } from './shared.js';
import { createPlayer } from '../player.js';
import { createFilesScope } from './files.js';
import { createReviewScope } from './moderation.js';
import { createTrashScope } from './trash.js';

// ── Shared preview player ─────────────────────────────────────────────────────
// One <audio>/player-bar for the whole page. A scope previews a row by calling
// play(items, index, highlight); next/prev/ended navigate the current context
// generically, and `highlight(key)` repaints the active scope's playing row.
let playCtx = null;
const player = createPlayer({
  onPrev:  () => { if (playCtx) playAt(playCtx.index > 0 ? playCtx.index - 1 : playCtx.items.length - 1); },
  onNext:  () => { if (playCtx) playAt(playCtx.index < playCtx.items.length - 1 ? playCtx.index + 1 : 0); },
  onEnded: () => { if (playCtx && playCtx.index < playCtx.items.length - 1) playAt(playCtx.index + 1); },
  onError: () => {
    toast('Couldn’t play this file.', 'error');
    if (playCtx && playCtx.index < playCtx.items.length - 1) playAt(playCtx.index + 1);
  },
});
function play(items, index, highlight) {
  if (!items || !items.length) return;
  playCtx = { items, index: index < 0 ? 0 : index, highlight };
  playAt(playCtx.index);
}
function playAt(i) {
  if (!playCtx || i < 0 || i >= playCtx.items.length) return;
  playCtx.index = i;
  const it = playCtx.items[i];
  const url = /^https?:/.test(it.url) ? it.url : `${API}${it.url}`;
  player.load({ url, title: it.title, artist: it.artist || '' });
  playCtx.highlight?.(it.key);
}

// ── Boot ──────────────────────────────────────────────────────────────────────
(async function boot() {
  const identity = await bootAdmin();          // admin gate; renders the notice on fail
  if (!identity) return;
  const perms = identity.permissions || [];

  // Build every scope, keep the ones this admin can use. (All files is always on.)
  const controllers = [
    createFilesScope({ play, perms }),
    createReviewScope({ play, perms }),
    createTrashScope({ play, perms }),
  ].filter(c => c.available);

  const switchEl = document.getElementById('scopeSwitch');
  const mounted = new Set();
  let active = null;

  function show(id) {
    if (active === id) return;
    active = id;
    for (const c of controllers) {
      const on = c.id === id;
      const panel = document.getElementById(`scope-${c.id}`);
      const pill = switchEl.querySelector(`[data-scope="${c.id}"]`);
      if (panel) panel.hidden = !on;
      if (pill) { pill.classList.toggle('is-active', on); pill.setAttribute('aria-selected', String(on)); }
      if (on) {
        if (mounted.has(c.id)) c.reload();
        else { mounted.add(c.id); c.mount(); }
      }
    }
  }

  for (const c of controllers) {
    const pill = document.createElement('button');
    pill.className = 'scope-btn';
    pill.dataset.scope = c.id;
    pill.type = 'button';
    pill.setAttribute('role', 'tab');
    pill.setAttribute('aria-selected', 'false');
    pill.textContent = c.label;
    pill.addEventListener('click', () => show(c.id));
    switchEl.appendChild(pill);
  }

  // Initial scope: honor a #review / #trash deep-link (the dashboard cards),
  // else the first available scope (All files).
  const want = location.hash.replace('#', '');
  show(controllers.some(c => c.id === want) ? want : controllers[0].id);
})();
