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

// Classification chip (recording-tagsets P4): the case the queue derived for a
// submission. A collision (offered appearance already exists) trumps the case
// label — there is nothing new to add.
const CLASS_CHIP = {
  new_recording:  { text: 'new recording',  cls: 'cls-new' },
  new_appearance: { text: 'new appearance', cls: 'cls-appear' },
  no_new_bytes:   { text: 'no new bytes',   cls: 'cls-bytes' },
};
function classChip(f) {
  if (f.collides) return { text: 'nothing new', cls: 'cls-warn', title: 'The offered appearance already exists on this recording' };
  return CLASS_CHIP[f.class] || null;
}
// A case-B submission (new appearance backed by a new blob) is the one where the
// moderator can keep the appearance but drop the blob (absorb at the gate).
const canDropBlob = f => f.class === 'new_appearance' && !f.collides;

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

  // moderationBulkCall is the single batched action over an explicit tagset-id
  // list OR a filter ("select all N matching"). Returns the count affected.
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
  const toIDs = keys => keys.map(Number);   // selection keys are tagset-id strings

  // Per-row actions hit the single-appearance endpoints (approve carries the
  // moderator's per-piece decisions: drop_bytes / force_new).
  async function callOne(tid, action, body) {
    const res = await fetch(`${API}/api/admin/moderation/${tid}/${action}`, {
      method: 'POST', headers: body ? { 'Content-Type': 'application/json' } : {},
      body: body ? JSON.stringify(body) : undefined,
    });
    if (handleAuthError(res)) throw new Error('Your session expired.');
    const data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
    return 1;
  }
  const approveOne = (tid, opts) => callOne(tid, 'approve', opts || {});
  const discardOne = tid => callOne(tid, 'discard');

  function toastApproved(n, drop) {
    if (n) toast(drop ? `Published ${n === 1 ? 'the appearance' : n + ' appearances'}, dropped the blob${n === 1 ? '' : 's'}.`
                      : `Approved ${n} ${n === 1 ? 'appearance' : 'appearances'} into the library.`, 'success');
  }
  function toastReturned(n) { if (n) toast(`Returned ${n} ${n === 1 ? 'submission' : 'submissions'} to the uploader.`, 'success'); }
  function toastDiscarded(n) { if (n) toast(`Discarded ${n} ${n === 1 ? 'appearance' : 'appearances'} to Trash.`, 'success'); }

  const approveIDs = keys => moderationBulkCall({ action: 'approve', tagset_ids: toIDs(keys) });
  const approveAll = filter => moderationBulkCall({ action: 'approve', ...filterBody(filter) });
  const returnIDs = (keys, note) => moderationBulkCall({ action: 'return', tagset_ids: toIDs(keys), note });
  const returnAll = (filter, note) => moderationBulkCall({ action: 'return', ...filterBody(filter), note });
  const discardIDs = keys => moderationBulkCall({ action: 'discard', tagset_ids: toIDs(keys) });
  const discardAll = filter => moderationBulkCall({ action: 'discard', ...filterBody(filter) });

  // Explicit-selection metadata edit (per-appearance PATCH); there is no
  // edit-by-filter for staged appearances, so the component disables "Edit
  // tags…" in select-all mode.
  async function moderationBulkPatch(keys, patch) {
    let ok = 0, fail = 0;
    for (const tid of keys) {
      try {
        const res = await fetch(`${API}/api/admin/moderation/${tid}/metadata`, {
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
    const items = (visible || []).map(x => ({ url: x.url, title: displayTitle(x), artist: x.artist || '', key: String(x.tagset_id) }));
    let idx = items.findIndex(x => x.key === String(f.tagset_id));
    if (idx < 0) { items.length = 0; items.push({ url: f.url, title: displayTitle(f), artist: f.artist || '', key: String(f.tagset_id) }); idx = 0; }
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
    // rows are appearances, keyed by tagset id (a byte-dup makes two rows share
    // one blob hash — recording-tagsets P4).
    rowKey: f => String(f.tagset_id),
    // State badge + the classification chip the moderator acts on.
    badge: f => {
      const state = { text: STATE_LABEL[f.state] || f.state, cls: 'is-' + f.state };
      const chip = classChip(f);
      return chip ? [state, chip] : [state];
    },
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
    editPatchURL: f => `${API}/api/admin/moderation/${f.tagset_id}/metadata`,
    editDetailURL: f => `${API}/api/admin/moderation/${f.tagset_id}/metadata`,
    editNote: 'Edits this appearance’s tags before approval.',
    rowActions: [
      { id: 'approve', label: 'Approve', kind: 'neutral', show: f => ACTIONABLE.has(f.state),
        run: async f => { toastApproved(await approveOne(f.tagset_id), false); } },
      // Case B: keep the appearance, drop the submitted blob (absorb at the gate).
      { id: 'approve-drop', label: 'Approve · drop blob', kind: 'neutral', show: f => ACTIONABLE.has(f.state) && canDropBlob(f),
        run: async f => { toastApproved(await approveOne(f.tagset_id, { drop_bytes: true }), true); } },
      {
        id: 'return', label: 'Return…', kind: 'neutral', show: f => ACTIONABLE.has(f.state),
        run: async f => {
          const note = await promptReturnNote('Send this submission back to its uploader to fix?');
          if (note == null) return false;
          toastReturned(await returnIDs([f.tagset_id], note));
        },
      },
      {
        id: 'discard', label: 'Discard', kind: 'danger',
        confirm: 'inline', confirmPrompt: 'Discard to Trash?', confirmLabel: 'Discard',
        show: f => ACTIONABLE.has(f.state),
        run: async f => { toastDiscarded(await discardOne(f.tagset_id)); },
      },
    ],
    bulkActions: [
      {
        id: 'approve', label: 'Approve selected', kind: 'neutral',
        run: async keys => { toastApproved(await approveIDs(keys), false); },
        runAll: async filter => { toastApproved(await approveAll(filter), false); },
      },
      {
        id: 'return', label: 'Return selected…', kind: 'neutral',
        run: async keys => {
          const note = await promptReturnNote(`Send ${keys.length} submissions back to their uploader with one note?`);
          if (note == null) return false;
          toastReturned(await returnIDs(keys, note));
        },
        runAll: async filter => {
          const note = await promptReturnNote('Send all matching submissions back to their uploaders with one note?');
          if (note == null) return false;
          toastReturned(await returnAll(filter, note));
        },
      },
      {
        id: 'discard', label: 'Discard selected', kind: 'danger',
        run: async keys => {
          if (!await confirmDiscard(`Discard ${keys.length} ${keys.length === 1 ? 'appearance' : 'appearances'} to Trash?`, `Discard ${keys.length}`)) return false;
          toastDiscarded(await discardIDs(keys));
        },
        runAll: async filter => {
          if (!await confirmDiscard('Discard all matching appearances to Trash?', 'Discard all')) return false;
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
