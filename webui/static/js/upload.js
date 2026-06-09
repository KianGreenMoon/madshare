import { gatePage, PAGE_PERMS, getIdentity } from './auth.js';
import { createHashPool } from './hash-pool.js';

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

// ── DOM refs (assigned by queryRefs() in init — they live in <main>) ─────────
let dropZone, fileInput, folderInput, addMusicBtn, addMenu, chooseFilesBtn, chooseFolderBtn;
let workersRange, workersValue, startBtn, clearBtn, queueList, queueEmpty;
let coverGrid, albumsEmpty, srStatus, toastStack, precheckToggle;

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
  coverGrid       = document.getElementById('coverGrid');
  albumsEmpty     = document.getElementById('albumsEmpty');
  srStatus        = document.getElementById('srStatus');
  toastStack      = document.getElementById('toastStack');
  precheckToggle  = document.getElementById('precheckToggle');
}

// ── State ───────────────────────────────────────────────────────────────────
// queue:   every intake file (audio uploaded, image = cover candidate, other = skipped)
// groups:  album cards keyed by `artist \x1f album` (formed from upload responses)
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
// Audio is detected by extension as well as MIME: browsers often leave file.type
// empty for non-MP3 formats, so MIME alone would wrongly skip real music. Mirrors
// the server's allowedExtensions.
const AUDIO_EXTS   = /\.(mp3|ogg|flac|wav|mp4|m4a|aac|opus)$/i;

// ── Lifecycle (driven by shell.js) ──────────────────────────────────────────
export async function init() {
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
}

export function teardown() {
  wireAbort?.abort();
  wireAbort = null;
  for (const g of groups.values()) stopPolling(g);
  for (const xhr of activeXhrs) { try { xhr.abort(); } catch { /* ignore */ } }
  activeXhrs.clear();
  if (hashPool) { hashPool.terminate(); hashPool = null; }
  // Reset for a clean next entry (the <main> is re-rendered fresh by the shell).
  queue.length = 0;
  groups.clear();
  folders.clear();
  nextId = 1;
  activeWorkers = 0;
  running = false;
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

  // Resume cover polling when the tab becomes visible again.
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) for (const g of groups.values()) if (g.poll) pollStatus(g);
  }, { signal });

  // Pre-upload precheck toggle (persisted, default on)
  if (precheckToggle) {
    precheckToggle.checked = precheckEnabled();
    precheckToggle.addEventListener('change', () => {
      localStorage.setItem(PRECHECK_KEY, precheckToggle.checked ? 'on' : 'off');
    }, { signal });
  }
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

// classify decides how a file is handled. Browsers frequently leave file.type
// EMPTY for non-MP3 audio (.flac/.m4a/.opus/.ogg/.wav…), so extension is the
// reliable signal — MIME alone would mislabel real music as "other" and skip it.
function classify(file, relPath) {
  const name = (relPath || file.name || '').toLowerCase();
  const type = file.type || '';
  if (type.startsWith('audio/') || AUDIO_EXTS.test(name)) return 'audio';
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
  if (item.kind === 'image') setItemState(item, 'cover', 'Cover image — applied to its album after upload.');
  else if (item.kind === 'other') setItemState(item, 'skipped', 'Skipped — only audio files are stored.');
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
  for (const g of groups.values()) stopPolling(g);
  groups.clear();
  folders.clear();
  coverGrid.replaceChildren();
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
      setItemState(item, 'done', 'Restored to library');
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

    const form = new FormData();
    form.append('file', item.file, item.relPath);

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
        setItemState(item, 'done', body?.existed ? 'Already present' : 'Uploaded');
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
  showToast(`Server busy — workers reduced to ${workerCap}.`);
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
  if (body.existed) return;

  item.title = body.title || '';
  item.album = body.album || '';
  item.artist = body.artist || '';
  item.groupKey = groupKeyFor(item.album, item.artist);
  const g = ensureGroup(item.groupKey, item.album, item.artist);
  g.items.push(item);
  updateCard(g);
  if (body.cover_found || body.cover_processing) g.embeddedCover = true;
}

