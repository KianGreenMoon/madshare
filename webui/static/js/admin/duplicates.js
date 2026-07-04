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

// ── Bulk selection toolbar ────────────────────────────────────────────────────
const toolbar           = document.getElementById('dupToolbar');
const btnAbsorbAll      = document.getElementById('absorbAll');
const btnSelectExtras   = document.getElementById('selectExtras');
const btnClearSel       = document.getElementById('clearSel');
const btnDeleteSelected = document.getElementById('deleteSelected');

const allChecks      = () => [...results.querySelectorAll('.dup-check')];
const selectedHashes = () => allChecks().filter(c => c.checked).map(c => c.dataset.hash);

function updateSelCount() {
  const n = selectedHashes().length;
  btnDeleteSelected.textContent = n ? `Delete selected (${n})` : 'Delete selected';
  btnDeleteSelected.disabled = n === 0;
}
// One delegated listener on the stable container survives every reload.
results.addEventListener('change', e => {
  if (e.target.classList?.contains('dup-check')) updateSelCount();
});
btnSelectExtras.addEventListener('click', () => selectExtrasIn(results));

// selectExtrasIn ticks every rendition except the best within a scope (one
// recording card, or `results` for the whole page) — the redundant copies.
function selectExtrasIn(scope) {
  scope.querySelectorAll('.dup-check').forEach(c => { c.checked = c.dataset.best !== '1'; });
  updateSelCount();
}

// toggleExtrasIn is selectExtrasIn with clear-on-repeat, used by the per-card
// button: when the card's selection is ALREADY exactly its non-best set (every
// redundant copy ticked and the best untouched, nothing else), a second click
// clears that card instead. Otherwise it selects the non-best. Scoped to the
// one card — other cards' selections are untouched.
function toggleExtrasIn(scope) {
  const checks = [...scope.querySelectorAll('.dup-check')];
  const extras = checks.filter(c => c.dataset.best !== '1');
  const exactlyExtras = extras.length > 0
    && extras.every(c => c.checked)
    && checks.every(c => c.dataset.best !== '1' || !c.checked); // best not ticked
  if (exactlyExtras) {
    checks.forEach(c => { c.checked = false; });
    updateSelCount();
  } else {
    selectExtrasIn(scope);
  }
}
btnClearSel.addEventListener('click', () => {
  allChecks().forEach(c => { c.checked = false; });
  updateSelCount();
});
btnDeleteSelected.addEventListener('click', deleteSelected);
btnAbsorbAll.addEventListener('click', absorbAll);

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

// ── Confirmation modal (shared by delete + absorb) ────────────────────────────
const delModal   = document.getElementById('delModal');
const delTitle   = document.getElementById('delModalTitle');
const delBody    = document.getElementById('delModalBody');
const delConfirm = document.getElementById('delConfirm');
const delCancel  = document.getElementById('delCancel');
const delClose   = document.getElementById('delClose');

