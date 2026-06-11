// Admin · Moderation — the review queue (docs/plans/moderation-review-bucket.md
// phase 3). Staged uploads grouped by uploader in collapsible sections.
//
// Selection model: only files *awaiting review* (submitted) are selectable —
// per row, per uploader (the group-header checkbox), or globally — and the one
// bulk toolbar acts across groups. Returned files deliberately carry no
// checkbox: a bulk approve right after a return must not republish the very
// files just sent back (they keep per-row actions for a deliberate change of
// mind). Drafts are info-only. Bulk actions loop the per-file endpoints
// client-side, matching the trash/entity-delete convention. Requires
// content.moderate.
import { bootAdmin, API, fmtBytes, fmtDate, shortHash, toast, handleAuthError, el } from './shared.js';
import { createPlayer } from '../player.js';
import { createTrackEditor } from '../track-edit.js';

const queueEl    = document.getElementById('modQueue');
const modCountEl = document.getElementById('modCount');

// Global bulk toolbar (static markup in moderation.html)
const modToolbar    = document.getElementById('modToolbar');
const modSelectAll  = document.getElementById('modSelectAll');
const modSelCount   = document.getElementById('modSelCount');
const btnApproveSel = document.getElementById('modApproveSel');
const btnReturnSel  = document.getElementById('modReturnSel');
const btnDiscardSel = document.getElementById('modDiscardSel');

let allItems  = [];          // last loaded /api/admin/moderation list
let canEdit   = false;       // metadata.edit → the per-row Edit button
const collapsed = new Set(); // uploader keys collapsed by the user (survives re-render)

const ACTIONABLE  = new Set(['submitted', 'returned']); // per-row actions
const selectable  = f => f.state === 'submitted';       // bulk selection
const STATE_LABEL = { submitted: 'Awaiting review', returned: 'Returned', draft: 'Draft' };

const displayTitle = f => f.title || f.filename || 'this file';
const uploaderKey  = f => String(f.uploader_id || 0);

// ── Preview player (page-local, like every admin page) ──────────────────────
let playCtx = null; // { items: [{url,title,artist,key}], index }

const player = createPlayer({
  onPrev:  () => { if (playCtx) playAt(playCtx.index > 0 ? playCtx.index - 1 : playCtx.items.length - 1); },
  onNext:  () => { if (playCtx) playAt(playCtx.index < playCtx.items.length - 1 ? playCtx.index + 1 : 0); },
  onEnded: () => { if (playCtx && playCtx.index < playCtx.items.length - 1) playAt(playCtx.index + 1); },
  onError: () => {
    toast('Couldn’t play this file.', 'error');
    if (playCtx && playCtx.index < playCtx.items.length - 1) playAt(playCtx.index + 1);
  },
});

function playFile(f) {
  const items = allItems.map(x => ({ url: x.url, title: displayTitle(x), artist: x.artist || '', key: x.hash }));
  let idx = items.findIndex(x => x.key === f.hash);
  if (idx < 0) idx = 0;
  playCtx = { items, index: idx };
  playAt(idx);
}

function playAt(i) {
  if (!playCtx || i < 0 || i >= playCtx.items.length) return;
  playCtx.index = i;
  const it = playCtx.items[i];
  const url = /^https?:/.test(it.url) ? it.url : `${API}${it.url}`;
  player.load({ url, title: it.title, artist: it.artist });
  highlightPlaying();
}

function highlightPlaying() {
  queueEl.querySelectorAll('tr.playing-row').forEach(tr => tr.classList.remove('playing-row'));
  if (!playCtx) return;
  const it = playCtx.items[playCtx.index];
  queueEl.querySelector(`tr[data-hash="${CSS.escape(it.key)}"]`)?.classList.add('playing-row');
}

// ── Loading + rendering ──────────────────────────────────────────────────────
async function loadQueue() {
  queueEl.setAttribute('aria-busy', 'true');
  queueEl.replaceChildren(el('p', { class: 'section-copy', text: 'Loading…' }));

  let items;
  try {
    const res = await fetch(`${API}/api/admin/moderation`);
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    items = await res.json();
  } catch (err) {
    queueEl.setAttribute('aria-busy', 'false');
    const retry = el('button', { class: 'btn btn-neutral btn-sm', text: 'Retry', onclick: loadQueue });
    queueEl.replaceChildren(el('div', { role: 'alert' }, [
      el('p', { class: 'section-copy', text: `Failed to load the queue: ${err.message}` }), retry,
    ]));
    return;
  }

  queueEl.setAttribute('aria-busy', 'false');
  allItems = Array.isArray(items) ? items : [];
  renderQueue();
}

