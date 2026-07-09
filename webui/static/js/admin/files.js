// Admin · Files — two views: the flat "All files" list (the shared
// file-management component, file-list.js) and the "By entity" drill-down
// (artist → album → track with rename / merge / cover / delete, kept here as a
// separate entity-management axis). A page-local preview player serves both.
import {
  API, LICENSE_OPTIONS,
  fmtTime, toast, handleAuthError, el,
} from './shared.js';
import { createTrackEditor } from '../track-edit.js';
import { createFileList } from '../file-list.js';
import { TRASH_ICON } from '../icons.js';
import { discKey, discLabel, isMultiDisc } from '../disc.js';

// createFilesScope builds the "All files" Library scope: the flat list (shared
// component) plus the By-entity drill-down (rename / merge / cover / delete).
// The shared preview player is injected as `play`; `perms` gates the actions.
export function createFilesScope({ play, perms }) {
let canEditMeta = perms.includes('metadata.edit');  // metadata + access edit + rename/merge
let canDelete   = perms.includes('file.delete');    // move-to-trash

// The flat table is the shared component, mounted lazily into #fileList.
let fileList     = null;
let filesMounted = false;
let entityPlayingKey = null;   // track-id of the entity row currently previewing

// ── Preview (via the shared player) ──────────────────────────────────────────
// entityHighlight marks the playing track row in the entity panel; the flat
// component tracks its own highlight (fileList.setPlaying). Each play source's
// closure clears the other panel.
function entityHighlight(key) {
  entityPanel.querySelectorAll('.playing-row').forEach(r => r.classList.remove('playing-row'));
  if (key != null) entityPanel.querySelector(`[data-track-id="${CSS.escape(key)}"]`)?.classList.add('playing-row');
}
function playEntity(navItems, idx) {
  play(navItems, idx, k => { entityPlayingKey = k; entityHighlight(k); fileList?.setPlaying(null); });
}
function playFile(f, visible) {
  const items = (visible || []).map(x => ({ url: x.url, title: displayTitle(x), artist: x.artist || '', key: x.hash }));
  let idx = items.findIndex(x => x.key === f.hash);
  if (idx < 0) { items.length = 0; items.push({ url: f.url, title: displayTitle(f), artist: f.artist || '', key: f.hash }); idx = 0; }
  play(items, idx, k => { entityPlayingKey = null; entityHighlight(null); fileList?.setPlaying(k); });
}

// ── Shared bits (used by the All-files scope + the entity view) ──────────────
const PLAY_ICON = '<svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M8 5v14l11-7z"/></svg>';
const displayTitle = f => f.title || f.filename || 'this file';

// showRemoved is the All-files physical-view toggle (recording-tagsets P5):
// when on, the listing also carries soft-removed blobs (absorbed / removed
// renditions), dimmed with a "removed" state. Off by default.
let showRemoved = false;

// loadFilesPage backs the All-files component's paged mode: one server page,
// filtered + sorted, as {total, items} (docs/architecture/file-list-scaling.md).
async function loadFilesPage({ limit, offset, q, field, sort }) {
  const params = new URLSearchParams({ limit: String(limit), offset: String(offset), sort: sort || 'created_desc' });
  if (q) params.set('q', q);
  if (field) params.set('field', field);
  if (showRemoved) params.set('show_removed', '1');
  const res = await fetch(`${API}/api/files?${params.toString()}`);
  if (handleAuthError(res)) throw new Error('Your session expired.');
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  const data = await res.json();
  return { total: data.total || 0, items: data.items || [] };
}

// bulkTrash moves a set to Trash in one transactional request — an explicit hash
// list, or a filter ("everything matching"). Returns the count actually trashed.
async function bulkTrash(body) {
  const res = await fetch(`${API}/api/admin/files/bulk`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ action: 'trash', ...body }),
  });
  if (handleAuthError(res)) throw new Error('Your session expired.');
  const data = await res.json().catch(() => ({}));
  if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
  return data.affected || 0;
}

