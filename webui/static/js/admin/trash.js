// Admin · Library — Trash scope. A factory over the shared file-management
// component (file-list.js), mounted by library.js into the Library page's Trash
// panel. Trash has per-file Edit (tags only — access endpoints reject
// soft-deleted rows), Play, Restore, Delete forever, and bulk actions. The
// shared preview player is injected as `play`. Requires file.delete.
//
// Design: docs/plans/file-management-view.md.
import { API, fmtDate, toast, handleAuthError } from './shared.js';
import { createFileList } from '../file-list.js';

const displayTitle = f => f.title || f.filename || 'this file';

export function createTrashScope({ play, perms }) {
  let fileList = null;

  // ── Fetch helpers ───────────────────────────────────────────────────────────
  async function loadTrash() {
    const res = await fetch(`${API}/api/admin/trash`);
    if (handleAuthError(res)) throw new Error('Your session expired.');
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    return await res.json();
  }
  async function restoreOne(hash) {
    const res = await fetch(`${API}/api/admin/trash/${encodeURIComponent(hash)}/restore`, { method: 'POST' });
    if (handleAuthError(res)) throw new Error('Your session expired.');
    const data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
  }
  async function hardDeleteOne(hash) {
    const res = await fetch(`${API}/api/admin/trash/${encodeURIComponent(hash)}`, { method: 'DELETE' });
    if (handleAuthError(res)) throw new Error('Your session expired.');
    const data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
  }
  async function runBulk(hashes, makeRequest) {
    let ok = 0, fail = 0, authFailed = false;
    for (const hash of hashes) {
      let res;
      try { res = await makeRequest(hash); } catch { fail++; continue; }
      if (res.status === 401) { handleAuthError(res); authFailed = true; break; }
      const data = await res.json().catch(() => ({}));
      if (res.ok && data.ok) ok++; else fail++;
    }
    return { ok, fail, authFailed };
  }

  // ── Bulk permanent-delete confirm modal (in library.html) ──────────────────
  const delModal   = document.getElementById('trashDeleteModal');
  const delBody    = document.getElementById('trashDeleteBody');
  const delError   = document.getElementById('trashDeleteError');
  const delConfirm = document.getElementById('trashDeleteConfirm');
  const delCancel  = document.getElementById('trashDeleteCancel');
  const delClose   = document.getElementById('trashDeleteClose');

  function confirmBulkDelete(n) {
    return new Promise(resolve => {
      delBody.textContent = `Permanently delete ${n} ${n === 1 ? 'file' : 'files'}?`;
      delConfirm.textContent = `Delete ${n} forever`;
      delError.hidden = true; delError.textContent = '';
      delModal.classList.remove('hidden');
      delConfirm.focus();
      const done = (val) => {
        delModal.classList.add('hidden');
        delConfirm.removeEventListener('click', onOk);
        delCancel.removeEventListener('click', onCancel);
        delClose.removeEventListener('click', onCancel);
        delModal.removeEventListener('click', onBackdrop);
        document.removeEventListener('keydown', onKey);
        resolve(val);
      };
      const onOk = () => done(true);
      const onCancel = () => done(false);
      const onBackdrop = e => { if (e.target === delModal) done(false); };
      const onKey = e => { if (e.key === 'Escape' && !delModal.classList.contains('hidden')) done(false); };
      delConfirm.addEventListener('click', onOk);
      delCancel.addEventListener('click', onCancel);
      delClose.addEventListener('click', onCancel);
      delModal.addEventListener('click', onBackdrop);
      document.addEventListener('keydown', onKey);
    });
  }

  // ── Preview (via the shared player) ─────────────────────────────────────────
  function playFile(f, visible) {
    const items = (visible || []).map(x => ({ url: x.url, title: displayTitle(x), artist: x.artist || '', key: x.hash }));
    let idx = items.findIndex(x => x.key === f.hash);
    if (idx < 0) { items.length = 0; items.push({ url: f.url, title: displayTitle(f), artist: f.artist || '', key: f.hash }); idx = 0; }
    play(items, idx, k => fileList.setPlaying(k));
  }

  // ── Scope descriptor ────────────────────────────────────────────────────────
  const scope = {
    title: 'Trash',
    desc: 'Files moved to trash are hidden from the library but their blobs remain on disk. '
        + 'Fix a tag before restoring, restore to bring a file back, or delete forever to remove it permanently.',
    emptyText: 'Trash is empty.',
    columns: ['check', 'title', 'artist', 'album', 'size', 'meta', 'actions'],
    metaLabel: 'Deleted',
    metaValue: f => fmtDate(f.deleted_at),
    badge: f => (f.review_state && f.review_state !== 'approved')
      ? { text: 'pending review', cls: 'is-' + f.review_state, title: 'Restore returns this file to the moderation queue' }
      : null,
    accessEditable: false,
    licenses: [],
    load: loadTrash,
    selectable: () => true,
    editPatchURL: f => `${API}/api/files/${encodeURIComponent(f.hash)}/metadata`,
    editNote: 'Fix a tag before restoring — the corrected tags decide where the file lands when it returns to the library.',
    rowActions: [
      {
        id: 'restore', label: 'Restore', kind: 'neutral',
        run: async f => {
          await restoreOne(f.hash);
          const pending = f.review_state && f.review_state !== 'approved';
          toast(`“${displayTitle(f)}” restored to ${pending ? 'the moderation queue' : 'library'}.`, 'success');
        },
      },
      {
        id: 'delete', label: 'Delete forever', kind: 'danger',
        confirm: 'inline', confirmPrompt: 'Delete forever?', confirmLabel: 'Delete forever',
        run: async f => { await hardDeleteOne(f.hash); toast(`“${displayTitle(f)}” permanently deleted.`, 'success'); },
      },
    ],
    bulkActions: [
      {
        id: 'restoreSel', label: 'Restore selected', kind: 'neutral',
        run: async hashes => {
          const { ok, fail, authFailed } = await runBulk(hashes, h => fetch(`${API}/api/admin/trash/${encodeURIComponent(h)}/restore`, { method: 'POST' }));
          if (authFailed) { if (ok) toast(`Restored ${ok} before the session expired.`, 'error'); return; }
          if (fail) toast(`Restored ${ok}; ${fail} failed.`, 'error');
          else if (ok) toast(`Restored ${ok} ${ok === 1 ? 'file' : 'files'}.`, 'success');
        },
      },
      {
        id: 'deleteSel', label: 'Delete selected', kind: 'danger',
        run: async hashes => {
          if (!await confirmBulkDelete(hashes.length)) return false;
          const { ok, fail, authFailed } = await runBulk(hashes, h => fetch(`${API}/api/admin/trash/${encodeURIComponent(h)}`, { method: 'DELETE' }));
          if (authFailed) { if (ok) toast(`Deleted ${ok} before the session expired.`, 'error'); return; }
          if (fail) toast(`Permanently deleted ${ok}; ${fail} failed.`, 'error');
          else if (ok) toast(`Permanently deleted ${ok} ${ok === 1 ? 'file' : 'files'}.`, 'success');
        },
      },
    ],
    bulkApply: async (hashes, patch) => {
      const { ok, fail } = await runBulk(hashes, h => fetch(`${API}/api/files/${encodeURIComponent(h)}/metadata`, {
        method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(patch),
      }));
      if (fail) throw new Error(`updated ${ok}, ${fail} failed`);
    },
    onPlay: playFile,
    toast, handleAuthError,
  };

  fileList = createFileList(scope);

  return {
    id: 'trash',
    label: 'Trash',
    available: perms.includes('file.delete'),
    mount: () => fileList.mount(document.getElementById('fileListTrash')),
    reload: () => fileList.reload(),
  };
}
