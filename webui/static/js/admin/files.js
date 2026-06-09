// Admin · Files — the files table with per-file access controls, metadata
// editing, two-step delete, and a preview player (the shared player.js).
import {
  bootAdmin, API, LICENSE_OPTIONS,
  fmtBytes, fmtTime, shortHash, toast, handleAuthError, el,
} from './shared.js';
import { createPlayer } from '../player.js';

const filesBody   = document.getElementById('filesBody');
const fileCountEl = document.getElementById('fileCount');
const fileFilter  = document.getElementById('fileFilter');

let allFiles    = [];     // last fetched list
let fileByURL   = new Map(); // url → file record, so the entity view can resolve
                             // a browse track (id/url only) to its hash for edit
let filterText  = '';
let canEditMeta = false;  // metadata.edit → access controls + metadata edit + rename
let canDelete   = false;  // file.delete → move-to-trash

// ── Preview player (shared by both views via a single play context) ──────────
// playCtx.items are normalised { url, title, artist, key }; kind picks which
// view's rows carry the playing highlight ('files' → data-hash, 'entity' →
// data-track-id). One <audio>, navigable next/prev within the current context.
let playCtx = null;

const player = createPlayer({
  onPrev:  () => { if (playCtx) playAtCtx(playCtx.index > 0 ? playCtx.index - 1 : playCtx.items.length - 1); },
  onNext:  () => { if (playCtx) playAtCtx(playCtx.index < playCtx.items.length - 1 ? playCtx.index + 1 : 0); },
  onEnded: () => { if (playCtx && playCtx.index < playCtx.items.length - 1) playAtCtx(playCtx.index + 1); },
  onError: () => {
    toast('Couldn’t play this file.', 'error');
    if (playCtx && playCtx.index < playCtx.items.length - 1) playAtCtx(playCtx.index + 1);
  },
});

function playFrom(items, index, kind) {
  if (!items.length) return;
  playCtx = { items, index, kind };
  playAtCtx(index < 0 ? 0 : index);
}

function playAtCtx(i) {
  if (!playCtx || i < 0 || i >= playCtx.items.length) return;
  playCtx.index = i;
  const it = playCtx.items[i];
  const url = /^https?:/.test(it.url) ? it.url : `${API}${it.url}`;
  player.load({ url, title: it.title, artist: it.artist || '' });
  highlightCtx();
}

// highlightCtx repaints the playing-row marker. Both panels persist in the DOM
// (the inactive view is just hidden), so we clear both and mark the current one.
function highlightCtx() {
  filesBody.querySelectorAll('tr.playing-row').forEach(tr => tr.classList.remove('playing-row'));
  entityPanel.querySelectorAll('.playing-row').forEach(r => r.classList.remove('playing-row'));
  if (!playCtx) return;
  const it = playCtx.items[playCtx.index];
  if (playCtx.kind === 'files') {
    filesBody.querySelector(`tr[data-hash="${CSS.escape(it.key)}"]`)?.classList.add('playing-row');
  } else {
    entityPanel.querySelector(`[data-track-id="${CSS.escape(it.key)}"]`)?.classList.add('playing-row');
  }
}

// playFile previews a row in the All-files table, navigable across the visible
// files. fileToItem normalises a file record into a play-context item.
function fileToItem(f) {
  return { url: f.url, title: displayTitle(f), artist: f.artist || '', key: f.hash };
}
function playFile(f) {
  const items = visibleFiles().map(fileToItem);
  let idx = items.findIndex(x => x.key === f.hash);
  if (idx < 0) { items.length = 0; items.push(fileToItem(f)); idx = 0; }
  playFrom(items, idx, 'files');
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
  fileByURL = new Map(allFiles.map(f => [f.url, f]));
  filesBody.setAttribute('aria-busy', 'false');
  renderFiles();
}

// fileForTrack resolves an entity-view browse track (which carries only id/url)
// back to its full file record (with the hash the edit endpoint needs). Returns
// undefined until /api/files has loaded or if the listings disagree.
function fileForTrack(t) { return fileByURL.get(t.url); }

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
  if (playCtx && playCtx.kind === 'files') highlightCtx();
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
let editFromEntity = false;  // edit opened from the entity view → refresh it on save

