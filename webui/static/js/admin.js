import { initAuth, openLoginModal } from './auth.js';

// Admin page — uploads (XHR w/ progress), files table (fetch/render/filter),
// hash-based delete with inline confirm, prune dry-run → modal → commit, toasts.
//
// Reads the API base from <meta name="api-url"> just like app.js. Empty default
// => relative, same-origin URLs (bundled server); a non-empty value points a
// separately hosted UI at a remote API origin.
const API = document.querySelector('meta[name="api-url"]')?.content || '';

// ── Content-access (Phase 3c) constants & permission helpers ────────────────
// License vocabulary mirrors api.knownLicenses; "" clears the license.
const LICENSE_OPTIONS = ['', 'CC0-1.0', 'CC-BY-4.0', 'CC-BY-SA-4.0', 'public-domain', 'all-rights-reserved', 'unknown'];
// Free licenses offered as the auto-publish allow-list.
const FREE_LICENSES = ['CC0-1.0', 'CC-BY-4.0', 'CC-BY-SA-4.0', 'public-domain'];

// Capabilities of the signed-in user, refreshed from /api/auth/me.
let canManageUsers = false; // user.manage → users/groups/grants/auto-derive
let canEditMeta    = false; // metadata.edit → per-file guest/license
let currentUsername = '';   // to suppress self-disable/self-delete in the UI

function applyPermissions(identity) {
  const perms = (identity && identity.permissions) || [];
  canManageUsers = perms.includes('user.manage');
  canEditMeta    = perms.includes('metadata.edit');
  currentUsername = (identity && identity.username) || '';
  document.getElementById('usersSection').hidden      = !canManageUsers;
  document.getElementById('accessSection').hidden     = !canManageUsers;
  document.getElementById('autoderiveSection').hidden = !canManageUsers;
}

// ── Theme (shared pattern with app.js) ─────────────────────────────────────

const VALID_THEMES = new Set(['dark', 'light', 'ocean', 'sunset']);
const htmlEl    = document.documentElement;
const themeDots = document.querySelectorAll('.theme-dot');

applyTheme(localStorage.getItem('madshare-theme') || 'dark');
themeDots.forEach(dot => dot.addEventListener('click', () => applyTheme(dot.dataset.theme)));

function applyTheme(name) {
  if (!VALID_THEMES.has(name)) name = 'dark';
  htmlEl.dataset.theme = name;
  localStorage.setItem('madshare-theme', name);
  themeDots.forEach(d => {
    const on = d.dataset.theme === name;
    d.classList.toggle('active', on);
    d.setAttribute('aria-pressed', String(on));
  });
}

// ── Utilities ───────────────────────────────────────────────────────────────

function fmtBytes(n) {
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1024) return n + ' B';
  const kb = n / 1024;
  if (kb < 1024) return kb.toFixed(kb < 10 ? 1 : 0) + ' KB';
  const mb = kb / 1024;
  if (mb < 1024) return mb.toFixed(mb < 10 ? 1 : 0) + ' MB';
  return (mb / 1024).toFixed(1) + ' GB';
}

function shortHash(h) {
  if (!h) return '';
  return h.length > 12 ? h.slice(0, 12) + '…' : h;
}

const AUDIO_EXT = new Set(['mp3', 'ogg', 'oga', 'flac', 'wav', 'mp4', 'm4a', 'aac', 'opus']);
function isAudioFile(file) {
  if (file.type && file.type.startsWith('audio/')) return true;
  const ext = file.name.split('.').pop().toLowerCase();
  return AUDIO_EXT.has(ext);
}

// ── Toasts ───────────────────────────────────────────────────────────────────
// Success/info → polite status region; errors → assertive alert region.
// Success auto-dismisses; errors persist until closed.

const toastStatus = document.getElementById('toastStatus');
const toastAlert  = document.getElementById('toastAlert');

function toast(message, type = 'info') {
  const stack = type === 'error' ? toastAlert : toastStatus;

  const el = document.createElement('div');
  el.className = 'toast' + (type === 'success' ? ' is-success' : type === 'error' ? ' is-error' : '');

  const icon = document.createElement('span');
  icon.className = 'toast-icon';
  icon.setAttribute('aria-hidden', 'true');
  icon.textContent = type === 'success' ? '✓' : type === 'error' ? '✕' : 'ℹ';

  const msg = document.createElement('span');
  msg.className = 'toast-msg';
  msg.textContent = message;

  const close = document.createElement('button');
  close.className = 'toast-close';
  close.setAttribute('aria-label', 'Dismiss');
  close.textContent = '×';
  close.addEventListener('click', () => el.remove());

  el.append(icon, msg, close);
  stack.appendChild(el);

  if (type !== 'error') {
    setTimeout(() => el.remove(), 4000);
  }
}

// ── Files table ───────────────────────────────────────────────────────────────

const filesBody  = document.getElementById('filesBody');
const fileCountEl = document.getElementById('fileCount');
const fileFilter  = document.getElementById('fileFilter');

let allFiles = [];      // last fetched list
let filterText = '';

async function loadFiles() {
  filesBody.setAttribute('aria-busy', 'true');
  renderStateRow('Loading files…');

  let files;
  try {
    const res = await fetch(`${API}/api/files`);
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    files = await res.json();
  } catch (err) {
    console.error('Failed to load files:', err);
    filesBody.setAttribute('aria-busy', 'false');
    renderErrorRow();
    return;
  }

  allFiles = Array.isArray(files) ? files : [];
  filesBody.setAttribute('aria-busy', 'false');
  renderFiles();
}

// Render a single full-width informational row (loading / empty / error).
function renderStateRow(text, extraClass) {
  filesBody.replaceChildren();
  const tr = document.createElement('tr');
  tr.className = 'table-state-row';
  const td = document.createElement('td');
  td.colSpan = 6;
  if (extraClass) td.className = extraClass;
  td.textContent = text;
  tr.appendChild(td);
  filesBody.appendChild(tr);
}

function renderErrorRow() {
  filesBody.replaceChildren();
  const tr = document.createElement('tr');
  tr.className = 'table-state-row';
  const td = document.createElement('td');
  td.colSpan = 6;

  const msg = document.createElement('div');
  msg.setAttribute('role', 'alert');
  msg.textContent = 'Failed to load files.';

  const retry = document.createElement('button');
  retry.className = 'btn btn-neutral btn-sm';
  retry.style.marginTop = 'var(--space-3)';
  retry.textContent = 'Retry';
  retry.addEventListener('click', loadFiles);

  td.append(msg, retry);
  tr.appendChild(td);
  filesBody.appendChild(tr);
}

function renderEmptyRow() {
  filesBody.replaceChildren();
  const tr = document.createElement('tr');
  tr.className = 'table-state-row';
  const td = document.createElement('td');
  td.colSpan = 6;

  const wrap = document.createElement('div');
  wrap.className = 'empty-state';
  const icon = document.createElement('div');
  icon.className = 'drop-icon';
  icon.setAttribute('aria-hidden', 'true');
  icon.textContent = '♪';
  const h = document.createElement('p');
  h.textContent = 'No files yet';
  const sub = document.createElement('p');
  sub.textContent = 'Upload above to get started.';
  wrap.append(icon, h, sub);

  td.appendChild(wrap);
  tr.appendChild(td);
  filesBody.appendChild(tr);
}

