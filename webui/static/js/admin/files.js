// Admin · Files — the files table with per-file access controls, metadata
// editing, two-step delete, and a preview player (the shared player.js).
import {
  bootAdmin, API, LICENSE_OPTIONS,
  fmtBytes, shortHash, toast, handleAuthError, el,
} from './shared.js';
import { createPlayer } from '../player.js';

const filesBody   = document.getElementById('filesBody');
const fileCountEl = document.getElementById('fileCount');
const fileFilter  = document.getElementById('fileFilter');

let allFiles    = [];     // last fetched list
let filterText  = '';
let canEditMeta = false;  // metadata.edit → access controls + metadata edit
let canDelete   = false;  // file.delete → move-to-trash

// ── Preview player (single list snapshot, navigable) ─────────────────────────
let previewList  = [];
let previewIndex = -1;

const player = createPlayer({
  onPrev:  () => { if (previewIndex >= 0) playAt(previewIndex > 0 ? previewIndex - 1 : previewList.length - 1); },
  onNext:  () => { if (previewIndex >= 0) playAt(previewIndex < previewList.length - 1 ? previewIndex + 1 : 0); },
  onEnded: () => { if (previewIndex >= 0 && previewIndex < previewList.length - 1) playAt(previewIndex + 1); },
  onError: () => {
    toast('Couldn’t play this file.', 'error');
    if (previewIndex >= 0 && previewIndex < previewList.length - 1) playAt(previewIndex + 1);
  },
});

function playFile(f) {
  previewList  = visibleFiles();
  previewIndex = previewList.findIndex(x => x.hash === f.hash);
  if (previewIndex < 0) { previewList = [f]; previewIndex = 0; }
  playAt(previewIndex);
}

function playAt(i) {
  if (i < 0 || i >= previewList.length) return;
  previewIndex = i;
  const f = previewList[i];
  player.load({ url: `${API}${f.url}`, title: displayTitle(f), artist: f.artist || '' });
  highlightPlaying(f.hash);
}

function highlightPlaying(hash) {
  filesBody.querySelectorAll('tr.playing-row').forEach(tr => tr.classList.remove('playing-row'));
  const row = filesBody.querySelector(`tr[data-hash="${CSS.escape(hash)}"]`);
  if (row) row.classList.add('playing-row');
}

// ── Loading + rendering ──────────────────────────────────────────────────────
async function loadFiles() {
  filesBody.setAttribute('aria-busy', 'true');
  renderStateRow('Loading files…');

  let files;
  try {
    const res = await fetch(`${API}/api/files`);
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    files = await res.json();
  } catch (err) {
    console.error('Failed to load files:', err);
    filesBody.setAttribute('aria-busy', 'false');
    renderErrorRow();
    return;
  }

  allFiles = Array.isArray(files) ? files : [];
  filesBody.setAttribute('aria-busy', 'false');
  renderFiles();
}

function renderStateRow(text) {
  filesBody.replaceChildren(
    el('tr', { class: 'table-state-row' }, [el('td', { colspan: '6', text })]),
  );
}

function renderErrorRow() {
  const retry = el('button', { class: 'btn btn-neutral btn-sm', onclick: loadFiles, text: 'Retry' });
  retry.style.marginTop = 'var(--space-3)';
  filesBody.replaceChildren(
    el('tr', { class: 'table-state-row' }, [
      el('td', { colspan: '6' }, [
        el('div', { role: 'alert', text: 'Failed to load files.' }),
        retry,
      ]),
    ]),
  );
}

function renderEmptyRow() {
  filesBody.replaceChildren(
    el('tr', { class: 'table-state-row' }, [
      el('td', { colspan: '6' }, [
        el('div', { class: 'empty-state' }, [
          el('div', { class: 'drop-icon', 'aria-hidden': 'true', text: '♪' }),
          el('p', { text: 'No files yet' }),
          el('p', { text: 'Add music from the Upload page.' }),
        ]),
      ]),
    ]),
  );
}

function matchesFilter(f, q) {
  if (!q) return true;
  const hay = [f.title, f.artist, f.album, f.filename].filter(Boolean).join(' ').toLowerCase();
  return hay.includes(q);
}

function visibleFiles() {
  const q = filterText.trim().toLowerCase();
  return allFiles.filter(f => matchesFilter(f, q));
}

function renderFiles() {
  fileCountEl.textContent = String(allFiles.length);

  if (allFiles.length === 0) { renderEmptyRow(); return; }

  const visible = visibleFiles();
  if (visible.length === 0) {
    renderStateRow(`No files match “${filterText.trim()}”`);
    return;
  }

  const frag = document.createDocumentFragment();
  visible.forEach(f => frag.appendChild(buildRow(f)));
  filesBody.replaceChildren(frag);

  // Keep the playing highlight after a re-render.
  if (previewIndex >= 0 && previewList[previewIndex]) {
    highlightPlaying(previewList[previewIndex].hash);
  }
}