// confirmModal resolves true/false. danger styles the confirm button red (delete);
// otherwise it is the neutral primary (absorb, which is reversible).
function confirmModal({ title, body, confirmLabel, danger = false }) {
  return new Promise(resolve => {
    delTitle.textContent = title;
    delBody.textContent = body;
    delConfirm.textContent = confirmLabel;
    delConfirm.className = 'btn ' + (danger ? 'btn-destructive-solid' : 'btn-neutral');
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

function confirmDelete(n) {
  return confirmModal({
    title: 'Delete to Trash?',
    body: n === 1
      ? 'Send 1 rendition to Trash? The blob is kept and can be restored.'
      : `Send ${n} renditions to Trash? The blobs are kept and can be restored.`,
    confirmLabel: n === 1 ? 'Delete to Trash' : `Delete ${n} to Trash`,
    danger: true,
  });
}

// ── Actions ───────────────────────────────────────────────────────────────────
async function deleteSelected() {
  const hashes = selectedHashes();
  if (!hashes.length || !(await confirmDelete(hashes.length))) return;
  let ok = 0, fail = 0, authFailed = false;
  for (const hash of hashes) {
    let res;
    try { res = await fetch(`${API}/api/admin/files/${encodeURIComponent(hash)}`, { method: 'DELETE' }); }
    catch { fail++; continue; }
    if (res.status === 401) { handleAuthError(res); authFailed = true; break; }
    if (res.ok) ok++; else fail++;
  }
  if (authFailed)   { if (ok) toast(`Deleted ${ok} before the session expired.`, 'error'); }
  else if (fail)    toast(`Deleted ${ok}; ${fail} failed.`, 'error');
  else if (ok)      toast(`Sent ${ok} ${ok === 1 ? 'rendition' : 'renditions'} to Trash.`, 'success');
  load();
}

async function deleteOne(r) {
  if (!(await confirmDelete(1))) return;
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

// absorbCard keeps the card's checked master rendition and absorbs the rest:
// their files are removed (restorable) but every distinct release is preserved,
// with duplicate / blank releases dropped. The keep radio defaults to the best.
async function absorbCard(group, cardEl) {
  const keepRadio = cardEl.querySelector('.dup-keep:checked');
  const keepId = keepRadio ? Number(keepRadio.dataset.fileId) : group.renditions[0].file_id;
  const absorbIds = group.renditions.map(r => r.file_id).filter(id => id !== keepId);
  if (!absorbIds.length) { toast('Only one rendition — nothing to absorb.', 'error'); return; }
  const keep = group.renditions.find(r => r.file_id === keepId) || group.renditions[0];
  const ok = await confirmModal({
    title: 'Absorb into the kept rendition?',
    body: `Keep “${keep.title || keep.hash}” (${keep.format || 'audio'}) as the master and absorb the other `
      + `${absorbIds.length} rendition${absorbIds.length === 1 ? '' : 's'}. Their files are removed (restorable), `
      + `but every distinct release is preserved as a separate track; duplicate or blank releases are dropped.`,
    confirmLabel: 'Absorb',
  });
  if (!ok) return;
  let res;
  try {
    res = await fetch(`${API}/api/admin/duplicates/absorb/${group.recording_id}`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ keep_file_id: keepId, absorb_file_ids: absorbIds }),
    });
  } catch { toast('Network error absorbing.', 'error'); return; }
  if (handleAuthError(res)) return;
  if (res.status === 404) { toast('The renditions changed — reloading.', 'error'); load(); return; }
  if (!res.ok) { toast(`Absorb failed (HTTP ${res.status}).`, 'error'); return; }
  const j = await res.json().catch(() => ({}));
  const kept = absorbIds.length - (j.appearances_dropped || 0);
  toast(`Absorbed ${j.renditions_removed ?? absorbIds.length} rendition(s); kept ${kept} extra release(s).`, 'success');
  load();
}

// absorbAll runs the "keep best" absorb across every duplicate recording at once.
async function absorbAll() {
  const ok = await confirmModal({
    title: 'Absorb all duplicates?',
    body: 'For every recording with more than one rendition, keep its best-quality audio and absorb the rest. '
      + 'The removed files are restorable, and every distinct release is preserved.',
    confirmLabel: 'Absorb all → keep best',
  });
  if (!ok) return;
  let res;
  try {
    res = await fetch(`${API}/api/admin/duplicates/absorb`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ all: true }),
    });
  } catch { toast('Network error.', 'error'); return; }
  if (handleAuthError(res)) return;
  if (!res.ok) { toast(`Absorb failed (HTTP ${res.status}).`, 'error'); return; }
  const j = await res.json().catch(() => ({}));
  toast(`Absorbed ${j.recordings_absorbed || 0} recording(s), removed ${j.renditions_removed || 0} rendition(s).`, 'success');
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
    el('button', { class: 'btn btn-sm btn-destructive-outline', title: 'Send this rendition to Trash', onclick: () => deleteOne(r) }, ['Delete']),
  );
  const check = el('input', {
    type: 'checkbox', class: 'dup-check', 'data-hash': r.hash, 'data-best': r.best ? '1' : '',
    'aria-label': `Select ${r.title || r.hash} for deletion`,
  });
  const keep = el('input', {
    type: 'radio', class: 'dup-keep', name: `keep-${group.recording_id}`,
    'data-file-id': String(r.file_id), 'aria-label': `Keep ${r.title || r.hash} as the master rendition`,
  });
  if (r.best) keep.checked = true;
  return el('tr', { 'data-hash': r.hash }, [
    el('td', { class: 'dup-checkcell' }, [check]),
    el('td', { class: 'dup-keepcell' }, [keep]),
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
      el('th', { class: 'dup-checkcell', 'aria-label': 'Select' }, ['']),
      el('th', { class: 'dup-keepcell', title: 'Master rendition kept on absorb' }, ['Keep']),
      el('th', {}, ['Rank']), el('th', {}, ['Track']), el('th', {}, ['Format']),
      el('th', {}, ['Quality']), el('th', {}, ['Length']), el('th', {}, ['Size']),
      el('th', {}, ['']),
    ])]),
    el('tbody', {}, group.renditions.map((r, i) => renditionRow(group, r, i))),
  ]);
  return el('section', { class: 'dup-card' }, [
    el('div', { class: 'dup-card-head' }, [
      el('button', {
        class: 'btn btn-sm btn-absorb',
        title: 'Keep the selected master rendition and absorb the rest, preserving every distinct release',
        onclick: e => absorbCard(group, e.target.closest('.dup-card')),
      }, ['Absorb into ★']),
      el('button', {
        class: 'btn btn-sm btn-neutral',
        title: 'Tick this recording’s non-best renditions; click again to clear them',
        onclick: e => toggleExtrasIn(e.target.closest('.dup-card')),
      }, ['Select non-best']),
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
    toolbar.classList.add('hidden');
    results.replaceChildren(el('p', { class: 'muted' }, ['No duplicate renditions — every recording has a single copy.']));
    return;
  }
  results.replaceChildren(...groups.map(recordingCard));
  toolbar.classList.remove('hidden');
  updateSelCount(); // reset the count/disabled state after the rebuild
}

(async function init() {
  const identity = await bootAdmin({ require: 'content.moderate' });
  if (!identity) return;
  // Edit tags is a metadata.edit capability; wire the shared modal only then.
  if ((identity.permissions || []).includes('metadata.edit')) {
    editor = createTrackEditor({
      patchURL: f => `${API}/api/files/${encodeURIComponent(f.hash)}/metadata`,
      detailURL: f => `${API}/api/files/${encodeURIComponent(f.hash)}/metadata`,
      note: 'Edits this rendition’s tags. A tag fix usually accompanies a Split off.',
      checkAuth: handleAuthError,
      onSaved: () => { toast('Tags updated.', 'success'); load(); },
      onError: () => toast('Couldn’t save tags.', 'error'),
    });
  }
  load();
})();
