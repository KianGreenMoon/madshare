// Admin · Recordings — the recording-centric curation view (recording-tagsets
// P5, docs/architecture/recording-tagsets.md "Admin surfaces"). Every recording,
// newest first, windowed (virtual-list.js in window-scroll mode) with search +
// filter pills. A card expands to both arms stacked: renditions (ladder-ranked,
// live + soft-removed; Play / Split off / Remove / Restore) over appearances
// (primary marked, review state; Edit / Set primary / Move… / Remove), plus the
// whole-recording footer (Trash = soft, Delete permanently = the count-aware
// cascade). Merge is selection-based: tick ≥2 recordings, the bulk bar wakes,
// the modal picks the surviving target (default = the ladder-best holder).
//
// Bespoke module (like moderation.js/mine-list.js): the two-arm expanded card
// is a shape file-list.js's flat rows can't express. Reuses the shared player
// core, track-edit modal, toast system, and the virtual-list windowing.
//
// Gates: page = content.moderate (bootAdmin); delete/trash/remove need
// file.delete, access edit + tag edit need metadata.edit — gated client-side by
// hiding controls (the API enforces for real).
import { API, el, fmtBytes, fmtTime, toast, handleAuthError, bootAdmin } from './shared.js';
import { createPlayer } from '../player.js';
import { createTrackEditor } from '../track-edit.js';
import { createVirtualList } from '../virtual-list.js';

const PAGE_SIZE = 100;

const listHost  = document.getElementById('recList');
const countEl   = document.getElementById('recCount');
const searchEl  = document.getElementById('recSearch');
const selInfo   = document.getElementById('recSelInfo');
const mergeBtn  = document.getElementById('recMergeBtn');
const trashBtn  = document.getElementById('recTrashBtn');
const clearBtn  = document.getElementById('recClearBtn');

// ── Page state ────────────────────────────────────────────────────────────────
let vlist = null;
let total = 0;
let filter = '';
let query = '';
let canDelete = false;   // file.delete
let canEdit = false;     // metadata.edit
let canModerate = false; // content.moderate (gates Add appearance, like Move/Merge)
let editor = null;       // track-edit modal for editing an appearance (canEdit)
let adder = null;        // track-edit modal in create mode for Add appearance

const selected = new Set();   // recording ids ticked for merge/trash
const expanded = new Map();   // recording id → detail object | null (loading)

// ── Fetch helpers ─────────────────────────────────────────────────────────────
function listURL(offset) {
  const p = new URLSearchParams();
  if (query)  p.set('q', query);
  if (filter) p.set('filter', filter);
  p.set('limit', String(PAGE_SIZE));
  p.set('offset', String(offset));
  return `${API}/api/admin/recordings?${p}`;
}

async function fetchPage(offset) {
  const res = await fetch(listURL(offset));
  if (handleAuthError(res)) throw new Error('auth');
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  return res.json();
}

