// playlists.js — the /playlists listening page (Phase 5 step 3 of
// docs/api/playlists.md). Shell-native ({ init, teardown }): a list of the
// user's playlists (favorites first) drilling into an editable detail view —
// play-all / play-from-row via the shared controller, rename/delete (regular
// playlists only), per-row remove, drag or Ctrl/Alt+Arrow reorder. Trashed
// tracks render grayed (metadata visible, unplayable) per Decision §3.
import { openLoginModal, gatePage, PAGE_PERMS } from './auth.js';
import { getController } from './player-controller.js';
import { fmtTime } from './player.js';
import { loadDurCache } from './dur-cache.js';

const API = document.querySelector('meta[name="api-url"]')?.content || '';

const controller = getController();
// Module-scoped (runs once, persists): re-paint the playing row on this page's
// detail view. Rows are matched by data-key (appearance identity), same contract
// as the library, and reflect the pause/resume indicator on play/pause.
controller.on('trackchange', track => highlightPlaying(track));
controller.on('playstate', reflectPlayState);

function playKeyOf(track) {
  if (!track) return null;
  return track.rowKey || (track.tagsetId ? `ts:${track.tagsetId}` : `url:${track.url}`);
}

function highlightPlaying(track) {
  const key = playKeyOf(track);
  const paused = controller.paused;
  document.querySelectorAll('#plPanel .track-row').forEach(row => {
    const on = !!key && row.dataset.key === key;
    row.classList.toggle('playing', on);
    row.classList.toggle('paused', on && paused);
  });
}

function reflectPlayState(playing) {
  document.querySelectorAll('#plPanel .track-row.playing')
    .forEach(row => row.classList.toggle('paused', !playing));
}

// ── State ─────────────────────────────────────────────────────────────────
let active = false;   // guards late async renders after teardown
let abort  = null;    // aborts in-flight fetches on teardown
let detail = null;    // { playlist, items } when the detail view is open

function panel()  { return document.getElementById('plPanel'); }
function crumbs() { return document.getElementById('plBreadcrumb'); }
function actionsEl() { return document.getElementById('plHeadActions'); }

// apiFetch wraps fetch with the page's error contract: 401/403 → login modal,
// non-OK → throws with the body text.
async function apiFetch(path, opts = {}) {
  const res = await fetch(`${API}${path}`, { signal: abort?.signal, ...opts });
  if (res.status === 401 || res.status === 403) {
    openLoginModal();
    throw new Error('not authorized');
  }
  if (!res.ok) {
    const msg = (await res.text().catch(() => '')).trim();
    throw new Error(msg || `HTTP ${res.status}`);
  }
  return res;
}

// ── List view ─────────────────────────────────────────────────────────────
async function showList() {
  detail = null;
  renderCrumbs();
  actionsEl().replaceChildren();
  panel().innerHTML = '<div class="panel-loading" aria-live="polite" role="status"></div>';

  let lists;
  try {
    lists = await (await apiFetch('/api/playlists')).json();
  } catch (err) {
    if (err.name === 'AbortError' || !active) return;
    panel().innerHTML = '<div class="panel-empty" role="alert">Failed to load playlists.</div>';
    return;
  }
  if (!active) return;

  const wrap = document.createElement('div');
  wrap.className = 'panel-fade-in';

  if (!lists.length) {
    wrap.innerHTML = '<div class="panel-empty">No playlists yet.</div>';
  }
  lists.forEach(p => {
    const row = document.createElement('div');
    row.className = 'panel-row pl-row';
    row.tabIndex = 0;
    row.setAttribute('role', 'button');
    row.setAttribute('aria-label', `Open playlist ${p.name}`);

    const name = document.createElement('span');
    name.className = 'row-name';
    name.textContent = p.name;
    if (p.kind === 'favorites') {
      const badge = document.createElement('span');
      badge.className = 'pl-kind';
      badge.textContent = '♥';
      badge.title = 'Favorites';
      badge.setAttribute('aria-label', 'Favorites');
      name.prepend(badge);
    }

    const meta = document.createElement('span');
    meta.className = 'row-meta';
    meta.textContent = `${p.track_count} track${p.track_count !== 1 ? 's' : ''}`;

    const chev = document.createElement('span');
    chev.className = 'row-chevron';
    chev.setAttribute('aria-hidden', 'true');
    chev.textContent = '›';

    row.append(name, meta, chev);
    const open = () => showDetail(p.id);
    row.addEventListener('click', open);
    row.addEventListener('keydown', e => {
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); open(); }
    });
    wrap.appendChild(row);
  });

  panel().replaceChildren(wrap);
}

