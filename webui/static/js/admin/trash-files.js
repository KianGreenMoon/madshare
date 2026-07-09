// Trash · Files — the file-grain lens (soft-delete.md): soft-removed blobs
// (removed renditions, absorbed/dormant blobs). A config over the shared
// trash-list core, plus Play via the page's shared preview player. Restore
// un-removes the blob (a dormant recording re-enters the library); Delete
// forever reclaims the blob — and if it was the recording's last file, cascade-
// prunes the whole recording. Requires file.delete (the Trash panel gate).
import { API, el, fmtBytes, fmtDate, handleAuthError, toast } from './shared.js';
import { createTrashList } from './trash-list.js';

async function req(url, opts) {
  const res = await fetch(url, opts);
  if (handleAuthError(res)) return null;
  const data = await res.json().catch(() => ({}));
  if (!res.ok || data.ok === false) {
    toast(data.error ? `Error: ${data.error}` : `HTTP ${res.status}`, 'error');
    return null;
  }
  return data;
}

const dispName = f => f.title || f.filename || 'this file';

export function createTrashFiles({ host, play, confirmDelete }) {
  let list = null;

  function cells(f) {
    return [
      el('td', {}, [
        el('div', {}, [dispName(f)]),
        f.artist ? el('div', { class: 'trash-sub' }, [f.artist]) : null,
        el('a', { class: 'trash-rec-link', href: `/admin/recordings#${f.recording_id}`, title: 'Open the recording this blob belongs to' }, [`recording #${f.recording_id} →`]),
      ]),
      el('td', { class: 'cell-size' }, [fmtBytes(f.byte_size)]),
      el('td', {}, [el('span', { class: 'trash-chip' }, [f.storage_backend || 'local'])]),
      el('td', { class: 'trash-sub' }, [fmtDate(f.removed_at)]),
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
    emptyText: 'No removed files.',
    columns: ['File', 'Size', 'Storage', 'Removed'],
    rowKey: f => f.id,
    rowLabel: dispName,
    rowClass: () => 'rec-row--removed',
    renderCells: cells,
    onPlay: playFile,
    confirmDelete,
    async fetchPage(offset, limit) {
      const data = await req(`${API}/api/admin/trash/files?limit=${limit}&offset=${offset}`);
      return data || { total: 0, items: [] };
    },
    async restoreOne(f) {
      if (!await req(`${API}/api/admin/renditions/${f.id}/restore`, { method: 'POST' })) return false;
      toast(`“${dispName(f)}” restored.`, 'success');
      return true;
    },
    async deleteOne(f) {
      if (!await req(`${API}/api/admin/renditions/${f.id}`, { method: 'DELETE' })) return false;
      toast(`“${dispName(f)}” permanently deleted.`, 'success');
      return true;
    },
    async bulkRestore(ids) {
      const d = await req(`${API}/api/admin/trash/files/bulk`, bulkBody('restore', ids));
      return d ? d.affected : null;
    },
    async bulkDelete(ids) {
      const d = await req(`${API}/api/admin/trash/files/bulk`, bulkBody('delete', ids));
      return d ? d.affected : null;
    },
  });

  return list;
}

function bulkBody(action, ids) {
  return { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action, ids }) };
}
