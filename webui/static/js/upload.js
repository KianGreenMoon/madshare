import { gatePage, PAGE_PERMS } from './auth.js';
import { createFileList } from './file-list.js';
import { getController } from './player-controller.js';
import { showToast } from './toast.js';
import { getUploadController } from './upload-controller.js';

// Upload page (/upload, /admin/upload) — the VIEW.
//
// The upload engine (queue, hashing, XHRs, pump, per-row rendering) lives in the
// document-lifetime singleton upload-controller.js, so uploads keep running and the
// queue survives in-shell navigation. This module owns only the swappable chrome —
// drop-zone, parallel-uploads slider, Upload/Clear buttons, the Upload⇄My-uploads
// tabs, and the "My uploads" staging list — re-attaching the controller's queue
// <ul> on entry and reflecting its events. See docs/ui/shells.md ("Uploads survive
// in-shell navigation") and docs/api/upload.md.

const API = document.querySelector('meta[name="api-url"]')?.content || '';

// previewPlay is the "My uploads" preview sink. Default = the listening shell's
// persistent player; the admin-shell host (/admin/upload) passes its own page-local
// player via init({ preview }). See docs/ui/shells.md.
let previewPlay = (tracks, idx) => getController().setQueue(tracks, idx);

// ── DOM refs (the swappable chrome; assigned by queryRefs() in init) ──────────
// uploadQueueList is NOT here — that <ul> belongs to the controller (persistent)
// and is re-parented into the page by attachQueueList(). Its id is distinct from
// the playback queue panel's #queueList so the two never collide on this page.
let dropZone, fileInput, folderInput, addMusicBtn, addMenu, chooseFilesBtn, chooseFolderBtn;
let workersRange, workersValue, startBtn, clearBtn, queueEmpty;
let srStatus, precheckToggle;
let tabBtnUpload, tabBtnMine, uploadPane, minePane, mineCountEl, mineFileListEl;

function queryRefs() {
  dropZone        = document.getElementById('dropZone');
  fileInput       = document.getElementById('fileInput');
  folderInput     = document.getElementById('folderInput');
  addMusicBtn     = document.getElementById('addMusic');
  addMenu         = document.getElementById('addMenu');
  chooseFilesBtn  = document.getElementById('chooseFiles');
  chooseFolderBtn = document.getElementById('chooseFolder');
  workersRange    = document.getElementById('workersRange');
  workersValue    = document.getElementById('workersValue');
  startBtn        = document.getElementById('startUpload');
  clearBtn        = document.getElementById('clearQueue');
  queueEmpty      = document.getElementById('queueEmpty');
  srStatus        = document.getElementById('srStatus');
  precheckToggle  = document.getElementById('precheckToggle');
  tabBtnUpload    = document.getElementById('tabBtnUpload');
  tabBtnMine      = document.getElementById('tabBtnMine');
  uploadPane      = document.getElementById('uploadPane');
  minePane        = document.getElementById('minePane');
  mineCountEl     = document.getElementById('mineCount');
  mineFileListEl  = document.getElementById('mineFileList');
}

// ── State (view-local) ────────────────────────────────────────────────────────
let ctrl = null;                  // the shared upload controller
let canEditMeta = false;          // metadata.edit (mirrors ctrl; drives mineScope)
let fileList = null;              // the "My uploads" component (file-list.js)
let wireAbort = null;            // AbortController for this activation's chrome listeners
let unsubs = [];                 // controller event unsubscribers

// ── Lifecycle (driven by shell.js / admin boot) ──────────────────────────────
export async function init(opts = {}) {
  if (opts.preview) previewPlay = opts.preview;
  ctrl = getUploadController();
  queryRefs();
  attachQueueList();
  wireAbort = new AbortController();
  wire(wireAbort.signal);
  subscribe();
  await ctrl.ensureConfig();
  canEditMeta = ctrl.getCanEditMeta();

  // Parallel-uploads slider: range from config, value from the live cap (a prior
  // 429 backoff may have lowered it while we were on another page).
  const { maxWorkers } = ctrl.defaults();
  const cap = ctrl.getWorkerCap();
  workersRange.max = String(Math.max(1, maxWorkers));
  workersRange.value = String(cap);
  workersValue.textContent = String(cap);
  if (precheckToggle) precheckToggle.checked = ctrl.precheckEnabled();

  syncChrome();                  // reflect any in-flight run into the fresh chrome

  // Block the page for anyone without upload rights (the API enforces it too).
  if (!gatePage(PAGE_PERMS.upload)) return;
  fileList = createFileList(mineScope());
  fileList.mount(mineFileListEl);
}