function matchesFilter(f, q) {
  if (!q) return true;
  const hay = [f.title, f.artist, f.album, f.filename]
    .filter(Boolean).join(' ').toLowerCase();
  return hay.includes(q);
}

function renderFiles() {
  fileCountEl.textContent = String(allFiles.length);

  if (allFiles.length === 0) {
    renderEmptyRow();
    return;
  }

  const q = filterText.trim().toLowerCase();
  const visible = allFiles.filter(f => matchesFilter(f, q));

  if (visible.length === 0) {
    renderStateRow(`No files match “${filterText.trim()}”`);
    return;
  }

  const frag = document.createDocumentFragment();
  visible.forEach(f => frag.appendChild(buildRow(f)));
  filesBody.replaceChildren(frag);
}

function buildRow(f) {
  const tr = document.createElement('tr');
  tr.dataset.hash = f.hash;

  // Title cell (+ hash second line)
  const tdTitle = document.createElement('td');
  tdTitle.className = 'cell-title-td';
  tdTitle.dataset.label = 'Title';
  const titleSpan = document.createElement('span');
  if (f.title) {
    titleSpan.className = 'cell-title';
    titleSpan.textContent = f.title;
  } else {
    titleSpan.className = 'cell-title is-fallback';
    titleSpan.textContent = f.filename || 'Untitled';
  }
  const hashSpan = document.createElement('span');
  hashSpan.className = 'cell-hash';
  hashSpan.textContent = shortHash(f.hash);
  hashSpan.title = f.hash || '';
  tdTitle.append(titleSpan, hashSpan);

  // Artist
  const tdArtist = document.createElement('td');
  tdArtist.dataset.label = 'Artist';
  if (f.artist) {
    tdArtist.textContent = f.artist;
  } else {
    tdArtist.className = 'cell-muted';
    tdArtist.textContent = '—';
  }

  // Album
  const tdAlbum = document.createElement('td');
  tdAlbum.dataset.label = 'Album';
  if (f.album) {
    tdAlbum.textContent = f.album;
  } else {
    tdAlbum.className = 'cell-muted';
    tdAlbum.textContent = '—';
  }

  // Size
  const tdSize = document.createElement('td');
  tdSize.className = 'cell-size';
  tdSize.dataset.label = 'Size';
  tdSize.textContent = fmtBytes(f.byte_size);

  // Access (guest-playable + license)
  const tdAccess = document.createElement('td');
  tdAccess.className = 'cell-access';
  tdAccess.dataset.label = 'Access';
  tdAccess.appendChild(buildAccessControls(f));

  // Actions
  const tdActions = document.createElement('td');
  tdActions.className = 'cell-actions';
  tdActions.dataset.label = 'Actions';
  tdActions.appendChild(makeDeleteButton(tr, f));

  tr.append(tdTitle, tdArtist, tdAlbum, tdSize, tdAccess, tdActions);
  return tr;
}

// buildAccessControls renders the per-file guest-playable toggle and license
// select. Without metadata.edit it falls back to a read-only summary.
function buildAccessControls(f) {
  const wrap = document.createElement('div');
  wrap.className = 'access-controls';

  if (!canEditMeta) {
    const span = document.createElement('span');
    span.className = 'cell-muted';
    span.textContent = (f.guest_playable ? 'Guest' : 'Private') + (f.license ? ` · ${f.license}` : '');
    wrap.appendChild(span);
    return wrap;
  }

  // Guest-playable toggle
  const label = document.createElement('label');
  label.className = 'guest-toggle';
  const cb = document.createElement('input');
  cb.type = 'checkbox';
  cb.checked = !!f.guest_playable;
  cb.addEventListener('change', () => setGuest(f, cb));
  const txt = document.createElement('span');
  txt.textContent = 'Guest';
  label.append(cb, txt);

  // License select
  const sel = document.createElement('select');
  sel.className = 'license-select';
  sel.setAttribute('aria-label', 'License');
  LICENSE_OPTIONS.forEach(lic => {
    const opt = document.createElement('option');
    opt.value = lic;
    opt.textContent = lic || '— license —';
    if ((f.license || '') === lic) opt.selected = true;
    sel.appendChild(opt);
  });
  sel.addEventListener('change', () => setLicense(f, sel, cb));

  wrap.append(label, sel);
  return wrap;
}

async function setGuest(f, cb) {
  const desired = cb.checked;
  cb.disabled = true;
  try {
    const res = await fetch(`${API}/api/admin/files/${encodeURIComponent(f.hash)}/guest`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ guest_playable: desired }),
    });
    if (handleAuthError(res)) { cb.checked = !desired; return; }
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    f.guest_playable = desired;
    toast(`"${displayTitle(f)}" is now ${desired ? 'guest-playable' : 'private'}.`, 'success');
  } catch (err) {
    cb.checked = !desired;
    toast(`Couldn't update access: ${err.message}`, 'error');
  } finally {
    cb.disabled = false;
  }
}

async function setLicense(f, sel, cb) {
  const desired = sel.value;
  const previous = f.license || '';
  sel.disabled = true;
  try {
    const res = await fetch(`${API}/api/admin/files/${encodeURIComponent(f.hash)}/license`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ license: desired }),
    });
    if (handleAuthError(res)) { sel.value = previous; return; }
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    f.license = desired;
    // Auto-derive may have flipped guest-playable; reflect it without a refetch
    // only on the optimistic path — a reload keeps things authoritative.
    toast(`License set to ${desired || 'none'} for "${displayTitle(f)}".`, 'success');
    if (cb) loadFiles();
  } catch (err) {
    sel.value = previous;
    toast(`Couldn't update license: ${err.message}`, 'error');
  } finally {
    sel.disabled = false;
  }
}

function displayTitle(f) {
  return f.title || f.filename || 'this file';
}

// ── Inline two-step delete ──────────────────────────────────────────────────

function makeDeleteButton(tr, f) {
  const btn = document.createElement('button');
  btn.className = 'btn btn-destructive-outline btn-sm';
  btn.textContent = 'Move to Trash';
  btn.addEventListener('click', () => enterDeleteConfirm(tr, f, btn));
  return btn;
}

