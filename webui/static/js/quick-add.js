// quick-add.js — the shared "⋯" quick-add layer for browse rows (library and
// madnetwork pages, docs/ui/madnetwork-page.md §Shared browse core). Extracted
// from app.js (Phase 5 step 4 of docs/api/playlists.md). Owns the queue actions
// (Play next / Add to queue), the playlist picker submenu, and the ⋯ button
// itself. Favorites are NOT a menu item — the inline heart on the row is the
// one favorites control (browse-rows.js).
//
// A page appends its own trailing items (library: Download; madnetwork:
// Materialize) via the extraItems option.
import { openLoginModal } from './auth.js';
import { getController } from './player-controller.js';
import { openRowMenu } from './row-menu.js';
import { showToast } from './shell.js';

const API = document.querySelector('meta[name="api-url"]')?.content || '';

// queueAdd runs a (possibly async) track collector and applies a queue action.
export async function queueAdd(collect, how) {
  let tracks;
  try { tracks = await collect(); }
  catch { showToast('Failed to load tracks.', { type: 'error' }); return; }
  if (!tracks.length) return;
  const controller = getController();
  if (how === 'next') controller.playNext(tracks);
  else controller.enqueue(tracks);
  showToast(`${tracks.length} track${tracks.length !== 1 ? 's' : ''} ${how === 'next' ? 'will play next' : 'added to queue'}.`,
    { type: 'success' });
}

// addToPlaylistMenu replaces the open row menu with a playlist picker (plus
// "New playlist…"), then posts the collected tracks' tagset ids.
export async function addToPlaylistMenu(anchor, collect) {
  let lists, tracks;
  try {
    const res = await fetch(`${API}/api/playlists`);
    if (res.status === 401 || res.status === 403) { openLoginModal(); return; }
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    lists = await res.json();
    tracks = await collect();
  } catch { showToast('Failed to load playlists.', { type: 'error' }); return; }
  const tagsetIDs = tracks.map(t => t.tagsetId).filter(Boolean);
  if (!tagsetIDs.length) return;

  const add = async (id, name) => {
    try {
      const res = await fetch(`${API}/api/playlists/${id}/items`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ tagset_ids: tagsetIDs }),
      });
      if (!res.ok) throw new Error((await res.text().catch(() => '')).trim() || `HTTP ${res.status}`);
      const { added } = await res.json();
      showToast(`Added ${added} track${added !== 1 ? 's' : ''} to "${name}".`, { type: 'success' });
      if (added === 0 && tagsetIDs.length) showToast(`Already in "${name}".`, { type: 'status' });
    } catch (err) {
      showToast(`Couldn't add to "${name}": ${err.message}`, { type: 'error' });
    }
  };
  const items = lists.map(p => ({
    label: (p.kind === 'favorites' ? '♥ ' : '') + p.name,
    onClick: () => add(p.id, p.name),
  }));
  items.push({
    input: 'New playlist…',
    onSubmit: async name => {
      try {
        const res = await fetch(`${API}/api/playlists`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name, tagset_ids: tagsetIDs }),
        });
        if (!res.ok) throw new Error((await res.text().catch(() => '')).trim() || `HTTP ${res.status}`);
        showToast(`Created "${name}" with ${tagsetIDs.length} track${tagsetIDs.length !== 1 ? 's' : ''}.`, { type: 'success' });
      } catch (err) {
        showToast(`Couldn't create playlist: ${err.message}`, { type: 'error' });
      }
    },
  });
  openRowMenu(anchor, items);
}

// quickAddItems builds the "⋯" menu for a row. collect yields the row's tracks
// (a single track, an album, or a whole artist). extraItems are appended last —
// the page-specific trailing actions.
export function quickAddItems(anchor, collect, { extraItems = [] } = {}) {
  return [
    { label: 'Play next',       onClick: () => queueAdd(collect, 'next') },
    { label: 'Add to queue',    onClick: () => queueAdd(collect, 'queue') },
    { label: 'Add to playlist…', keepOpen: true, onClick: () => addToPlaylistMenu(anchor, collect) },
    ...extraItems,
  ];
}

// mkMoreBtn returns the "⋯" row button wired to the quick-add menu.
export function mkMoreBtn(label, makeItems) {
  const btn = document.createElement('button');
  btn.className = 'row-more';
  btn.setAttribute('aria-label', label);
  btn.setAttribute('aria-haspopup', 'menu');
  btn.title = 'More actions';
  btn.textContent = '⋯';
  btn.addEventListener('click', e => {
    e.stopPropagation();
    openRowMenu(btn, makeItems(btn));
  });
  return btn;
}