export function teardown() {
  wireAbort?.abort();
  wireAbort = null;
  for (const u of unsubs) { try { u(); } catch { /* ignore */ } }
  unsubs = [];
  fileList?.destroy();
  fileList = null;
  // The upload controller is intentionally NOT torn down: uploads keep running and
  // the queue (with its <ul>) survives navigation. See docs/ui/shells.md.
}

// attachQueueList re-parents the controller's persistent <ul> into the freshly
// swapped page, replacing the server-rendered placeholder of the same id.
function attachQueueList() {
  const placeholder = document.getElementById('uploadQueueList');
  const ul = ctrl.uploadQueueListEl;
  if (placeholder && placeholder !== ul) placeholder.replaceWith(ul);
}

// ── Controller events → chrome ────────────────────────────────────────────────
function subscribe() {
  const add = (evt, fn) => unsubs.push(ctrl.on(evt, fn));
  add('change', syncChrome);
  add('announce', announce);
  add('workercap', (n) => {
    if (!workersRange) return;
    workersRange.value = String(n);
    workersValue.textContent = String(n);
  });
  add('staged', () => fileList?.reload());
}

function syncChrome() {
  if (startBtn)   startBtn.disabled = ctrl.isRunning() || !ctrl.hasPending();
  if (clearBtn)   clearBtn.disabled = ctrl.size() === 0;
  if (queueEmpty) queueEmpty.style.display = ctrl.size() ? 'none' : '';
}

function announce(msg) { if (srStatus) srStatus.textContent = msg; }

// ── Chrome wiring (listeners call into the controller) ────────────────────────
function wire(signal) {
  // Parallel-uploads slider
  workersRange.addEventListener('input', () => {
    const n = Number(workersRange.value);
    workersValue.textContent = String(n);
    ctrl.setWorkerCap(n);
  }, { signal });

  // One-button "Add music" → menu (Choose files… / Choose folder…)
  addMusicBtn.addEventListener('click', () => toggleAddMenu(), { signal });
  chooseFilesBtn.addEventListener('click', () => { closeAddMenu(); fileInput.click(); }, { signal });
  chooseFolderBtn.addEventListener('click', () => { closeAddMenu(); folderInput.click(); }, { signal });
  document.addEventListener('click', (e) => {
    if (!addMenu.hidden && !e.target.closest('.add-menu')) closeAddMenu();
  }, { signal });
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') closeAddMenu(); }, { signal });

  fileInput.addEventListener('change', () => { addFileList(fileInput.files); fileInput.value = ''; }, { signal });
  folderInput.addEventListener('change', () => { addFileList(folderInput.files); folderInput.value = ''; }, { signal });

  // Drop zone (click opens the files picker; drag/drop accepts files OR a folder)
  dropZone.addEventListener('click', (e) => {
    if (e.target.closest('button')) return;
    fileInput.click();
  }, { signal });
  dropZone.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); fileInput.click(); }
  }, { signal });
  ['dragenter', 'dragover'].forEach(evt =>
    dropZone.addEventListener(evt, (e) => { e.preventDefault(); dropZone.classList.add('is-dragover'); }, { signal }));
  ['dragleave', 'dragend'].forEach(evt =>
    dropZone.addEventListener(evt, (e) => {
      if (evt === 'dragleave' && dropZone.contains(e.relatedTarget)) return;
      dropZone.classList.remove('is-dragover');
    }, { signal }));
  dropZone.addEventListener('drop', onDrop, { signal });

  // Run controls
  startBtn.addEventListener('click', () => ctrl.start(), { signal });
  clearBtn.addEventListener('click', () => ctrl.clear(), { signal });

  // Pre-upload precheck toggle (persisted, default on)
  if (precheckToggle) {
    precheckToggle.addEventListener('change', () => ctrl.setPrecheck(precheckToggle.checked), { signal });
  }

  // Upload ⇄ My uploads tabs
  tabBtnUpload.addEventListener('click', () => selectTab('upload'), { signal });
  tabBtnMine.addEventListener('click', () => selectTab('mine'), { signal });
}