function enterDeleteConfirm(tr, f, deleteBtn) {
  const cell = deleteBtn.parentElement;

  const wrap = document.createElement('div');
  wrap.className = 'delete-confirm';

  const label = document.createElement('span');
  label.className = 'delete-confirm-label';
  label.textContent = 'Move to Trash?';

  const cancel = document.createElement('button');
  cancel.className = 'btn btn-neutral btn-sm';
  cancel.textContent = 'Cancel';

  const confirm = document.createElement('button');
  confirm.className = 'btn btn-destructive-solid btn-sm';
  confirm.textContent = 'Confirm';

  const restore = () => {
    cell.replaceChildren(makeDeleteButton(tr, f));
    cell.querySelector('button')?.focus();
  };

  cancel.addEventListener('click', restore);
  confirm.addEventListener('click', () => doDelete(tr, f, wrap));

  // Escape cancels — scoped to this confirm group.
  wrap.addEventListener('keydown', e => {
    if (e.key === 'Escape') { e.stopPropagation(); restore(); }
  });

  wrap.append(label, cancel, confirm);
  cell.replaceChildren(wrap);
  cancel.focus(); // Cancel receives focus by default (safe default)
}

async function doDelete(tr, f, wrap) {
  tr.setAttribute('aria-busy', 'true');
  wrap.querySelectorAll('button').forEach(b => (b.disabled = true));

  const spinner = document.createElement('span');
  spinner.className = 'row-spinner';
  spinner.setAttribute('aria-hidden', 'true');
  wrap.appendChild(spinner);

  // Figure out the next row's delete button for focus restoration.
  const nextRow = tr.nextElementSibling;

  let data;
  try {
    const res = await fetch(`${API}/api/admin/files/${encodeURIComponent(f.hash)}`, {
      method: 'DELETE',
    });
    if (handleAuthError(res)) {
      tr.removeAttribute('aria-busy');
      return;
    }
    data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) {
      throw new Error(data.error || `HTTP ${res.status}`);
    }
  } catch (err) {
    // Revert row, keep it, surface error.
    tr.removeAttribute('aria-busy');
    wrap.replaceChildren(); // clear confirm UI
    const cell = wrap.parentElement;
    cell.replaceChildren(makeDeleteButton(tr, f));
    toast(`Couldn’t delete “${displayTitle(f)}”: ${err.message}`, 'error');
    cell.querySelector('button')?.focus();
    return;
  }

  // Confirmed removal — drop from data model and animate out.
  allFiles = allFiles.filter(x => x.hash !== f.hash);
  fileCountEl.textContent = String(allFiles.length);

  tr.classList.add('row-removing');
  const finish = () => {
    tr.remove();
    // Move focus to next row's Delete, else the files heading.
    const target = nextRow && nextRow.isConnected
      ? nextRow.querySelector('.cell-actions button')
      : document.getElementById('filesHeading');
    target?.focus?.();
    // If the table is now empty, re-render the empty state.
    if (allFiles.length === 0) renderEmptyRow();
  };
  tr.addEventListener('animationend', finish, { once: true });
  // Fallback if animation is disabled / doesn't fire.
  setTimeout(() => { if (tr.isConnected) finish(); }, 220);

  toast(`”${displayTitle(f)}” moved to Trash`, 'success');
  loadTrash();
}

// Debounced filter input (~150ms).
let filterTimer;
fileFilter.addEventListener('input', () => {
  clearTimeout(filterTimer);
  filterTimer = setTimeout(() => {
    filterText = fileFilter.value;
    renderFiles();
  }, 150);
});

// ── Upload (XHR for progress) ─────────────────────────────────────────────────

const dropZone   = document.getElementById('dropZone');
const fileInput  = document.getElementById('fileInput');
const uploadQueue = document.getElementById('uploadQueue');

let filesAddedThisSession = false; // mark table stale → refetch when idle

dropZone.addEventListener('dragover',  e => { e.preventDefault(); dropZone.classList.add('dragover'); });
dropZone.addEventListener('dragenter', e => { e.preventDefault(); dropZone.classList.add('dragover'); });
dropZone.addEventListener('dragleave', () => dropZone.classList.remove('dragover'));
dropZone.addEventListener('drop', e => {
  e.preventDefault();
  dropZone.classList.remove('dragover');
  handleFiles(Array.from(e.dataTransfer.files));
});

fileInput.addEventListener('change', () => {
  handleFiles(Array.from(fileInput.files));
  fileInput.value = '';
});

function handleFiles(files) {
  if (!files.length) return;
  files.forEach(file => {
    const item = makeQueueItem(file);
    uploadQueue.appendChild(item.li);
    if (!isAudioFile(file)) {
      setItemError(item, 'Not an audio file', file, /*retryable*/ false);
    } else {
      enqueue(() => uploadOne(file, item));
    }
  });
}

// Simple concurrency-capped queue (cap 3).
const MAX_CONCURRENT = 3;
let active = 0;
const pending = [];

function enqueue(task) {
  pending.push(task);
  pump();
}
function pump() {
  while (active < MAX_CONCURRENT && pending.length) {
    const task = pending.shift();
    active++;
    Promise.resolve()
      .then(task)
      .finally(() => {
        active--;
        if (active === 0 && pending.length === 0) onQueueIdle();
        else pump();
      });
  }
}
function onQueueIdle() {
  if (filesAddedThisSession) {
    filesAddedThisSession = false;
    loadFiles();
  }
}

function makeQueueItem(file) {
  const li = document.createElement('li');
  li.className = 'upload-item';

  const icon = document.createElement('span');
  icon.className = 'upload-item-icon';
  icon.setAttribute('aria-hidden', 'true');
  icon.textContent = '♪';

  const name = document.createElement('span');
  name.className = 'upload-item-name';
  name.textContent = file.name;
  name.title = file.name;

  const status = document.createElement('span');
  status.className = 'upload-item-status';
  status.textContent = 'Waiting…';

  li.append(icon, name, status);
  return { li, status, name };
}

function setItemQueued(item) {
  item.status.className = 'upload-item-status';
  item.status.replaceChildren(document.createTextNode('Waiting…'));
}

function setItemUploading(item) {
  item.status.className = 'upload-item-status';
  item.status.replaceChildren();

  const bar = document.createElement('span');
  bar.className = 'progress';
  const fill = document.createElement('span');
  fill.className = 'progress-fill';
  bar.appendChild(fill);

  const pct = document.createElement('span');
  pct.className = 'upload-item-pct';
  pct.textContent = '0%';

  item.status.append(bar, pct);
  item._fill = fill;
  item._pct = pct;
}

function setItemProgress(item, ratio) {
  if (!item._fill) return;
  const p = Math.round(ratio * 100);
  item._fill.style.width = p + '%';
  item._pct.textContent = p + '%';
}

function setItemDone(item, kind /* 'created' | 'deduped' */) {
  item.status.className = 'upload-item-status ' + (kind === 'created' ? 'is-created' : 'is-deduped');
  const icon = document.createElement('span');
  icon.setAttribute('aria-hidden', 'true');
  icon.textContent = kind === 'created' ? '✓' : '◑';
  const text = document.createElement('span');
  text.textContent = kind === 'created' ? 'Added' : 'Already in library';
  item.status.replaceChildren(icon, text);
}

