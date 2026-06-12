// Admin · Library — Review scope (moderation queue). A factory over the shared
// file-management component, mounted by library.js into the Library page's
// Review panel. Staged uploads grouped by uploader; approve / return-with-note /
// discard, selection limited to submitted rows. The shared preview player is
// injected as `play`. Available when the caller has content.moderate; Edit
// needs metadata.edit.
//
// Design: docs/plans/file-management-view.md.
import { API, fmtDate, toast, handleAuthError } from './shared.js';
import { createFileList } from '../file-list.js';

const ACTIONABLE  = new Set(['submitted', 'returned']);
const STATE_LABEL = { submitted: 'Awaiting review', returned: 'Returned', draft: 'Draft' };
const displayTitle = f => f.title || f.filename || 'this file';

export function createReviewScope({ play, perms }) {
  const canEdit = perms.includes('metadata.edit');
  let fileList = null;

  // ── Fetch helpers ───────────────────────────────────────────────────────────
  async function loadQueue() {
    const res = await fetch(`${API}/api/admin/moderation`);
    if (handleAuthError(res)) throw new Error('Your session expired.');
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    return await res.json();
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
  async function approveMany(hashes) {
    const { ok, fail, authFailed } = await runBulk(hashes, h => fetch(`${API}/api/admin/moderation/${encodeURIComponent(h)}/approve`, { method: 'POST' }));
    if (authFailed) { if (ok) toast(`Approved ${ok} before the session expired.`, 'error'); return; }
    if (fail) toast(`Approved ${ok}; ${fail} failed.`, 'error');
    else if (ok) toast(`Approved ${ok} ${ok === 1 ? 'file' : 'files'} into the library.`, 'success');
  }
  async function discardMany(hashes) {
    const { ok, fail, authFailed } = await runBulk(hashes, h => fetch(`${API}/api/admin/files/${encodeURIComponent(h)}`, { method: 'DELETE' }));
    if (authFailed) { if (ok) toast(`Discarded ${ok} before the session expired.`, 'error'); return; }
    if (fail) toast(`Discarded ${ok} to Trash; ${fail} failed.`, 'error');
    else if (ok) toast(`Discarded ${ok} ${ok === 1 ? 'file' : 'files'} to Trash.`, 'success');
  }
  async function returnMany(hashes, note) {
    const { ok, fail, authFailed } = await runBulk(hashes, h => fetch(`${API}/api/admin/moderation/${encodeURIComponent(h)}/return`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ note }),
    }));
    if (authFailed) { if (ok) toast(`Returned ${ok} before the session expired.`, 'error'); return; }
    if (fail) toast(`Returned ${ok}; ${fail} failed.`, 'error');
    else if (ok) toast(`Returned ${ok} ${ok === 1 ? 'file' : 'files'} to the uploader.`, 'success');
  }
  async function moderationBulkPatch(hashes, patch) {
    const { ok, fail } = await runBulk(hashes, h => fetch(`${API}/api/files/${encodeURIComponent(h)}/metadata`, {
      method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(patch),
    }));
    if (fail) throw new Error(`updated ${ok}, ${fail} failed`);
  }

  // ── Return-with-note modal (in library.html) ────────────────────────────────
  const returnModal   = document.getElementById('returnModal');
  const returnForm    = document.getElementById('returnForm');
  const returnBody    = document.getElementById('returnBody');
  const returnNote    = document.getElementById('returnNote');
  const returnError   = document.getElementById('returnError');
  const returnConfirm = document.getElementById('returnConfirm');
  const returnCancel  = document.getElementById('returnCancel');
  const returnClose   = document.getElementById('returnClose');

  function returnFlow(hashes) {
    return new Promise(resolve => {
      if (!hashes.length) { resolve(false); return; }
      returnBody.textContent = hashes.length === 1
        ? 'Send this file back to its uploader to fix?'
        : `Send ${hashes.length} files back to their uploader with one note?`;
      returnNote.value = '';
      returnError.hidden = true; returnError.textContent = '';
      returnModal.classList.remove('hidden');
      returnNote.focus();
      const cleanup = () => {
        returnForm.removeEventListener('submit', onSubmit);
        returnCancel.removeEventListener('click', onCancel);
        returnClose.removeEventListener('click', onCancel);
        returnModal.removeEventListener('click', onBackdrop);
        document.removeEventListener('keydown', onKey);
      };
      const onSubmit = async e => {
        e.preventDefault();
        const note = returnNote.value.trim();
        if (!note) { returnError.textContent = 'A note is required.'; returnError.hidden = false; return; }
        returnConfirm.disabled = true;
        returnModal.classList.add('hidden');
        await returnMany(hashes, note);
        returnConfirm.disabled = false;
        cleanup();
        resolve(true);
      };
      const onCancel   = () => { returnModal.classList.add('hidden'); cleanup(); resolve(false); };
      const onBackdrop = e => { if (e.target === returnModal) onCancel(); };
      const onKey      = e => { if (e.key === 'Escape' && !returnModal.classList.contains('hidden')) onCancel(); };
      returnForm.addEventListener('submit', onSubmit);
      returnCancel.addEventListener('click', onCancel);
      returnClose.addEventListener('click', onCancel);
      returnModal.addEventListener('click', onBackdrop);
      document.addEventListener('keydown', onKey);
    });
  }

  // ── Bulk discard confirm modal (in library.html) ────────────────────────────
  const discardModal   = document.getElementById('discardModal');
  const discardBody    = document.getElementById('discardBody');
  const discardError   = document.getElementById('discardError');
  const discardConfirm = document.getElementById('discardConfirm');
  const discardCancel  = document.getElementById('discardCancel');
  const discardClose   = document.getElementById('discardClose');

  function discardFlow(hashes) {
    return new Promise(resolve => {
      if (!hashes.length) { resolve(false); return; }
      discardBody.textContent = `Discard ${hashes.length} ${hashes.length === 1 ? 'file' : 'files'} to Trash?`;
      discardConfirm.textContent = `Discard ${hashes.length}`;
      discardError.hidden = true; discardError.textContent = '';
      discardModal.classList.remove('hidden');
      discardConfirm.focus();
      const cleanup = () => {
        discardConfirm.removeEventListener('click', onOk);
        discardCancel.removeEventListener('click', onCancel);
        discardClose.removeEventListener('click', onCancel);
        discardModal.removeEventListener('click', onBackdrop);
        document.removeEventListener('keydown', onKey);
      };
      const onOk = async () => { discardModal.classList.add('hidden'); cleanup(); await discardMany(hashes); resolve(true); };
      const onCancel   = () => { discardModal.classList.add('hidden'); cleanup(); resolve(false); };
      const onBackdrop = e => { if (e.target === discardModal) onCancel(); };
      const onKey      = e => { if (e.key === 'Escape' && !discardModal.classList.contains('hidden')) onCancel(); };
      discardConfirm.addEventListener('click', onOk);
      discardCancel.addEventListener('click', onCancel);
      discardClose.addEventListener('click', onCancel);
      discardModal.addEventListener('click', onBackdrop);
      document.addEventListener('keydown', onKey);
    });
  }

  function groupCounts(items) {
    const n = s => items.filter(f => f.state === s).length;
    const parts = [];
    if (n('submitted')) parts.push(`${n('submitted')} awaiting`);
    if (n('returned'))  parts.push(`${n('returned')} returned`);
    if (n('draft'))     parts.push(`${n('draft')} draft${n('draft') === 1 ? '' : 's'}`);
    return parts.join(' · ');
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
    title: 'Review queue',
    desc: 'Uploads staged for review, grouped by uploader. Fix tags before approving, approve to '
        + 'publish into the library, return with a note so the uploader can fix the metadata, or '
        + 'discard to Trash. Selection and the bulk actions cover files awaiting review; returned '
        + 'files act per row; drafts are shown for awareness only.',
    emptyText: 'Nothing awaiting review',
    columns: ['check', 'title', 'artist', 'album', 'size', 'meta', 'actions'],
    artistAlbumSort: true,
    allowCoverAdd: canEdit,  // grouped "Add cover" on coverless artist/album separators
    apiBase: API,
    metaLabel: 'Submitted',
    metaValue: f => (f.submitted_at ? fmtDate(f.submitted_at) : ''),
    badge: f => ({ text: STATE_LABEL[f.state] || f.state, cls: 'is-' + f.state }),
    accessEditable: false,
    load: loadQueue,
    grouping: {
      kind: 'collapsible',
      by: f => f.uploader_id || 0,
      label: f => f.uploader || '(unknown uploader)',
      counts: groupCounts,
    },
    selectable: f => f.state === 'submitted',
    editable: f => canEdit && ACTIONABLE.has(f.state),
    editPatchURL: f => `${API}/api/files/${encodeURIComponent(f.hash)}/metadata`,
    editNote: 'Edits this submission’s tags before approval.',
    rowActions: [
      { id: 'approve', label: 'Approve', kind: 'neutral', show: f => ACTIONABLE.has(f.state), run: f => approveMany([f.hash]) },
      { id: 'return',  label: 'Return…', kind: 'neutral', show: f => ACTIONABLE.has(f.state), run: f => returnFlow([f.hash]) },
      {
        id: 'discard', label: 'Discard', kind: 'danger',
        confirm: 'inline', confirmPrompt: 'Discard to Trash?', confirmLabel: 'Discard',
        show: f => ACTIONABLE.has(f.state),
        run: f => discardMany([f.hash]),
      },
    ],
    bulkActions: [
      { id: 'approve', label: 'Approve selected', kind: 'neutral', run: hashes => approveMany(hashes) },
      { id: 'return',  label: 'Return selected…', kind: 'neutral', run: hashes => returnFlow(hashes) },
      { id: 'discard', label: 'Discard selected', kind: 'danger',  run: hashes => discardFlow(hashes) },
    ],
    bulkApply: canEdit ? moderationBulkPatch : undefined,
    onPlay: playFile,
    toast, handleAuthError,
  };

  fileList = createFileList(scope);

  return {
    id: 'review',
    label: 'Review',
    available: perms.includes('content.moderate'),
    mount: () => fileList.mount(document.getElementById('fileListReview')),
    reload: () => fileList.reload(),
  };
}