// ── Access writes (per file). License first, then guest, so the explicit guest
//    wins over any auto-derive the license change triggers. ────────────────────
async function saveFileAccess(f, { guest, license }) {
  const r1 = await fetch(`${API}/api/admin/files/${encodeURIComponent(f.hash)}/license`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ license }),
  });
  if (!r1.ok) throw new Error((await r1.text()).trim() || `HTTP ${r1.status}`);
  const r2 = await fetch(`${API}/api/admin/files/${encodeURIComponent(f.hash)}/guest`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ guest_playable: guest }),
  });
  if (!r2.ok) throw new Error((await r2.text()).trim() || `HTTP ${r2.status}`);
}

// bulkEdit applies a tag/access patch in one request, over an explicit hash list
// or a filter ("select all matching"). The bulk editor's patch object already
// carries exactly the tag keys + license/guest, so it is forwarded as-is.
async function bulkEdit(body) {
  const res = await fetch(`${API}/api/admin/files/bulk`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ action: 'edit', ...body }),
  });
  if (handleAuthError(res)) throw new Error('Your session expired.');
  const data = await res.json().catch(() => ({}));
  if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
  return data; // { affected, failed:[{hash,error}] }
}

// filesBulkApply edits the explicitly-selected page rows; bulkApplyAll edits the
// whole filtered set. Both surface a partial failure as a throw (the editor shows
// it). bulkApplyAll owns its own success toast (the component doesn't, in filter
// mode); filesBulkApply lets the component toast the page count.
async function filesBulkApply(hashes, patch) {
  const data = await bulkEdit({ hashes, patch });
  if (data.failed?.length) throw new Error(`updated ${data.affected}, ${data.failed.length} failed`);
}
async function bulkApplyAll(filter, patch) {
  // Mirror bulkTrashAll: an empty filter is the whole library, which the server
  // refuses unless the request explicitly opts in with all:true.
  const data = await bulkEdit({ filter, patch, all: !filter.q });
  if (data.failed?.length) throw new Error(`updated ${data.affected}, ${data.failed.length} failed`);
  toast(`Updated ${data.affected} ${data.affected === 1 ? 'file' : 'files'}.`, 'success');
}

async function trashOne(f) {
  const res = await fetch(`${API}/api/admin/files/${encodeURIComponent(f.hash)}`, { method: 'DELETE' });
  if (handleAuthError(res)) throw new Error('Your session expired.');
  const data = await res.json().catch(() => ({}));
  if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
  toast(`“${displayTitle(f)}” moved to Trash.`, 'success');
}
// bulkTrashSelection trashes the explicitly-selected page rows (one request);
// bulkTrashAll trashes the whole filtered set ("select all N matching").
async function bulkTrashSelection(hashes) {
  const n = await bulkTrash({ hashes });
  toast(`Moved ${n} ${n === 1 ? 'file' : 'files'} to Trash.`, 'success');
}
async function bulkTrashAll({ q, field }) {
  const n = await bulkTrash({ filter: { q, field }, all: !q });
  toast(`Moved ${n} ${n === 1 ? 'file' : 'files'} to Trash.`, 'success');
}

// ── The All-files scope (the flat list, via the shared component) ────────────
// Access is a read-only summary column; editing tags + access happens in the
// per-file Edit modal and the bulk Edit-tags modal. Selection + bulk actions
// (Move to Trash, Edit tags…) are gated on the relevant permission.
// storageCell renders the physical column: backend, live/removed state, and
// the recording link jumping to /admin/recordings with that recording expanded.
function storageCell(f) {
  const bits = [el('span', { class: 'files-backend', text: f.storage_backend || 'local' })];
  if (f.removed) bits.push(el('span', { class: 'files-removed-badge', text: 'removed' }));
  if (f.recording_id) {
    bits.push(el('a', {
      class: 'files-rec-link', href: `/admin/recordings#${f.recording_id}`,
      title: 'Open this file’s recording (renditions & appearances)',
      text: `#${f.recording_id} →`,
    }));
  }
  return el('td', { class: 'cell-text files-storage-cell', 'data-label': 'Storage' }, bits);
}

