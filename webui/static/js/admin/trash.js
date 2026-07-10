// Admin · Library — Trash scope: the **Appearances** lens. A factory over the
// shared file-management component (file-list.js), mounted by library.js into
// the Library page's Trash panel. Per-appearance Edit (tags only — access is a
// recording property), Play, Restore, Delete forever, and bulk actions. The
// shared preview player is injected as `play`. Requires file.delete.
//
// The row is an APPEARANCE, keyed by tagset_id (recording-tagsets P7c). A blob
// can host several trashed appearances, so a hash names no single row; and an
// appearance whose origin blob was absorbed or purged has no hash at all — it
// still belongs in Trash, just without preview or size.
//
// Design: docs/architecture/file-management-view.md.
import { API, el, fmtDate, toast, handleAuthError } from './shared.js';
import { createFileList } from '../file-list.js';
import { RESTORE_ICON, TRASH_ICON } from '../icons.js';
import { createTrashRecordings } from './trash-recordings.js';
import { createTrashFiles } from './trash-files.js';

const displayTitle = f => f.title || f.filename || 'this file';

export function createTrashScope({ play, perms }) {
  let fileList = null;

  // ── Fetch helpers ───────────────────────────────────────────────────────────
  // loadTrashPage backs the paged component: one server page of Trash, filtered +
  // sorted, as {total, items} (file-list-scaling.md). Every trashed row is
  // selectable, so there is no selectable_total — the banner uses total.
  async function loadTrashPage({ limit, offset, q, field, sort }) {
    const params = new URLSearchParams({ limit: String(limit), offset: String(offset) });
    if (sort) params.set('sort', sort);
    if (q) params.set('q', q);
    if (field) params.set('field', field);
    const res = await fetch(`${API}/api/admin/trash?${params.toString()}`);
    if (handleAuthError(res)) throw new Error('Your session expired.');
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const data = await res.json();
    return { total: data.total || 0, items: data.items || [] };
  }
  async function restoreOne(tagsetID) {
    const res = await fetch(`${API}/api/admin/tagsets/${tagsetID}/restore`, { method: 'POST' });
    if (handleAuthError(res)) throw new Error('Your session expired.');
    const data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
  }
  async function hardDeleteOne(tagsetID) {
    const res = await fetch(`${API}/api/admin/tagsets/${tagsetID}`, { method: 'DELETE' });
    if (handleAuthError(res)) throw new Error('Your session expired.');
    const data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
  }

  // trashBulkCall is the single batched Trash action (restore / delete / edit)
  // over an explicit tagset_ids list OR a filter ("select all N matching").
  // file-list.js hands selection keys back as strings; the API wants numbers.
  async function trashBulkCall(body) {
    const res = await fetch(`${API}/api/admin/trash/bulk`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
    });
    if (handleAuthError(res)) throw new Error('Your session expired.');
    const data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
    return data;
  }
  const trashFilterBody = filter => ({ filter: { q: filter.q, field: filter.field }, all: !filter.q });
  const asIDs = keys => keys.map(Number);
  const noun = n => (n === 1 ? 'appearance' : 'appearances');
  const restoreToast = n => { if (n) toast(`Restored ${n} ${noun(n)}.`, 'success'); };
  const deleteToast = n => { if (n) toast(`Permanently deleted ${n} ${noun(n)}.`, 'success'); };

  // ── Bulk permanent-delete confirm modal (in library.html) ──────────────────
  const delModal   = document.getElementById('trashDeleteModal');
  const delBody    = document.getElementById('trashDeleteBody');
  const delError   = document.getElementById('trashDeleteError');
  const delConfirm = document.getElementById('trashDeleteConfirm');
  const delCancel  = document.getElementById('trashDeleteCancel');
  const delClose   = document.getElementById('trashDeleteClose');

  function confirmBulkDelete(bodyText, confirmLabel) {
    return new Promise(resolve => {
      delBody.textContent = bodyText;
      delConfirm.textContent = confirmLabel;
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
  // A trashed appearance whose origin blob is gone has no url — it cannot be
  // previewed, and it must not sit in the queue as a dead entry.
  function playFile(f, visible) {
    if (!f.url) { toast('This appearance has no audio of its own — the blob was absorbed or purged.', 'error'); return; }
    const items = (visible || []).filter(x => x.url)
      .map(x => ({ url: x.url, title: displayTitle(x), artist: x.artist || '', key: x.tagset_id }));
    let idx = items.findIndex(x => x.key === f.tagset_id);
    if (idx < 0) { items.length = 0; items.push({ url: f.url, title: displayTitle(f), artist: f.artist || '', key: f.tagset_id }); idx = 0; }
    play(items, idx, k => fileList.setPlaying(k));
  }

  // ── Scope descriptor ────────────────────────────────────────────────────────
  const scope = {
    title: 'Trash',
    desc: 'Appearances moved to trash are hidden from the library; the audio stays on disk. '
        + 'Fix a tag before restoring, restore to bring one back, or delete forever to remove it permanently '
        + '(the last appearance of a recording takes the recording and its files with it).',
    emptyText: 'Trash is empty.',
    columns: ['check', 'title', 'artist', 'album', 'size', 'meta', 'actions'],
    artistAlbumSort: true,
    allowCoverAdd: perms.includes('metadata.edit'),   // grouped "Add cover" on coverless separators
    allowCoverEdit: perms.includes('metadata.edit'),  // "Edit cover" on separators that already have one
    apiBase: API,
    // The row identity is the appearance, not the blob.
    rowKey: f => f.tagset_id,
    metaLabel: 'Deleted',
    metaValue: f => fmtDate(f.deleted_at),
    badge: f => (f.review_state && f.review_state !== 'approved')
      ? { text: 'pending review', cls: 'is-' + f.review_state, title: 'Restore returns this file to the moderation queue' }
      : null,
    accessEditable: false,
    licenses: [],
    // Server-paged like the live library (file-list-scaling.md): flat list +
    // "By artist / album", every row selectable, default newest-deleted-first.
    paged: true,
    pageSize: 100,
    loadPage: loadTrashPage,
    selectable: () => true,
    editPatchURL: f => `${API}/api/admin/tagsets/${f.tagset_id}/metadata`,
    editDetailURL: f => `${API}/api/admin/tagsets/${f.tagset_id}/metadata`,
    editNote: 'Fix a tag before restoring — the corrected tags decide where the file lands when it returns to the library.',
    rowActions: [
      {
        id: 'restore', label: 'Restore', icon: RESTORE_ICON, kind: 'neutral',
        run: async f => {
          await restoreOne(f.tagset_id);
          const pending = f.review_state && f.review_state !== 'approved';
          toast(`“${displayTitle(f)}” restored to ${pending ? 'the moderation queue' : 'library'}.`, 'success');
        },
      },
      {
        // Permanent delete confirms in the same modal the Recordings and Files
        // lenses use, so all three ask the same way before the irreversible step.
        id: 'delete', label: 'Delete forever', icon: TRASH_ICON, kind: 'danger',
        run: async f => {
          if (!await confirmBulkDelete(`Permanently delete “${displayTitle(f)}”?`, 'Delete forever')) return false;
          await hardDeleteOne(f.tagset_id);
          toast(`“${displayTitle(f)}” permanently deleted.`, 'success');
        },
      },
    ],
    bulkActions: [
      {
        id: 'restoreSel', label: 'Restore selected', kind: 'neutral',
        run: async keys => { restoreToast((await trashBulkCall({ action: 'restore', tagset_ids: asIDs(keys) })).affected); },
        runAll: async filter => { restoreToast((await trashBulkCall({ action: 'restore', ...trashFilterBody(filter) })).affected); },
      },
      {
        id: 'deleteSel', label: 'Delete selected', kind: 'danger',
        run: async keys => {
          if (!await confirmBulkDelete(`Permanently delete ${keys.length} ${noun(keys.length)}?`, `Delete ${keys.length} forever`)) return false;
          deleteToast((await trashBulkCall({ action: 'delete', tagset_ids: asIDs(keys) })).affected);
        },
        runAll: async filter => {
          if (!await confirmBulkDelete('Permanently delete all matching appearances?', 'Delete all forever')) return false;
          deleteToast((await trashBulkCall({ action: 'delete', ...trashFilterBody(filter) })).affected);
        },
      },
    ],
    // Explicit-selection tag edit and "select all N matching" edit both go through
    // the Trash bulk endpoint (action:"edit"); the component owns the success toast
    // for the page selection, bulkApplyAll owns its own.
    bulkApply: async (keys, patch) => {
      const data = await trashBulkCall({ action: 'edit', tagset_ids: asIDs(keys), patch });
      if (data.failed?.length) throw new Error(`updated ${data.affected}, ${data.failed.length} failed`);
    },
    bulkApplyAll: async (filter, patch) => {
      const data = await trashBulkCall({ action: 'edit', ...trashFilterBody(filter), patch });
      if (data.failed?.length) throw new Error(`updated ${data.affected}, ${data.failed.length} failed`);
      toast(`Updated ${data.affected} ${noun(data.affected)}.`, 'success');
    },
    onPlay: playFile,
    // No origin blob (absorbed / purged) → nothing to preview.
    canPlay: f => !!f.url,
    toast, handleAuthError,
  };

  fileList = createFileList(scope);

  // ── Sub-mode coordinator (soft-delete.md) ───────────────────────────────────
  // The Trash panel has three perspectives over the same not-in-library set:
  // Appearances (the file-list.js scope above), Recordings, and Files (bespoke
  // lists sharing this page's one preview player). Each is its own lens — never
  // merged. Permanent delete lives here and nowhere else.
  const confirmDelete = count => confirmBulkDelete(
    count === 1 ? 'Permanently delete this item?' : `Permanently delete ${count} items?`,
    count === 1 ? 'Delete forever' : `Delete ${count} forever`);

  const recordings = createTrashRecordings({ host: document.getElementById('trashRecordingsList'), confirmDelete });
  const files = createTrashFiles({ host: document.getElementById('trashFilesList'), play, confirmDelete });

  const modes = [
    { id: 'appearances', label: 'Appearances', panel: 'trashMode-appearances', ctrl: { mount: () => fileList.mount(document.getElementById('fileListTrash')), reload: () => fileList.reload() } },
    { id: 'recordings', label: 'Recordings', panel: 'trashMode-recordings', ctrl: recordings },
    { id: 'files', label: 'Files', panel: 'trashMode-files', ctrl: files },
  ];
  const mounted = new Set();
  let activeMode = null;

  function showMode(id) {
    if (activeMode === id) return;
    activeMode = id;
    for (const m of modes) {
      const on = m.id === id;
      document.getElementById(m.panel).hidden = !on;
      const btn = switchEl.querySelector(`[data-mode="${m.id}"]`);
      if (btn) { btn.classList.toggle('view-tab--active', on); btn.setAttribute('aria-selected', String(on)); }
      if (on) {
        if (mounted.has(m.id)) m.ctrl.reload();
        else { mounted.add(m.id); m.ctrl.mount(); }
      }
    }
  }

  let switchEl = null;
  function buildSwitch() {
    switchEl = document.getElementById('trashModeSwitch');
    if (switchEl.childElementCount) return; // already built
    for (const m of modes) {
      switchEl.appendChild(el('button', {
        class: 'view-tab', 'data-mode': m.id, type: 'button', role: 'tab',
        'aria-selected': 'false', 'aria-controls': m.panel,
        onclick: () => showMode(m.id),
      }, [m.label]));
    }
  }

  return {
    id: 'trash',
    label: 'Trash',
    available: perms.includes('file.delete'),
    mount: () => { buildSwitch(); showMode('appearances'); },
    reload: () => { if (activeMode) modes.find(m => m.id === activeMode).ctrl.reload(); },
  };
}