function groupKeyFor(album, artist) {
  return album ? `${artist}\u001f${album}` : UNSORTED_KEY;
}

// ── Album groups / verify cards ─────────────────────────────────────────────
function ensureGroup(key, album, artist) {
  let g = groups.get(key);
  if (g) return g;
  g = {
    key,
    album: album || '',
    artist: artist || '',
    unsorted: key === UNSORTED_KEY,
    items: [],              // audio QueueItem[] in this album
    coverFile: null,        // a client-side cover image (candidate or replacement)
    coverAmbiguous: false,  // co-located cover spanned >1 album
    embeddedCover: false,   // server extracted an embedded cover for this album
    poll: null,
    el: null,
  };
  groups.set(key, g);
  renderCard(g);
  syncEmptyStates();
  return g;
}

function renderCard(g) {
  const card = document.createElement('article');
  card.className = 'cover-card';
  card.dataset.processing = 'false';

  const art = document.createElement('div');
  art.className = 'cover-card__art';
  const ph = document.createElement('span');
  ph.className = 'cover-card__placeholder';
  ph.textContent = '♫';                  // ♫
  ph.setAttribute('aria-hidden', 'true');
  const spinner = document.createElement('div');
  spinner.className = 'cover-card__spinner';
  spinner.setAttribute('aria-hidden', 'true');
  art.append(ph, spinner);

  const body = document.createElement('div');
  body.className = 'cover-card__body';

  card.append(art, body);
  coverGrid.appendChild(card);
  g.el = { card, art, body };
  updateCard(g);
}

// updateCard rebuilds the card body (titles, track list, controls) in place,
// leaving the art element untouched so a loaded cover / spinner survives.
function updateCard(g) {
  if (!g.el) return;
  const body = g.el.body;
  body.replaceChildren();

  // --- album + artist (editable with metadata.edit) ---
  if (g.unsorted) {
    const h = document.createElement('h3');
    h.className = 'cover-card__title';
    h.textContent = 'Unsorted / no album tag';
    body.appendChild(h);
    const note = document.createElement('p');
    note.className = 'cover-card__meta';
    note.textContent = `${g.items.length} track${g.items.length === 1 ? '' : 's'} with no album tag.`;
    body.appendChild(note);
  } else if (canEditMeta) {
    body.appendChild(field('Album', g.album, (el) => { g.el.albumInput = el; }));
    body.appendChild(field('Artist', g.artist, (el) => { g.el.artistInput = el; }));
  } else {
    const h = document.createElement('h3');
    h.className = 'cover-card__title';
    h.textContent = g.album || 'Untitled album';
    h.title = h.textContent;
    body.appendChild(h);
    const meta = document.createElement('p');
    meta.className = 'cover-card__meta';
    meta.textContent = g.artist || 'Unknown artist';
    body.appendChild(meta);
  }

  // --- track list ---
  const count = document.createElement('p');
  count.className = 'cover-card__meta';
  count.textContent = `${g.items.length} track${g.items.length === 1 ? '' : 's'}`;
  body.appendChild(count);

  if (g.items.length) {
    const list = document.createElement('ul');
    list.className = 'cover-card__tracks';
    for (const it of g.items) {
      const row = document.createElement('li');
      if (canEditMeta && !g.unsorted) {
        const input = document.createElement('input');
        input.type = 'text';
        input.className = 'cover-card__track-input';
        input.value = it.title || baseName(it.relPath);
        input.setAttribute('aria-label', `Title for ${baseName(it.relPath)}`);
        it.titleInput = input;
        row.appendChild(input);
      } else {
        row.textContent = it.title || baseName(it.relPath);
      }
      list.appendChild(row);
    }
    body.appendChild(list);
  }

  // --- note (ambiguity / permission hints) ---
  const note = document.createElement('p');
  note.className = 'cover-card__note';
  g.el.note = note;
  if (g.coverAmbiguous) note.textContent = 'Cover ambiguous — this folder spanned several albums; confirm it is right.';
  body.appendChild(note);

  // --- actions ---
  if (canEditMeta && !g.unsorted) {
    const actions = document.createElement('div');
    actions.className = 'cover-card__actions';

    const save = document.createElement('button');
    save.type = 'button';
    save.className = 'btn btn--accent';
    save.textContent = 'Save changes';
    save.addEventListener('click', () => saveCard(g));
    actions.appendChild(save);

    const replace = document.createElement('button');
    replace.type = 'button';
    replace.className = 'btn';
    replace.textContent = 'Replace cover';
    replace.addEventListener('click', () => pickReplacement(g));
    actions.appendChild(replace);

    body.appendChild(actions);
  }
}