function setItemError(item, reason, file, retryable = true) {
  item.status.className = 'upload-item-status is-error';
  item.status.replaceChildren();

  const icon = document.createElement('span');
  icon.setAttribute('aria-hidden', 'true');
  icon.textContent = '✕';
  const text = document.createElement('span');
  text.textContent = reason;
  item.status.append(icon, text);

  if (retryable && file) {
    const retry = document.createElement('button');
    retry.className = 'btn btn-neutral btn-sm';
    retry.textContent = 'Retry';
    retry.addEventListener('click', () => {
      setItemQueued(item);
      enqueue(() => uploadOne(file, item));
    });
    item.status.appendChild(retry);
  }
}

function uploadOne(file, item) {
  setItemUploading(item);

  return new Promise(resolve => {
    const xhr = new XMLHttpRequest();
    xhr.open('POST', `${API}/files/upload`);

    xhr.upload.addEventListener('progress', e => {
      if (e.lengthComputable) setItemProgress(item, e.loaded / e.total);
    });

    xhr.addEventListener('load', () => {
      let data = null;
      try { data = JSON.parse(xhr.responseText); } catch { /* ignore */ }

      if (xhr.status >= 200 && xhr.status < 300 && data) {
        if (data.existed) {
          setItemDone(item, 'deduped');
        } else {
          setItemDone(item, 'created');
          filesAddedThisSession = true; // table is now stale
        }
      } else {
        const reason = (xhr.responseText || `HTTP ${xhr.status}`).slice(0, 200);
        setItemError(item, reason, file);
      }
      resolve();
    });

    xhr.addEventListener('error', () => {
      setItemError(item, 'Network error', file);
      resolve();
    });

    const fd = new FormData();
    fd.append('file', file);
    xhr.send(fd);
  });
}

// ── Verify & Prune ────────────────────────────────────────────────────────────

const previewBtn   = document.getElementById('previewPrune');
const pruneResults = document.getElementById('pruneResults');
const deepVerify   = document.getElementById('deepVerify');

// lastPruneDeep captures the scan mode used for the preview, so the subsequent
// commit prunes exactly what was previewed even if the checkbox is toggled after.
let lastPruneDeep = false;

previewBtn.addEventListener('click', runPreview);

async function runPreview() {
  previewBtn.disabled = true;
  previewBtn.setAttribute('aria-busy', 'true');
  const original = previewBtn.textContent;
  lastPruneDeep = deepVerify.checked;
  previewBtn.textContent = lastPruneDeep ? 'Verifying…' : 'Scanning…';

  let data;
  try {
    const res = await fetch(`${API}/api/admin/prune`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ confirm: false, deep: lastPruneDeep }),
    });
    if (handleAuthError(res)) return;
    data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
  } catch (err) {
    renderPrunePanel('error', 'Prune scan failed', err.message);
    toast(`Prune scan failed: ${err.message}`, 'error');
    return;
  } finally {
    previewBtn.disabled = false;
    previewBtn.removeAttribute('aria-busy');
    previewBtn.textContent = original;
  }

  if ((data.dangling_count || 0) === 0) {
    renderPrunePanel('success', 'All records verified',
      `${data.scanned} file${data.scanned === 1 ? '' : 's'} checked, nothing to prune.`);
    return;
  }

  renderDanglingPanel(data);
}

// Generic message panel (success / error / plain warning).
function renderPrunePanel(kind, title, detail) {
  pruneResults.replaceChildren();
  const panel = document.createElement('div');
  panel.className = 'result-panel is-' + kind;

  const head = document.createElement('div');
  head.className = 'result-panel-head';
  const icon = document.createElement('span');
  icon.className = 'result-panel-icon';
  icon.setAttribute('aria-hidden', 'true');
  icon.textContent = kind === 'success' ? '✓' : kind === 'error' ? '✕' : '⚠';
  const h = document.createElement('span');
  h.textContent = title;
  head.append(icon, h);

  panel.appendChild(head);
  if (detail) {
    const p = document.createElement('p');
    p.textContent = detail;
    panel.appendChild(p);
  }
  pruneResults.appendChild(panel);
}

function renderDanglingPanel(data) {
  pruneResults.replaceChildren();
  const panel = document.createElement('div');
  panel.className = 'result-panel is-warning';

  const head = document.createElement('div');
  head.className = 'result-panel-head';
  const icon = document.createElement('span');
  icon.className = 'result-panel-icon';
  icon.setAttribute('aria-hidden', 'true');
  icon.textContent = '⚠';
  const h = document.createElement('span');
  const n = data.dangling_count;
  h.textContent = `${n} dangling record${n === 1 ? '' : 's'} found (of ${data.scanned} checked).`;
  head.append(icon, h);
  panel.appendChild(head);

  const sub = document.createElement('p');
  sub.textContent = data.deep
    ? 'These files are missing from disk or their contents are corrupted.'
    : 'These point to files missing from disk.';
  panel.appendChild(sub);

  panel.appendChild(buildDanglingList(data.dangling));

  const pruneNow = document.createElement('button');
  pruneNow.className = 'btn btn-destructive-solid';
  pruneNow.textContent = `Prune ${n} record${n === 1 ? '' : 's'}`;
  pruneNow.addEventListener('click', () => openPruneModal(n));
  panel.appendChild(pruneNow);

  pruneResults.appendChild(panel);
}

function buildDanglingList(entries) {
  const ul = document.createElement('ul');
  ul.className = 'dangling-list';
  (entries || []).forEach(entry => {
    const li = document.createElement('li');
    const name = document.createElement('span');
    name.className = 'dangling-name';
    const fn = Array.isArray(entry.filenames) && entry.filenames.length
      ? entry.filenames.join(', ')
      : '(no filename)';
    name.textContent = fn;
    const hash = document.createElement('span');
    hash.className = 'dangling-hash';
    hash.textContent = shortHash(entry.hash);
    hash.title = entry.hash || '';
    li.append(name, hash);
    if (entry.reason) {
      const reason = document.createElement('span');
      reason.className = 'dangling-reason is-' + entry.reason;
      reason.textContent = entry.reason; // "missing" | "corrupt"
      li.appendChild(reason);
    }
    ul.appendChild(li);
  });
  return ul;
}

// ── Prune confirmation modal (focus trap, Esc to close) ───────────────────────

const pruneModal      = document.getElementById('pruneModal');
const pruneModalBody  = document.getElementById('pruneModalBody');
const confirmPruneBtn = document.getElementById('confirmPrune');
const cancelPruneBtn  = document.getElementById('cancelPrune');
const closePruneBtn   = document.getElementById('closePruneModal');

let lastFocusBeforeModal = null;

function openPruneModal(count) {
  lastFocusBeforeModal = document.activeElement;
  document.getElementById('pruneModalTitle').textContent =
    `Prune ${count} record${count === 1 ? '' : 's'}?`;
  pruneModalBody.textContent =
    `This permanently removes ${count} database record${count === 1 ? '' : 's'} whose ` +
    `file${count === 1 ? ' is' : 's are'} already gone. This cannot be undone.`;
  confirmPruneBtn.textContent = `Prune ${count} record${count === 1 ? '' : 's'}`;
  confirmPruneBtn.disabled = false;
  confirmPruneBtn.removeAttribute('aria-busy');

  pruneModal.classList.remove('hidden');
  cancelPruneBtn.focus(); // Cancel focused on open (safe default)
}