function filesScope() {
  return {
    title: 'Files',
    emptyText: 'No files yet. Add music from the Upload page.',
    columns: ['check', 'title', 'artist', 'album', 'size', 'access', 'storage', 'actions'],
    cells: { storage: { label: 'Storage', cls: 'col-storage', render: storageCell } },
    rowClass: f => (f.removed ? 'files-row--removed' : ''),
    // Server-paged: the flat list can be huge, so it loads pages by infinite
    // scroll (windowed DOM). The grouped "By artist / album" view loads the whole
    // set once and windows it (docs/architecture/infinite-scroll-virtualization.md).
    paged: true,
    pageSize: 100,
    artistAlbumSort: true,
    apiBase: API,
    accessEditable: canEditMeta,
    licenses: LICENSE_OPTIONS,
    loadPage: loadFilesPage,
    selectable: () => canEditMeta || canDelete,
    editPatchURL: canEditMeta ? (f => `${API}/api/files/${encodeURIComponent(f.hash)}/metadata`) : undefined,
    editDetailURL: canEditMeta ? (f => `${API}/api/files/${encodeURIComponent(f.hash)}/metadata`) : undefined,
    editNote: 'Edits one track’s tags + access, and reclassifies just that track. ' +
              'To rename a whole album or artist (cover and tracks stay attached), use Rename in the By-entity view.',
    saveAccess: canEditMeta ? saveFileAccess : undefined,
    bulkApply: canEditMeta ? filesBulkApply : undefined,
    bulkApplyAll: canEditMeta ? bulkApplyAll : undefined,   // "select all N matching" → server-side edit
    // The row action confirms in the same modal the By-entity deletes use, which
    // owns the request and the reload — hence `false` (nothing left to reload).
    rowActions: canDelete ? [{
      id: 'trash', label: 'Move to Trash', icon: TRASH_ICON, kind: 'danger',
      run: f => {
        confirmDelete({
          title: 'Move file to Trash',
          body: `Move “${displayTitle(f)}” to Trash?`,
          run: async () => { await trashOne(f); fileList?.reload(); reloadEntityLevel(); },
        });
        return false;
      },
    }] : [],
    bulkActions: canDelete ? [{
      id: 'trash', label: 'Move to Trash', kind: 'danger',
      run: hashes => bulkTrashSelection(hashes),   // explicit page selection
      runAll: filter => bulkTrashAll(filter),       // "select all N matching"
    }] : [],
    onPlay: playFile,
    toast, handleAuthError,
  };
}

// ── Entity-view per-track editor (tags-only; reuses track-edit.js) ───────────
// The flat list's editor lives inside the component (with access). This one is
// opened by editTrack() in the drill-down, where access isn't surfaced.
const entityEditor = createTrackEditor({
  patchURL: f => `${API}/api/files/${encodeURIComponent(f.hash)}/metadata`,
  detailURL: f => `${API}/api/files/${encodeURIComponent(f.hash)}/metadata`,
  note: 'This edits one track’s tags and reclassifies just that track. ' +
        'To rename a whole album or artist (cover and all tracks stay attached), ' +
        'use Rename in the “By entity” view instead.',
  checkAuth: handleAuthError,
  onSaved: (f) => {
    toast(`Metadata saved for "${displayTitle(f)}".`, 'success');
    reloadEntityLevel();    // a tag edit can move the track between groupings
    fileList?.reload();     // keep the All-files list + url→file index fresh
  },
  onError: err => toast(`Couldn't save metadata: ${err.message}`, 'error'),
});

// ── Entity view: artists → albums → tracks (with rename) ─────────────────────
// Drives off the public browse endpoints (/api/artists, /api/albums?artist_id=,
// /api/tracks?album_id=) so it reflects the resolved artist/album entities, not
// raw per-file tags. Browse + cover reads are id-addressed; the cover-upload and
// rename endpoints still resolve by name (the display name rides in entityDrill);
// the empty-name bucket is shown but not renamable.
const entityPanel  = document.getElementById('entityPanel');
const entityCrumb  = document.getElementById('entityCrumb');
const entityFilter = document.getElementById('entityFilter');

// Browse fetches address entities by id (artistId / albumId); artist / album
// hold the display names, still used by the crumb and the name-addressed cover
// upload + rename endpoints.
const entityDrill = { level: 'artists', artist: null, album: null, artistId: null, albumId: null };
let entItems       = [];   // raw items for the current level
let entFilterText  = '';

async function entFetch(path) {
  const res = await fetch(`${API}${path}`);
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  return (await res.json()) || [];
}

