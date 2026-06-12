import { gatePage, PAGE_PERMS, getIdentity } from './auth.js';
import { createHashPool } from './hash-pool.js';
import { createFileList } from './file-list.js';
import { getController } from './player-controller.js';
import { showToast } from './toast.js';

// Upload page (/upload) controller.
//
// Vanilla ES module, no dependencies. Same-origin: all API calls are relative
// when <meta name="api-url"> is empty (the bundled server). Mirrors app.js for
// the theme handling and API-base patterns.
//
// Phase 5 revision — grouping by TAGS, not folders:
//   - Audio files are uploaded via POST /files/upload; the response echoes the
//     extracted {title, album, artist}. Tracks are grouped into album cards by
//     `artist \x1f album` (NOT by directory prefix), so selecting loose files
//     from different artists no longer collapses them into one bucket.
//   - Cover IMAGE files are never sent to /files/upload (the server takes audio
//     only). They are held as per-folder candidates and attached to the album
//     that the audio tracks IN THE SAME FOLDER resolved to (folder co-location),
//     which is the only signal a tagless image gives us. This kills the old bug
//     where a cover bled onto an unrelated album.
//   - Each album card doubles as a verify/edit panel: with metadata.edit the user
//     can fix the album, artist, and per-track titles, and replace the cover. See
//     docs/plans/upload-and-covers.md §5.

const API = document.querySelector('meta[name="api-url"]')?.content || '';

// Theme + auth are owned by shell.js (this is a shell-native page). This module
// exports init()/teardown(); the shell calls them on entry/exit. Everything that
// touches the swappable <main> is (re)wired in init() under an AbortController.

// previewPlay is the "My uploads" preview sink. Default = the listening shell's
// persistent player (this page lives in that shell). The admin-shell host
// (/admin/upload) has no shell player, so it passes its own page-local player
// via init({ preview }). See docs/plans/cross-shell-upload.md.
let previewPlay = (tracks, idx) => getController().setQueue(tracks, idx);

// ── DOM refs (assigned by queryRefs() in init — they live in <main>) ─────────
let dropZone, fileInput, folderInput, addMusicBtn, addMenu, chooseFilesBtn, chooseFolderBtn;
let workersRange, workersValue, startBtn, clearBtn, queueList, queueEmpty;
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
  queueList       = document.getElementById('queueList');
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

// ── State ───────────────────────────────────────────────────────────────────
// queue:   every intake file (audio uploaded, image = cover candidate, other = skipped)
// groups:  album identities keyed by `artist \x1f album` (formed from upload
//          responses; headless — only feeds folder-cover co-location)
// folders: per-directory bookkeeping for cover co-location
let nextId = 1;
const queue   = [];           // QueueItem[]
const groups  = new Map();    // groupKey -> Group
const folders = new Map();    // dir -> { dir, audio: QueueItem[], images: QueueItem[] }
let activeWorkers = 0;
let workerCap = 3;
let running = false;
let canEditMeta = false;      // metadata.edit permission (set after /auth/me)
const activeXhrs = new Set(); // in-flight uploads, aborted on teardown
let wireAbort = null;         // AbortController for this activation's listeners

// ── My uploads (staging / review bucket) ────────────────────────────────────
let fileList = null;          // the shared file-management component (file-list.js)
let stagedThisRun = 0;        // freshly staged uploads in the current run (for the nudge)

// ── Pre-upload hash-precheck (3b) ────────────────────────────────────────────
// When enabled (default), each file is SHA-256'd in a worker pool and checked
// against the server; an already-present file is skipped instead of re-uploaded.
// Advisory only — the server re-hashes and dedupes on receipt regardless.
const PRECHECK_KEY = 'madshare-upload-precheck';
const precheckEnabled = () => localStorage.getItem(PRECHECK_KEY) !== 'off'; // default ON
let hashPool = null;
const getHashPool = () => (hashPool ||= createHashPool());
let trashPolicy = 'reupload_restores'; // from /api/ui/config; how a trashed match is handled

const UNSORTED_KEY = '\u0000unsorted';
const COVER_STEMS  = new Set(['cover', 'folder', 'front', 'albumart', 'artwork', 'album']);
const IMAGE_MIMES  = new Set(['image/jpeg', 'image/png']);

