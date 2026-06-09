// Admin · Trash — restore or permanently delete trashed files, individually or
// in bulk (checkbox selection + restore/delete selected/all). Requires
// file.delete. Bulk ops loop the per-file endpoints client-side.
import { bootAdmin, API, fmtBytes, fmtDate, shortHash, toast, handleAuthError, el } from './shared.js';

const trashBody    = document.getElementById('trashBody');
const trashCountEl = document.getElementById('trashCount');

// Toolbar
const trashToolbar   = document.getElementById('trashToolbar');
const trashSelCount  = document.getElementById('trashSelCount');
const trashSelectAll = document.getElementById('trashSelectAll');
const btnRestoreSel  = document.getElementById('restoreSelected');
const btnDeleteSel   = document.getElementById('deleteSelected');
const btnRestoreAll  = document.getElementById('restoreAll');
const btnDeleteAll   = document.getElementById('deleteAll');

let allTrash = [];   // last loaded list (keeps "all" actions + count authoritative)

const displayTitle = f => f.title || f.filename || 'this file';
const stateRow = text => el('tr', { class: 'table-state-row' }, [el('td', { colspan: '7', text })]);

async function loadTrash() {
  trashBody.setAttribute('aria-busy', 'true');
  trashBody.replaceChildren(stateRow('Loading…'));

  let items;
  try {
    const res = await fetch(`${API}/api/admin/trash`);
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    items = await res.json();
  } catch (err) {
    trashBody.setAttribute('aria-busy', 'false');
    trashBody.replaceChildren(stateRow(`Failed to load trash: ${err.message}`));
    return;
  }

  trashBody.setAttribute('aria-busy', 'false');
  allTrash = Array.isArray(items) ? items : [];
  trashCountEl.textContent = String(allTrash.length);

  if (allTrash.length === 0) {
    trashBody.replaceChildren(stateRow('Trash is empty.'));
    updateToolbar();
    return;
  }

  const frag = document.createDocumentFragment();
  allTrash.forEach(f => frag.appendChild(buildTrashRow(f)));
  trashBody.replaceChildren(frag);
  updateToolbar();
}

function buildTrashRow(f) {
  const tr = el('tr', { 'data-hash': f.hash });

  const check = el('input', { type: 'checkbox', class: 'trash-check', 'aria-label': `Select ${displayTitle(f)}` });
  check.addEventListener('change', updateToolbar);
  const tdCheck = el('td', { class: 'cell-check' }, [check]);

  const titleSpan = f.title
    ? el('span', { class: 'cell-title', text: f.title })
    : el('span', { class: 'cell-title is-fallback', text: f.filename || 'Untitled' });
  const hashSpan = el('span', { class: 'cell-hash', title: f.hash || '', text: shortHash(f.hash) });
  const tdTitle = el('td', { class: 'cell-title-td', 'data-label': 'Title' }, [titleSpan, hashSpan]);

  const tdArtist = f.artist
    ? el('td', { 'data-label': 'Artist', text: f.artist })
    : el('td', { class: 'cell-muted', 'data-label': 'Artist', text: '—' });
  const tdAlbum = f.album
    ? el('td', { 'data-label': 'Album', text: f.album })
    : el('td', { class: 'cell-muted', 'data-label': 'Album', text: '—' });

  const tdSize = el('td', { class: 'cell-size', 'data-label': 'Size', text: fmtBytes(f.byte_size) });
  const tdDate = el('td', { 'data-label': 'Deleted', text: fmtDate(f.deleted_at) });
  const wrap = el('div', { class: 'trash-actions' }, buildTrashActions(tr, f));
  const tdActions = el('td', { class: 'cell-actions', 'data-label': 'Actions' }, [wrap]);

  tr.append(tdCheck, tdTitle, tdArtist, tdAlbum, tdSize, tdDate, tdActions);
  return tr;
}

// buildTrashActions returns the [Restore, Delete forever] buttons; the caller's
// .trash-actions wrapper is passed back to the handlers for in-place swapping.
function buildTrashActions(tr, f) {
  return [
    el('button', { class: 'btn btn-neutral btn-sm', text: 'Restore', onclick: e => doTrashRestore(tr, f, e.currentTarget.parentElement) }),
    el('button', { class: 'btn btn-destructive-outline btn-sm', text: 'Delete forever', onclick: e => enterTrashDeleteConfirm(tr, f, e.currentTarget.parentElement) }),
  ];
}