function loadEntityArtists() {
  entityDrill.level = 'artists';
  entityDrill.artist = null; entityDrill.artistId = null;
  entityDrill.album = null; entityDrill.albumId = null;
  return loadEntityLevel(() => entFetch('/api/artists'));
}
function drillAlbums(artistId, artistName) {
  entityDrill.level = 'albums';
  entityDrill.artist = artistName; entityDrill.artistId = artistId;
  entityDrill.album = null; entityDrill.albumId = null;
  return loadEntityLevel(() => entFetch(`/api/albums?artist_id=${encodeURIComponent(artistId)}`));
}
function drillTracks(albumId, artistId, artistName, albumTitle) {
  entityDrill.level = 'tracks';
  entityDrill.artist = artistName; entityDrill.artistId = artistId;
  entityDrill.album = albumTitle; entityDrill.albumId = albumId;
  return loadEntityLevel(() => entFetch(`/api/tracks?album_id=${encodeURIComponent(albumId)}`));
}
function reloadEntityLevel() {
  if (entityDrill.level === 'albums') return drillAlbums(entityDrill.artistId, entityDrill.artist);
  if (entityDrill.level === 'tracks') return drillTracks(entityDrill.albumId, entityDrill.artistId, entityDrill.artist, entityDrill.album);
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
    parts.push(crumbNode(label, entityDrill.level === 'albums' ? null : () => drillAlbums(entityDrill.artistId, entityDrill.artist)));
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
  const thumb = coverThumb(a.has_image, artistImageURL(a.id));
  const main = el('button', {
    class: 'entity-main', onclick: () => drillAlbums(a.id, a.name),
    'aria-label': `Open ${a.name || 'unnamed artist'}`,
  }, [
    el('span', { class: 'entity-name' + (a.name ? '' : ' is-fallback'), text: a.name || '(no artist)' }),
    el('span', { class: 'entity-meta', text: trackCount(a.track_count) }),
  ]);
  const row = el('div', { class: 'entity-row' }, [thumb, main]);
  if (canEditMeta || canDelete) {
    const actions = el('div', { class: 'entity-actions' });
    if (canEditMeta && a.name) {
      actions.append(
        el('button', { class: 'btn btn-neutral btn-sm', text: a.has_image ? 'Cover…' : 'Add cover',
          onclick: () => pickCover({ kind: 'artist', name: a.name }) }),
        el('button', { class: 'btn btn-neutral btn-sm', text: 'Rename', onclick: () => openRenameArtist(a) }),
      );
    }
    // Merge is id-addressed, so it works even for the empty-name bucket; it needs
    // at least one other artist to merge into.
    if (canEditMeta && entItems.length > 1) {
      actions.append(el('button', { class: 'btn btn-neutral btn-sm', text: 'Merge…',
        onclick: () => openMergeArtist(a) }));
    }
    if (canDelete) {
      actions.append(el('button', { class: 'btn btn-destructive-outline btn-sm', text: 'Delete',
        onclick: () => deleteArtist(a) }));
    }
    if (actions.childElementCount) row.appendChild(actions);
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
  const thumb = coverThumb(a.has_image, albumImageURL(a.id));
  const main = el('button', {
    class: 'entity-main', onclick: () => drillTracks(a.id, a.artist_id, a.artist_name ?? entityDrill.artist, a.title),
    'aria-label': `Open ${a.title || 'unnamed album'}`,
  }, [
    el('span', { class: 'entity-name' + (a.title ? '' : ' is-fallback'), text: a.title || '(no album)' }),
    el('span', { class: 'entity-meta', text: meta.join(' · ') }),
  ]);
  const row = el('div', { class: 'entity-row' }, [thumb, main]);
  if (canEditMeta || canDelete) {
    const actions = el('div', { class: 'entity-actions' });
    if (canEditMeta && a.title) {
      actions.append(
        el('button', { class: 'btn btn-neutral btn-sm', text: a.has_image ? 'Cover…' : 'Add cover',
          onclick: () => pickCover({ kind: 'album', title: a.title, artist: entityDrill.artist }) }),
        el('button', { class: 'btn btn-neutral btn-sm', text: 'Rename', onclick: () => openRenameAlbum(a) }),
      );
    }
    // Album merge targets the current artist's other albums (id-addressed).
    if (canEditMeta && entItems.length > 1) {
      actions.append(el('button', { class: 'btn btn-neutral btn-sm', text: 'Merge…',
        onclick: () => openMergeAlbum(a) }));
    }
    if (canDelete) {
      actions.append(el('button', { class: 'btn btn-destructive-outline btn-sm', text: 'Delete',
        onclick: () => deleteAlbum(a) }));
    }
    if (actions.childElementCount) row.appendChild(actions);
  }
  return row;
}

// ── Cover images (artist / album) ────────────────────────────────────────────
// has_image comes from the browse DTOs. Artist covers are served raw (no variant
// pipeline); album covers are served from a derived variant, and we request the
// small (150px) crop here since the list only renders a small thumbnail. Album
// variants are generated async, so a freshly uploaded album cover only appears
// once its variants are ready.
// coverBust busts the browser cache after an upload so a replaced cover shows.
let coverBust = 0;
function artistImageURL(id) { return `/api/artists/${encodeURIComponent(id)}/image`; }
function albumImageURL(id)  { return `/api/albums/${encodeURIComponent(id)}/image?size=small`; }
function coverThumb(hasImage, url) {
  if (!hasImage) return el('div', { class: 'entity-cover entity-cover--empty', 'aria-hidden': 'true', text: '♪' });
  const sep = url.includes('?') ? '&' : '?';
  return el('img', { class: 'entity-cover', alt: '', loading: 'lazy', src: `${API}${url}${sep}cb=${coverBust}` });
}

const coverInput = document.getElementById('coverInput');
let coverTarget = null; // { kind:'artist'|'album', name? , title?, artist? }

function pickCover(target) {
  coverTarget = target;
  coverInput.value = '';   // allow re-picking the same file
  coverInput.click();
}
coverInput.addEventListener('change', () => {
  const file = coverInput.files[0];
  if (file && coverTarget) uploadCover(coverTarget, file);
});

async function uploadCover(target, file) {
  if (!['image/jpeg', 'image/png'].includes(file.type)) { toast('Cover must be a JPEG or PNG.', 'error'); return; }
  if (file.size > 10 * 1024 * 1024) { toast('Cover must be 10 MB or smaller.', 'error'); return; }

  const path = target.kind === 'artist'
    ? `/api/artists/${encodeURIComponent(target.name)}/image`
    : `/api/albums/${encodeURIComponent(target.title)}/image?artist=${encodeURIComponent(target.artist || '')}`;
  const fd = new FormData();
  fd.append('image', file);

  try {
    const res = await fetch(`${API}${path}`, { method: 'POST', body: fd });
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    coverBust = Date.now();   // force thumbnails to refetch the replaced image
    toast('Cover updated.', 'success');
    reloadEntityLevel();
  } catch (err) {
    toast(`Couldn't upload cover: ${err.message}`, 'error');
  } finally {
    coverTarget = null;
  }
}

function renderTracks(items) {
  if (!items.length) { entityPanel.replaceChildren(entEmpty(entFilterText ? 'No matching tracks.' : 'No tracks in this album.')); return; }
  const navItems = items.map(trackToItem);
  // Multi-disc albums (>1 distinct disc key — untagged/0/N each count) get a quiet
  // "Disc N" subheading before each disc; purely visual, the play order is
  // unchanged. disc.js is the shared rule (docs/architecture/disc-numbering.md).
  const multiDisc = isMultiDisc(items);
  let shownDisc;   // undefined: no real disc key equals it
  const frag = document.createDocumentFragment();
  items.forEach((t, i) => {
    const disc = discKey(t.disc_number);
    if (multiDisc && disc !== shownDisc) {
      shownDisc = disc;
      frag.appendChild(el('div', { class: 'entity-disc-header', text: discLabel(disc) }));
    }
    frag.appendChild(trackRow(t, navItems, i));
  });
  entityPanel.replaceChildren(frag);
  if (entityPlayingKey != null) entityHighlight(entityPlayingKey);
}
function trackToItem(t) {
  return { url: t.url, title: t.title || 'Untitled', artist: entityDrill.artist || '', key: String(t.id) };
}
function trackRow(t, navItems, idx) {
  const play = el('button', { class: 'play-btn', title: 'Preview', 'aria-label': `Preview ${t.title || 'track'}`,
    onclick: () => playEntity(navItems, idx) });
  play.innerHTML = PLAY_ICON;
  const num = el('span', { class: 'entity-tracknum', text: t.track_number != null ? String(t.track_number) : '' });
  const title = el('span', { class: 'entity-name' + (t.title ? '' : ' is-fallback'), text: t.title || 'Untitled' });
  const dur = el('span', { class: 'entity-meta', text: t.duration_seconds != null ? fmtTime(t.duration_seconds) : '' });
  const children = [play, num, title, dur];
  if (canEditMeta || canDelete) {
    const actions = el('div', { class: 'entity-actions' });
    if (canEditMeta) {
      actions.append(el('button', { class: 'btn btn-neutral btn-sm', text: 'Edit',
        'aria-label': `Edit ${t.title || 'track'}`, onclick: () => editTrack(t) }));
    }
    if (canDelete) {
      actions.append(el('button', { class: 'btn btn-destructive-outline btn-sm', text: 'Delete',
        'aria-label': `Delete ${t.title || 'track'}`, onclick: () => deleteTrack(t) }));
    }
    children.push(actions);
  }
  return el('div', { class: 'entity-row entity-row--track', 'data-track-id': String(t.id) }, children);
}

// editTrack opens the per-track metadata modal from the entity view. The track
// DTO carries its content hash (GET /api/tracks), so the editor fetches the full
// tags via detailURL(hash) — no whole-library fetch needed.
function editTrack(t) {
  if (!t.hash) { toast('Couldn’t find this file’s details.', 'error'); return; }
  entityEditor.open({ hash: t.hash, title: t.title, artist: t.artist_name || '', album: entityDrill.album || '' });
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
    fileList?.reload();   // refresh the All-files list + url→file index
  } catch (err) {
    showRenameError(`Couldn't rename: ${err.message}`);
  } finally {
    renameSubmit.disabled = false;
  }
});