// acceptedAudio maps an accepted extension to its canonical MIME. It is the
// client side of the server's acceptedAudioTypes (api/handlers.go), fetched from
// /api/ui/config in loadUIConfig() so the two never drift; this built-in copy is
// the fallback when the config is unavailable (older server / offline). The
// allow-check is extension-based (browsers leave file.type empty for FLAC/M4A/
// OPUS, so MIME alone is unreliable); the MIME value is what we send as the
// upload's Content-Type so the server stores it correctly.
// Advisory only — the server re-validates every upload. No UI toggle.
// See docs/plans/upload-type-precheck.md.
let acceptedAudio = {
  '.mp3': 'audio/mpeg', '.ogg': 'audio/ogg', '.flac': 'audio/flac',
  '.wav': 'audio/wav', '.mp4': 'audio/mp4', '.m4a': 'audio/mp4',
  '.aac': 'audio/aac', '.opus': 'audio/opus',
};
// extOf returns the lowercased dotted extension of a path ("" if none).
const extOf = (path) => {
  const m = /\.[^./\\]+$/.exec((path || '').toLowerCase());
  return m ? m[0] : '';
};
const audioMimeFor = (path) => acceptedAudio[extOf(path)];

// ── Lifecycle (driven by shell.js) ──────────────────────────────────────────
export async function init(opts = {}) {
  if (opts.preview) previewPlay = opts.preview;
  queryRefs();
  wireAbort = new AbortController();
  wire(wireAbort.signal);
  syncEmptyStates();
  updateButtons();
  await loadUIConfig();
  // Block the page for anyone without upload rights (the API enforces it too).
  // initAuth already ran in the shell, so the identity is available now.
  if (!gatePage(PAGE_PERMS.upload)) return;
  const identity = getIdentity();
  canEditMeta = Array.isArray(identity?.permissions) && identity.permissions.includes('metadata.edit');
  fileList = createFileList(mineScope());
  fileList.mount(mineFileListEl);
}

export function teardown() {
  wireAbort?.abort();
  wireAbort = null;
  for (const xhr of activeXhrs) { try { xhr.abort(); } catch { /* ignore */ } }
  activeXhrs.clear();
  if (hashPool) { hashPool.terminate(); hashPool = null; }
  fileList?.destroy();
  fileList = null;
  // Reset for a clean next entry (the <main> is re-rendered fresh by the shell).
  queue.length = 0;
  groups.clear();
  folders.clear();
  nextId = 1;
  activeWorkers = 0;
  running = false;
  stagedThisRun = 0;
}