function openEditModal(f, fromEntity = false) {
  editingFile = f;
  editFromEntity = fromEntity;
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
    // A tag edit can move the track between artist/album groupings, so refresh
    // the entity view too when the edit came from there.
    if (editFromEntity) reloadEntityLevel();
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

// ── Entity view: artists → albums → tracks (with rename) ─────────────────────
// Drives off the public browse endpoints (/api/artists, /api/albums?artist=,
// /api/tracks?artist=&album=) so it reflects the resolved artist/album entities,
// not raw per-file tags. Entities are addressed by current name (the rename
// endpoints resolve by name); the empty-name bucket is shown but not renamable.
const entityPanel  = document.getElementById('entityPanel');
const entityCrumb  = document.getElementById('entityCrumb');
const entityFilter = document.getElementById('entityFilter');

const entityDrill = { level: 'artists', artist: null, album: null };
let entItems       = [];   // raw items for the current level
let entFilterText  = '';

async function entFetch(path) {
  const res = await fetch(`${API}${path}`);
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  return (await res.json()) || [];
}

function loadEntityArtists() {
  entityDrill.level = 'artists'; entityDrill.artist = null; entityDrill.album = null;
  return loadEntityLevel(() => entFetch('/api/artists'));
}
function drillAlbums(artist) {
  entityDrill.level = 'albums'; entityDrill.artist = artist; entityDrill.album = null;
  return loadEntityLevel(() => entFetch(`/api/albums?artist=${encodeURIComponent(artist)}`));
}
function drillTracks(artist, album) {
  entityDrill.level = 'tracks'; entityDrill.artist = artist; entityDrill.album = album;
  return loadEntityLevel(() => entFetch(`/api/tracks?artist=${encodeURIComponent(artist)}&album=${encodeURIComponent(album)}`));
}
function reloadEntityLevel() {
  if (entityDrill.level === 'albums') return drillAlbums(entityDrill.artist);
  if (entityDrill.level === 'tracks') return drillTracks(entityDrill.artist, entityDrill.album);
  return loadEntityArtists();
}

async function loadEntityLevel(fetcher) {
  entityPanel.setAttribute('aria-busy', 'true');
  entityPanel.replaceChildren(el('div', { class: 'table-state-row', text: 'Loading…' }));
  renderCrumb();
  try {
    entItems = await fetcher();
  } catch (err) {
    console.error('Failed to load library level:', err);
    entityPanel.setAttribute('aria-busy', 'false');
    const retry = el('button', { class: 'btn btn-neutral btn-sm', text: 'Retry', onclick: reloadEntityLevel });
    entityPanel.replaceChildren(el('div', { class: 'table-state-row', role: 'alert' }, [
      el('div', { text: 'Failed to load.' }), retry,
    ]));
    return;
  }
  entityPanel.setAttribute('aria-busy', 'false');
  renderEntity();
}

function renderCrumb() {
  const parts = [crumbNode('Artists', entityDrill.level === 'artists' ? null : loadEntityArtists)];
  if (entityDrill.artist !== null) {
    parts.push(crumbSep());
    const label = entityDrill.artist || '(no artist)';
    parts.push(crumbNode(label, entityDrill.level === 'albums' ? null : () => drillAlbums(entityDrill.artist)));
  }
  if (entityDrill.album !== null) {
    parts.push(crumbSep());
    parts.push(crumbNode(entityDrill.album || '(no album)', null));
  }
  entityCrumb.replaceChildren(...parts);
}
function crumbNode(label, onClick) {
  return onClick
    ? el('button', { class: 'crumb-link', text: label, onclick: onClick })
    : el('span', { class: 'crumb-current', text: label });
}
function crumbSep() {
  return el('span', { class: 'crumb-sep', 'aria-hidden': 'true', text: '›' });
}

function renderEntity() {
  const q = entFilterText.trim().toLowerCase();
  if (entityDrill.level === 'artists') return renderArtists(entItems.filter(a => match(a.name, q)));
  if (entityDrill.level === 'albums')  return renderAlbums(entItems.filter(a => match(a.title, q)));
  return renderTracks(entItems.filter(t => match(t.title, q)));
}
function match(s, q) { return !q || (s || '').toLowerCase().includes(q); }

function entEmpty(text) {
  return el('div', { class: 'table-state-row', text });
}

function renderArtists(items) {
  if (!items.length) { entityPanel.replaceChildren(entEmpty(entFilterText ? 'No matching artists.' : 'No artists yet.')); return; }
  const frag = document.createDocumentFragment();
  items.forEach(a => frag.appendChild(artistRow(a)));
  entityPanel.replaceChildren(frag);
}
function artistRow(a) {
  const main = el('button', {
    class: 'entity-main', onclick: () => drillAlbums(a.name),
    'aria-label': `Open ${a.name || 'unnamed artist'}`,
  }, [
    el('span', { class: 'entity-name' + (a.name ? '' : ' is-fallback'), text: a.name || '(no artist)' }),
    el('span', { class: 'entity-meta', text: trackCount(a.track_count) }),
  ]);
  const row = el('div', { class: 'entity-row' }, [main]);
  if (canEditMeta && a.name) {
    row.appendChild(el('div', { class: 'entity-actions' }, [
      el('button', { class: 'btn btn-neutral btn-sm', text: 'Rename', onclick: () => openRenameArtist(a) }),
    ]));
  }
  return row;
}

function renderAlbums(items) {
  if (!items.length) { entityPanel.replaceChildren(entEmpty(entFilterText ? 'No matching albums.' : 'No albums for this artist.')); return; }
  const frag = document.createDocumentFragment();
  items.forEach(a => frag.appendChild(albumRow(a)));
  entityPanel.replaceChildren(frag);
}
function albumRow(a) {
  const meta = [trackCount(a.track_count)];
  if (a.year != null) meta.unshift(String(a.year));
  const main = el('button', {
    class: 'entity-main', onclick: () => drillTracks(entityDrill.artist, a.title),
    'aria-label': `Open ${a.title || 'unnamed album'}`,
  }, [
    el('span', { class: 'entity-name' + (a.title ? '' : ' is-fallback'), text: a.title || '(no album)' }),
    el('span', { class: 'entity-meta', text: meta.join(' · ') }),
  ]);
  const row = el('div', { class: 'entity-row' }, [main]);
  if (canEditMeta && a.title) {
    row.appendChild(el('div', { class: 'entity-actions' }, [
      el('button', { class: 'btn btn-neutral btn-sm', text: 'Rename', onclick: () => openRenameAlbum(a) }),
    ]));
  }
  return row;
}

function renderTracks(items) {
  if (!items.length) { entityPanel.replaceChildren(entEmpty(entFilterText ? 'No matching tracks.' : 'No tracks in this album.')); return; }
  const navItems = items.map(trackToItem);
  const frag = document.createDocumentFragment();
  items.forEach((t, i) => frag.appendChild(trackRow(t, navItems, i)));
  entityPanel.replaceChildren(frag);
  if (playCtx && playCtx.kind === 'entity') highlightCtx();
}
function trackToItem(t) {
  return { url: t.url, title: t.title || 'Untitled', artist: entityDrill.artist || '', key: String(t.id) };
}
function trackRow(t, navItems, idx) {
  const play = el('button', { class: 'play-btn', title: 'Preview', 'aria-label': `Preview ${t.title || 'track'}`,
    onclick: () => playFrom(navItems, idx, 'entity') });
  play.innerHTML = PLAY_ICON;
  const num = el('span', { class: 'entity-tracknum', text: t.track_number != null ? String(t.track_number) : '' });
  const title = el('span', { class: 'entity-name' + (t.title ? '' : ' is-fallback'), text: t.title || 'Untitled' });
  const dur = el('span', { class: 'entity-meta', text: t.duration_seconds != null ? fmtTime(t.duration_seconds) : '' });
  const children = [play, num, title, dur];
  if (canEditMeta) {
    const edit = el('button', { class: 'btn btn-neutral btn-sm', text: 'Edit',
      'aria-label': `Edit ${t.title || 'track'}`, onclick: () => editTrack(t) });
    children.push(el('div', { class: 'entity-actions' }, [edit]));
  }
  return el('div', { class: 'entity-row entity-row--track', 'data-track-id': String(t.id) }, children);
}

// editTrack opens the per-track metadata modal from the entity view. The browse
// track carries no hash, so it lazily loads /api/files on first need and resolves
// the track to its file record there.
async function editTrack(t) {
  await ensureFilesLoaded();
  const f = fileForTrack(t);
  if (!f) { toast('Couldn’t find this file’s details.', 'error'); return; }
  openEditModal(f, true);
}

function trackCount(n) { return `${n} track${n === 1 ? '' : 's'}`; }

let entFilterTimer;
entityFilter.addEventListener('input', () => {
  clearTimeout(entFilterTimer);
  entFilterTimer = setTimeout(() => { entFilterText = entityFilter.value; renderEntity(); }, 150);
});

// ── Rename modal (whole artist / album entity) ───────────────────────────────
const renameModal     = document.getElementById('renameModal');
const renameForm      = document.getElementById('renameForm');
const renameInput     = document.getElementById('renameInput');
const renameError     = document.getElementById('renameError');
const renameTitleEl   = document.getElementById('renameTitle');
const renameFieldText = document.getElementById('renameFieldText');
const renameNote      = document.getElementById('renameNote');
const renameSubmit    = document.getElementById('renameSubmit');
let renameTarget      = null; // { kind, oldName? , oldTitle?, artist? }

function openRenameArtist(a) {
  renameTarget = { kind: 'artist', oldName: a.name };
  renameTitleEl.textContent = 'Rename artist';
  renameFieldText.textContent = 'Artist name';
  renameNote.textContent = 'Renames the whole artist — its cover and all its tracks stay attached.';
  showRename(a.name);
}
function openRenameAlbum(a) {
  renameTarget = { kind: 'album', oldTitle: a.title, artist: a.artist_name ?? entityDrill.artist ?? '' };
  renameTitleEl.textContent = 'Rename album';
  renameFieldText.textContent = 'Album title';
  renameNote.textContent = 'Renames the whole album — its cover and all its tracks stay attached.';
  showRename(a.title);
}
function showRename(current) {
  renameError.hidden = true; renameError.textContent = '';
  renameInput.value = current || '';
  renameModal.classList.remove('hidden');
  renameInput.focus(); renameInput.select();
}
function closeRename() {
  renameModal.classList.add('hidden');
  renameTarget = null;
}
function showRenameError(msg) {
  renameError.textContent = msg;
  renameError.hidden = false;
}

document.getElementById('renameClose').addEventListener('click', closeRename);
document.getElementById('renameCancel').addEventListener('click', closeRename);
renameModal.addEventListener('click', e => { if (e.target === renameModal) closeRename(); });
document.addEventListener('keydown', e => {
  if (e.key === 'Escape' && !renameModal.classList.contains('hidden')) closeRename();
});

renameForm.addEventListener('submit', async e => {
  e.preventDefault();
  if (!renameTarget) return;
  const val = renameInput.value.trim();
  if (!val) { showRenameError('A name is required.'); return; }

  const t = renameTarget;
  let path, body;
  if (t.kind === 'artist') {
    path = `/api/artists/${encodeURIComponent(t.oldName)}/rename`;
    body = { name: val };
  } else {
    path = `/api/albums/${encodeURIComponent(t.oldTitle)}/rename?artist=${encodeURIComponent(t.artist || '')}`;
    body = { title: val };
  }

  renameSubmit.disabled = true;
  renameError.hidden = true;
  try {
    const res = await fetch(`${API}${path}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (handleAuthError(res)) return;
    const data = await res.json().catch(() => ({}));
    if (res.status === 409) { showRenameError(data.error || 'That name is already taken.'); return; }
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);

    toast(`Renamed to “${val}”.`, 'success');
    closeRename();
    // If we renamed the artist whose albums we're viewing, keep the crumb in sync.
    if (t.kind === 'artist' && entityDrill.artist === t.oldName) entityDrill.artist = val;
    reloadEntityLevel();
    filesLoaded = false;   // flat table + url→file index are now stale; reload on next need
  } catch (err) {
    showRenameError(`Couldn't rename: ${err.message}`);
  } finally {
    renameSubmit.disabled = false;
  }
});

// ── View tabs (entity ⇄ all files) ───────────────────────────────────────────
const tabEntity  = document.getElementById('tabEntity');
const tabFiles   = document.getElementById('tabFiles');
const entityView = document.getElementById('entityView');
const filesView  = document.getElementById('filesView');
let filesLoaded  = false;   // the flat table + url→file index load lazily

// ensureFilesLoaded fetches /api/files once, on first need — when the All-files
// tab is opened or when the entity view needs a track's hash to edit it.
async function ensureFilesLoaded() {
  if (filesLoaded) return;
  filesLoaded = true;
  await loadFiles();
}

function showEntityView() {
  tabEntity.classList.add('view-tab--active'); tabEntity.setAttribute('aria-selected', 'true');
  tabFiles.classList.remove('view-tab--active'); tabFiles.setAttribute('aria-selected', 'false');
  entityView.classList.remove('hidden'); filesView.classList.add('hidden');
}
function showFilesView() {
  tabFiles.classList.add('view-tab--active'); tabFiles.setAttribute('aria-selected', 'true');
  tabEntity.classList.remove('view-tab--active'); tabEntity.setAttribute('aria-selected', 'false');
  filesView.classList.remove('hidden'); entityView.classList.add('hidden');
  ensureFilesLoaded();
}
tabEntity.addEventListener('click', showEntityView);
tabFiles.addEventListener('click', showFilesView);

// ── Boot ────────────────────────────────────────────────────────────────────
(async function boot() {
  const identity = await bootAdmin();
  if (!identity) return; // gate already rendered the notice
  const perms = identity.permissions || [];
  canEditMeta = perms.includes('metadata.edit');
  canDelete   = perms.includes('file.delete');
  loadEntityArtists();   // entity view is the default; files load lazily on demand
})();