// ── Merge modal (fold one entity into another — destructive, id-addressed) ───
const mergeModal    = document.getElementById('mergeModal');
const mergeForm     = document.getElementById('mergeForm');
const mergeTitleEl  = document.getElementById('mergeTitle');
const mergeSourceEl = document.getElementById('mergeSource');
const mergeTarget   = document.getElementById('mergeTarget');
const mergeTargetLabel = document.getElementById('mergeTargetLabel');
const mergeWarn     = document.getElementById('mergeWarn');
const mergeError    = document.getElementById('mergeError');
const mergeSubmit   = document.getElementById('mergeSubmit');
let mergeSource     = null; // { kind, id, label, candidates: [{id,label,count}] }

function openMergeArtist(a) {
  openMerge({
    kind: 'artist', id: a.id, label: a.name || '(no artist)', targetLabel: 'Merge into artist',
    candidates: entItems.filter(x => x.id !== a.id)
      .map(x => ({ id: x.id, label: x.name || '(no artist)', count: x.track_count })),
  });
}
function openMergeAlbum(a) {
  openMerge({
    kind: 'album', id: a.id, label: a.title || '(no album)', targetLabel: 'Merge into album',
    candidates: entItems.filter(x => x.id !== a.id)
      .map(x => ({ id: x.id, label: x.title || '(no album)', count: x.track_count })),
  });
}
function openMerge(src) {
  if (!src.candidates.length) { toast('Nothing here to merge into.', 'error'); return; }
  mergeSource = src;
  mergeTitleEl.textContent = src.kind === 'artist' ? 'Merge artist' : 'Merge album';
  mergeTargetLabel.textContent = src.targetLabel;
  mergeSourceEl.textContent = src.label;
  mergeTarget.replaceChildren(...src.candidates.map(c =>
    el('option', { value: String(c.id), text: `${c.label} · ${trackCount(c.count)}` })));
  updateMergeWarn();
  mergeError.hidden = true; mergeError.textContent = '';
  mergeModal.classList.remove('hidden');
  mergeTarget.focus();
}
// updateMergeWarn shows a live dry-run of the selected merge: it posts to the
// read-only preview endpoint and renders the real move/collapse/cover effects.
// The static one-liner is shown immediately as a fallback while the preview
// loads (and stays if it fails). A sequence guard discards out-of-order replies
// from rapid target changes.
let mergeWarnSeq = 0;
async function updateMergeWarn() {
  const intoId = Number(mergeTarget.value);
  const c = mergeSource?.candidates.find(x => x.id === intoId);
  if (!mergeSource || !c) { mergeWarn.textContent = ''; return; }

  mergeWarn.textContent =
    `Moves all tracks into “${c.label}”, then deletes “${mergeSource.label}”. This can’t be undone.`;

  const seq = ++mergeWarnSeq;
  const path = mergeSource.kind === 'artist' ? '/api/artists/merge/preview' : '/api/albums/merge/preview';
  try {
    const res = await fetch(`${API}${path}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ from_id: mergeSource.id, into_id: intoId }),
    });
    if (!res.ok) return;             // keep the static fallback
    const p = await res.json();
    if (seq !== mergeWarnSeq) return; // a newer target was picked
    mergeWarn.textContent = describeMerge(mergeSource.kind, p);
  } catch { /* network error — keep the static fallback */ }
}

// describeMerge turns a merge-preview payload into the modal's plain-text warning.
function describeMerge(kind, p) {
  const into = `“${p.into_label}”`;
  const parts = [`Moves ${trackCount(p.tracks_moved)} into ${into}.`];
  if (kind === 'artist' && p.albums_collapsed > 0) {
    const titles = (p.collapsed_titles || []).map(t => `“${t}”`).join(', ');
    parts.push(`${p.albums_collapsed} album${p.albums_collapsed === 1 ? '' : 's'} merge into ${into}’s existing ones${titles ? ` (${titles})` : ''}.`);
  }
  if (p.source_has_cover) {
    parts.push(p.target_has_cover ? `${into} keeps its own cover.` : `Its cover becomes ${into}’s.`);
  }
  if (kind === 'album' && p.source_artist_orphaned) {
    parts.push(`Its artist will be left empty.`);
  }
  parts.push(`Then deletes “${p.from_label}”. This can’t be undone.`);
  return parts.join(' ');
}
function closeMerge() { mergeModal.classList.add('hidden'); mergeSource = null; }

mergeTarget.addEventListener('change', updateMergeWarn);
document.getElementById('mergeClose').addEventListener('click', closeMerge);
document.getElementById('mergeCancel').addEventListener('click', closeMerge);
mergeModal.addEventListener('click', e => { if (e.target === mergeModal) closeMerge(); });
document.addEventListener('keydown', e => {
  if (e.key === 'Escape' && !mergeModal.classList.contains('hidden')) closeMerge();
});

mergeForm.addEventListener('submit', async e => {
  e.preventDefault();
  if (!mergeSource) return;
  const intoID = Number(mergeTarget.value);
  if (!intoID) return;
  const path = mergeSource.kind === 'artist' ? '/api/artists/merge' : '/api/albums/merge';

  mergeSubmit.disabled = true;
  mergeError.hidden = true;
  try {
    const res = await fetch(`${API}${path}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ from_id: mergeSource.id, into_id: intoID }),
    });
    if (handleAuthError(res)) return;
    const data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
    toast('Merged.', 'success');
    closeMerge();
    fileList?.reload();       // refresh the All-files list + index
    reloadEntityLevel();
  } catch (err) {
    mergeError.textContent = `Couldn't merge: ${err.message}`;
    mergeError.hidden = false;
  } finally {
    mergeSubmit.disabled = false;
  }
});