// ── Detail view ───────────────────────────────────────────────────────────
async function showDetail(id) {
  panel().innerHTML = '<div class="panel-loading" aria-live="polite" role="status"></div>';
  let data;
  try {
    data = await (await apiFetch(`/api/playlists/${id}`)).json();
  } catch (err) {
    if (err.name === 'AbortError' || !active) return;
    panel().innerHTML = '<div class="panel-empty" role="alert">Failed to load playlist.</div>';
    return;
  }
  if (!active) return;
  detail = data;
  renderCrumbs();
  renderActions();
  renderItems();
}

// The breadcrumb holds only the open playlist's name: the "Playlists" subtab
// already labels the section and is the way back to the list, so we never repeat
// it here. The list view shows no breadcrumb at all; its whole bar is hidden so
// there's no empty strip (the bar also carries the detail-view head actions).
function renderCrumbs() {
  const bc = crumbs();
  bc.replaceChildren();
  if (detail) {
    const cur = document.createElement('span');
    cur.className = 'bc-item bc-current';
    cur.textContent = detail.name;
    bc.appendChild(cur);
  }
  const bar = bc.closest('.library-bar');
  if (bar) bar.style.display = detail ? '' : 'none';
}

// playableQueue maps the playlist's live (non-trashed) items to controller
// tracks, plus a lookup from item index → queue index for play-from-row.
function playableQueue() {
  const tracks = [];
  const queueIndexOf = new Map();
  detail.items.forEach((it, i) => {
    if (it.status !== 'ok') return;
    queueIndexOf.set(i, tracks.length);
    tracks.push({
      url: `${API}${it.url}`,
      tagsetId: it.tagset_id || null,
      // Remote madnetwork items stay likable by hash from the player bar.
      remoteLike: it.remote ? {
        hash: it.hash, title: it.title || '', artist: it.artist || '', album: it.album || '',
      } : null,
      rowKey: it.tagset_id ? `ts:${it.tagset_id}` : `url:${API}${it.url}`,
      title: it.title || 'Unknown',
      artist: it.artist || '',
      dur: it.duration_seconds || undefined,
    });
  });
  return { tracks, queueIndexOf };
}

function renderActions() {
  const el = actionsEl();
  el.replaceChildren();
  if (!detail) return;

  const playAll = document.createElement('button');
  playAll.className = 'btn btn-neutral pl-btn';
  playAll.textContent = '▶ Play all';
  playAll.disabled = !detail.items.some(it => it.status === 'ok');
  playAll.addEventListener('click', () => {
    const { tracks } = playableQueue();
    if (tracks.length) controller.setQueue(tracks, 0);
  });
  el.appendChild(playAll);

  if (detail.kind !== 'regular') return; // favorites: not renamable/deletable

  const rename = document.createElement('button');
  rename.className = 'btn btn-neutral pl-btn';
  rename.textContent = 'Rename';
  rename.addEventListener('click', () => showRenameForm());
  el.appendChild(rename);

  // Two-step destructive confirm: first click arms the button, second deletes.
  const del = document.createElement('button');
  del.className = 'btn btn-neutral pl-btn';
  del.textContent = 'Delete';
  let armed = false, disarmTimer = null;
  del.addEventListener('click', async () => {
    if (!armed) {
      armed = true;
      del.textContent = 'Really delete?';
      del.classList.add('pl-btn-danger');
      disarmTimer = setTimeout(() => {
        armed = false;
        del.textContent = 'Delete';
        del.classList.remove('pl-btn-danger');
      }, 4000);
      return;
    }
    clearTimeout(disarmTimer);
    try {
      await apiFetch(`/api/playlists/${detail.id}`, { method: 'DELETE' });
    } catch { return; }
    if (active) showList();
  });
  el.appendChild(del);
}