// wire attaches every listener that targets the swappable <main>, under the
// activation's AbortController so teardown() removes them all at once.
function wire(signal) {
  // Worker slider
  workersRange.addEventListener('input', () => {
    workerCap = Number(workersRange.value);
    workersValue.textContent = String(workerCap);
    if (running) pump();
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
  startBtn.addEventListener('click', () => { running = true; startBtn.disabled = true; pump(); }, { signal });
  clearBtn.addEventListener('click', clearQueue, { signal });

  // Pre-upload precheck toggle (persisted, default on)
  if (precheckToggle) {
    precheckToggle.checked = precheckEnabled();
    precheckToggle.addEventListener('change', () => {
      localStorage.setItem(PRECHECK_KEY, precheckToggle.checked ? 'on' : 'off');
    }, { signal });
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

async function loadUIConfig() {
  try {
    const res = await fetch(`${API}/api/ui/config`);
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const cfg = await res.json();
    const def = cfg?.upload?.default_parallel_workers ?? 3;
    const max = cfg?.upload?.max_parallel_workers ?? 10;
    workersRange.max = String(Math.max(1, max));
    workersRange.value = String(Math.min(Math.max(1, def), Math.max(1, max)));
    if (cfg?.trash_restore_policy) trashPolicy = cfg.trash_restore_policy;
    // Adopt the server's accepted-audio allow-list verbatim (keeps the client's
    // skip decision and outgoing Content-Type in lockstep with the server).
    if (cfg?.accepted_audio && typeof cfg.accepted_audio === 'object'
        && Object.keys(cfg.accepted_audio).length) {
      acceptedAudio = cfg.accepted_audio;
    }
  } catch (err) {
    console.warn('UI config unavailable, using defaults:', err);
  }
  workerCap = Number(workersRange.value);
  workersValue.textContent = String(workerCap);
}

// ── File intake ─────────────────────────────────────────────────────────────
// Drop must traverse DROPPED FOLDERS. dataTransfer.files only contains top-level
// entries — never a directory's contents — so a dropped folder would otherwise
// add nothing. We use the webkitGetAsEntry() filesystem API to walk directories
// recursively, falling back to dataTransfer.files when the entries API is absent.
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
    if (collected.length) { addEntries(collected); return; }
  }
  // Fallback: no directory entries — plain files only.
  addEntries(flat.map(f => ({ file: f, relPath: f.webkitRelativePath || f.name })));
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
      // readEntries yields in batches; keep calling until it returns empty.
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

// addFileList adapts a FileList (from the file/folder <input>s) to addEntries.
// webkitRelativePath carries the folder structure for a directory pick.
function addFileList(fileList) {
  addEntries(Array.from(fileList || []).map(f => ({ file: f, relPath: f.webkitRelativePath || f.name })));
}

function addEntries(entries) {
  if (!entries.length) return;

  for (const { file, relPath } of entries) {
    const path = relPath || file.name;
    const kind = classify(file, path);
    const item = {
      id: nextId++,
      file,
      relPath: path,
      dir: dirOf(path),
      kind,                    // 'audio' | 'image' | 'other'
      state: kind === 'audio' ? 'pending' : (kind === 'image' ? 'cover' : 'skipped'),
      hash: '', title: '', album: '', artist: '',
      groupKey: '',
      el: null,
    };
    queue.push(item);
    renderQueueItem(item);

    const folder = ensureFolder(item.dir);
    if (kind === 'audio') folder.audio.push(item);
    else if (kind === 'image') folder.images.push(item);
  }

  syncEmptyStates();
  updateButtons();
  const audioCount = queue.filter(it => it.kind === 'audio').length;
  announce(`${audioCount} audio file${audioCount === 1 ? '' : 's'} ready to upload.`);
}

// classify decides how a file is handled. Audio is gated by EXTENSION against
// the server's accepted-audio allow-list (acceptedAudio) — browsers leave
// file.type empty for FLAC/M4A/OPUS, and accepting any audio/* MIME would queue
// files the server then rejects (e.g. .weba). Matching the server here means a
// queued audio file is one the server will actually accept.
function classify(file, relPath) {
  const name = (relPath || file.name || '').toLowerCase();
  const type = file.type || '';
  if (audioMimeFor(name)) return 'audio';
  if (IMAGE_MIMES.has(type) || /\.(jpe?g|png)$/i.test(name)) return 'image';
  return 'other';
}

// Directory of a path ("" for a loose file with no folder).
function dirOf(relPath) {
  return relPath.split('/').slice(0, -1).join('/');
}

function baseName(relPath) {
  const parts = relPath.split('/');
  return parts[parts.length - 1];
}

function ensureFolder(dir) {
  let f = folders.get(dir);
  if (!f) { f = { dir, audio: [], images: [] }; folders.set(dir, f); }
  return f;
}

// ── Queue rendering ─────────────────────────────────────────────────────────
function renderQueueItem(item) {
  const li = document.createElement('li');
  li.className = 'queue-item';
  li.dataset.state = item.state;

  const name = document.createElement('span');
  name.className = 'queue-item__name';
  name.textContent = item.relPath;          // textContent — never innerHTML with user data

  const size = document.createElement('span');
  size.className = 'queue-item__size';
  size.textContent = formatBytes(item.file.size);

  const bar = document.createElement('div');
  bar.className = 'queue-item__bar';
  bar.setAttribute('role', 'progressbar');
  bar.setAttribute('aria-valuemin', '0');
  bar.setAttribute('aria-valuemax', '100');
  bar.setAttribute('aria-valuenow', '0');
  bar.setAttribute('aria-label', `Upload progress for ${item.relPath}`);
  const fill = document.createElement('div');
  fill.className = 'queue-item__fill';
  bar.appendChild(fill);

  const msg = document.createElement('div');
  msg.className = 'queue-item__msg';
  msg.setAttribute('role', 'status');

  li.append(name, size, bar, msg);
  item.el = { li, bar, fill, msg };
  queueList.appendChild(li);

  // Non-audio rows carry an explanatory note instead of a progress lifecycle.
  // Folder cover images are co-located server-side only with metadata.edit
  // (the cover endpoint is gated on it) — say so instead of pretending.
  if (item.kind === 'image' && canEditMeta) setItemState(item, 'cover', 'Cover image — applied to its album after upload.');
  else if (item.kind === 'image') setItemState(item, 'skipped', 'Cover image — needs the metadata permission; skipped.');
  else if (item.kind === 'other') setItemState(item, 'skipped', 'Skipped — not an accepted audio format.');
}

function setItemState(item, state, message) {
  item.state = state;
  if (!item.el) return;
  item.el.li.dataset.state = state;
  item.el.msg.textContent = '';
  if (message) item.el.msg.append(document.createTextNode(message));
}

function setProgress(item, pct) {
  if (!item.el) return;
  const v = Math.round(pct);
  item.el.fill.style.width = `${v}%`;
  item.el.bar.setAttribute('aria-valuenow', String(v));
}

function addRetry(item) {
  if (!item.el) return;
  const btn = document.createElement('button');
  btn.type = 'button';
  btn.className = 'retry-btn';
  btn.textContent = 'Retry';
  btn.addEventListener('click', () => {
    setItemState(item, 'pending');
    setProgress(item, 0);
    item.el.li.querySelector('.retry-btn')?.remove();
    if (!queue.includes(item)) queue.push(item);
    updateButtons();
    if (running) pump();
  });
  item.el.msg.appendChild(btn);
}

// ── Upload run ──────────────────────────────────────────────────────────────
// (start/clear listeners are wired in wire(); clearQueue is the handler.)
function clearQueue() {
  // Remove everything not currently uploading; stop all polling.
  for (let i = queue.length - 1; i >= 0; i--) {
    if (queue[i].state !== 'uploading') {
      queue[i].el?.li.remove();
      queue.splice(i, 1);
    }
  }
  groups.clear();
  folders.clear();
  syncEmptyStates();
  updateButtons();
}

// pump fills available worker slots with pending AUDIO files, up to workerCap.
// Images/other never enter the upload loop (they are not pending).
function pump() {
  while (running && activeWorkers < workerCap) {
    const item = queue.find(it => it.state === 'pending');
    if (!item) break;
    activeWorkers++;
    uploadItem(item).finally(() => {
      activeWorkers--;
      if (queue.some(it => it.state === 'pending')) pump();
      else if (activeWorkers === 0) finishRun();
    });
  }
  if (running && activeWorkers === 0 && !queue.some(it => it.state === 'pending')) finishRun();
}

function finishRun() {
  if (!running) return;                        // idempotent guard
  running = false;
  updateButtons();
  resolveFolderCovers();                       // attach covers by co-location
  announce('Uploads finished. Review the albums below.');
  // Staged uploads need a second step: nudge towards the My uploads tab.
  if (stagedThisRun > 0) {
    const n = stagedThisRun;
    stagedThisRun = 0;
    fileList?.reload();
    showToast(`${n} file${n === 1 ? '' : 's'} staged — open “My uploads” to check tags and send to approval.`, { type: 'success' });
    announce(`${n} file${n === 1 ? '' : 's'} staged in My uploads.`);
  }
}

// uploadItem runs the advisory precheck (hash in a worker → /api/files/check)
// and skips an already-present file; otherwise it uploads. A "trashed" match is
// uploaded normally — the server restores it on receipt (existing behavior).
// Any precheck failure falls through to a plain upload (the server dedupes).
async function uploadItem(item) {
  if (precheckEnabled()) {
    try {
      setItemState(item, 'checking', 'Checking…');
      const hash = await getHashPool().hashFile(item.file);
      item.hash = hash;
      const res = await fetch(`${API}/api/files/check`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ hash }),
      });
      if (res.ok) {
        const { status } = await res.json();
        if (status === 'present') {
          setProgress(item, 100);
          setItemState(item, 'done', 'Already in library — skipped');
          return;                                  // skip the upload
        }
        if (status === 'pending') {
          setProgress(item, 100);
          setItemState(item, 'done', 'Already uploaded — awaiting review');
          return;                                  // skip the upload
        }
        if (status === 'trashed') {
          if (trashPolicy === 'inform') {
            setItemState(item, 'waiting', 'Already on the server (in Trash) — ask an admin to restore.');
            return;                                // don't upload
          }
          if (trashPolicy === 'uploader_restore') {
            setItemState(item, 'waiting', 'In Trash. ');
            addRestoreButton(item);
            return;                                // user restores via the button
          }
          // reupload_restores → fall through and upload (server restores it)
        }
        // 'absent' (or reupload_restores trashed) → upload normally
      }
    } catch { /* hashing/check failed → upload normally */ }
  }
  return uploadXhr(item);
}