function closePruneModal() {
  pruneModal.classList.add('hidden');
  lastFocusBeforeModal?.focus?.();
}

cancelPruneBtn.addEventListener('click', closePruneModal);
closePruneBtn.addEventListener('click', closePruneModal);
pruneModal.addEventListener('click', e => { if (e.target === pruneModal) closePruneModal(); });
confirmPruneBtn.addEventListener('click', commitPrune);

// Esc + focus trap, scoped to the modal (mirrors app.js upload-modal behavior).
pruneModal.addEventListener('keydown', e => {
  if (e.key === 'Escape') { closePruneModal(); return; }
  if (e.key !== 'Tab') return;
  const focusable = pruneModal.querySelectorAll(
    'button:not([disabled]), [href], input, [tabindex]:not([tabindex="-1"])'
  );
  if (!focusable.length) return;
  const first = focusable[0];
  const last  = focusable[focusable.length - 1];
  if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
  else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
});

async function commitPrune() {
  confirmPruneBtn.disabled = true;
  cancelPruneBtn.disabled = true;
  confirmPruneBtn.setAttribute('aria-busy', 'true');
  const original = confirmPruneBtn.textContent;
  confirmPruneBtn.textContent = 'Pruning…';

  let data;
  try {
    const res = await fetch(`${API}/api/admin/prune`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ confirm: true, deep: lastPruneDeep }),
    });
    if (handleAuthError(res)) { closePruneModal(); return; }
    data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
  } catch (err) {
    cancelPruneBtn.disabled = false;
    confirmPruneBtn.disabled = false;
    confirmPruneBtn.removeAttribute('aria-busy');
    confirmPruneBtn.textContent = original;
    toast(`Prune failed: ${err.message}`, 'error');
    return;
  }

  closePruneModal();
  renderPruneCommitResult(data);

  const pruned = data.pruned_count || 0;
  const failed = (data.failed && data.failed.length) || 0;
  if (failed) {
    toast(`Pruned ${pruned}, ${failed} failed.`, 'error');
  } else {
    toast(`Pruned ${pruned} record${pruned === 1 ? '' : 's'}.`, 'success');
  }

  loadFiles(); // refresh files table
}

function renderPruneCommitResult(data) {
  pruneResults.replaceChildren();
  const pruned = data.pruned_count || 0;
  const failed = data.failed || [];

  const panel = document.createElement('div');
  panel.className = 'result-panel ' + (failed.length ? 'is-warning' : 'is-success');

  const head = document.createElement('div');
  head.className = 'result-panel-head';
  const icon = document.createElement('span');
  icon.className = 'result-panel-icon';
  icon.setAttribute('aria-hidden', 'true');
  icon.textContent = failed.length ? '⚠' : '✓';
  const h = document.createElement('span');
  h.textContent = `Pruned ${pruned} record${pruned === 1 ? '' : 's'}.`;
  head.append(icon, h);
  panel.appendChild(head);

  if (Array.isArray(data.pruned) && data.pruned.length) {
    panel.appendChild(buildDanglingList(data.pruned));
  }

  if (failed.length) {
    const fp = document.createElement('p');
    fp.textContent = `${failed.length} record${failed.length === 1 ? '' : 's'} could not be removed:`;
    panel.appendChild(fp);

    const ul = document.createElement('ul');
    ul.className = 'dangling-list';
    failed.forEach(entry => {
      const li = document.createElement('li');
      const hash = document.createElement('span');
      hash.className = 'dangling-hash';
      hash.textContent = shortHash(entry.hash);
      hash.title = entry.hash || '';
      const err = document.createElement('span');
      err.className = 'dangling-name';
      err.textContent = entry.error || 'unknown error';
      li.append(hash, err);
      ul.appendChild(li);
    });
    panel.appendChild(ul);
  }

  pruneResults.appendChild(panel);
}

// handleAuthError opens the shared login modal when a response is 401.
// Returns true when it handled an auth failure (caller should stop).
function handleAuthError(res) {
  if (res && res.status === 401) {
    toast('Your session expired — please sign in again.', 'error');
    openLoginModal();
    return true;
  }
  return false;
}

// ── Access groups & grants (Phase 3c) ───────────────────────────────────────
// All endpoints require user.manage; the section is hidden otherwise.

const groupsList      = document.getElementById('groupsList');
const groupCreateForm = document.getElementById('groupCreateForm');
const groupName       = document.getElementById('groupName');

let allUsers = [];

// el is a tiny DOM builder: el('button', {class:'btn'}, ['Label']).
function el(tag, props = {}, children = []) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (k === 'class') node.className = v;
    else if (k === 'text') node.textContent = v;
    else if (k.startsWith('on') && typeof v === 'function') node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v);
  }
  (Array.isArray(children) ? children : [children]).forEach(c => {
    if (c != null) node.append(c.nodeType ? c : document.createTextNode(c));
  });
  return node;
}

async function loadGroups() {
  groupsList.replaceChildren(el('p', { class: 'cell-muted', text: 'Loading groups…' }));
  let groups;
  try {
    const res = await fetch(`${API}/api/admin/access/groups`);
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    groups = await res.json();
  } catch (err) {
    groupsList.replaceChildren(el('p', { class: 'cell-muted', text: `Failed to load groups: ${err.message}` }));
    return;
  }
  renderGroups(groups || []);
}

function renderGroups(groups) {
  if (!groups.length) {
    groupsList.replaceChildren(el('p', { class: 'cell-muted', text: 'No groups yet.' }));
    return;
  }
  groupsList.replaceChildren(...groups.map(buildGroupCard));
}

function buildGroupCard(g) {
  const card = el('div', { class: 'group-card' });

  const head = el('div', { class: 'group-head' }, [
    el('h3', { class: 'group-name', text: g.name }),
    el('button', {
      class: 'btn btn-destructive-outline btn-sm',
      text: 'Delete group',
      onclick: () => deleteGroup(g),
    }),
  ]);

  // Members
  const memberItems = (g.members || []).map(m =>
    el('li', {}, [
      el('span', { text: m.username }),
      el('button', {
        class: 'btn btn-neutral btn-sm', text: 'Remove',
        onclick: () => removeMember(g, m.user_id),
      }),
    ]));
  const memberList = el('ul', { class: 'member-list' },
    memberItems.length ? memberItems : [el('li', { class: 'cell-muted', text: 'No members.' })]);

  // Add-member control: users not already in the group.
  const memberIds = new Set((g.members || []).map(m => m.user_id));
  const candidates = allUsers.filter(u => !memberIds.has(u.id));
  const memberSelect = el('select', { class: 'license-select', 'aria-label': 'Add user' },
    candidates.length
      ? candidates.map(u => el('option', { value: String(u.id), text: u.username }))
      : [el('option', { value: '', text: 'All users are members' })]);
  const addMemberBtn = el('button', {
    class: 'btn btn-neutral btn-sm', text: 'Add member',
    onclick: () => { if (memberSelect.value) addMember(g, Number(memberSelect.value)); },
  });
  if (!candidates.length) addMemberBtn.disabled = true;

  // Grants
  const grantItems = (g.grants || []).map(gr =>
    el('li', {}, [
      el('span', { text: describeGrant(gr) }),
      el('button', {
        class: 'btn btn-neutral btn-sm', text: 'Remove',
        onclick: () => deleteGrant(g, gr.id),
      }),
    ]));
  const grantList = el('ul', { class: 'grant-list' },
    grantItems.length ? grantItems : [el('li', { class: 'cell-muted', text: 'No grants — group can reach nothing.' })]);

  card.append(
    head,
    el('h4', { class: 'group-sub', text: 'Members' }),
    memberList,
    el('div', { class: 'inline-form' }, [memberSelect, addMemberBtn]),
    el('h4', { class: 'group-sub', text: 'Grants' }),
    grantList,
    buildGrantForm(g),
  );
  return card;
}

