// Admin · Duplicates — recordings with more than one rendition of the same audio
// (recordings P2, docs/architecture/recordings.md). Lists each recording with
// its renditions ranked by the quality ladder and a keep/variant suggestion.
//
// Reuses the shared building blocks: the player core (player.js, the same bar as
// every listening page) driven by a page-local play context, and the track-edit
// modal (track-edit.js) for fixing tags. Per-rendition actions: Play, Edit tags
// (metadata.edit), Split off (detach into a new pinned recording), and Delete
// (soft delete → Trash). Bespoke list rather than file-list.js: the rows are
// renditions grouped under a recording with tech-compare columns, a shape
// file-list's flat-file model doesn't carry.
//
// Moderator-accessible: bootAdmin additionally requires content.moderate.
import { API, el, fmtBytes, fmtTime, toast, handleAuthError, bootAdmin } from './shared.js';
import { createPlayer } from '../player.js';
import { createTrackEditor } from '../track-edit.js';

const results = document.getElementById('dupResults');
const loading = document.getElementById('dupLoading');

// display artist: prefer the album artist, fall back to the track artist.
const dispArtist = r => r.album_artist || r.artist || '';

// ── Shared preview player ─────────────────────────────────────────────────────
// One player-bar for the page (createPlayer), driven by a page-local play
// context like /admin/library: playing a rendition queues the whole recording's
// renditions so Prev/Next/auto-advance walk them, and the playing row highlights.
let playCtx = null;
const player = createPlayer({
  onPrev:  () => { if (playCtx) playAt(playCtx.index > 0 ? playCtx.index - 1 : playCtx.items.length - 1); },
  onNext:  () => { if (playCtx) playAt(playCtx.index < playCtx.items.length - 1 ? playCtx.index + 1 : 0); },
  onEnded: () => { if (playCtx && playCtx.index < playCtx.items.length - 1) playAt(playCtx.index + 1); },
  onError: () => {
    toast('Couldn’t play this rendition.', 'error');
    if (playCtx && playCtx.index < playCtx.items.length - 1) playAt(playCtx.index + 1);
  },
});

function playGroup(group, index) {
  const items = group.renditions.map(r => ({
    url: API + r.url, title: r.title || r.hash, artist: dispArtist(r), key: r.hash,
  }));
  playCtx = { items, index: index < 0 ? 0 : index };
  playAt(playCtx.index);
}
function playAt(i) {
  if (!playCtx || i < 0 || i >= playCtx.items.length) return;
  playCtx.index = i;
  const it = playCtx.items[i];
  player.load({ url: it.url, title: it.title, artist: it.artist });
  setPlaying(it.key);
}
function setPlaying(hash) {
  results.querySelectorAll('tr.dup-row--playing').forEach(tr => tr.classList.remove('dup-row--playing'));
  results.querySelector(`tr[data-hash="${CSS.escape(hash)}"]`)?.classList.add('dup-row--playing');
}

// ── Edit-tags modal (shared track-edit.js), gated on metadata.edit ────────────
let editor = null; // created in init() only when the caller may edit
function editTags(r) {
  editor?.open({
    hash: r.hash, title: r.title, artist: r.artist,
    album_artist: r.album_artist, album: r.album,
  });
}

// ── Delete confirmation modal ─────────────────────────────────────────────────
const delModal   = document.getElementById('delModal');
const delBody    = document.getElementById('delModalBody');
const delConfirm = document.getElementById('delConfirm');
const delCancel  = document.getElementById('delCancel');
const delClose   = document.getElementById('delClose');

function confirmDelete(r) {
  return new Promise(resolve => {
    delBody.textContent = `Send "${r.title || r.hash}" to Trash? The blob is kept and can be restored.`;
    delModal.classList.remove('hidden');
    delConfirm.focus();
    const cleanup = () => {
      delConfirm.removeEventListener('click', onOk);
      delCancel.removeEventListener('click', onCancel);
      delClose.removeEventListener('click', onCancel);
      delModal.removeEventListener('click', onBackdrop);
      document.removeEventListener('keydown', onKey);
    };
    const onOk       = () => { delModal.classList.add('hidden'); cleanup(); resolve(true); };
    const onCancel   = () => { delModal.classList.add('hidden'); cleanup(); resolve(false); };
    const onBackdrop = e => { if (e.target === delModal) onCancel(); };
    const onKey      = e => { if (e.key === 'Escape' && !delModal.classList.contains('hidden')) onCancel(); };
    delConfirm.addEventListener('click', onOk);
    delCancel.addEventListener('click', onCancel);
    delClose.addEventListener('click', onCancel);
    delModal.addEventListener('click', onBackdrop);
    document.addEventListener('keydown', onKey);
  });
}

// ── Actions ───────────────────────────────────────────────────────────────────
async function deleteRendition(r) {
  if (!(await confirmDelete(r))) return;
  let res;
  try { res = await fetch(`${API}/api/admin/files/${encodeURIComponent(r.hash)}`, { method: 'DELETE' }); }
  catch { toast('Network error deleting rendition.', 'error'); return; }
  if (handleAuthError(res)) return;
  if (!res.ok) { toast(`Delete failed (HTTP ${res.status}).`, 'error'); return; }
  toast('Rendition sent to Trash.', 'success');
  load();
}