function selectTab(which) {
  const mineActive = which === 'mine';
  uploadPane.hidden = mineActive;
  minePane.hidden = !mineActive;
  tabBtnUpload.classList.toggle('is-active', !mineActive);
  tabBtnMine.classList.toggle('is-active', mineActive);
  tabBtnUpload.setAttribute('aria-selected', String(!mineActive));
  tabBtnMine.setAttribute('aria-selected', String(mineActive));
  if (mineActive) fileList?.reload();             // refresh on entry
}

function toggleAddMenu() {
  const open = addMenu.hidden;
  addMenu.hidden = !open;
  addMusicBtn.setAttribute('aria-expanded', String(open));
}
function closeAddMenu() {
  addMenu.hidden = true;
  addMusicBtn.setAttribute('aria-expanded', 'false');
}

// ── File intake (DOM-side; hands plain { file, relPath } to the controller) ───
// Drop must traverse DROPPED FOLDERS. dataTransfer.files only contains top-level
// entries — never a directory's contents — so a dropped folder would otherwise add
// nothing. We walk directories via webkitGetAsEntry(), falling back to
// dataTransfer.files when the entries API is absent.
async function onDrop(e) {
  e.preventDefault();
  dropZone.classList.remove('is-dragover');
  const dt = e.dataTransfer;
  if (!dt) return;

  // Capture entries + files SYNCHRONOUSLY: the DataTransfer goes inert once this
  // handler yields at the first await.
  let entries = null;
  if (dt.items && dt.items.length && typeof dt.items[0].webkitGetAsEntry === 'function') {
    entries = [];
    for (const it of dt.items) {
      const entry = it.webkitGetAsEntry();
      if (entry) entries.push(entry);
    }
  }
  const flat = Array.from(dt.files || []);

  if (entries && entries.length) {
    const collected = [];
    for (const entry of entries) await readEntry(entry, '', collected);
    if (collected.length) { ctrl.addEntries(collected); return; }
  }
  // Fallback: no directory entries — plain files only.
  ctrl.addEntries(flat.map(f => ({ file: f, relPath: f.webkitRelativePath || f.name })));
}

// readEntry walks a FileSystemEntry, collecting { file, relPath } pairs. relPath
// preserves the dropped folder structure so grouping + cover co-location work
// exactly as they do for a webkitdirectory pick.
function readEntry(entry, prefix, out) {
  return new Promise((resolve) => {
    if (entry.isFile) {
      entry.file(
        (file) => { out.push({ file, relPath: prefix + entry.name }); resolve(); },
        () => resolve(),
      );
    } else if (entry.isDirectory) {
      const reader = entry.createReader();
      const dirPrefix = `${prefix + entry.name}/`;
      const readBatch = () => reader.readEntries(async (batch) => {
        if (!batch.length) { resolve(); return; }
        for (const child of batch) await readEntry(child, dirPrefix, out);
        readBatch();
      }, () => resolve());
      readBatch();
    } else {
      resolve();
    }
  });
}

// addFileList adapts a FileList (from the file/folder <input>s) to the controller.
// webkitRelativePath carries the folder structure for a directory pick.
function addFileList(files) {
  ctrl.addEntries(Array.from(files || []).map(f => ({ file: f, relPath: f.webkitRelativePath || f.name })));
}

// ── My uploads tab (staging / review bucket) ────────────────────────────────
// The owner's non-approved files (GET /api/my/uploads), rendered through the
// shared file-management component (file-list.js) grouped by review state:
// returned files (with the moderator's note), editable drafts, and locked
// submitted rows. Draft/returned rows edit via the owner-scoped endpoint and
// send to approval; every row previews through the shared shell player.

const MINE_EDITABLE = f => f.state === 'draft' || f.state === 'returned';
const mineTitle = f => f.title || f.filename || 'this file';