function describeGrant(gr) {
  switch (gr.scope_type) {
    case 'all':    return 'Whole library';
    case 'artist': return `Artist: ${gr.artist}`;
    case 'album':  return `Album: ${gr.artist} — ${gr.album}`;
    case 'file':   return `File #${gr.file_id}`;
    default:       return gr.scope_type;
  }
}

function buildGrantForm(g) {
  const scope = el('select', { class: 'license-select', 'aria-label': 'Grant scope' }, [
    el('option', { value: 'all', text: 'Whole library' }),
    el('option', { value: 'artist', text: 'Artist' }),
    el('option', { value: 'album', text: 'Album' }),
    el('option', { value: 'file', text: 'File (hash)' }),
  ]);
  const artist = el('input', { type: 'text', placeholder: 'Artist', class: 'grant-input' });
  const album  = el('input', { type: 'text', placeholder: 'Album', class: 'grant-input' });
  const fileHash = el('input', { type: 'text', placeholder: 'File hash', class: 'grant-input' });

  function sync() {
    artist.hidden   = !(scope.value === 'artist' || scope.value === 'album');
    album.hidden    = scope.value !== 'album';
    fileHash.hidden = scope.value !== 'file';
  }
  scope.addEventListener('change', sync);
  sync();

  const addBtn = el('button', {
    class: 'btn btn-neutral btn-sm', text: 'Add grant',
    onclick: () => addGrant(g, {
      scope_type: scope.value,
      artist: artist.value.trim(),
      album: album.value.trim(),
      file_hash: fileHash.value.trim(),
    }),
  });

  return el('div', { class: 'inline-form grant-form' }, [scope, artist, album, fileHash, addBtn]);
}

groupCreateForm.addEventListener('submit', async e => {
  e.preventDefault();
  const name = groupName.value.trim();
  if (!name) return;
  try {
    const res = await fetch(`${API}/api/admin/access/groups`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name }),
    });
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    groupName.value = '';
    toast(`Group "${name}" created.`, 'success');
    loadGroups();
  } catch (err) {
    toast(`Couldn't create group: ${err.message}`, 'error');
  }
});

async function deleteGroup(g) {
  if (!confirm(`Delete group "${g.name}"? Its members and grants are removed.`)) return;
  await mutateGroup(`/api/admin/access/groups/${g.id}`, 'DELETE', null, `Group "${g.name}" deleted.`);
}
async function addMember(g, userID) {
  await mutateGroup(`/api/admin/access/groups/${g.id}/members`, 'POST', { user_id: userID }, 'Member added.');
}
async function removeMember(g, userID) {
  await mutateGroup(`/api/admin/access/groups/${g.id}/members/${userID}`, 'DELETE', null, 'Member removed.');
}
async function addGrant(g, body) {
  await mutateGroup(`/api/admin/access/groups/${g.id}/grants`, 'POST', body, 'Grant added.');
}
async function deleteGrant(g, grantID) {
  await mutateGroup(`/api/admin/access/grants/${grantID}`, 'DELETE', null, 'Grant removed.');
}

// mutateGroup performs a group/grant mutation, then refreshes the list.
async function mutateGroup(path, method, body, okMsg) {
  try {
    const opts = { method };
    if (body != null) {
      opts.headers = { 'Content-Type': 'application/json' };
      opts.body = JSON.stringify(body);
    }
    const res = await fetch(`${API}${path}`, opts);
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    toast(okMsg, 'success');
    loadGroups();
  } catch (err) {
    toast(`Action failed: ${err.message}`, 'error');
  }
}

// ── Users (user.manage) ─────────────────────────────────────────────────────
// Create / edit roles / reset password / enable-disable / delete accounts.
// The section is hidden without user.manage.

const ROLE_LABELS = { admin: 'Admin', moderator: 'Moderator', uploader: 'Uploader', listener: 'Listener' };
const roleLabel = name => ROLE_LABELS[name] || name;

const usersList      = document.getElementById('usersList');
const userCreateForm = document.getElementById('userCreateForm');
const newUserName    = document.getElementById('newUserName');
const newUserPass    = document.getElementById('newUserPass');
const newUserRole    = document.getElementById('newUserRole');
const newUserForceChange = document.getElementById('newUserForceChange');

let availableRoles = []; // [{name, built_in}]

async function loadRoles() {
  try {
    const res = await fetch(`${API}/api/admin/roles`);
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    availableRoles = await res.json();
  } catch (err) {
    console.error('load roles:', err);
    availableRoles = [];
  }
  // Populate the create-user role picker, defaulting to "listener".
  newUserRole.replaceChildren(...availableRoles.map(r =>
    el('option', { value: r.name, ...(r.name === 'listener' ? { selected: 'selected' } : {}) }, [roleLabel(r.name)])));
}

async function loadUsersAdmin() {
  usersList.replaceChildren(el('p', { class: 'cell-muted', text: 'Loading users…' }));
  let users;
  try {
    const res = await fetch(`${API}/api/admin/users`);
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    users = await res.json();
  } catch (err) {
    usersList.replaceChildren(el('p', { class: 'cell-muted', text: `Failed to load users: ${err.message}` }));
    return;
  }
  allUsers = users || []; // keep the group-membership picker in sync too
  renderUsers(allUsers);
}

function renderUsers(users) {
  if (!users.length) {
    usersList.replaceChildren(el('p', { class: 'cell-muted', text: 'No users yet.' }));
    return;
  }
  usersList.replaceChildren(...users.map(buildUserCard));
}