// ── Delete (track / album / artist) — move files to Trash, batched ───────────
// Reuses the per-file endpoint DELETE /api/admin/files/{hash} (move to Trash,
// reversible from the Trash page). Entity deletes gather their tracks' hashes via
// the browse endpoints (so they match exactly what's shown) + the url→file index.
const deleteModal     = document.getElementById('deleteModal');
const deleteTitleEl   = document.getElementById('deleteTitle');
const deleteBodyEl    = document.getElementById('deleteBody');
const deleteError     = document.getElementById('deleteError');
const deleteConfirmBtn = document.getElementById('deleteConfirm');
let deleteRun = null;

function confirmDelete({ title, body, confirmLabel = 'Move to Trash', run }) {
  deleteRun = run;
  deleteTitleEl.textContent = title;
  deleteBodyEl.textContent = body;
  deleteConfirmBtn.textContent = confirmLabel;
  deleteError.hidden = true; deleteError.textContent = '';
  deleteModal.classList.remove('hidden');
  deleteConfirmBtn.focus();
}
function closeDelete() { deleteModal.classList.add('hidden'); deleteRun = null; }

deleteConfirmBtn.addEventListener('click', async () => {
  if (!deleteRun) return;
  deleteConfirmBtn.disabled = true;
  deleteError.hidden = true;
  try {
    await deleteRun();
    closeDelete();
  } catch (err) {
    deleteError.textContent = err.message;
    deleteError.hidden = false;
  } finally {
    deleteConfirmBtn.disabled = false;
  }
});
document.getElementById('deleteClose').addEventListener('click', closeDelete);
document.getElementById('deleteCancel').addEventListener('click', closeDelete);
deleteModal.addEventListener('click', e => { if (e.target === deleteModal) closeDelete(); });
document.addEventListener('keydown', e => {
  if (e.key === 'Escape' && !deleteModal.classList.contains('hidden')) closeDelete();
});

