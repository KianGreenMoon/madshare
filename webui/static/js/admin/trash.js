// Admin · Trash — restore or permanently delete trashed files. Requires
// file.delete.
import { bootAdmin, API, fmtBytes, fmtDate, shortHash, toast, handleAuthError, el } from './shared.js';

const trashBody    = document.getElementById('trashBody');
const trashCountEl = document.getElementById('trashCount');

const displayTitle = f => f.title || f.filename || 'this file';
const stateRow = text => el('tr', { class: 'table-state-row' }, [el('td', { colspan: '6', text })]);

function decTrashCount() {
  trashCountEl.textContent = String(Math.max(0, parseInt(trashCountEl.textContent, 10) - 1));
}

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
  items = Array.isArray(items) ? items : [];
  trashCountEl.textContent = String(items.length);

  if (items.length === 0) { trashBody.replaceChildren(stateRow('Trash is empty.')); return; }

  const frag = document.createDocumentFragment();
  items.forEach(f => frag.appendChild(buildTrashRow(f)));
  trashBody.replaceChildren(frag);
}

function buildTrashRow(f) {
  const tr = el('tr', { 'data-hash': f.hash });

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

  tr.append(tdTitle, tdArtist, tdAlbum, tdSize, tdDate, tdActions);
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
  tr.classList.add('row-removing');
  const finish = () => {
    tr.remove();
    decTrashCount();
    if (!trashBody.querySelector('tr[data-hash]')) trashBody.replaceChildren(stateRow('Trash is empty.'));
  };
  tr.addEventListener('animationend', finish, { once: true });
  setTimeout(() => { if (tr.isConnected) finish(); }, 220);
}

// ── Boot ────────────────────────────────────────────────────────────────────
(async function boot() {
  const identity = await bootAdmin({ require: 'file.delete' });
  if (!identity) return;
  loadTrash();
})();