function buildUserCard(u) {
  const isSelf = u.username === currentUsername;
  const card = el('div', { class: 'group-card' });

  const badges = [];
  (u.roles || []).forEach(r => badges.push(el('span', { class: 'role-badge', text: roleLabel(r) })));
  if (!u.roles || !u.roles.length) badges.push(el('span', { class: 'cell-muted', text: 'no roles' }));
  if (u.disabled) badges.push(el('span', { class: 'role-badge is-disabled', text: 'Disabled' }));
  if (isSelf) badges.push(el('span', { class: 'role-badge is-you', text: 'you' }));

  const delBtn = el('button', {
    class: 'btn btn-destructive-outline btn-sm', text: 'Delete',
    onclick: () => deleteUserAccount(u),
  });
  if (isSelf) delBtn.disabled = true;

  const head = el('div', { class: 'group-head' }, [
    el('div', { class: 'user-head-info' }, [el('h3', { class: 'group-name', text: u.username }), ...badges]),
    delBtn,
  ]);

  // Role editor: one checkbox per available role, plus a Save button.
  const have = new Set(u.roles || []);
  const checks = availableRoles.map(r => {
    const cb = el('input', { type: 'checkbox', value: r.name, ...(have.has(r.name) ? { checked: 'checked' } : {}) });
    return el('label', { class: 'role-check' }, [cb, el('span', { text: roleLabel(r.name) })]);
  });
  const saveRolesBtn = el('button', {
    class: 'btn btn-neutral btn-sm', text: 'Save roles',
    onclick: () => {
      const roles = checks.map(l => l.querySelector('input')).filter(cb => cb.checked).map(cb => cb.value);
      updateUser(u, { roles });
    },
  });

  const disableBtn = el('button', {
    class: 'btn btn-neutral btn-sm', text: u.disabled ? 'Enable' : 'Disable',
    onclick: () => updateUser(u, { disabled: !u.disabled }),
  });
  if (isSelf) disableBtn.disabled = true;

  const resetBtn = el('button', {
    class: 'btn btn-neutral btn-sm', text: 'Reset password',
    onclick: () => resetPassword(u),
  });

  card.append(
    head,
    el('h4', { class: 'group-sub', text: 'Roles' }),
    el('div', { class: 'role-checks' }, checks),
    el('div', { class: 'inline-form' }, [saveRolesBtn, resetBtn, disableBtn]),
  );
  return card;
}

userCreateForm.addEventListener('submit', async e => {
  e.preventDefault();
  const username = newUserName.value.trim();
  const password = newUserPass.value;
  if (!username || !password) return;
  try {
    const res = await fetch(`${API}/api/admin/users`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        username, password,
        roles: newUserRole.value ? [newUserRole.value] : [],
        require_password_change: newUserForceChange.checked,
      }),
    });
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    newUserName.value = '';
    newUserPass.value = '';
    newUserForceChange.checked = false;
    toast(`User "${username}" created.`, 'success');
    loadUsersAdmin();
  } catch (err) {
    toast(`Couldn't create user: ${err.message}`, 'error');
  }
});

async function updateUser(u, body) {
  try {
    const res = await fetch(`${API}/api/admin/users/${u.id}`, {
      method: 'PATCH', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    toast(`User "${u.username}" updated.`, 'success');
    loadUsersAdmin();
  } catch (err) {
    toast(`Couldn't update user: ${err.message}`, 'error');
  }
}

async function resetPassword(u) {
  const pw = prompt(`New password for "${u.username}" (min 8 characters):`);
  if (pw == null) return; // cancelled
  if (pw.length < 8) { toast('Password too short (min 8).', 'error'); return; }
  try {
    const res = await fetch(`${API}/api/admin/users/${u.id}/password`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ new_password: pw, require_password_change: false }),
    });
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    toast(`Password reset for "${u.username}". Active sessions were signed out.`, 'success');
  } catch (err) {
    toast(`Couldn't reset password: ${err.message}`, 'error');
  }
}

async function deleteUserAccount(u) {
  if (!confirm(`Delete user "${u.username}"? This removes their account, sessions and tokens. This cannot be undone.`)) return;
  try {
    const res = await fetch(`${API}/api/admin/users/${u.id}`, { method: 'DELETE' });
    if (handleAuthError(res)) return;
    if (!res.ok && res.status !== 204) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    toast(`User "${u.username}" deleted.`, 'success');
    loadUsersAdmin();
  } catch (err) {
    toast(`Couldn't delete user: ${err.message}`, 'error');
  }
}

// ── Auto-publish (license auto-derivation) policy ───────────────────────────

const autoderiveForm    = document.getElementById('autoderiveForm');
const autoderiveEnabled = document.getElementById('autoderiveEnabled');
const autoderiveLicenses = document.getElementById('autoderiveLicenses');

// Build the allow-list checkboxes once.
FREE_LICENSES.forEach(lic => {
  autoderiveLicenses.appendChild(el('label', { class: 'check-row' }, [
    el('input', { type: 'checkbox', value: lic, 'data-license': lic }),
    el('span', { text: lic }),
  ]));
});

async function loadAutoDerive() {
  try {
    const res = await fetch(`${API}/api/admin/settings/autoderive`);
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const p = await res.json();
    autoderiveEnabled.checked = !!p.enabled;
    const on = new Set(p.licenses || []);
    autoderiveLicenses.querySelectorAll('input[type=checkbox]').forEach(cb => {
      cb.checked = on.has(cb.value);
    });
  } catch (err) {
    console.error('load auto-derive:', err);
  }
}

async function saveAutoDerive() {
  const licenses = Array.from(autoderiveLicenses.querySelectorAll('input:checked')).map(cb => cb.value);
  try {
    const res = await fetch(`${API}/api/admin/settings/autoderive`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ enabled: autoderiveEnabled.checked, licenses }),
    });
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    toast('Auto-publish policy saved.', 'success');
  } catch (err) {
    toast(`Couldn't save policy: ${err.message}`, 'error');
  }
}

autoderiveForm.addEventListener('submit', e => { e.preventDefault(); saveAutoDerive(); });

// ── Trash bucket ─────────────────────────────────────────────────────────────

const trashBody    = document.getElementById('trashBody');
const trashCountEl = document.getElementById('trashCount');

function fmtDate(unix) {
  if (!unix) return '—';
  return new Date(unix * 1000).toLocaleDateString(undefined, { dateStyle: 'medium' });
}

async function loadTrash() {
  trashBody.setAttribute('aria-busy', 'true');
  trashBody.replaceChildren();
  const tr = document.createElement('tr');
  tr.className = 'table-state-row';
  const td = document.createElement('td');
  td.colSpan = 6;
  td.textContent = 'Loading…';
  tr.appendChild(td);
  trashBody.appendChild(tr);

  let items;
  try {
    const res = await fetch(`${API}/api/admin/trash`);
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    items = await res.json();
  } catch (err) {
    console.error('Failed to load trash:', err);
    trashBody.setAttribute('aria-busy', 'false');
    trashBody.replaceChildren();
    const tr2 = document.createElement('tr');
    tr2.className = 'table-state-row';
    const td2 = document.createElement('td');
    td2.colSpan = 6;
    td2.textContent = `Failed to load trash: ${err.message}`;
    tr2.appendChild(td2);
    trashBody.appendChild(tr2);
    return;
  }

  trashBody.setAttribute('aria-busy', 'false');
  items = Array.isArray(items) ? items : [];
  trashCountEl.textContent = String(items.length);

  if (items.length === 0) {
    trashBody.replaceChildren();
    const tr3 = document.createElement('tr');
    tr3.className = 'table-state-row';
    const td3 = document.createElement('td');
    td3.colSpan = 6;
    td3.textContent = 'Trash is empty.';
    tr3.appendChild(td3);
    trashBody.appendChild(tr3);
    return;
  }

  const frag = document.createDocumentFragment();
  items.forEach(f => frag.appendChild(buildTrashRow(f)));
  trashBody.replaceChildren(frag);
}