// trashByFilter moves a whole entity's tracks to Trash in one request and
// refreshes both views. It throws on failure so the confirm modal surfaces it.
async function trashByFilter(filter) {
  const n = await bulkTrash({ filter });
  fileList?.reload();
  reloadEntityLevel();
  toast(`Moved ${n} ${n === 1 ? 'track' : 'tracks'} to Trash.`, 'success');
}

async function deleteTrack(t) {
  if (!t.hash) { toast('Couldn’t find this file’s details.', 'error'); return; }
  confirmDelete({
    title: 'Move track to Trash',
    body: `Move “${t.title || 'this track'}” to Trash?`,
    run: async () => {
      await bulkTrash({ hashes: [t.hash] });
      fileList?.reload();
      reloadEntityLevel();
      toast('Moved to Trash.', 'success');
    },
  });
}
function deleteAlbum(a) {
  const n = a.track_count || 0;
  confirmDelete({
    title: 'Delete album',
    body: `Move all ${n} ${n === 1 ? 'track' : 'tracks'} in “${a.title || '(no album)'}” to Trash?`,
    confirmLabel: `Move ${n} to Trash`,
    run: () => trashByFilter({ album_id: a.id }),
  });
}
function deleteArtist(a) {
  const n = a.track_count || 0;
  confirmDelete({
    title: 'Delete artist',
    body: `Move all ${n} ${n === 1 ? 'track' : 'tracks'} by “${a.name || '(no artist)'}” to Trash?`,
    confirmLabel: `Move ${n} to Trash`,
    run: () => trashByFilter({ artist_id: a.id }),
  });
}

