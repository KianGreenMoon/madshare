// Admin · Upload — the same upload page as /upload, hosted in the admin shell.
//
// The body markup ({{define "upload-main"}}) and all upload logic come from the
// shared core (../upload.js); this boot only supplies what differs between the
// shells: the admin gate (bootAdmin) and a page-local preview player. The
// listening-shell /upload page is booted by shell.js instead, which previews
// through its persistent player. Design: docs/plans/cross-shell-upload.md.
//
// Gating: like every admin page, the HTML is ungated server-side (so it can
// render the login prompt); bootAdmin() gates the content to admin-access
// identities (file.delete or user.manage), and upload.js's init() additionally
// runs gatePage(file.upload). Admin and moderator — the only admin-access roles
// — both hold file.upload (003_auth.sql, 017), so no extra guard is needed. All
// writes are enforced server-side regardless (/files/upload, /api/my/uploads).
import { bootAdmin, API, toast } from './shared.js';
import { createPlayer } from '../player.js';
import { init } from '../upload.js';

// ── Page-local preview player ─────────────────────────────────────────────────
// One <audio>/player-bar for the page (the same component admin/library.js uses).
// A play context lets prev/next/ended walk the previewed list generically.
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

// previewPlay matches the shell controller's setQueue(tracks, idx) signature so
// upload.js can drive it the same way. Tracks already carry absolute URLs.
function previewPlay(tracks, idx) {
  if (!tracks || !tracks.length) return;
  playCtx = { items: tracks, index: idx < 0 ? 0 : idx };
  playAt(playCtx.index);
}
function playAt(i) {
  if (!playCtx || i < 0 || i >= playCtx.items.length) return;
  playCtx.index = i;
  const it = playCtx.items[i];
  const url = /^https?:/.test(it.url) ? it.url : `${API}${it.url}`;
  player.load({ url, title: it.title, artist: it.artist || '' });
}

// ── Boot ──────────────────────────────────────────────────────────────────────
(async function boot() {
  const identity = await bootAdmin();   // admin gate; renders the notice on fail
  if (!identity) return;
  await init({ preview: previewPlay });
})();