// field builds a labelled text input and hands the element back via `keep`.
function field(label, value, keep) {
  const wrap = document.createElement('label');
  wrap.className = 'cover-card__field';
  const span = document.createElement('span');
  span.className = 'cover-card__field-label';
  span.textContent = label;
  const input = document.createElement('input');
  input.type = 'text';
  input.value = value || '';
  input.placeholder = label === 'Album' ? 'Album title' : 'Artist';
  wrap.append(span, input);
  keep(input);
  return wrap;
}

// ── Verify/edit save ────────────────────────────────────────────────────────
// Applies the card's edits: per-track title changes, then (if album/artist
// changed) re-tags every track and re-targets the cover to the new identity.
async function saveCard(g) {
  if (!canEditMeta || g.unsorted) return;
  const newAlbum  = (g.el.albumInput?.value ?? g.album).trim();
  const newArtist = (g.el.artistInput?.value ?? g.artist).trim();
  const renamed = newAlbum !== g.album || newArtist !== g.artist;

  g.el.note.textContent = 'Saving…';
  try {
    // 1) Per-track title edits (+ album/artist when renamed).
    for (const it of g.items) {
      if (!it.hash) continue;
      const patch = {};
      const newTitle = (it.titleInput?.value ?? it.title).trim();
      if (newTitle !== it.title) { patch.title = newTitle; it.title = newTitle; }
      if (renamed) { patch.album = newAlbum; patch.album_artist = newArtist; }
      if (Object.keys(patch).length === 0) continue;
      const res = await fetch(`${API}/api/files/${encodeURIComponent(it.hash)}/metadata`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(patch),
      });
      if (!res.ok) throw new Error(`PATCH ${it.hash}: HTTP ${res.status}`);
    }

    if (renamed) {
      // 2) Re-key the group to its new identity and re-target the cover. Album
      //    covers are keyed by the album_artist+album strings, so a rename would
      //    orphan the cover unless we re-POST it (only possible when we hold the
      //    image bytes — i.e. a client-side cover file). An embedded-only cover
      //    cannot be moved here; the user can Replace it.
      stopPolling(g);
      groups.delete(g.key);
      g.album = newAlbum;
      g.artist = newArtist;
      g.key = groupKeyFor(newAlbum, newArtist);
      groups.set(g.key, g);
      for (const it of g.items) { it.album = newAlbum; it.artist = newArtist; it.groupKey = g.key; }

      if (g.coverFile) {
        await postCover(g, g.coverFile);       // re-POSTs + restarts polling
      } else {
        startPolling(g);                        // the cover may already exist under the new name
        if (g.embeddedCover) g.el.note.textContent = 'Renamed. If the cover is missing, use Replace cover.';
        else g.el.note.textContent = 'Saved.';
      }
    } else {
      g.el.note.textContent = 'Saved.';
    }
    updateCard(g);
    announce('Changes saved.');
  } catch (err) {
    console.error('Save failed:', err);
    g.el.note.textContent = 'Save failed — see console.';
  }
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
    g.coverFile = cover;
    g.coverAmbiguous = tally.size > 1;
    updateCard(g);

    if (canEditMeta) postCover(g, cover);       // explicit file beats embedded art
    // Without permission: the candidate is offered via Replace (button hidden,
    // so it simply stays as a grey placeholder unless an embedded cover exists).
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