// post wraps a mutating call; returns the response (or null on network error).
async function post(url, { method = 'POST', body } = {}) {
  let res;
  try {
    res = await fetch(url, {
      method,
      headers: body !== undefined ? { 'Content-Type': 'application/json' } : undefined,
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
  } catch { toast('Network error.', 'error'); return null; }
  if (handleAuthError(res)) return null;
  return res;
}

// ── Display helpers ───────────────────────────────────────────────────────────
const dispName = rec => rec.title || `recording #${rec.id}`;

function techCell(f) {
  const parts = [];
  if (f.bitrate)     parts.push(`${Math.round(f.bitrate / 1000)} kbps`);
  if (f.sample_rate) parts.push(`${(f.sample_rate / 1000).toFixed(1)} kHz`);
  if (f.bit_depth)   parts.push(`${f.bit_depth}-bit`);
  return parts.join(' · ') || '—';
}

// formatClass ranks a format chip for the merge-target default, mirroring the
// server ladder's codec classes (0 lossless, 1 lossy, 2 unknown).
function formatClass(fmt) {
  const f = (fmt || '').toLowerCase();
  if (f === 'flac' || f === 'alac') return 0;
  if (['mp3', 'aac', 'vorbis', 'opus', 'wmav2', 'ac3', 'mp2'].includes(f)) return 1;
  return 2;
}

const APP_STATE = {
  approved:  'st-ok',
  submitted: 'st-warn',
  returned:  'st-warn',
  draft:     'st-warn',
};

// ── Selection / bulk bar ──────────────────────────────────────────────────────
function updateBulk() {
  const n = selected.size;
  mergeBtn.disabled = n < 2;
  mergeBtn.textContent = n >= 2 ? `Merge selected (${n})` : 'Merge selected';
  trashBtn.disabled = n === 0;
  trashBtn.textContent = n ? `Trash selected (${n})` : 'Trash selected';
  selInfo.textContent = n
    ? `${n} recording${n === 1 ? '' : 's'} selected.`
    : 'Tick recordings to merge or trash them.';
}

clearBtn.addEventListener('click', () => { selected.clear(); updateBulk(); vlist?.refresh(); });

// ── Confirmation modal ────────────────────────────────────────────────────────
const confModal  = document.getElementById('confModal');
const confTitle  = document.getElementById('confTitle');
const confBody   = document.getElementById('confBody');
const confGo     = document.getElementById('confGo');
const confCancel = document.getElementById('confCancel');
const confClose  = document.getElementById('confClose');

function confirmModal({ title, body, confirmLabel, danger = true }) {
  return new Promise(resolve => {
    confTitle.textContent = title;
    confBody.textContent = body;
    confGo.textContent = confirmLabel;
    confGo.className = 'btn ' + (danger ? 'btn-destructive-solid' : 'btn-neutral');
    confModal.classList.remove('hidden');
    confGo.focus();
    const cleanup = () => {
      confGo.removeEventListener('click', onOk);
      confCancel.removeEventListener('click', onNo);
      confClose.removeEventListener('click', onNo);
      confModal.removeEventListener('click', onBackdrop);
      document.removeEventListener('keydown', onKey);
    };
    const onOk       = () => { confModal.classList.add('hidden'); cleanup(); resolve(true); };
    const onNo       = () => { confModal.classList.add('hidden'); cleanup(); resolve(false); };
    const onBackdrop = e => { if (e.target === confModal) onNo(); };
    const onKey      = e => { if (e.key === 'Escape' && !confModal.classList.contains('hidden')) onNo(); };
    confGo.addEventListener('click', onOk);
    confCancel.addEventListener('click', onNo);
    confClose.addEventListener('click', onNo);
    confModal.addEventListener('click', onBackdrop);
    document.addEventListener('keydown', onKey);
  });
}

// wireModal adds close/backdrop/Escape handling to a standing modal; the
// caller wires its own confirm button.
function wireModal(backdrop, closeBtns) {
  const hide = () => backdrop.classList.add('hidden');
  closeBtns.forEach(b => b.addEventListener('click', hide));
  backdrop.addEventListener('click', e => { if (e.target === backdrop) hide(); });
  document.addEventListener('keydown', e => {
    if (e.key === 'Escape' && !backdrop.classList.contains('hidden')) hide();
  });
  return hide;
}

// ── Shared preview player (page-local, like /admin/duplicates) ────────────────
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

// playRecording queues the recording's live renditions (ladder order) starting
// at `startFileID` (or the best). Row titles come from the primary appearance.
function playRecording(rec, detail, startFileID = 0) {
  const live = (detail?.renditions || []).filter(f => !f.removed);
  if (!live.length) { toast('No playable rendition — the recording is dormant.', 'error'); return; }
  const items = live.map(f => ({
    url: API + f.url,
    title: `${dispName(rec)} (${f.format || 'audio'})`,
    artist: rec.artist || '',
    key: f.file_id,
  }));
  let index = items.findIndex(it => it.key === startFileID);
  if (index < 0) index = 0;
  playCtx = { items, index };
  playAt(index);
}
function playAt(i) {
  if (!playCtx || i < 0 || i >= playCtx.items.length) return;
  playCtx.index = i;
  const it = playCtx.items[i];
  player.load({ url: it.url, title: it.title, artist: it.artist });
  document.querySelectorAll('tr.rec-row--playing').forEach(tr => tr.classList.remove('rec-row--playing'));
  document.querySelector(`tr[data-file-id="${it.key}"]`)?.classList.add('rec-row--playing');
}

// ── Detail loading / expansion ────────────────────────────────────────────────
async function loadDetail(recID) {
  const res = await fetch(`${API}/api/admin/recordings/${recID}`);
  if (handleAuthError(res)) return null;
  if (!res.ok) { toast(`Couldn’t load recording #${recID} (HTTP ${res.status}).`, 'error'); return null; }
  return res.json();
}

async function toggleExpand(rec) {
  if (expanded.has(rec.id)) {
    expanded.delete(rec.id);
    vlist.refresh();
    return;
  }
  expanded.set(rec.id, null); // loading placeholder
  vlist.refresh();
  const detail = await loadDetail(rec.id);
  if (!expanded.has(rec.id)) return; // collapsed while loading
  if (!detail) { expanded.delete(rec.id); vlist.refresh(); return; }
  expanded.set(rec.id, detail);
  vlist.refresh();
}

// refreshDetail re-fetches an expanded card (after a mutation) and re-paints.
async function refreshDetail(recID) {
  if (!expanded.has(recID)) return;
  const detail = await loadDetail(recID);
  if (detail) expanded.set(recID, detail); else expanded.delete(recID);
  vlist.refresh();
}

// reload re-runs the whole listing (filter/search/mutation that changes rows).
async function reload() {
  document.getElementById('recLoading')?.remove();
  let first;
  try { first = await fetchPage(0); }
  catch { listHost.replaceChildren(el('p', { class: 'error' }, ['Failed to load recordings.'])); return; }
  total = first.total;
  countEl.textContent = `${total} recording${total === 1 ? '' : 's'}`;
  // Drop expansion/selection state for rows that no longer exist server-side —
  // a stale id would silently mis-target a later merge/trash.
  const ids = new Set(first.items.map(r => r.id));
  for (const id of [...selected]) if (!ids.has(id) && first.items.length >= total) selected.delete(id);
  vlist.setItems(first.items, { keepScroll: true });
  updateBulk();
}

// ── Row actions ───────────────────────────────────────────────────────────────
async function splitOff(rec, f) {
  const res = await post(`${API}/api/admin/duplicates/${f.file_id}/split`);
  if (!res) return;
  if (!res.ok) { toast(`Split failed (HTTP ${res.status}).`, 'error'); return; }
  toast('Split into its own recording.', 'success');
  expanded.delete(rec.id);
  reload();
}

async function removeRendition(rec, f, restore) {
  const res = await post(`${API}/api/admin/renditions/${f.file_id}/${restore ? 'restore' : 'remove'}`);
  if (!res) return;
  if (!res.ok) { toast(`${restore ? 'Restore' : 'Remove'} failed (HTTP ${res.status}).`, 'error'); return; }
  toast(restore ? 'Rendition restored.' : 'Rendition removed (restorable).', 'success');
  await refreshDetail(rec.id);
  reload();
}

async function removeAppearance(rec, a) {
  const ok = await confirmModal({
    title: 'Remove this appearance?',
    body: `“${a.title}” goes to Trash (reversible). The audio stays — other appearances keep playing it; removing the last one makes the recording dormant.`,
    confirmLabel: 'Remove appearance',
  });
  if (!ok) return;
  const res = await post(`${API}/api/admin/moderation/${a.tagset_id}/discard`);
  if (!res) return;
  if (!res.ok) { toast(`Remove failed (HTTP ${res.status}).`, 'error'); return; }
  toast('Appearance sent to Trash.', 'success');
  await refreshDetail(rec.id);
  reload();
}

async function restoreAppearance(rec, a) {
  const res = await post(`${API}/api/admin/tagsets/${a.tagset_id}/restore`);
  if (!res) return;
  if (!res.ok) { toast(`Restore failed (HTTP ${res.status}).`, 'error'); return; }
  toast('Appearance restored to the library.', 'success');
  await refreshDetail(rec.id);
  reload();
}

async function setPrimary(rec, a) {
  const res = await post(`${API}/api/admin/recordings/${rec.id}/primary`, { body: { tagset_id: a.tagset_id } });
  if (!res) return;
  if (!res.ok) { toast(`Set primary failed (HTTP ${res.status}).`, 'error'); return; }
  toast('Primary appearance changed — it now names the recording.', 'success');
  await refreshDetail(rec.id);
  reload();
}

async function trashRecording(rec) {
  const apps = rec.appearances + rec.trashed_appearances;
  const ok = await confirmModal({
    title: 'Trash this recording?',
    body: `All ${apps} appearance${apps === 1 ? '' : 's'} of “${dispName(rec)}” move to Trash (reversible). The audio files stay on disk.`,
    confirmLabel: 'Trash recording',
  });
  if (!ok) return;
  const res = await post(`${API}/api/admin/recordings/${rec.id}/trash`);
  if (!res) return;
  if (!res.ok) { toast(`Trash failed (HTTP ${res.status}).`, 'error'); return; }
  toast('Recording trashed — restorable from Trash.', 'success');
  expanded.delete(rec.id);
  reload();
}

async function bulkTrash() {
  const ids = [...selected];
  if (!ids.length) return;
  const ok = await confirmModal({
    title: `Trash ${ids.length} recording${ids.length === 1 ? '' : 's'}?`,
    body: 'Every appearance of the selected recordings moves to Trash (reversible). The audio files stay on disk.',
    confirmLabel: `Trash ${ids.length}`,
  });
  if (!ok) return;
  const res = await post(`${API}/api/admin/recordings/trash`, { body: { recording_ids: ids } });
  if (!res) return;
  if (!res.ok) { toast(`Trash failed (HTTP ${res.status}).`, 'error'); return; }
  const j = await res.json().catch(() => ({}));
  toast(`Trashed ${j.recordings ?? ids.length} recording(s), ${j.appearances_trashed ?? '?'} appearance(s).`, 'success');
  selected.clear();
  ids.forEach(id => expanded.delete(id));
  reload();
}
trashBtn.addEventListener('click', bulkTrash);

// ── Merge modal ───────────────────────────────────────────────────────────────
const mergeModal = document.getElementById('mergeModal');
const mergePick  = document.getElementById('mergePick');
const mergeSum   = document.getElementById('mergeSum');
const mergeGo    = document.getElementById('mergeGo');
const hideMerge  = wireModal(mergeModal,
  [document.getElementById('mergeClose'), document.getElementById('mergeCancel')]);

// mergeTargetDefault picks the selected recording most likely to hold the
// union's ladder-best rendition: best format class, then more live renditions,
// then the oldest id (stable). The human can always switch the radio.
function mergeTargetDefault(recs) {
  return recs.reduce((best, r) => {
    if (!best) return r;
    const c = formatClass(r.best_format) - formatClass(best.best_format);
    if (c !== 0) return c < 0 ? r : best;
    if (r.live_renditions !== best.live_renditions) return r.live_renditions > best.live_renditions ? r : best;
    return r.id < best.id ? r : best;
  }, null);
}

function openMerge() {
  const recs = vlist.getItems().filter(r => selected.has(r.id));
  if (recs.length < 2) return;
  const def = mergeTargetDefault(recs);
  mergePick.replaceChildren(...recs.map(r => el('label', { class: 'rec-pick-row' }, [
    el('input', { type: 'radio', name: 'mergeTarget', value: String(r.id), ...(r.id === def.id ? { checked: '' } : {}) }),
    el('span', { class: 'rec-id' }, [`#${r.id}`]),
    el('strong', {}, [dispName(r)]),
    r.artist ? el('span', { class: 'muted' }, [` — ${r.artist}`]) : null,
    el('span', { class: 'rec-pick-meta' }, [
      `${r.live_renditions} rend · ${r.appearances} app${r.best_format ? ` · ${r.best_format}` : ''}`,
    ]),
  ])));
  const totR = recs.reduce((n, r) => n + r.live_renditions, 0);
  const totA = recs.reduce((n, r) => n + r.appearances, 0);
  mergeSum.textContent = `Result: ${totR} renditions re-ranked on the quality ladder, up to ${totA} appearances `
    + `(identical ones collapse — the target’s copy wins). The target keeps its primary and its license.`;
  mergeGo.textContent = `Merge ${recs.length} recordings`;
  mergeModal.classList.remove('hidden');
}
mergeBtn.addEventListener('click', openMerge);

mergeGo.addEventListener('click', async () => {
  const target = Number(mergeModal.querySelector('input[name="mergeTarget"]:checked')?.value || 0);
  if (!target) return;
  const sources = [...selected].filter(id => id !== target);
  hideMerge();
  const res = await post(`${API}/api/admin/recordings/merge`, { body: { target_id: target, source_ids: sources } });
  if (!res) return;
  if (res.status === 404) { toast('The selection changed — reloading.', 'error'); reload(); return; }
  if (!res.ok) { toast(`Merge failed (HTTP ${res.status}).`, 'error'); return; }
  const j = await res.json().catch(() => ({}));
  toast(`Merged ${j.sources_merged ?? sources.length} recording(s) into #${target}: `
    + `${j.renditions_moved ?? '?'} rendition(s) moved, ${j.appearances_dropped ?? 0} duplicate appearance(s) collapsed.`, 'success');
  selected.clear();
  sources.forEach(id => expanded.delete(id));
  expanded.delete(target);
  reload();
});

// ── Move-appearance modal ─────────────────────────────────────────────────────
const moveModal  = document.getElementById('moveModal');
const moveBody   = document.getElementById('moveBody');
const moveSearch = document.getElementById('moveSearch');
const movePick   = document.getElementById('movePick');
const moveGo     = document.getElementById('moveGo');
const hideMove   = wireModal(moveModal,
  [document.getElementById('moveClose'), document.getElementById('moveCancel')]);

let moveCtx = null;      // { rec, appearance }
let moveSearchToken = 0; // discards stale async search results

async function runMoveSearch() {
  const q = moveSearch.value.trim();
  const token = ++moveSearchToken;
  if (!q) {
    movePick.replaceChildren(el('p', { class: 'muted rec-pick-empty' }, ['Type to search for the target recording.']));
    moveGo.disabled = true;
    return;
  }
  let page;
  try {
    const res = await fetch(`${API}/api/admin/recordings?q=${encodeURIComponent(q)}&limit=20&offset=0`);
    if (handleAuthError(res) || !res.ok) return;
    page = await res.json();
  } catch { return; }
  if (token !== moveSearchToken || !moveCtx) return;
  const rows = (page.items || []).filter(r => r.id !== moveCtx.rec.id);
  moveGo.disabled = true;
  if (!rows.length) {
    movePick.replaceChildren(el('p', { class: 'muted rec-pick-empty' }, ['No matching recordings.']));
    return;
  }
  movePick.replaceChildren(...rows.map(r => el('label', { class: 'rec-pick-row' }, [
    el('input', { type: 'radio', name: 'moveTarget', value: String(r.id), onchange: () => { moveGo.disabled = false; } }),
    el('span', { class: 'rec-id' }, [`#${r.id}`]),
    el('strong', {}, [dispName(r)]),
    r.artist ? el('span', { class: 'muted' }, [` — ${r.artist}`]) : null,
    el('span', { class: 'rec-pick-meta' }, [`${r.live_renditions} rend${r.best_format ? ` · ${r.best_format}` : ''}${r.dormant ? ' · dormant' : ''}`]),
  ])));
}
let moveTimer = 0;
moveSearch.addEventListener('input', () => { clearTimeout(moveTimer); moveTimer = setTimeout(runMoveSearch, 250); });

function openMove(rec, a) {
  moveCtx = { rec, appearance: a };
  moveBody.replaceChildren(
    'Re-home ', el('strong', {}, [`“${a.title}”`]),
    a.artist ? ` — ${a.artist}` : '', ` from recording #${rec.id} onto another recording:`,
  );
  moveSearch.value = '';
  movePick.replaceChildren(el('p', { class: 'muted rec-pick-empty' }, ['Type to search for the target recording.']));
  moveGo.disabled = true;
  moveModal.classList.remove('hidden');
  moveSearch.focus();
}

moveGo.addEventListener('click', async () => {
  const target = Number(moveModal.querySelector('input[name="moveTarget"]:checked')?.value || 0);
  if (!target || !moveCtx) return;
  const { rec, appearance } = moveCtx;
  hideMove();
  const res = await post(`${API}/api/admin/tagsets/${appearance.tagset_id}/move`, { body: { target_recording_id: target } });
  if (!res) return;
  if (res.status === 409) {
    const j = await res.json().catch(() => ({}));
    toast(j.reason === 'last_appearance'
      ? 'This is the recording’s only appearance — merge the recordings instead.'
      : 'An identical appearance already exists on the target recording.', 'error');
    return;
  }
  if (!res.ok) { toast(`Move failed (HTTP ${res.status}).`, 'error'); return; }
  toast(`Appearance moved to recording #${target}.`, 'success');
  expanded.delete(rec.id);
  expanded.delete(target);
  reload();
});

// ── License & guest modal ─────────────────────────────────────────────────────
const accessModal   = document.getElementById('accessModal');
const accessBody    = document.getElementById('accessBody');
const accessLicense = document.getElementById('accessLicense');
const accessGuest   = document.getElementById('accessGuest');
const accessGo      = document.getElementById('accessGo');
const hideAccess    = wireModal(accessModal,
  [document.getElementById('accessClose'), document.getElementById('accessCancel')]);

let accessCtx = null;

function openAccess(rec) {
  accessCtx = rec;
  accessBody.replaceChildren('Recording ', el('span', { class: 'rec-id' }, [`#${rec.id}`]), ' ',
    el('strong', {}, [`“${dispName(rec)}”`]));
  accessLicense.value = rec.license || '';
  accessGuest.checked = !!rec.guest_playable;
  accessModal.classList.remove('hidden');
}

accessGo.addEventListener('click', async () => {
  if (!accessCtx) return;
  const rec = accessCtx;
  hideAccess();
  const res = await post(`${API}/api/admin/recordings/${rec.id}/access`, {
    method: 'PATCH',
    body: { license: accessLicense.value, guest_playable: accessGuest.checked },
  });
  if (!res) return;
  if (!res.ok) { toast(`Access update failed (HTTP ${res.status}).`, 'error'); return; }
  toast('License & guest access updated.', 'success');
  reload();
});

// ── Rendering ─────────────────────────────────────────────────────────────────
function chip(text, cls = '', title = '') {
  return el('span', { class: `rec-chip ${cls}`, ...(title ? { title } : {}) }, [text]);
}

function headChips(rec) {
  const chips = [
    chip(`${rec.live_renditions} rendition${rec.live_renditions === 1 ? '' : 's'}`, rec.live_renditions > 1 ? 'rec-chip--n' : ''),
    chip(`${rec.appearances} appearance${rec.appearances === 1 ? '' : 's'}`, rec.appearances > 1 ? 'rec-chip--n' : ''),
  ];
  if (!rec.appearances && rec.trashed_appearances) {
    chips.push(chip(`${rec.trashed_appearances} trashed`, 'rec-chip--dorm',
      'Every appearance is in Trash — the recording is hidden until one is restored'));
  }
  if (rec.best_format) chips.push(chip(`★ ${rec.best_format}`, 'rec-chip--best', 'Best rendition by the quality ladder'));
  if (rec.dormant) chips.push(chip('dormant', 'rec-chip--dorm', 'No playable rendition — hidden from the library'));
  if (rec.pinned) chips.push(chip('pinned', '', 'Holds a pinned file — the resolver never regroups it'));
  if (canEdit) {
    chips.push(el('button', {
      class: 'rec-chip rec-chip--lic',
      title: 'Edit license & guest access (recording-level)',
      onclick: e => { e.stopPropagation(); openAccess(rec); },
    }, [`${rec.license || 'no license'}${rec.guest_playable ? ' · guest ✓' : ''} ✎`]));
  } else {
    chips.push(chip(`${rec.license || 'no license'}${rec.guest_playable ? ' · guest ✓' : ''}`, 'rec-chip--lic'));
  }
  return chips;
}

function renditionRow(rec, detail, f) {
  const actions = [];
  if (!f.removed) {
    actions.push(el('button', { class: 'btn btn-sm btn-neutral', onclick: () => playRecording(rec, detail, f.file_id) }, ['Play']));
    actions.push(el('button', {
      class: 'btn btn-sm btn-neutral', title: 'Detach into its own pinned recording',
      onclick: () => splitOff(rec, f),
    }, ['Split off']));
    if (canDelete) actions.push(el('button', {
      class: 'btn btn-sm btn-destructive-outline',
      title: 'Soft-remove this file (restorable); removing the last one makes the recording dormant',
      onclick: () => removeRendition(rec, f, false),
    }, ['Remove']));
  } else if (canDelete) {
    actions.push(el('button', { class: 'btn btn-sm btn-neutral', title: 'Bring the file back', onclick: () => removeRendition(rec, f, true) }, ['Restore']));
  }
  return el('tr', { 'data-file-id': String(f.file_id), class: f.removed ? 'rec-row--removed' : '' }, [
    el('td', { class: 'rec-rank' }, [f.best
      ? el('span', { class: 'rec-best', title: 'Best by the quality ladder' }, ['★ best'])
      : (f.rank ? `#${f.rank}` : '—')]),
    el('td', {}, [f.format || '—', f.pinned ? el('span', { class: 'rec-pin-mark', title: 'Pinned to this recording' }, [' ⚲']) : null]),
    el('td', { class: 'rec-tech' }, [techCell(f)]),
    el('td', {}, [fmtTime(f.duration)]),
    el('td', {}, [fmtBytes(f.size)]),
    el('td', {}, [el('span', { class: `rec-state ${f.removed ? 'st-off' : 'st-ok'}` }, [f.removed ? 'removed' : 'live'])]),
    el('td', { class: 'rec-actions' }, actions),
  ]);
}

function appearanceRow(rec, a) {
  const actions = [];
  if (editor && !a.trashed) {
    actions.push(el('button', {
      class: 'btn btn-sm btn-neutral',
      onclick: () => editor.open({ tagset_id: a.tagset_id, title: a.title, artist: a.artist, album_artist: a.album_artist, album: a.album, _rec: rec.id }),
    }, ['Edit']));
  }
  if (!a.is_primary && !a.trashed) {
    actions.push(el('button', {
      class: 'btn btn-sm btn-neutral', title: 'Make this the appearance that names the recording',
      onclick: () => setPrimary(rec, a),
    }, ['Set primary']));
  }
  if (!a.trashed) {
    actions.push(el('button', {
      class: 'btn btn-sm btn-neutral', title: 'Re-home this appearance onto another recording',
      onclick: () => openMove(rec, a),
    }, ['Move…']));
    if (canDelete) actions.push(el('button', {
      class: 'btn btn-sm btn-destructive-outline', title: 'Trash this appearance (reversible)',
      onclick: () => removeAppearance(rec, a),
    }, ['Remove']));
  } else if (canDelete) {
    actions.push(el('button', {
      class: 'btn btn-sm btn-neutral', title: 'Bring this appearance back into the library',
      onclick: () => restoreAppearance(rec, a),
    }, ['Restore']));
    // Permanent delete lives only on the Trash page (soft-delete.md) — the
    // Appearances lens there. No hard delete here.
  }
  const albumBits = [a.album || '—'];
  if (a.disc != null || a.track != null) albumBits.push(` · ${a.disc ?? '–'}-${a.track ?? '–'}`);
  const state = a.trashed ? ['trashed', 'st-off'] : [a.review_state, APP_STATE[a.review_state] || ''];
  return el('tr', { class: a.trashed ? 'rec-row--removed' : '' }, [
    el('td', { class: 'rec-prim-cell' }, [a.is_primary ? el('span', { class: 'rec-prim', title: 'Primary appearance — names the recording' }, ['●']) : '']),
    el('td', {}, [
      el('div', { class: 'rec-app-title' }, [a.title]),
      a.artist ? el('div', { class: 'muted rec-app-artist' }, [a.artist]) : null,
    ]),
    el('td', { class: 'rec-tech' }, albumBits),
    el('td', {}, [el('span', { class: `rec-state ${state[1]}` }, [state[0]])]),
    el('td', { class: 'rec-actions' }, actions),
  ]);
}

function expandedBody(rec) {
  const detail = expanded.get(rec.id);
  if (detail === null) {
    return el('div', { class: 'rec-body' }, [el('p', { class: 'muted' }, ['Loading…'])]);
  }
  const rendTable = el('table', { class: 'rec-table' }, [
    el('thead', {}, [el('tr', {}, [
      el('th', {}, ['Rank']), el('th', {}, ['Format']), el('th', {}, ['Quality']),
      el('th', {}, ['Length']), el('th', {}, ['Size']), el('th', {}, ['State']), el('th', {}, ['']),
    ])]),
    el('tbody', {}, detail.renditions.map(f => renditionRow(rec, detail, f))),
  ]);
  // The "Add appearance" affordance is the appearances table's own footer row
  // (a hand-authored, blobless appearance on this recording, recording-tagsets
  // P7d) — sitting inside the table so it reads as "add a row here", not as a
  // detached card control. Gated content.moderate, like Move / Set primary.
  const appTable = el('table', { class: 'rec-table' }, [
    el('thead', {}, [el('tr', {}, [
      el('th', { 'aria-label': 'Primary' }, ['']), el('th', {}, ['Track']),
      el('th', {}, ['Album · disc-track']), el('th', {}, ['State']), el('th', {}, ['']),
    ])]),
    el('tbody', {}, detail.appearances.map(a => appearanceRow(rec, a))),
    adder ? el('tfoot', {}, [el('tr', { class: 'rec-app-add-row' }, [
      el('td', { colspan: '5' }, [el('button', {
        class: 'btn btn-sm btn-neutral rec-add-app',
        title: 'Add a metadata-only appearance to this recording (plays its best rendition)',
        onclick: () => adder.openCreate(rec),
      }, ['+ Add appearance'])]),
    ])]) : null,
  ]);
  const foot = el('div', { class: 'rec-foot' }, [
    el('span', { class: 'muted rec-foot-note' }, [rec.dormant
      ? 'Dormant: no playable rendition. Restore a rendition to bring it back to the library.'
      : 'Trash = every appearance to Trash (reversible). Permanent delete lives on the Trash page.']),
    canDelete ? el('button', { class: 'btn btn-sm btn-destructive-outline', onclick: () => trashRecording(rec) }, ['Trash recording']) : null,
  ]);
  return el('div', { class: 'rec-body' }, [
    el('h4', { class: 'rec-arm-h' }, ['Renditions ', el('span', { class: 'muted' }, ['· the audio files'])]),
    el('div', { class: 'rec-tblwrap' }, [rendTable]),
    el('h4', { class: 'rec-arm-h' }, ['Appearances ', el('span', { class: 'muted' }, ['· what listeners see'])]),
    el('div', { class: 'rec-tblwrap' }, [appTable]),
    foot,
  ]);
}

function renderCard(rec) {
  const isOpen = expanded.has(rec.id);
  const check = el('input', {
    type: 'checkbox', class: 'rec-sel', 'aria-label': `Select recording ${rec.id}`,
    ...(selected.has(rec.id) ? { checked: '' } : {}),
    onclick: e => e.stopPropagation(),
    onchange: e => { if (e.target.checked) selected.add(rec.id); else selected.delete(rec.id); updateBulk(); },
  });
  const head = el('div', {
    class: 'rec-head', role: 'button', tabindex: '0', 'aria-expanded': String(isOpen),
    onclick: () => toggleExpand(rec),
    onkeydown: e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggleExpand(rec); } },
  }, [
    check,
    el('span', { class: 'rec-caret', 'aria-hidden': 'true' }, ['▶']),
    el('span', { class: 'rec-id' }, [`#${rec.id}`]),
    el('span', { class: 'rec-name' }, [dispName(rec), rec.artist ? el('span', { class: 'rec-by' }, [` — ${rec.artist}`]) : null]),
    el('span', { class: 'rec-chips' }, headChips(rec)),
    rec.dormant ? null : el('button', {
      class: 'btn btn-sm btn-neutral rec-play',
      onclick: async e => {
        e.stopPropagation();
        let detail = expanded.get(rec.id);
        if (!detail) detail = await loadDetail(rec.id);
        if (detail) playRecording(rec, detail);
      },
    }, ['▶ Play']),
  ]);
  return el('section', {
    class: `rec-card${isOpen ? ' rec-card--open' : ''}${rec.dormant ? ' rec-card--dormant' : ''}`,
    'data-id': String(rec.id),
  }, [head, isOpen ? expandedBody(rec) : null]);
}