// addRestoreButton offers an inline Restore (uploader_restore policy) that
// un-trashes the file by hash instead of re-uploading the bytes.
function addRestoreButton(item) {
  if (!item.el) return;
  const btn = document.createElement('button');
  btn.type = 'button';
  btn.className = 'retry-btn';
  btn.textContent = 'Restore';
  btn.addEventListener('click', async () => {
    btn.disabled = true;
    try {
      const res = await fetch(`${API}/api/files/${encodeURIComponent(item.hash)}/restore`, { method: 'POST' });
      const data = await res.json().catch(() => ({}));
      if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
      if (data.staged) { setItemState(item, 'done', 'Restored — staged in My uploads'); fileList?.reload(); }
      else setItemState(item, 'done', 'Restored to library');
    } catch (err) {
      btn.disabled = false;
      announce(`Restore failed: ${err.message}`);
    }
  });
  item.el.msg.appendChild(btn);
}

function uploadXhr(item) {
  return new Promise((resolve) => {
    setItemState(item, 'uploading');
    setProgress(item, 0);

    // Send the canonical audio MIME as the part's Content-Type. Browsers leave
    // File.type empty for FLAC/M4A/OPUS, which would otherwise go out as
    // application/octet-stream; the server keys on the extension regardless, but
    // an accurate type means it stores the right MIME. Wrap in a retyped File
    // only when needed. See docs/plans/upload-type-precheck.md.
    const want = audioMimeFor(item.relPath);
    const blob = (want && item.file.type !== want)
      ? new File([item.file], item.file.name, { type: want })
      : item.file;
    const form = new FormData();
    form.append('file', blob, item.relPath);

    const xhr = new XMLHttpRequest();
    activeXhrs.add(xhr);
    const done = () => { activeXhrs.delete(xhr); resolve(); };
    xhr.open('POST', `${API}/files/upload`);
    xhr.upload.onprogress = (e) => { if (e.lengthComputable) setProgress(item, (e.loaded / e.total) * 100); };
    xhr.onabort = done;

    xhr.onload = () => {
      let body = null;
      try { body = JSON.parse(xhr.responseText); } catch { /* non-JSON */ }

      if (xhr.status === 429) { handleBackoff(item); done(); return; }

      if (xhr.status >= 200 && xhr.status < 300 && body?.ok !== false) {
        setProgress(item, 100);
        let msg = 'Uploaded';
        if (body?.existed) {
          if (body?.restored) msg = body?.pending ? 'Restored — staged in My uploads' : 'Restored to library';
          else msg = body?.pending ? 'Already uploaded — awaiting review' : 'Already present';
        } else if (body?.pending) msg = 'Uploaded — staged in My uploads';
        setItemState(item, 'done', msg);
        onUploaded(item, body || {});
        done();
        return;
      }

      if (xhr.status === 415 || xhr.status === 400) {
        setItemState(item, 'rejected', body?.error || `Rejected (HTTP ${xhr.status})`);
      } else {
        setItemState(item, 'error', body?.error || `Upload failed (HTTP ${xhr.status})`);
        addRetry(item);
      }
      done();
    };
    xhr.onerror = () => { setItemState(item, 'error', 'Network error'); addRetry(item); done(); };
    xhr.send(form);
  });
}