// postCover POSTs an image to the album cover endpoint, then polls for variants.
async function postCover(g, file) {
  if (!g.album) { if (g.el?.note) g.el.note.textContent = 'No album — cannot set a cover.'; return; }
  g.el.card.dataset.processing = 'true';
  if (g.el.note) g.el.note.textContent = 'Uploading cover…';
  try {
    const form = new FormData();
    form.append('image', file, file.name || 'cover.jpg');
    const url = `${API}/api/albums/${encodeURIComponent(g.album)}/image?artist=${encodeURIComponent(g.artist)}`;
    const res = await fetch(url, { method: 'POST', body: form });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    g.coverFile = file;
    if (g.el.note) g.el.note.textContent = '';
    startPolling(g);
  } catch (err) {
    console.error('Cover upload failed:', err);
    g.el.card.dataset.processing = 'false';
    if (g.el.note) g.el.note.textContent = 'Cover upload failed.';
  }
}

function pickReplacement(g) {
  const input = document.createElement('input');
  input.type = 'file';
  input.accept = 'image/jpeg,image/png';
  input.addEventListener('change', () => { if (input.files?.length) postCover(g, input.files[0]); });
  input.click();
}

// ── Status polling ──────────────────────────────────────────────────────────
function startPolling(g) {
  if (!g.album) return;
  stopPolling(g);
  g.el.card.dataset.processing = 'true';
  const tick = () => pollStatus(g);
  tick();
  g.poll = setInterval(tick, 2000);
}

function stopPolling(g) {
  if (g.poll) { clearInterval(g.poll); g.poll = null; }
}

async function pollStatus(g) {
  if (document.hidden) return;
  if (!g.album) { stopPolling(g); return; }
  try {
    const url = `${API}/api/albums/${encodeURIComponent(g.album)}/image/status?artist=${encodeURIComponent(g.artist)}`;
    const res = await fetch(url);
    if (res.status === 404) { stopPolling(g); g.el.card.dataset.processing = 'false'; return; }
    if (!res.ok) return;                        // transient; retry next tick
    const status = await res.json();
    if (status?.variants_ready) {
      stopPolling(g);
      showCover(g, status?.variants?.medium_crop);
    }
  } catch (err) {
    console.warn('Status poll failed:', err);
  }
}

function showCover(g, variantUrl) {
  g.el.card.dataset.processing = 'false';
  if (!variantUrl) return;
  const img = document.createElement('img');
  img.alt = '';                                 // decorative — the title conveys the album
  img.loading = 'lazy';
  img.src = variantUrl.startsWith('http') ? variantUrl : `${API}${variantUrl}`;
  g.el.art.replaceChildren(img);
}

// ── Helpers ─────────────────────────────────────────────────────────────────
function updateButtons() {
  startBtn.disabled = running || !queue.some(it => it.state === 'pending');
  clearBtn.disabled = queue.length === 0 && groups.size === 0;
}

function syncEmptyStates() {
  queueEmpty.style.display = queue.length ? 'none' : '';
  albumsEmpty.style.display = groups.size ? 'none' : '';
}

function formatBytes(n) {
  if (n < 1024) return `${n} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let v = n / 1024, i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(v < 10 ? 1 : 0)} ${units[i]}`;
}

function announce(msg) { srStatus.textContent = msg; }

function showToast(msg) {
  if (!toastStack) return;
  const toast = document.createElement('div');
  toast.className = 'toast';
  toast.textContent = msg;                      // textContent — no untrusted markup
  toastStack.appendChild(toast);
  requestAnimationFrame(() => toast.classList.add('is-visible'));
  setTimeout(() => {
    toast.classList.remove('is-visible');
    toast.addEventListener('transitionend', () => toast.remove(), { once: true });
    setTimeout(() => toast.remove(), 300);
  }, 4000);
}
// (init() runs syncEmptyStates()/updateButtons(); the visibilitychange listener
// is wired in wire(). Lifecycle is driven by shell.js.)