function mineScope() {
  return {
    title: 'My uploads',
    desc: 'Files you uploaded that aren’t in the library yet. Check their tags (Edit), then send '
        + 'them to approval — a moderator reviews them, or, if you have moderation rights, they '
        + 'publish immediately. A file sent back shows the moderator’s note; Remove discards one '
        + 'you don’t want to publish.',
    emptyText: 'Nothing staged. Files you upload appear here for a metadata check before they reach the library.',
    columns: ['check', 'title', 'artist', 'album', 'size', 'actions'],
    artistAlbumSort: true,
    allowCoverAdd: true,            // uploaders may add a missing artist/album cover (server enforces add-only)
    allowCoverEdit: canEditMeta,    // replacing an existing cover needs metadata.edit (server enforces it too)
    apiBase: API,
    grouping: {
      kind: 'sections',
      sections: [
        { key: 'returned',  label: 'Returned by a moderator', match: f => f.state === 'returned' },
        { key: 'draft',     label: 'Drafts',                  match: f => f.state === 'draft' },
        { key: 'submitted', label: 'Awaiting review',         match: f => f.state === 'submitted' },
      ],
    },
    selectable: MINE_EDITABLE,
    autoSelect: true,                 // "send the lot unless you untick"
    editable: MINE_EDITABLE,
    editPatchURL: f => `${API}/api/my/uploads/${encodeURIComponent(f.hash)}/metadata`,
    editNote: 'Fix the tags before sending to approval — title, artist and album decide where the track lands in the library.',
    accessEditable: false,            // an uploader sets tags on drafts, not access
    badge: (f, grouped) => grouped && f.state !== 'submitted'
      ? { text: f.state === 'returned' ? 'Returned' : 'Draft', cls: 'is-' + f.state }
      : null,

    rowActions: [
      {
        id: 'remove', label: 'Remove', kind: 'danger',
        confirm: 'inline', confirmPrompt: 'Remove?', confirmLabel: 'Remove',
        show: MINE_EDITABLE,
        run: async f => { await mineDelete(f.hash); showToast(`Removed “${mineTitle(f)}”.`, { type: 'success' }); },
      },
    ],
    bulkActions: [
      { id: 'send',   label: 'Send to approval', kind: 'neutral', run: hashes => mineSend(hashes) },
      { id: 'remove', label: 'Remove selected',  kind: 'danger',  run: hashes => mineRemoveMany(hashes) },
    ],
    bulkApply: (hashes, patch) => mineBulkPatch(hashes, patch),

    onPlay: playMine,
    toast: msg => showToast(msg),

    load: async () => {
      const res = await fetch(`${API}/api/my/uploads`);
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      updateMineCount(Array.isArray(data) ? data.length : 0);
      return data;
    },
  };
}

function updateMineCount(n) {
  if (!mineCountEl) return;
  mineCountEl.textContent = String(n);
  mineCountEl.hidden = n === 0;
}

async function mineDelete(hash) {
  const res = await fetch(`${API}/api/my/uploads/${encodeURIComponent(hash)}`, { method: 'DELETE' });
  const data = await res.json().catch(() => ({}));
  if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
}

async function mineRemoveMany(hashes) {
  let ok = 0, fail = 0;
  for (const hash of hashes) {
    try { await mineDelete(hash); ok++; } catch { fail++; }
  }
  if (fail) showToast(`Removed ${ok}; ${fail} failed.`, { type: 'error' });
  else if (ok) showToast(`Removed ${ok} file${ok === 1 ? '' : 's'}.`, { type: 'success' });
  announce(`Removed ${ok} file${ok === 1 ? '' : 's'}.`);
}

async function mineSend(hashes) {
  const res = await fetch(`${API}/api/my/uploads/submit`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ hashes }),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
  const n = data.submitted ?? hashes.length;
  showToast(data.approved
    ? `Published ${n} file${n === 1 ? '' : 's'} to the library.`
    : `Sent ${n} file${n === 1 ? '' : 's'} for review.`, { type: 'success' });
  // A duplicate-flagged submission never auto-publishes (recordings P3); surface
  // the server's explanation so the uploader knows why it went to review.
  if (data.warning) showToast(data.warning, { type: 'info' });
  announce(data.approved ? 'Published to the library.' : 'Sent for review.');
}

async function mineBulkPatch(hashes, patch) {
  let ok = 0, fail = 0;
  for (const hash of hashes) {
    try {
      const res = await fetch(`${API}/api/my/uploads/${encodeURIComponent(hash)}/metadata`, {
        method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(patch),
      });
      const data = await res.json().catch(() => ({}));
      if (res.ok && data.ok) ok++; else fail++;
    } catch { fail++; }
  }
  if (fail) throw new Error(`updated ${ok}, ${fail} failed`);
}

// playMine queues the visible staging list into the preview sink (the shell player
// by default; a page-local one under the admin shell), starting at the clicked row.
function playMine(entry, visible) {
  const list = (visible && visible.length) ? visible : [entry];
  const tracks = list.map(e => ({
    url: `${API}${e.url}`,
    hash: e.hash,
    title: e.title || e.filename,
    artist: e.artist || '',
    dur: e.duration || undefined,
  }));
  const idx = list.findIndex(e => e.hash === entry.hash);
  previewPlay(tracks, idx < 0 ? 0 : idx);
}