// handleBackoff reacts to a 429 by lowering concurrency and re-queueing.
function handleBackoff(item) {
  if (workerCap > 1) {
    workerCap--;
    workersRange.value = String(workerCap);
    workersValue.textContent = String(workerCap);
  }
  console.log(`Server limit hit — workers reduced to ${workerCap}`);
  showToast(`Server busy — workers reduced to ${workerCap}.`, { type: 'status' });
  announce(`Workers reduced to ${workerCap}.`);

  setItemState(item, 'pending');
  setProgress(item, 0);
  const idx = queue.indexOf(item);
  if (idx !== -1) queue.splice(idx, 1);
  queue.unshift(item);                          // re-queue at the FRONT
}

// onUploaded records the tag-derived metadata and slots the track into its album
// group (by tags). A deduped file (existed:true) carries no tags, so it cannot be
// grouped — it stays in the queue marked "already present".
function onUploaded(item, body) {
  item.hash = body.hash || '';
  // Staged: a fresh pending upload, or a trashed file the re-upload restored
  // into the staging area (the server re-stages approved-then-trashed files).
  if (body.pending && (!body.existed || body.restored)) stagedThisRun++;
  if (body.existed) return;

  item.title = body.title || '';
  item.album = body.album || '';
  item.artist = body.artist || '';
  item.groupKey = groupKeyFor(item.album, item.artist);
  const g = ensureGroup(item.groupKey, item.album, item.artist);
  g.items.push(item);
}

