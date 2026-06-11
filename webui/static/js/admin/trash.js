// Admin · Trash — restore or permanently delete trashed files, individually or
// in bulk, now through the shared file-management component (file-list.js).
// Trash gains what it lacked: a preview Play button and per-file metadata Edit
// (tags only — access is meaningless on a non-served file, and its endpoints
// reject soft-deleted rows). Requires file.delete.
//
// Design: docs/plans/file-management-view.md.
import { bootAdmin, API, fmtDate, toast, handleAuthError } from './shared.js';
import { createPlayer } from '../player.js';
import { createFileList } from '../file-list.js';

const displayTitle = f => f.title || f.filename || 'this file';

// ── Page-local preview player (admin stays out of the persistent shell) ──────
let fileList = null;
let playCtx = null; // { items: [{url,title,artist,key}], index }

const player = createPlayer({
  onPrev:  () => { if (playCtx) playAt(playCtx.index > 0 ? playCtx.index - 1 : playCtx.items.length - 1); },
  onNext:  () => { if (playCtx) playAt(playCtx.index < playCtx.items.length - 1 ? playCtx.index + 1 : 0); },
  onEnded: () => { if (playCtx && playCtx.index < playCtx.items.length - 1) playAt(playCtx.index + 1); },
  onError: () => {
    toast('Couldn’t play this file.', 'error');
    if (playCtx && playCtx.index < playCtx.items.length - 1) playAt(playCtx.index + 1);
  },
});

function playFile(f, visible) {
  const items = (visible || []).map(x => ({ url: x.url, title: displayTitle(x), artist: x.artist || '', key: x.hash }));
  let idx = items.findIndex(x => x.key === f.hash);
  if (idx < 0) { items.length = 0; items.push({ url: f.url, title: displayTitle(f), artist: f.artist || '', key: f.hash }); idx = 0; }
  playCtx = { items, index: idx };
  playAt(idx);
}
function playAt(i) {
  if (!playCtx || i < 0 || i >= playCtx.items.length) return;
  playCtx.index = i;
  const it = playCtx.items[i];
  const url = /^https?:/.test(it.url) ? it.url : `${API}${it.url}`;
  player.load({ url, title: it.title, artist: it.artist });
  fileList?.setPlaying(it.key);
}

// ── Fetch helpers ─────────────────────────────────────────────────────────────
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

// runBulk applies a request per hash sequentially and tallies; never throws.
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

// ── Bulk permanent-delete confirm modal (kept in trash.html) ─────────────────
const delModal   = document.getElementById('trashDeleteModal');
const delBody     = document.getElementById('trashDeleteBody');
const delError    = document.getElementById('trashDeleteError');
const delConfirm  = document.getElementById('trashDeleteConfirm');
const delCancel   = document.getElementById('trashDeleteCancel');
const delClose    = document.getElementById('trashDeleteClose');

// confirmBulkDelete resolves true on confirm, false on any dismiss.
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

// ── The Trash scope ───────────────────────────────────────────────────────────
const scope = {
  title: 'Trash',
  desc: 'Files moved to trash are hidden from the library but their blobs remain on disk. '
      + 'Fix a tag before restoring, restore to bring a file back, or delete forever to remove it permanently.',
  emptyText: 'Trash is empty.',
  columns: ['check', 'title', 'artist', 'album', 'size', 'meta', 'actions'],
  metaLabel: 'Deleted',
  metaValue: f => fmtDate(f.deleted_at),
  // A non-approved file restores into the moderation queue, not the library.
  badge: f => (f.review_state && f.review_state !== 'approved')
    ? { text: 'pending review', cls: 'is-' + f.review_state, title: 'Restore returns this file to the moderation queue' }
    : null,
  accessEditable: false,              // access endpoints reject trashed rows; tags only
  licenses: [],

  load: loadTrash,
  selectable: () => true,

  // Edit (tags) — works on trashed rows; corrected tags decide where the file
  // lands when it returns to the library.
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
        if (!await confirmBulkDelete(hashes.length)) return false;   // keep selection on cancel
        const { ok, fail, authFailed } = await runBulk(hashes, h => fetch(`${API}/api/admin/trash/${encodeURIComponent(h)}`, { method: 'DELETE' }));
        if (authFailed) { if (ok) toast(`Deleted ${ok} before the session expired.`, 'error'); return; }
        if (fail) toast(`Permanently deleted ${ok}; ${fail} failed.`, 'error');
        else if (ok) toast(`Permanently deleted ${ok} ${ok === 1 ? 'file' : 'files'}.`, 'success');
      },
    },
  ],

  // Bulk tag edit (no access on trashed rows) — loop the per-file metadata PATCH.
  bulkApply: async (hashes, patch) => {
    const { ok, fail } = await runBulk(hashes, h => fetch(`${API}/api/files/${encodeURIComponent(h)}/metadata`, {
      method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(patch),
    }));
    if (fail) throw new Error(`updated ${ok}, ${fail} failed`);
  },

  onPlay: playFile,
  toast, handleAuthError,
};

// ── Boot ────────────────────────────────────────────────────────────────────
(async function boot() {
  const identity = await bootAdmin({ require: 'file.delete' });
  if (!identity) return;
  fileList = createFileList(scope);
  fileList.mount(document.getElementById('fileList'));
})();
