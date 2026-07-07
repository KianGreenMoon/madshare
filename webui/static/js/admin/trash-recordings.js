// Trash · Recordings — the recording-grain lens (soft-delete.md): recordings
// wholly out of the library (all appearances trashed and/or dormant). A config
// over the shared trash-list core: whole-recording Restore (un-trash + un-dormant)
// and Delete forever (the count-aware cascade). Requires file.delete (the Trash
// panel is gated there). Reuses the recordings-listing DTO shape.
import { API, el, handleAuthError, toast } from './shared.js';
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

const dispName = rec => rec.title || 'Untitled recording';

export function createTrashRecordings({ host, confirmDelete }) {
  function cells(rec) {
    const chips = [];
    if (rec.dormant) chips.push(el('span', { class: 'trash-chip trash-chip--dorm', title: 'No surviving rendition' }, ['dormant']));
    if (rec.trashed_appearances) chips.push(el('span', { class: 'trash-chip trash-chip--n' }, [`${rec.trashed_appearances} trashed appearance${rec.trashed_appearances === 1 ? '' : 's'}`]));
    if (rec.removed_files) chips.push(el('span', { class: 'trash-chip' }, [`${rec.removed_files} removed file${rec.removed_files === 1 ? '' : 's'}`]));
    return [
      el('td', {}, [
        el('div', {}, [dispName(rec)]),
        rec.artist ? el('div', { class: 'trash-sub' }, [rec.artist]) : null,
        el('a', { class: 'trash-rec-link', href: `/admin/recordings#${rec.id}`, title: 'Open in the recordings curation view' }, [`recording #${rec.id} →`]),
      ]),
      el('td', {}, [el('span', { class: 'trash-chips' }, chips)]),
    ];
  }

  const list = createTrashList({
    host,
    pageSize: 100,
    emptyText: 'No trashed recordings.',
    columns: ['Recording', 'State'],
    rowKey: rec => rec.id,
    renderCells: cells,
    confirmDelete,
    async fetchPage(offset, limit) {
      const data = await req(`${API}/api/admin/trash/recordings?limit=${limit}&offset=${offset}`);
      return data || { total: 0, items: [] };
    },
    async restoreOne(rec) {
      if (!await req(`${API}/api/admin/recordings/${rec.id}/restore`, { method: 'POST' })) return false;
      toast(`“${dispName(rec)}” restored to the library.`, 'success');
      return true;
    },
    async deleteOne(rec) {
      if (!await req(`${API}/api/admin/recordings/${rec.id}`, { method: 'DELETE' })) return false;
      toast(`“${dispName(rec)}” permanently deleted.`, 'success');
      return true;
    },
    async bulkRestore(ids) {
      const d = await req(`${API}/api/admin/trash/recordings/bulk`, bulkBody('restore', ids));
      return d ? d.affected : null;
    },
    async bulkDelete(ids) {
      const d = await req(`${API}/api/admin/trash/recordings/bulk`, bulkBody('delete', ids));
      return d ? d.affected : null;
    },
  });

  return list;
}

function bulkBody(action, ids) {
  return { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action, ids }) };
}
