import { gatePage, PAGE_PERMS } from './auth.js';
import { createMineList } from './mine-list.js';
import { getController } from './player-controller.js';
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
let canEditMeta = false;          // metadata.edit (mirrors ctrl; passed to mine-list)
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
  fileList = createMineList({
    API,
    preview: (tracks, idx) => previewPlay(tracks, idx),
    canEditMeta,
    onCount: updateMineCount,
  });
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
// The owner's non-approved appearances (GET /api/my/uploads) are rendered by the
// bespoke staging list (mine-list.js), grouped by review state — returned (with
// the moderator's note), drafts, and locked submitted rows. This module only
// wires it up (see init) and keeps the tab's count badge in sync.

// updateMineCount refreshes the "My uploads" tab badge; passed to mine-list.js as
// its onCount hook and called on load/removal.
function updateMineCount(n) {
  if (!mineCountEl) return;
  mineCountEl.textContent = String(n);
  mineCountEl.hidden = n === 0;
}