// groups preserves the server's by-uploader ordering.
function groups() {
  const byKey = new Map();
  for (const f of allItems) {
    const key = uploaderKey(f);
    if (!byKey.has(key)) byKey.set(key, { key, name: f.uploader || '(unknown uploader)', items: [] });
    byKey.get(key).items.push(f);
  }
  return [...byKey.values()];
}

function renderQueue() {
  modCountEl.textContent = String(allItems.length);

  if (allItems.length === 0) {
    queueEl.replaceChildren(
      el('div', { class: 'empty-state' }, [
        el('div', { class: 'drop-icon', 'aria-hidden': 'true', text: '✓' }),
        el('p', { text: 'Nothing awaiting review' }),
        el('p', { text: 'New uploads appear here once their uploaders send them to approval.' }),
      ]),
    );
    updateSelection();
    return;
  }

  const frag = document.createDocumentFragment();
  groups().forEach(g => frag.appendChild(buildGroup(g)));
  queueEl.replaceChildren(frag);
  highlightPlaying();
  updateSelection();
}

function groupCounts(items) {
  const n = s => items.filter(f => f.state === s).length;
  const parts = [];
  if (n('submitted')) parts.push(`${n('submitted')} awaiting`);
  if (n('returned'))  parts.push(`${n('returned')} returned`);
  if (n('draft'))     parts.push(`${n('draft')} draft${n('draft') === 1 ? '' : 's'}`);
  return parts.join(' · ');
}

function buildGroup(g) {
  const section = el('section', { class: 'mod-group' + (collapsed.has(g.key) ? ' is-collapsed' : '') });
  const bodyId = `modGroupBody-${g.key}`;

  // The group checkbox selects this uploader's whole awaiting-review batch —
  // it works even while the group is collapsed ("approve all of this user").
  const groupCheck = el('input', {
    type: 'checkbox', class: 'mod-group-check',
    'aria-label': `Select all files awaiting review from ${g.name}`,
  });
  groupCheck.addEventListener('change', () => {
    section.querySelectorAll('input.mod-check').forEach(c => (c.checked = groupCheck.checked));
    updateSelection();
  });
  if (!g.items.some(selectable)) groupCheck.disabled = true;

  const toggle = el('button', {
    class: 'mod-group-toggle', 'aria-expanded': String(!collapsed.has(g.key)), 'aria-controls': bodyId,
    onclick: () => {
      const isCollapsed = section.classList.toggle('is-collapsed');
      if (isCollapsed) collapsed.add(g.key); else collapsed.delete(g.key);
      toggle.setAttribute('aria-expanded', String(!isCollapsed));
    },
  }, [
    el('span', { class: 'mod-group-chevron', 'aria-hidden': 'true', text: '▾' }),
    el('span', { text: g.name }),
    el('span', { class: 'mod-group-counts', text: groupCounts(g.items) }),
  ]);

  const header = el('div', { class: 'mod-group-header' }, [groupCheck, toggle]);

  const tbody = el('tbody');
  g.items.forEach(f => tbody.appendChild(buildRow(f)));

  const body = el('div', { class: 'mod-group-body', id: bodyId }, [
    el('div', { class: 'files-table-wrap' }, [
      el('table', { class: 'files-table' }, [
        el('thead', {}, [el('tr', {}, [
          el('th', { scope: 'col', class: 'col-check' }),
          el('th', { scope: 'col', text: 'Title' }),
          el('th', { scope: 'col', text: 'Artist' }),
          el('th', { scope: 'col', text: 'Album' }),
          el('th', { scope: 'col', class: 'col-size', text: 'Size' }),
          el('th', { scope: 'col', text: 'State' }),
          el('th', { scope: 'col', text: 'Submitted' }),
          el('th', { scope: 'col', class: 'col-actions', text: 'Actions' }),
        ])]),
        tbody,
      ]),
    ]),
  ]);

  section.append(header, body);
  return section;
}