// ── Filters / search wiring ───────────────────────────────────────────────────
document.querySelectorAll('.rec-pill').forEach(p => p.addEventListener('click', () => {
  document.querySelectorAll('.rec-pill').forEach(x => x.classList.remove('rec-pill--on'));
  p.classList.add('rec-pill--on');
  filter = p.dataset.filter;
  reload();
}));

let searchTimer = 0;
searchEl.addEventListener('input', () => {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => { query = searchEl.value.trim(); reload(); }, 300);
});

// ── Init ──────────────────────────────────────────────────────────────────────
(async function init() {
  const identity = await bootAdmin({ require: 'content.moderate' });
  if (!identity) return;
  const perms = identity.permissions || [];
  canDelete = perms.includes('file.delete');
  canEdit = perms.includes('metadata.edit');
  canModerate = perms.includes('content.moderate');
  trashBtn.hidden = !canDelete;

  if (canEdit) {
    editor = createTrackEditor({
      patchURL: f => `${API}/api/admin/moderation/${f.tagset_id}/metadata`,
      detailURL: f => `${API}/api/admin/moderation/${f.tagset_id}/metadata`,
      note: 'Edits this appearance’s tags. Identity changes (album/artist) re-resolve its entities.',
      checkAuth: handleAuthError,
      onSaved: async f => { toast('Tags updated.', 'success'); await refreshDetail(f._rec); reload(); },
      onError: () => toast('Couldn’t save tags.', 'error'),
    });
  }

  // Add appearance is content.moderate — the page's own gate, matching Move /
  // Set primary / Merge. The created appearance is blobless (it plays the
  // recording's best rendition) and immediately approved.
  if (canModerate) {
    adder = createTrackEditor({
      create: true,
      createTitle: 'Add appearance',
      createNote: 'A metadata-only appearance on this recording — it carries no audio of its own and '
        + 'plays the recording’s best rendition. Give it at least an artist or an album.',
      createURL: rec => `${API}/api/admin/recordings/${rec.id}/appearances`,
      checkAuth: handleAuthError,
      onCreated: async (_data, rec) => {
        toast('Appearance added.', 'success');
        await refreshDetail(rec.id);
        reload();
      },
    });
  }

  vlist = createVirtualList({
    windowScroll: true,
    sizerEl: listHost,
    makeSpacer: px => el('div', { style: `height:${px}px` }),
    renderRow: renderCard,
    estimateHeight: rec => (expanded.has(rec.id) ? 620 : 58),
    buffer: 4,
    fetchMore: async () => {
      const have = vlist.count();
      if (have >= total) return { items: [], done: true };
      const page = await fetchPage(have);
      total = page.total;
      return { items: page.items, done: have + page.items.length >= page.total };
    },
  });

  // Deep link: #<id> expands that recording (the files view's recording links).
  const anchor = Number((location.hash || '').replace('#', ''));
  await reload();
  if (anchor > 0) {
    searchEl.value = `#${anchor}`;
    query = `#${anchor}`;
    await reload();
    const rec = vlist.getItems().find(r => r.id === anchor);
    if (rec) toggleExpand(rec);
  }
})();