// ── View tabs (entity ⇄ all files) ───────────────────────────────────────────
const tabEntity  = document.getElementById('tabEntity');
const tabFiles   = document.getElementById('tabFiles');
const entityView = document.getElementById('entityView');
const filesView  = document.getElementById('filesView');
const fileListEl = document.getElementById('fileList');

// "Show removed" — moderation-capability toggle for the physical view. Hidden
// for admins without it (the server would ignore the param anyway).
const showRemovedToggle = document.getElementById('showRemovedToggle');
if (showRemovedToggle) {
  const canSeeRemoved = perms.includes('content.moderate') || canDelete;
  if (!canSeeRemoved) showRemovedToggle.closest('.files-show-removed').hidden = true;
  showRemovedToggle.addEventListener('change', () => {
    showRemoved = showRemovedToggle.checked;
    if (filesMounted) fileList.reload();
  });
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
  // Mount the component on first open; refresh on subsequent visits.
  if (!filesMounted) { filesMounted = true; fileList.mount(fileListEl); }
  else fileList.reload();
}
tabEntity.addEventListener('click', showEntityView);
tabFiles.addEventListener('click', showFilesView);

// ── Controller (mounted by library.js) ───────────────────────────────────────
fileList = createFileList(filesScope());     // flat list; mounted on the All-files sub-tab

return {
  id: 'all',
  label: 'All files',
  available: true,
  mount() { loadEntityArtists(); },          // By-entity is the default sub-view
  reload() { reloadEntityLevel(); if (filesMounted) fileList.reload(); },
};
}