function buildRow(f) {
  const tr = el('tr', { 'data-hash': f.hash });

  // Selection: awaiting-review rows only (returned/drafts stay out of bulk).
  let check = '';
  if (selectable(f)) {
    check = el('input', { type: 'checkbox', class: 'mod-check', 'aria-label': `Select ${displayTitle(f)}` });
    check.addEventListener('change', updateSelection);
  }
  const tdCheck = el('td', { class: 'cell-check' }, [check]);

  const titleSpan = f.title
    ? el('span', { class: 'cell-title', text: f.title })
    : el('span', { class: 'cell-title is-fallback', text: f.filename || 'Untitled' });
  const hashSpan = el('span', { class: 'cell-hash', title: f.hash || '', text: shortHash(f.hash) });
  const titleKids = [titleSpan, hashSpan];
  if (f.state === 'returned' && f.note) {
    titleKids.push(el('span', { class: 'mod-note', text: `Note: ${f.note}` }));
  }
  const tdTitle = el('td', { class: 'cell-title-td', 'data-label': 'Title' }, titleKids);

  const tdArtist = f.artist
    ? el('td', { 'data-label': 'Artist', text: f.artist })
    : el('td', { class: 'cell-muted', 'data-label': 'Artist', text: '—' });
  const tdAlbum = f.album
    ? el('td', { 'data-label': 'Album', text: f.album })
    : el('td', { class: 'cell-muted', 'data-label': 'Album', text: '—' });

  const tdSize  = el('td', { class: 'cell-size', 'data-label': 'Size', text: fmtBytes(f.byte_size) });
  const tdState = el('td', { 'data-label': 'State' }, [
    el('span', { class: `state-badge is-${f.state}`, text: STATE_LABEL[f.state] || f.state }),
  ]);
  const tdWhen = f.submitted_at
    ? el('td', { 'data-label': 'Submitted', text: fmtDate(f.submitted_at) })
    : el('td', { class: 'cell-muted', 'data-label': 'Submitted', text: '—' });

  const wrap = el('div', { class: 'trash-actions' }, buildActions(tr, f));
  const tdActions = el('td', { class: 'cell-actions', 'data-label': 'Actions' }, [wrap]);

  tr.append(tdCheck, tdTitle, tdArtist, tdAlbum, tdSize, tdState, tdWhen, tdActions);
  return tr;
}

const PLAY_ICON = '<svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M8 5v14l11-7z"/></svg>';

function buildActions(tr, f) {
  const play = el('button', {
    class: 'play-btn', title: 'Preview', 'aria-label': `Preview ${displayTitle(f)}`,
    onclick: () => playFile(f),
  });
  play.innerHTML = PLAY_ICON;

  const actions = [play];
  if (!ACTIONABLE.has(f.state)) return actions; // drafts: preview only

  if (canEdit) {
    actions.push(el('button', { class: 'btn btn-neutral btn-sm', text: 'Edit', onclick: () => trackEditor.open(f) }));
  }
  actions.push(
    el('button', { class: 'btn btn-neutral btn-sm', text: 'Approve', onclick: () => approveHashes([f.hash]) }),
    el('button', { class: 'btn btn-neutral btn-sm', text: 'Return…', onclick: () => openReturnModal([f.hash]) }),
    el('button', {
      class: 'btn btn-destructive-outline btn-sm', text: 'Discard',
      onclick: e => enterDiscardConfirm(tr, f, e.currentTarget.parentElement),
    }),
  );
  return actions;
}

// ── Selection (cross-group) + global toolbar ────────────────────────────────
function rowChecks() { return [...queueEl.querySelectorAll('input.mod-check')]; }
function selectedHashes() {
  return rowChecks().filter(c => c.checked).map(c => c.closest('tr').dataset.hash);
}

// updateSelection recomputes the toolbar and syncs the global and per-group
// select-all checkboxes against the row checks.
function updateSelection() {
  const checks = rowChecks();
  const total = checks.length;
  const sel = checks.filter(c => c.checked).length;

  modToolbar.hidden = total === 0;
  modSelCount.textContent = `${sel} selected`;
  [btnApproveSel, btnReturnSel, btnDiscardSel].forEach(b => (b.disabled = sel === 0));
  modSelectAll.checked = total > 0 && sel === total;
  modSelectAll.indeterminate = sel > 0 && sel < total;

  queueEl.querySelectorAll('.mod-group').forEach(section => {
    const gc = section.querySelector('input.mod-group-check');
    if (!gc) return;
    const cs = [...section.querySelectorAll('input.mod-check')];
    const gSel = cs.filter(c => c.checked).length;
    gc.checked = cs.length > 0 && gSel === cs.length;
    gc.indeterminate = gSel > 0 && gSel < cs.length;
  });
}

