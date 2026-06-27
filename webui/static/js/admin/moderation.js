// Admin · Library — Review scope (moderation queue). A factory over the shared
// file-management component, mounted by library.js into the Library page's
// Review panel. Staged uploads grouped by uploader; approve / return-with-note /
// discard, selection limited to submitted rows. The shared preview player is
// injected as `play`. Available when the caller has content.moderate; Edit
// needs metadata.edit.
//
// Design: docs/architecture/file-management-view.md.
import { API, fmtDate, toast, handleAuthError } from './shared.js';
import { createFileList } from '../file-list.js';

const ACTIONABLE  = new Set(['submitted', 'returned']);
const STATE_LABEL = { submitted: 'Awaiting review', returned: 'Returned', draft: 'Draft' };
const displayTitle = f => f.title || f.filename || 'this file';

export function createReviewScope({ play, perms }) {
  const canEdit = perms.includes('metadata.edit');
  let fileList = null;

  // ── Fetch helpers ───────────────────────────────────────────────────────────
  // loadModerationPage backs the paged component: one server page of the queue,
  // filtered + sorted, as {total, selectable_total, items}. selectable_total is the
  // submitted (actionable) count, so the "Select all N matching" banner reflects
  // only the rows the bulk actions touch (file-list-scaling.md).
  async function loadModerationPage({ limit, offset, q, field, sort }) {
    const params = new URLSearchParams({ limit: String(limit), offset: String(offset) });
    if (sort) params.set('sort', sort);
    if (q) params.set('q', q);
    if (field) params.set('field', field);
    const res = await fetch(`${API}/api/admin/moderation?${params.toString()}`);
    if (handleAuthError(res)) throw new Error('Your session expired.');
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const data = await res.json();
    return { total: data.total || 0, selectable_total: data.selectable_total, items: data.items || [] };
  }

  // moderationBulkCall is the single batched action over an explicit hash list OR
  // a filter ("select all N matching"). Returns the count actually affected.
  async function moderationBulkCall(body) {
    const res = await fetch(`${API}/api/admin/moderation/bulk`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
    });
    if (handleAuthError(res)) throw new Error('Your session expired.');
    const data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
    return data.affected || 0;
  }
  const filterBody = filter => ({ filter: { q: filter.q, field: filter.field }, all: !filter.q });

  function toastApproved(n) { if (n) toast(`Approved ${n} ${n === 1 ? 'file' : 'files'} into the library.`, 'success'); }
  function toastReturned(n) { if (n) toast(`Returned ${n} ${n === 1 ? 'file' : 'files'} to the uploader.`, 'success'); }
  function toastDiscarded(n) { if (n) toast(`Discarded ${n} ${n === 1 ? 'file' : 'files'} to Trash.`, 'success'); }

  const approveHashes = hashes => moderationBulkCall({ action: 'approve', hashes });
  const approveAll = filter => moderationBulkCall({ action: 'approve', ...filterBody(filter) });
  const returnHashes = (hashes, note) => moderationBulkCall({ action: 'return', hashes, note });
  const returnAll = (filter, note) => moderationBulkCall({ action: 'return', ...filterBody(filter), note });
  const discardHashes = hashes => moderationBulkCall({ action: 'discard', hashes });
  const discardAll = filter => moderationBulkCall({ action: 'discard', ...filterBody(filter) });

  // Explicit-selection metadata edit (per-hash PATCH); there is no edit-by-filter
  // for staged files, so the component disables "Edit tags…" in select-all mode.
  async function moderationBulkPatch(hashes, patch) {
    let ok = 0, fail = 0;
    for (const h of hashes) {
      try {
        const res = await fetch(`${API}/api/files/${encodeURIComponent(h)}/metadata`, {
          method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(patch),
        });
        const data = await res.json().catch(() => ({}));
        if (res.ok && data.ok) ok++; else fail++;
      } catch { fail++; }
    }
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

  // promptReturnNote shows the return modal and resolves the trimmed note, or null
  // if cancelled. The caller does the actual return (one batched request).
  function promptReturnNote(bodyText) {
    return new Promise(resolve => {
      returnBody.textContent = bodyText;
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
      const onSubmit = e => {
        e.preventDefault();
        const note = returnNote.value.trim();
        if (!note) { returnError.textContent = 'A note is required.'; returnError.hidden = false; return; }
        returnModal.classList.add('hidden');
        cleanup();
        resolve(note);
      };
      const onCancel   = () => { returnModal.classList.add('hidden'); cleanup(); resolve(null); };
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

  // confirmDiscard shows the bulk discard confirm; resolves true/false. The caller
  // does the actual discard (one batched request).
  function confirmDiscard(bodyText, confirmLabel) {
    return new Promise(resolve => {
      discardBody.textContent = bodyText;
      discardConfirm.textContent = confirmLabel;
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
      const onOk       = () => { discardModal.classList.add('hidden'); cleanup(); resolve(true); };
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
    allowCoverAdd: canEdit,   // grouped "Add cover" on coverless artist/album separators
    allowCoverEdit: canEdit,  // "Edit cover" on separators that already have one
    apiBase: API,
    metaLabel: 'Submitted',
    metaValue: f => (f.submitted_at ? fmtDate(f.submitted_at) : ''),
    // A duplicate-flagged row (recordings P3) is marked so the moderator looks
    // before publishing; full side-by-side tech compare lives on /admin/duplicates.
    badge: f => f.duplicate
      ? { text: (STATE_LABEL[f.state] || f.state) + ' · possible duplicate', cls: 'is-' + f.state + ' is-duplicate' }
      : { text: STATE_LABEL[f.state] || f.state, cls: 'is-' + f.state },
    accessEditable: false,
    // Server-paged: the queue can be large, so it loads pages by infinite scroll
    // (file-list-scaling.md). The by-uploader grouping streams non-collapsible
    // uploader separators as pages arrive (sort=uploader).
    paged: true,
    pageSize: 100,
    loadPage: loadModerationPage,
    grouping: {
      kind: 'collapsible',
      groupSort: 'uploader',
      by: f => f.uploader_id || 0,
      label: f => f.uploader || '(unknown uploader)',
      counts: groupCounts,
    },
    selectable: f => f.state === 'submitted',
    editable: f => canEdit && ACTIONABLE.has(f.state),
    editPatchURL: f => `${API}/api/files/${encodeURIComponent(f.hash)}/metadata`,
    editDetailURL: f => `${API}/api/files/${encodeURIComponent(f.hash)}/metadata`,
    editNote: 'Edits this submission’s tags before approval.',
    rowActions: [
      { id: 'approve', label: 'Approve', kind: 'neutral', show: f => ACTIONABLE.has(f.state), run: async f => { toastApproved(await approveHashes([f.hash])); } },
      {
        id: 'return', label: 'Return…', kind: 'neutral', show: f => ACTIONABLE.has(f.state),
        run: async f => {
          const note = await promptReturnNote('Send this file back to its uploader to fix?');
          if (note == null) return false;
          toastReturned(await returnHashes([f.hash], note));
        },
      },
      {
        id: 'discard', label: 'Discard', kind: 'danger',
        confirm: 'inline', confirmPrompt: 'Discard to Trash?', confirmLabel: 'Discard',
        show: f => ACTIONABLE.has(f.state),
        run: async f => { toastDiscarded(await discardHashes([f.hash])); },
      },
    ],
    bulkActions: [
      {
        id: 'approve', label: 'Approve selected', kind: 'neutral',
        run: async hashes => { toastApproved(await approveHashes(hashes)); },
        runAll: async filter => { toastApproved(await approveAll(filter)); },
      },
      {
        id: 'return', label: 'Return selected…', kind: 'neutral',
        run: async hashes => {
          const note = await promptReturnNote(`Send ${hashes.length} files back to their uploader with one note?`);
          if (note == null) return false;
          toastReturned(await returnHashes(hashes, note));
        },
        runAll: async filter => {
          const note = await promptReturnNote('Send all matching files back to their uploaders with one note?');
          if (note == null) return false;
          toastReturned(await returnAll(filter, note));
        },
      },
      {
        id: 'discard', label: 'Discard selected', kind: 'danger',
        run: async hashes => {
          if (!await confirmDiscard(`Discard ${hashes.length} ${hashes.length === 1 ? 'file' : 'files'} to Trash?`, `Discard ${hashes.length}`)) return false;
          toastDiscarded(await discardHashes(hashes));
        },
        runAll: async filter => {
          if (!await confirmDiscard('Discard all matching files to Trash?', 'Discard all')) return false;
          toastDiscarded(await discardAll(filter));
        },
      },
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