function buildRow(f) {
  const tr = el('tr', { 'data-hash': f.hash });

  // Title (+ hash)
  const titleSpan = f.title
    ? el('span', { class: 'cell-title', text: f.title })
    : el('span', { class: 'cell-title is-fallback', text: f.filename || 'Untitled' });
  const hashSpan = el('span', { class: 'cell-hash', title: f.hash || '', text: shortHash(f.hash) });
  const tdTitle = el('td', { class: 'cell-title-td', 'data-label': 'Title' }, [titleSpan, hashSpan]);

  const tdArtist = f.artist
    ? el('td', { 'data-label': 'Artist', text: f.artist })
    : el('td', { class: 'cell-muted', 'data-label': 'Artist', text: '—' });

  const tdAlbum = f.album
    ? el('td', { 'data-label': 'Album', text: f.album })
    : el('td', { class: 'cell-muted', 'data-label': 'Album', text: '—' });

  const tdSize = el('td', { class: 'cell-size', 'data-label': 'Size', text: fmtBytes(f.byte_size) });

  const tdAccess = el('td', { class: 'cell-access', 'data-label': 'Access' }, [buildAccessControls(f)]);

  const tdActions = el('td', { class: 'cell-actions', 'data-label': 'Actions' }, buildActions(tr, f));

  tr.append(tdTitle, tdArtist, tdAlbum, tdSize, tdAccess, tdActions);
  return tr;
}

// ── Per-row actions: play, edit, delete ──────────────────────────────────────
const PLAY_ICON = '<svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M8 5v14l11-7z"/></svg>';

function buildActions(tr, f) {
  const play = el('button', {
    class: 'play-btn', title: 'Preview', 'aria-label': `Preview ${displayTitle(f)}`,
    onclick: () => playFile(f),
  });
  play.innerHTML = PLAY_ICON;

  const actions = [play];
  if (canEditMeta) {
    actions.push(el('button', {
      class: 'btn btn-neutral btn-sm', text: 'Edit',
      onclick: () => openEditModal(f),
    }));
  }
  if (canDelete) {
    actions.push(makeDeleteButton(tr, f));
  }
  return actions;
}

// ── Access controls (guest toggle + license) ─────────────────────────────────
function buildAccessControls(f) {
  if (!canEditMeta) {
    const summary = (f.guest_playable ? 'Guest' : 'Private') + (f.license ? ` · ${f.license}` : '');
    return el('div', { class: 'access-controls' }, [el('span', { class: 'cell-muted', text: summary })]);
  }

  const cb = el('input', { type: 'checkbox' });
  cb.checked = !!f.guest_playable;
  cb.addEventListener('change', () => setGuest(f, cb));
  const label = el('label', { class: 'guest-toggle' }, [cb, el('span', { text: 'Guest' })]);

  const sel = el('select', { class: 'license-select', 'aria-label': 'License' });
  LICENSE_OPTIONS.forEach(lic => {
    const opt = el('option', { value: lic, text: lic || '— license —' });
    if ((f.license || '') === lic) opt.selected = true;
    sel.appendChild(opt);
  });
  sel.addEventListener('change', () => setLicense(f, sel));

  return el('div', { class: 'access-controls' }, [label, sel]);
}

async function setGuest(f, cb) {
  const desired = cb.checked;
  cb.disabled = true;
  try {
    const res = await fetch(`${API}/api/admin/files/${encodeURIComponent(f.hash)}/guest`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ guest_playable: desired }),
    });
    if (handleAuthError(res)) { cb.checked = !desired; return; }
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    f.guest_playable = desired;
    toast(`"${displayTitle(f)}" is now ${desired ? 'guest-playable' : 'private'}.`, 'success');
  } catch (err) {
    cb.checked = !desired;
    toast(`Couldn't update access: ${err.message}`, 'error');
  } finally {
    cb.disabled = false;
  }
}

async function setLicense(f, sel) {
  const desired = sel.value;
  const previous = f.license || '';
  sel.disabled = true;
  try {
    const res = await fetch(`${API}/api/admin/files/${encodeURIComponent(f.hash)}/license`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ license: desired }),
    });
    if (handleAuthError(res)) { sel.value = previous; return; }
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    f.license = desired;
    toast(`License set to ${desired || 'none'} for "${displayTitle(f)}".`, 'success');
    // Auto-derive may have flipped guest-playable; reload to stay authoritative.
    loadFiles();
  } catch (err) {
    sel.value = previous;
    toast(`Couldn't update license: ${err.message}`, 'error');
  } finally {
    sel.disabled = false;
  }
}

function displayTitle(f) {
  return f.title || f.filename || 'this file';
}

// ── Metadata edit modal ──────────────────────────────────────────────────────
const editModal   = document.getElementById('editModal');
const editForm    = document.getElementById('editForm');
const editTitleIn = document.getElementById('editTitleInput');
const editArtist  = document.getElementById('editArtist');
const editAlbumAr = document.getElementById('editAlbumArtist');
const editAlbum   = document.getElementById('editAlbum');
const editSubmit  = document.getElementById('editSubmit');
let editingFile   = null;