modSelectAll.addEventListener('change', () => {
  rowChecks().forEach(c => (c.checked = modSelectAll.checked));
  updateSelection();
});
btnApproveSel.addEventListener('click', () => approveHashes(selectedHashes()));
btnReturnSel.addEventListener('click', () => openReturnModal(selectedHashes()));
btnDiscardSel.addEventListener('click', () => confirmBulkDiscard(selectedHashes()));

// ── Metadata edit (the shared track-edit.js component, metadata.edit gated) ──
const trackEditor = createTrackEditor({
  patchURL: f => `${API}/api/files/${encodeURIComponent(f.hash)}/metadata`,
  note: 'Edits this submission’s tags before approval.',
  checkAuth: handleAuthError,
  onSaved: (f, data) => {
    f.title = data.title; f.artist = data.artist;
    f.album = data.album; f.album_artist = data.album_artist;
    renderQueue();
    toast(`Metadata saved for "${displayTitle(f)}".`, 'success');
  },
  onError: err => toast(`Couldn't save metadata: ${err.message}`, 'error'),
});

// ── Actions: approve / return / discard ─────────────────────────────────────
// runBulk applies one request per hash sequentially and tallies the results;
// callers own the user-facing summary (the trash-page convention).
async function runBulk(hashes, makeRequest) {
  let ok = 0, fail = 0, authFailed = false;
  const okHashes = [];
  for (const hash of hashes) {
    let res;
    try { res = await makeRequest(hash); }
    catch { fail++; continue; }
    if (res.status === 401) { handleAuthError(res); authFailed = true; break; }
    const data = await res.json().catch(() => ({}));
    if (res.ok && data.ok) { ok++; okHashes.push(hash); } else fail++;
  }
  return { ok, fail, okHashes, authFailed };
}

function setQueueBusy(busy) {
  [modSelectAll, btnApproveSel, btnReturnSel, btnDiscardSel].forEach(n => (n.disabled = busy));
  queueEl.querySelectorAll('button, input[type="checkbox"]').forEach(n => (n.disabled = busy));
}

async function approveHashes(hashes) {
  if (!hashes.length) return;
  setQueueBusy(true);
  const { ok, fail, okHashes, authFailed } = await runBulk(hashes, hash =>
    fetch(`${API}/api/admin/moderation/${encodeURIComponent(hash)}/approve`, { method: 'POST' }));
  const done = new Set(okHashes);
  allItems = allItems.filter(f => !done.has(f.hash));
  renderQueue();
  if (authFailed) { if (ok) toast(`Approved ${ok} before the session expired.`, 'error'); return; }
  if (fail) toast(`Approved ${ok}; ${fail} failed.`, 'error');
  else if (ok) toast(`Approved ${ok} ${ok === 1 ? 'file' : 'files'} into the library.`, 'success');
}

async function returnHashes(hashes, note) {
  setQueueBusy(true);
  const { ok, fail, okHashes, authFailed } = await runBulk(hashes, hash =>
    fetch(`${API}/api/admin/moderation/${encodeURIComponent(hash)}/return`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ note }),
    }));
  const done = new Set(okHashes);
  allItems.forEach(f => { if (done.has(f.hash)) { f.state = 'returned'; f.note = note; } });
  renderQueue();
  if (authFailed) { if (ok) toast(`Returned ${ok} before the session expired.`, 'error'); return; }
  if (fail) toast(`Returned ${ok}; ${fail} failed.`, 'error');
  else if (ok) toast(`Returned ${ok} ${ok === 1 ? 'file' : 'files'} to the uploader.`, 'success');
}

// Discard = the existing soft delete; restore from Trash re-enters this queue.
async function discardHashes(hashes) {
  if (!hashes.length) return;
  setQueueBusy(true);
  const { ok, fail, okHashes, authFailed } = await runBulk(hashes, hash =>
    fetch(`${API}/api/admin/files/${encodeURIComponent(hash)}`, { method: 'DELETE' }));
  const done = new Set(okHashes);
  allItems = allItems.filter(f => !done.has(f.hash));
  renderQueue();
  if (authFailed) { if (ok) toast(`Discarded ${ok} before the session expired.`, 'error'); return; }
  if (fail) toast(`Discarded ${ok} to Trash; ${fail} failed.`, 'error');
  else if (ok) toast(`Discarded ${ok} ${ok === 1 ? 'file' : 'files'} to Trash.`, 'success');
}