async function doTrashRestore(tr, f, wrap) {
  wrap.querySelectorAll('button').forEach(b => (b.disabled = true));
  try {
    const res = await fetch(`${API}/api/admin/trash/${encodeURIComponent(f.hash)}/restore`, { method: 'POST' });
    if (handleAuthError(res)) { wrap.querySelectorAll('button').forEach(b => (b.disabled = false)); return; }
    const data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
  } catch (err) {
    wrap.querySelectorAll('button').forEach(b => (b.disabled = false));
    toast(`Couldn't restore "${displayTitle(f)}": ${err.message}`, 'error');
    return;
  }
  removeRow(tr);
  toast(`"${displayTitle(f)}" restored to library.`, 'success');
}

function enterTrashDeleteConfirm(tr, f, actionsWrap) {
  const restore = () => {
    actionsWrap.replaceChildren(...buildTrashActions(tr, f));
    actionsWrap.querySelector('button')?.focus();
  };
  const cancel  = el('button', { class: 'btn btn-neutral btn-sm', text: 'Cancel', onclick: restore });
  const confirm = el('button', { class: 'btn btn-destructive-solid btn-sm', text: 'Delete forever' });
  confirm.addEventListener('click', () => doTrashHardDelete(tr, f, actionsWrap));

  actionsWrap.replaceChildren(
    el('span', { class: 'delete-confirm-label', text: 'Delete forever?' }),
    cancel, confirm,
  );
  actionsWrap.addEventListener('keydown', e => { if (e.key === 'Escape') { e.stopPropagation(); restore(); } });
  cancel.focus();
}

async function doTrashHardDelete(tr, f, wrap) {
  tr.setAttribute('aria-busy', 'true');
  wrap.querySelectorAll('button').forEach(b => (b.disabled = true));
  try {
    const res = await fetch(`${API}/api/admin/trash/${encodeURIComponent(f.hash)}`, { method: 'DELETE' });
    if (handleAuthError(res)) { tr.removeAttribute('aria-busy'); return; }
    const data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
  } catch (err) {
    tr.removeAttribute('aria-busy');
    wrap.replaceChildren(...buildTrashActions(tr, f));
    toast(`Couldn't delete "${displayTitle(f)}": ${err.message}`, 'error');
    return;
  }
  removeRow(tr);
  toast(`"${displayTitle(f)}" permanently deleted.`, 'success');
}

function removeRow(tr) {
  const hash = tr.dataset.hash;
  tr.classList.add('row-removing');
  const finish = () => {
    tr.remove();
    allTrash = allTrash.filter(f => f.hash !== hash);
    trashCountEl.textContent = String(allTrash.length);
    if (!allTrash.length) trashBody.replaceChildren(stateRow('Trash is empty.'));
    updateToolbar();
  };
  tr.addEventListener('animationend', finish, { once: true });
  setTimeout(() => { if (tr.isConnected) finish(); }, 220);
}

// ── Selection + bulk actions ─────────────────────────────────────────────────
function rowChecks() { return [...trashBody.querySelectorAll('input.trash-check')]; }
function selectedHashes() {
  return rowChecks().filter(c => c.checked).map(c => c.closest('tr').dataset.hash);
}

function updateToolbar() {
  const checks = rowChecks();
  const total = checks.length;
  const sel = checks.filter(c => c.checked).length;

  trashToolbar.hidden = total === 0;
  trashSelCount.textContent = `${sel} selected`;
  btnRestoreSel.disabled = sel === 0;
  btnDeleteSel.disabled  = sel === 0;
  btnRestoreAll.disabled = total === 0;
  btnDeleteAll.disabled  = total === 0;

  trashSelectAll.checked = total > 0 && sel === total;
  trashSelectAll.indeterminate = sel > 0 && sel < total;
}

function setBusy(busy) {
  [btnRestoreSel, btnDeleteSel, btnRestoreAll, btnDeleteAll, trashSelectAll].forEach(b => (b.disabled = busy));
  rowChecks().forEach(c => (c.disabled = busy));
  trashBody.querySelectorAll('.trash-actions button').forEach(b => (b.disabled = busy));
}