function groupKeyFor(album, artist) {
  return album ? `${artist}\u001f${album}` : UNSORTED_KEY;
}

// ── Album groups ─────────────────────────────────────────────────────────────
// Headless bookkeeping: which album identity each uploaded track resolved to.
// Its only consumer is folder-cover co-location (resolveFolderCovers below) —
// tag verification/editing lives on the "My uploads" tab, and there is no
// album-card UI on this page anymore.
function ensureGroup(key, album, artist) {
  let g = groups.get(key);
  if (g) return g;
  g = { key, album: album || '', artist: artist || '', items: [] };
  groups.set(key, g);
  return g;
}

// ── Cover co-location ───────────────────────────────────────────────────────
// After uploads settle, attach each folder's cover candidate to the album its
// audio tracks resolved to. A tagless image has no album of its own, so the
// folder it sits in is the only link.
function resolveFolderCovers() {
  for (const folder of folders.values()) {
    const cover = pickCoverFile(folder);
    if (!cover) continue;

    // Tally the album groups the folder's uploaded tracks belong to.
    const tally = new Map();    // groupKey -> count
    for (const it of folder.audio) {
      if (it.state !== 'done' || !it.groupKey || it.groupKey === UNSORTED_KEY) continue;
      tally.set(it.groupKey, (tally.get(it.groupKey) || 0) + 1);
    }
    if (tally.size === 0) continue;             // no album resolved → leave unassigned

    // Majority album wins; flag ambiguity when the folder spanned several.
    let bestKey = null, bestN = -1;
    for (const [k, n] of tally) if (n > bestN) { bestKey = k; bestN = n; }
    const g = groups.get(bestKey);
    if (!g) continue;
    if (canEditMeta) postCover(g, cover);       // explicit file beats embedded art
    // Without metadata.edit the cover endpoint would 403 — the image was
    // marked "skipped" at intake (see renderQueueItem).
  }
}

// pickCoverFile chooses a folder's cover: the largest cover-named JPEG/PNG, or a
// lone image of any name if the folder has exactly one.
function pickCoverFile(folder) {
  const imgs = folder.images;
  if (!imgs.length) return null;
  let best = null;
  for (const it of imgs) {
    const stem = baseName(it.relPath).replace(/\.[^.]+$/, '').toLowerCase();
    if (!COVER_STEMS.has(stem)) continue;
    if (!best || it.file.size > best.size) best = it.file;
  }
  if (best) return best;
  return imgs.length === 1 ? imgs[0].file : null;
}

// postCover POSTs an image to the album cover endpoint. Variants are generated
// asynchronously server-side; without the album cards there is nothing to poll
// for here — the cover shows up in the library/admin views when ready.
async function postCover(g, file) {
  if (!g.album) return;
  try {
    const form = new FormData();
    form.append('image', file, file.name || 'cover.jpg');
    const url = `${API}/api/albums/${encodeURIComponent(g.album)}/image?artist=${encodeURIComponent(g.artist)}`;
    const res = await fetch(url, { method: 'POST', body: form });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    showToast(`Cover uploaded for “${g.album}”.`, { type: 'success' });
  } catch (err) {
    console.error('Cover upload failed:', err);
    showToast(`Cover upload failed for “${g.album}”.`, { type: 'error' });
  }
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
    // In grouped sort the per-state sections are gone, so badge draft/returned
    // (submitted is locked anyway — no badge).
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

// playMine queues the visible staging list into the preview sink (the shell
// player by default; a page-local one under the admin shell), starting at the
// clicked row — same continuity behavior as the library pages.
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

// ── Helpers ─────────────────────────────────────────────────────────────────
function updateButtons() {
  startBtn.disabled = running || !queue.some(it => it.state === 'pending');
  clearBtn.disabled = queue.length === 0;
}

function syncEmptyStates() {
  queueEmpty.style.display = queue.length ? 'none' : '';
}

function formatBytes(n) {
  if (n < 1024) return `${n} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let v = n / 1024, i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(v < 10 ? 1 : 0)} ${units[i]}`;
}

function announce(msg) { srStatus.textContent = msg; }

// showToast is the shared toast system (toast.js), rendering into the
// #toastStatus/#toastAlert stacks from the auth-modals partial. The separate
// srStatus aria-live region above stays — it narrates upload progress to screen
// readers, which is a different concern from the visible toasts.
// (init() runs syncEmptyStates()/updateButtons(); the visibilitychange listener
// is wired in wire(). Lifecycle is driven by shell.js.)