function openEditModal(f) {
  editingFile = f;
  editTitleIn.value = f.title || '';
  editArtist.value  = f.artist || '';
  editAlbumAr.value = f.album_artist || '';
  editAlbum.value   = f.album || '';
  editModal.classList.remove('hidden');
  editTitleIn.focus();
}

function closeEditModal() {
  editModal.classList.add('hidden');
  editingFile = null;
}

document.getElementById('editClose').addEventListener('click', closeEditModal);
document.getElementById('editCancel').addEventListener('click', closeEditModal);
editModal.addEventListener('click', e => { if (e.target === editModal) closeEditModal(); });
document.addEventListener('keydown', e => {
  if (e.key === 'Escape' && !editModal.classList.contains('hidden')) closeEditModal();
});

editForm.addEventListener('submit', async e => {
  e.preventDefault();
  if (!editingFile) return;
  const f = editingFile;
  const body = {
    title:        editTitleIn.value,
    artist:       editArtist.value,
    album_artist: editAlbumAr.value,
    album:        editAlbum.value,
  };
  editSubmit.disabled = true;
  try {
    const res = await fetch(`${API}/api/files/${encodeURIComponent(f.hash)}/metadata`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (handleAuthError(res)) return;
    const data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
    // Reflect the authoritative values returned by the server.
    f.title = data.title; f.artist = data.artist;
    f.album = data.album; f.album_artist = data.album_artist;
    renderFiles();
    toast(`Metadata saved for "${displayTitle(f)}".`, 'success');
    closeEditModal();
  } catch (err) {
    toast(`Couldn't save metadata: ${err.message}`, 'error');
  } finally {
    editSubmit.disabled = false;
  }
});

// ── Two-step delete (move to trash) ──────────────────────────────────────────
function makeDeleteButton(tr, f) {
  return el('button', {
    class: 'btn btn-destructive-outline btn-sm', text: 'Move to Trash',
    onclick: e => enterDeleteConfirm(tr, f, e.currentTarget),
  });
}

function enterDeleteConfirm(tr, f, deleteBtn) {
  const cell = deleteBtn.parentElement;

  const restore = () => {
    cell.replaceChildren(...buildActions(tr, f));
    cell.querySelector('button')?.focus();
  };

  const cancel  = el('button', { class: 'btn btn-neutral btn-sm', text: 'Cancel', onclick: restore });
  const confirm = el('button', { class: 'btn btn-destructive-solid btn-sm', text: 'Confirm' });
  const wrap = el('div', { class: 'delete-confirm' }, [
    el('span', { class: 'delete-confirm-label', text: 'Move to Trash?' }),
    cancel, confirm,
  ]);
  confirm.addEventListener('click', () => doDelete(tr, f, wrap));
  wrap.addEventListener('keydown', e => { if (e.key === 'Escape') { e.stopPropagation(); restore(); } });

  cell.replaceChildren(wrap);
  cancel.focus();
}

async function doDelete(tr, f, wrap) {
  tr.setAttribute('aria-busy', 'true');
  wrap.querySelectorAll('button').forEach(b => (b.disabled = true));
  wrap.appendChild(el('span', { class: 'row-spinner', 'aria-hidden': 'true' }));

  const nextRow = tr.nextElementSibling;

  let data;
  try {
    const res = await fetch(`${API}/api/admin/files/${encodeURIComponent(f.hash)}`, { method: 'DELETE' });
    if (handleAuthError(res)) { tr.removeAttribute('aria-busy'); return; }
    data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
  } catch (err) {
    tr.removeAttribute('aria-busy');
    const cell = wrap.parentElement;
    cell.replaceChildren(...buildActions(tr, f));
    toast(`Couldn’t delete “${displayTitle(f)}”: ${err.message}`, 'error');
    cell.querySelector('button')?.focus();
    return;
  }

  allFiles = allFiles.filter(x => x.hash !== f.hash);
  fileCountEl.textContent = String(allFiles.length);

  tr.classList.add('row-removing');
  const finish = () => {
    tr.remove();
    const target = nextRow && nextRow.isConnected
      ? nextRow.querySelector('.cell-actions button')
      : document.getElementById('filesHeading');
    target?.focus?.();
    if (allFiles.length === 0) renderEmptyRow();
  };
  tr.addEventListener('animationend', finish, { once: true });
  setTimeout(() => { if (tr.isConnected) finish(); }, 220);

  toast(`”${displayTitle(f)}” moved to Trash`, 'success');
}

// ── Filter ────────────────────────────────────────────────────────────────────
let filterTimer;
fileFilter.addEventListener('input', () => {
  clearTimeout(filterTimer);
  filterTimer = setTimeout(() => {
    filterText = fileFilter.value;
    renderFiles();
  }, 150);
});

// ── Boot ────────────────────────────────────────────────────────────────────
(async function boot() {
  const identity = await bootAdmin();
  if (!identity) return; // gate already rendered the notice
  const perms = identity.permissions || [];
  canEditMeta = perms.includes('metadata.edit');
  canDelete   = perms.includes('file.delete');
  loadFiles();
})();