trashSelectAll.addEventListener('change', () => {
  rowChecks().forEach(c => (c.checked = trashSelectAll.checked));
  updateToolbar();
});

btnRestoreSel.addEventListener('click', () => bulkRestore(selectedHashes()));
btnRestoreAll.addEventListener('click', () => bulkRestore(allTrash.map(f => f.hash)));
btnDeleteSel.addEventListener('click', () => confirmBulkDelete(selectedHashes()));
btnDeleteAll.addEventListener('click', () => confirmBulkDelete(allTrash.map(f => f.hash)));

// runBulk applies an action over hashes sequentially and returns the tally. It
// never throws — callers own the user-facing summary.
async function runBulk(hashes, makeRequest) {
  let ok = 0, fail = 0, authFailed = false;
  for (const hash of hashes) {
    let res;
    try { res = await makeRequest(hash); }
    catch { fail++; continue; }
    if (res.status === 401) { handleAuthError(res); authFailed = true; break; }
    const data = await res.json().catch(() => ({}));
    if (res.ok && data.ok) ok++; else fail++;
  }
  return { ok, fail, authFailed };
}

async function bulkRestore(hashes) {
  if (!hashes.length) return;
  setBusy(true);
  const { ok, fail, authFailed } = await runBulk(hashes, hash =>
    fetch(`${API}/api/admin/trash/${encodeURIComponent(hash)}/restore`, { method: 'POST' }));
  await loadTrash();
  if (authFailed) { if (ok) toast(`Restored ${ok} before the session expired.`, 'error'); return; }
  if (fail) toast(`Restored ${ok} to the library; ${fail} failed.`, 'error');
  else if (ok) toast(`Restored ${ok} ${ok === 1 ? 'file' : 'files'} to the library.`, 'success');
}

async function bulkDelete(hashes) {
  if (!hashes.length) return;
  setBusy(true);
  const { ok, fail, authFailed } = await runBulk(hashes, hash =>
    fetch(`${API}/api/admin/trash/${encodeURIComponent(hash)}`, { method: 'DELETE' }));
  await loadTrash();
  if (authFailed) { if (ok) toast(`Deleted ${ok} before the session expired.`, 'error'); return; }
  if (fail) toast(`Permanently deleted ${ok}; ${fail} failed.`, 'error');
  else if (ok) toast(`Permanently deleted ${ok} ${ok === 1 ? 'file' : 'files'}.`, 'success');
}

// ── Bulk permanent-delete confirm modal ──────────────────────────────────────
const delModal   = document.getElementById('trashDeleteModal');
const delBody     = document.getElementById('trashDeleteBody');
const delError    = document.getElementById('trashDeleteError');
const delConfirm  = document.getElementById('trashDeleteConfirm');
let delHashes = null;

function confirmBulkDelete(hashes) {
  if (!hashes.length) return;
  delHashes = hashes;
  delBody.textContent = `Permanently delete ${hashes.length} ${hashes.length === 1 ? 'file' : 'files'}?`;
  delConfirm.textContent = `Delete ${hashes.length} forever`;
  delError.hidden = true; delError.textContent = '';
  delModal.classList.remove('hidden');
  delConfirm.focus();
}
function closeBulkDelete() { delModal.classList.add('hidden'); delHashes = null; }

delConfirm.addEventListener('click', async () => {
  if (!delHashes) return;
  const hashes = delHashes;
  closeBulkDelete();
  await bulkDelete(hashes);
});
document.getElementById('trashDeleteClose').addEventListener('click', closeBulkDelete);
document.getElementById('trashDeleteCancel').addEventListener('click', closeBulkDelete);
delModal.addEventListener('click', e => { if (e.target === delModal) closeBulkDelete(); });
document.addEventListener('keydown', e => {
  if (e.key === 'Escape' && !delModal.classList.contains('hidden')) closeBulkDelete();
});

// ── Boot ────────────────────────────────────────────────────────────────────
(async function boot() {
  const identity = await bootAdmin({ require: 'file.delete' });
  if (!identity) return;
  loadTrash();
})();