// Single-row discard confirms inline (the Files-page pattern).
function enterDiscardConfirm(tr, f, actionsWrap) {
  const restore = () => {
    actionsWrap.replaceChildren(...buildActions(tr, f));
    actionsWrap.querySelector('button')?.focus();
  };
  const cancel  = el('button', { class: 'btn btn-neutral btn-sm', text: 'Cancel', onclick: restore });
  const confirm = el('button', {
    class: 'btn btn-destructive-solid btn-sm', text: 'Discard',
    onclick: () => discardHashes([f.hash]),
  });
  actionsWrap.replaceChildren(
    el('span', { class: 'delete-confirm-label', text: 'Discard to Trash?' }),
    cancel, confirm,
  );
  actionsWrap.addEventListener('keydown', e => { if (e.key === 'Escape') { e.stopPropagation(); restore(); } });
  cancel.focus();
}

// ── Return-with-note modal (one note for the whole selection) ───────────────
const returnModal   = document.getElementById('returnModal');
const returnForm    = document.getElementById('returnForm');
const returnBody    = document.getElementById('returnBody');
const returnNote    = document.getElementById('returnNote');
const returnError   = document.getElementById('returnError');
const returnConfirm = document.getElementById('returnConfirm');
let returnTarget = null; // hashes

function openReturnModal(hashes) {
  if (!hashes.length) return;
  returnTarget = hashes;
  returnBody.textContent = hashes.length === 1
    ? 'Send this file back to its uploader to fix?'
    : `Send ${hashes.length} files back to their uploader with one note?`;
  returnNote.value = '';
  returnError.hidden = true; returnError.textContent = '';
  returnModal.classList.remove('hidden');
  returnNote.focus();
}
function closeReturnModal() { returnModal.classList.add('hidden'); returnTarget = null; }

returnForm.addEventListener('submit', async e => {
  e.preventDefault();
  if (!returnTarget) return;
  const note = returnNote.value.trim();
  if (!note) { returnError.textContent = 'A note is required.'; returnError.hidden = false; return; }
  const hashes = returnTarget;
  returnConfirm.disabled = true;
  closeReturnModal();
  await returnHashes(hashes, note);
  returnConfirm.disabled = false;
});
document.getElementById('returnClose').addEventListener('click', closeReturnModal);
document.getElementById('returnCancel').addEventListener('click', closeReturnModal);
returnModal.addEventListener('click', e => { if (e.target === returnModal) closeReturnModal(); });

// ── Bulk discard confirm modal ───────────────────────────────────────────────
const discardModal   = document.getElementById('discardModal');
const discardBody    = document.getElementById('discardBody');
const discardError   = document.getElementById('discardError');
const discardConfirm = document.getElementById('discardConfirm');
let discardTarget = null; // hashes

function confirmBulkDiscard(hashes) {
  if (!hashes.length) return;
  discardTarget = hashes;
  discardBody.textContent = `Discard ${hashes.length} ${hashes.length === 1 ? 'file' : 'files'} to Trash?`;
  discardConfirm.textContent = `Discard ${hashes.length}`;
  discardError.hidden = true; discardError.textContent = '';
  discardModal.classList.remove('hidden');
  discardConfirm.focus();
}
function closeBulkDiscard() { discardModal.classList.add('hidden'); discardTarget = null; }

discardConfirm.addEventListener('click', async () => {
  if (!discardTarget) return;
  const hashes = discardTarget;
  closeBulkDiscard();
  await discardHashes(hashes);
});
document.getElementById('discardClose').addEventListener('click', closeBulkDiscard);
document.getElementById('discardCancel').addEventListener('click', closeBulkDiscard);
discardModal.addEventListener('click', e => { if (e.target === discardModal) closeBulkDiscard(); });

document.addEventListener('keydown', e => {
  if (e.key !== 'Escape') return;
  if (!returnModal.classList.contains('hidden')) closeReturnModal();
  if (!discardModal.classList.contains('hidden')) closeBulkDiscard();
});

// ── Boot ────────────────────────────────────────────────────────────────────
(async function boot() {
  const identity = await bootAdmin({ require: 'content.moderate' });
  if (!identity) return;
  canEdit = (identity.permissions || []).includes('metadata.edit');
  loadQueue();
})();