function buildTrashRow(f) {
  const tr = document.createElement('tr');
  tr.dataset.hash = f.hash;

  const tdTitle = document.createElement('td');
  tdTitle.className = 'cell-title-td';
  tdTitle.dataset.label = 'Title';
  const titleSpan = document.createElement('span');
  if (f.title) {
    titleSpan.className = 'cell-title';
    titleSpan.textContent = f.title;
  } else {
    titleSpan.className = 'cell-title is-fallback';
    titleSpan.textContent = f.filename || 'Untitled';
  }
  const hashSpan = document.createElement('span');
  hashSpan.className = 'cell-hash';
  hashSpan.textContent = shortHash(f.hash);
  hashSpan.title = f.hash || '';
  tdTitle.append(titleSpan, hashSpan);

  const tdArtist = document.createElement('td');
  tdArtist.dataset.label = 'Artist';
  if (f.artist) {
    tdArtist.textContent = f.artist;
  } else {
    tdArtist.className = 'cell-muted';
    tdArtist.textContent = '—';
  }

  const tdAlbum = document.createElement('td');
  tdAlbum.dataset.label = 'Album';
  if (f.album) {
    tdAlbum.textContent = f.album;
  } else {
    tdAlbum.className = 'cell-muted';
    tdAlbum.textContent = '—';
  }

  const tdSize = document.createElement('td');
  tdSize.className = 'cell-size';
  tdSize.dataset.label = 'Size';
  tdSize.textContent = fmtBytes(f.byte_size);

  const tdDate = document.createElement('td');
  tdDate.dataset.label = 'Deleted';
  tdDate.textContent = fmtDate(f.deleted_at);

  const tdActions = document.createElement('td');
  tdActions.className = 'cell-actions';
  tdActions.dataset.label = 'Actions';
  tdActions.appendChild(buildTrashActions(tr, f));

  tr.append(tdTitle, tdArtist, tdAlbum, tdSize, tdDate, tdActions);
  return tr;
}

function buildTrashActions(tr, f) {
  const wrap = document.createElement('div');
  wrap.style.display = 'flex';
  wrap.style.gap = 'var(--space-2)';

  const restoreBtn = document.createElement('button');
  restoreBtn.className = 'btn btn-neutral btn-sm';
  restoreBtn.textContent = 'Restore';
  restoreBtn.addEventListener('click', () => doTrashRestore(tr, f, wrap));

  const deleteBtn = document.createElement('button');
  deleteBtn.className = 'btn btn-destructive-outline btn-sm';
  deleteBtn.textContent = 'Delete forever';
  deleteBtn.addEventListener('click', () => enterTrashDeleteConfirm(tr, f, wrap));

  wrap.append(restoreBtn, deleteBtn);
  return wrap;
}

async function doTrashRestore(tr, f, wrap) {
  wrap.querySelectorAll('button').forEach(b => (b.disabled = true));
  try {
    const res = await fetch(`${API}/api/admin/trash/${encodeURIComponent(f.hash)}/restore`, {
      method: 'POST',
    });
    if (handleAuthError(res)) { wrap.querySelectorAll('button').forEach(b => (b.disabled = false)); return; }
    const data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
  } catch (err) {
    wrap.querySelectorAll('button').forEach(b => (b.disabled = false));
    toast(`Couldn't restore "${displayTitle(f)}": ${err.message}`, 'error');
    return;
  }

  tr.classList.add('row-removing');
  const finish = () => {
    tr.remove();
    trashCountEl.textContent = String(Math.max(0, parseInt(trashCountEl.textContent, 10) - 1));
  };
  tr.addEventListener('animationend', finish, { once: true });
  setTimeout(() => { if (tr.isConnected) finish(); }, 220);

  toast(`"${displayTitle(f)}" restored to library.`, 'success');
  loadFiles();
}

function enterTrashDeleteConfirm(tr, f, actionsWrap) {
  actionsWrap.replaceChildren();

  const label = document.createElement('span');
  label.className = 'delete-confirm-label';
  label.textContent = 'Delete forever?';

  const cancel = document.createElement('button');
  cancel.className = 'btn btn-neutral btn-sm';
  cancel.textContent = 'Cancel';

  const confirm = document.createElement('button');
  confirm.className = 'btn btn-destructive-solid btn-sm';
  confirm.textContent = 'Delete forever';

  const restore = () => {
    actionsWrap.replaceChildren(buildTrashActions(tr, f));
    actionsWrap.querySelector('button')?.focus();
  };

  cancel.addEventListener('click', restore);
  confirm.addEventListener('click', () => doTrashHardDelete(tr, f, actionsWrap));
  actionsWrap.addEventListener('keydown', e => {
    if (e.key === 'Escape') { e.stopPropagation(); restore(); }
  });

  actionsWrap.append(label, cancel, confirm);
  cancel.focus();
}

async function doTrashHardDelete(tr, f, wrap) {
  tr.setAttribute('aria-busy', 'true');
  wrap.querySelectorAll('button').forEach(b => (b.disabled = true));
  try {
    const res = await fetch(`${API}/api/admin/trash/${encodeURIComponent(f.hash)}`, {
      method: 'DELETE',
    });
    if (handleAuthError(res)) { tr.removeAttribute('aria-busy'); return; }
    const data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
  } catch (err) {
    tr.removeAttribute('aria-busy');
    wrap.replaceChildren(buildTrashActions(tr, f));
    toast(`Couldn't delete "${displayTitle(f)}": ${err.message}`, 'error');
    return;
  }

  tr.classList.add('row-removing');
  const finish = () => {
    tr.remove();
    trashCountEl.textContent = String(Math.max(0, parseInt(trashCountEl.textContent, 10) - 1));
  };
  tr.addEventListener('animationend', finish, { once: true });
  setTimeout(() => { if (tr.isConnected) finish(); }, 220);

  toast(`"${displayTitle(f)}" permanently deleted.`, 'success');
}

// ── Boot ──────────────────────────────────────────────────────────────────────
(async function boot() {
  const identity = await initAuth();
  if (identity) {
    applyPermissions(identity);
    if (canManageUsers) {
      await loadRoles();
      await loadUsersAdmin(); // populates allUsers (shared with the group picker)
      loadGroups();
      loadAutoDerive();
    }
  }
  loadFiles();
  loadTrash();
})();