function showRenameForm() {
  const el = actionsEl();
  el.replaceChildren();
  const form = document.createElement('form');
  form.className = 'pl-rename-form';
  const input = document.createElement('input');
  input.type = 'text';
  input.maxLength = 200;
  input.required = true;
  input.value = detail.name;
  input.setAttribute('aria-label', 'Playlist name');
  const save = document.createElement('button');
  save.type = 'submit';
  save.className = 'btn btn-neutral pl-btn';
  save.textContent = 'Save';
  const cancel = document.createElement('button');
  cancel.type = 'button';
  cancel.className = 'btn btn-neutral pl-btn';
  cancel.textContent = 'Cancel';
  cancel.addEventListener('click', () => renderActions());
  form.append(input, save, cancel);
  form.addEventListener('submit', async e => {
    e.preventDefault();
    const name = input.value.trim();
    if (!name) return;
    try {
      await apiFetch(`/api/playlists/${detail.id}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name }),
      });
    } catch { return; }
    if (!active) return;
    detail.name = name;
    renderCrumbs();
    renderActions();
  });
  el.appendChild(form);
  input.focus();
  input.select();
}

let dragIndex = -1;

function renderItems() {
  const { tracks, queueIndexOf } = playableQueue();
  const durCache = loadDurCache();

  const wrap = document.createElement('div');
  wrap.className = 'panel-fade-in';

  if (!detail.items.length) {
    wrap.innerHTML =
      '<div class="panel-empty">This playlist is empty. ' +
      'Add tracks from the queue panel or the library.</div>';
  }

  detail.items.forEach((it, i) => {
    // "trashed" = a local appearance in Trash; "unavailable" = a remote track
    // no source can currently provide. Both render dimmed and unplayable.
    const unavailable = it.status === 'unavailable';
    const trashed = it.status !== 'ok';
    const row = document.createElement('div');
    row.className = 'track-row' + (trashed ? ' trashed' : '');
    row.tabIndex = 0;
    row.draggable = true;
    row.dataset.i = i;
    if (!trashed) {
      row.dataset.url = `${API}${it.url}`;
      row.dataset.key = it.tagset_id ? `ts:${it.tagset_id}` : `url:${API}${it.url}`;
    }
    row.setAttribute('role', 'button');
    row.setAttribute('aria-label', trashed
      ? `${it.title || 'Unknown'} (${unavailable ? 'currently unavailable' : 'in Trash, not playable'})`
      : `Play ${it.title || 'Unknown'}`);
    if (unavailable) row.title = 'Not in the local library — currently unavailable';
    else if (trashed) row.title = 'In Trash — not playable';

    const num = document.createElement('span');
    num.className = 'track-num';
    num.textContent = i + 1;

    const playIcon = document.createElement('span');
    playIcon.className = 'track-icon-playing';
    playIcon.setAttribute('aria-hidden', 'true');
    playIcon.innerHTML =
      '<svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor"><path d="M8 5v14l11-7z"/></svg>';

    const info = document.createElement('div');
    info.className = 'track-info';
    const title = document.createElement('div');
    title.className = 'track-title';
    title.textContent = it.title || 'Unknown';
    if (it.remote) {
      const badge = document.createElement('span');
      badge.className = 'pl-remote';
      badge.textContent = 'remote';
      badge.title = 'Remote madnetwork track — not in the local library, may become unavailable';
      title.append(badge);
    }
    const meta = document.createElement('div');
    meta.className = 'track-meta';
    meta.textContent = [it.artist, it.album].filter(Boolean).join(' · ');
    info.append(title, meta);

    const dur = document.createElement('span');
    dur.className = 'track-dur';
    dur.textContent = it.duration_seconds
      ? fmtTime(it.duration_seconds)
      : (durCache[`${API}${it.url}`] || '');

    const rm = document.createElement('button');
    rm.className = 'pl-remove';
    rm.textContent = '×';
    rm.title = 'Remove from playlist';
    rm.setAttribute('aria-label', `Remove ${it.title || 'track'} from playlist`);
    rm.addEventListener('click', e => { e.stopPropagation(); removeItem(it.item_id); });

    row.append(num, playIcon, info, dur, rm);

    const play = () => {
      if (trashed) return;
      // Click the playing row to pause/resume; any other row starts fresh.
      const qi = queueIndexOf.get(i);
      const cur = controller.current();
      if (cur && playKeyOf(cur.track) === tracks[qi].rowKey) controller.toggle();
      else controller.setQueue(tracks, qi);
    };
    row.addEventListener('click', play);
    row.addEventListener('keydown', e => {
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); play(); }
      else if (e.key === 'Delete' || e.key === 'Backspace') { e.preventDefault(); removeItem(it.item_id); }
      else if ((e.ctrlKey || e.altKey) && e.key === 'ArrowUp' && i > 0) {
        e.preventDefault(); moveItem(i, i - 1);
      } else if ((e.ctrlKey || e.altKey) && e.key === 'ArrowDown' && i < detail.items.length - 1) {
        e.preventDefault(); moveItem(i, i + 1);
      }
    });

    // Drag reorder — same pattern as the queue panel.
    row.addEventListener('dragstart', e => {
      dragIndex = i;
      row.classList.add('dragging');
      e.dataTransfer.effectAllowed = 'move';
    });
    row.addEventListener('dragend', () => {
      dragIndex = -1;
      row.classList.remove('dragging');
      clearDropMarks();
    });
    row.addEventListener('dragover', e => {
      if (dragIndex < 0 || dragIndex === i) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';
      clearDropMarks();
      row.classList.add(dragIndex < i ? 'drop-below' : 'drop-above');
    });
    row.addEventListener('drop', e => {
      e.preventDefault();
      if (dragIndex >= 0 && dragIndex !== i) moveItem(dragIndex, i);
      dragIndex = -1;
    });

    wrap.appendChild(row);
  });

  panel().replaceChildren(wrap);

  const cur = controller.current();
  if (cur) highlightPlaying(cur.track);
}

function clearDropMarks() {
  panel().querySelectorAll('.drop-above, .drop-below')
    .forEach(r => r.classList.remove('drop-above', 'drop-below'));
}

async function removeItem(itemID) {
  try {
    await apiFetch(`/api/playlists/${detail.id}/items/${itemID}`, { method: 'DELETE' });
  } catch { return; }
  if (!active || !detail) return;
  detail.items = detail.items.filter(it => it.item_id !== itemID);
  renderItems();
  renderActions(); // play-all enable state may change
}

// moveItem reorders optimistically, then PUTs the full ordering; a failed PUT
// reloads the detail so the view matches the server again.
async function moveItem(from, to) {
  const [it] = detail.items.splice(from, 1);
  detail.items.splice(to, 0, it);
  renderItems();
  try {
    await apiFetch(`/api/playlists/${detail.id}/items`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ item_ids: detail.items.map(x => x.item_id) }),
    });
  } catch {
    if (active && detail) showDetail(detail.id);
  }
}

// ── Lifecycle (driven by shell.js) ─────────────────────────────────────────
export function init() {
  active = true;
  abort = new AbortController();
  if (!gatePage(PAGE_PERMS.playlists)) return;
  showList();
}

export function teardown() {
  active = false;
  abort?.abort();
  abort = null;
  detail = null;
}
