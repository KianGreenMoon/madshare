// Full Library · Files — the file-grain lens over the live library: every
// non-removed blob, one row per file (the physical view; the descriptive view
// is the All Appearances lens). A config over the shared trash-list core, plus
// Play via the page's shared preview player. Remove soft-removes the blob
// (restorable from Trash › Files; removing a recording's last rendition makes
// it dormant). Removed blobs live exactly one place: Trash › Files.
import { API, el, fmtBytes, fmtDate, handleAuthError, toast } from './shared.js';
import { createTrashList } from './trash-list.js';
import { TRASH_ICON } from '../icons.js';

const dispName = f => f.title || f.filename || 'this file';

export function createLibraryFiles({ host, play, perms }) {
  let list = null;
  const canDelete = perms.includes('file.delete');

  function cells(f) {
    return [
      el('td', {}, [
        el('div', {}, [dispName(f)]),
        f.artist ? el('div', { class: 'trash-sub' }, [f.artist]) : null,
        el('a', {
          class: 'trash-rec-link', href: `/admin/library#recordings-${f.recording_id}`,
          title: 'Open this blob’s recording (renditions & appearances)',
        }, [`recording #${f.recording_id} →`]),
      ]),
      el('td', { class: 'cell-size' }, [fmtBytes(f.byte_size)]),
      el('td', {}, [el('span', { class: 'trash-chip' }, [f.storage_backend || 'local'])]),
      el('td', { class: 'trash-sub' }, [fmtDate(f.created_at)]),
    ];
  }

  function playFile(f, visible) {
    const items = (visible || []).map(x => ({ url: x.url, title: dispName(x), artist: x.artist || '', key: x.id }));
    let idx = items.findIndex(x => x.key === f.id);
    if (idx < 0) { items.length = 0; items.push({ url: f.url, title: dispName(f), artist: f.artist || '', key: f.id }); idx = 0; }
    play(items, idx, k => list.setPlaying(k));
  }

  list = createTrashList({
    host,
    pageSize: 100,
    emptyText: 'No files yet. Add music from the Upload page.',
    loadErrorText: 'Couldn’t load the file list.',
    columns: ['File', 'Size', 'Storage', 'Uploaded'],
    rowKey: f => f.id,
    rowLabel: dispName,
    renderCells: cells,
    onPlay: playFile,
    // The one verb — per row and in bulk — is the reversible soft-remove
    // (file.delete, the same gate as the recordings-arm Remove). Without the
    // permission there is no bulk action, so no selection column renders.
    itemNoun: 'file',
    bulkActions: canDelete ? [{
      label: n => `Remove selected (${n})`, kind: 'danger',
      async run(ids, all) {
        const res = await fetch(`${API}/api/admin/renditions/bulk`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(all ? { action: 'remove', all: true } : { action: 'remove', ids }),
        });
        if (handleAuthError(res)) return null;
        if (!res.ok) { toast(`Remove failed (HTTP ${res.status}).`, 'error'); return null; }
        const n = (await res.json()).affected || 0;
        toast(`Removed ${n} file${n === 1 ? '' : 's'} — restorable from Trash › Files.`, 'success');
        return n;
      },
    }] : [],
    rowActions: canDelete ? [{
      icon: TRASH_ICON, title: 'Remove file', kind: 'danger',
      async run(f) {
        const res = await fetch(`${API}/api/admin/renditions/${f.id}/remove`, { method: 'POST' });
        if (handleAuthError(res)) return false;
        if (!res.ok) { toast(`Remove failed (HTTP ${res.status}).`, 'error'); return false; }
        toast(`“${dispName(f)}” removed — restorable from Trash › Files. Removing a recording’s last file makes it dormant.`, 'success');
        return true;
      },
    }] : [],
    async fetchPage(offset, limit) {
      const res = await fetch(`${API}/api/files?limit=${limit}&offset=${offset}`);
      if (handleAuthError(res)) throw new Error('auth');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      return res.json();
    },
  });

  return list;
}