async function splitRendition(r) {
  let res;
  try { res = await fetch(`${API}/api/admin/duplicates/${r.file_id}/split`, { method: 'POST' }); }
  catch { toast('Network error splitting rendition.', 'error'); return; }
  if (handleAuthError(res)) return;
  if (!res.ok) { toast(`Split failed (HTTP ${res.status}).`, 'error'); return; }
  toast('Split into its own recording.', 'success');
  load();
}

// ── Rendering ─────────────────────────────────────────────────────────────────
function techCell(r) {
  const parts = [];
  if (r.bitrate)     parts.push(`${Math.round(r.bitrate / 1000)} kbps`);
  if (r.sample_rate) parts.push(`${(r.sample_rate / 1000).toFixed(1)} kHz`);
  if (r.bit_depth)   parts.push(`${r.bit_depth}-bit`);
  return parts.join(' · ') || '—';
}

function renditionRow(group, r, index) {
  const artist = dispArtist(r);
  const actions = [
    el('button', { class: 'btn btn-sm btn-neutral', onclick: () => playGroup(group, index) }, ['Play']),
  ];
  if (editor) actions.push(el('button', { class: 'btn btn-sm btn-neutral', title: 'Edit this rendition’s tags', onclick: () => editTags(r) }, ['Edit']));
  actions.push(
    el('button', { class: 'btn btn-sm btn-neutral', title: 'Detach into its own recording', onclick: () => splitRendition(r) }, ['Split off']),
    el('button', { class: 'btn btn-sm btn-destructive-outline', onclick: () => deleteRendition(r) }, ['Delete']),
  );
  return el('tr', { 'data-hash': r.hash }, [
    el('td', { class: 'dup-rank' }, [r.best ? el('span', { class: 'dup-best', title: 'Best by the quality ladder' }, ['★ best']) : `#${r.rank}`]),
    el('td', {}, [
      el('div', { class: 'dup-title' }, [r.title || '(untitled)']),
      artist ? el('div', { class: 'dup-artist muted' }, [artist]) : null,
    ]),
    el('td', {}, [r.format || '—']),
    el('td', { class: 'dup-tech' }, [techCell(r)]),
    el('td', {}, [fmtTime(r.duration)]),
    el('td', {}, [fmtBytes(r.size)]),
    el('td', { class: 'dup-actions' }, actions),
  ]);
}

function recordingCard(group) {
  const table = el('table', { class: 'dup-table' }, [
    el('thead', {}, [el('tr', {}, [
      el('th', {}, ['Rank']), el('th', {}, ['Track']), el('th', {}, ['Format']),
      el('th', {}, ['Quality']), el('th', {}, ['Length']), el('th', {}, ['Size']),
      el('th', {}, ['']),
    ])]),
    el('tbody', {}, group.renditions.map((r, i) => renditionRow(group, r, i))),
  ]);
  // Recording-level play: start at the best (top-ranked) rendition.
  const bestIdx = Math.max(0, group.renditions.findIndex(r => r.best));
  return el('section', { class: 'dup-card' }, [
    el('div', { class: 'dup-card-head' }, [
      el('button', { class: 'btn btn-sm btn-neutral dup-play-best', title: 'Play the best rendition', onclick: () => playGroup(group, bestIdx) }, ['▶ Play best']),
      el('span', { class: 'dup-count' }, [`${group.renditions.length} renditions`]),
      el('span', { class: 'dup-suggestion' }, [group.suggestion]),
    ]),
    table,
  ]);
}

async function load() {
  if (loading) loading.remove();
  let res;
  try { res = await fetch(`${API}/api/admin/duplicates`); }
  catch { results.replaceChildren(el('p', { class: 'error' }, ['Network error loading duplicates.'])); return; }
  if (handleAuthError(res)) return;
  if (!res.ok) { results.replaceChildren(el('p', { class: 'error' }, [`Failed to load (HTTP ${res.status}).`])); return; }
  const groups = await res.json();
  if (!groups.length) {
    results.replaceChildren(el('p', { class: 'muted' }, ['No duplicate renditions — every recording has a single copy.']));
    return;
  }
  results.replaceChildren(...groups.map(recordingCard));
}

(async function init() {
  const identity = await bootAdmin({ require: 'content.moderate' });
  if (!identity) return;
  // Edit tags is a metadata.edit capability; wire the shared modal only then.
  if ((identity.permissions || []).includes('metadata.edit')) {
    editor = createTrackEditor({
      patchURL: f => `${API}/api/files/${encodeURIComponent(f.hash)}/metadata`,
      note: 'Edits this rendition’s tags. A tag fix usually accompanies a Split off.',
      checkAuth: handleAuthError,
      onSaved: () => { toast('Tags updated.', 'success'); load(); },
      onError: () => toast('Couldn’t save tags.', 'error'),
    });
  }
  load();
})();
