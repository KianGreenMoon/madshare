// Upload page (/upload) controller.
//
// Vanilla ES module, no dependencies. Same-origin: all API calls are relative
// when <meta name="api-url"> is empty (the bundled server). Mirrors app.js for
// the theme handling and API-base patterns.

// Read API base from HTML meta. Empty => relative, same-origin URLs.
const API = document.querySelector('meta[name="api-url"]')?.content || '';

// ── Theme (same pattern as app.js) ──────────────────────────────────────────
const VALID_THEMES = new Set(['dark', 'light', 'ocean', 'sunset']);
const html = document.documentElement;
const themeDots = document.querySelectorAll('.theme-dot');

applyTheme(localStorage.getItem('madshare-theme') || 'dark');
themeDots.forEach(dot => dot.addEventListener('click', () => applyTheme(dot.dataset.theme)));

function applyTheme(name) {
  if (!VALID_THEMES.has(name)) name = 'dark';
  html.dataset.theme = name;
  localStorage.setItem('madshare-theme', name);
  themeDots.forEach(d => {
    const on = d.dataset.theme === name;
    d.classList.toggle('active', on);
    d.setAttribute('aria-pressed', String(on));
  });
}

// ── DOM refs ────────────────────────────────────────────────────────────────
const dropZone     = document.getElementById('dropZone');
const fileInput    = document.getElementById('fileInput');
const folderInput  = document.getElementById('folderInput');
const browseFiles  = document.getElementById('browseFiles');
const browseFolder = document.getElementById('browseFolder');
const workersRange = document.getElementById('workersRange');
const workersValue = document.getElementById('workersValue');
const startBtn     = document.getElementById('startUpload');
const clearBtn     = document.getElementById('clearQueue');
const queueList    = document.getElementById('queueList');
const queueEmpty   = document.getElementById('queueEmpty');
const coverGrid    = document.getElementById('coverGrid');
const albumsEmpty  = document.getElementById('albumsEmpty');
const srStatus     = document.getElementById('srStatus');
const toastStack   = document.getElementById('toastStack');

// ── State ───────────────────────────────────────────────────────────────────

// Each queue entry tracks a File plus its UI element and lifecycle state.
// state: pending | uploading | done | error | rejected
let nextId = 1;
const queue = [];                 // QueueItem[]
const groups = new Map();         // prefix -> GroupCard
let activeWorkers = 0;            // slots currently in flight
let workerCap = 3;                // live concurrency cap (from slider / backoff)
let running = false;              // an upload run is in progress
let canEditMeta = false;         // metadata.edit permission (set after /auth/me)

const COVER_NAMES = new Set(['cover', 'folder', 'front', 'albumart', 'artwork', 'album']);
const IMAGE_MIMES = new Set(['image/jpeg', 'image/png']);

// ── Init: pull UI config + permissions ──────────────────────────────────────
init();

async function init() {
  await Promise.allSettled([loadUIConfig(), loadPermissions()]);
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
  } catch (err) {
    console.warn('UI config unavailable, using defaults:', err);
  }
  workerCap = Number(workersRange.value);
  workersValue.textContent = String(workerCap);
}

async function loadPermissions() {
  // "Replace cover" + auto-cover-upload require metadata.edit. If the call fails
  // or the user is anonymous, leave canEditMeta false so those controls hide.
  try {
    const res = await fetch(`${API}/api/auth/me`);
    if (!res.ok) return;
    const me = await res.json();
    canEditMeta = Array.isArray(me?.permissions) && me.permissions.includes('metadata.edit');
  } catch {
    canEditMeta = false;
  }
}

// ── Worker slider ───────────────────────────────────────────────────────────
workersRange.addEventListener('input', () => {
  workerCap = Number(workersRange.value);
  workersValue.textContent = String(workerCap);
  // Raising the cap mid-run should immediately spin up more slots.
  if (running) pump();
});

// ── File intake ─────────────────────────────────────────────────────────────
browseFiles.addEventListener('click', () => fileInput.click());
browseFolder.addEventListener('click', () => folderInput.click());
fileInput.addEventListener('change', () => { addFiles(fileInput.files); fileInput.value = ''; });
folderInput.addEventListener('change', () => { addFiles(folderInput.files); folderInput.value = ''; });

// Activating the drop zone itself (click / keyboard) opens the file picker.
dropZone.addEventListener('click', (e) => {
  if (e.target.closest('button')) return; // the browse buttons handle themselves
  fileInput.click();
});
dropZone.addEventListener('keydown', (e) => {
  if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); fileInput.click(); }
});

// Drag & drop. dragover must preventDefault to allow a drop.
['dragenter', 'dragover'].forEach(evt =>
  dropZone.addEventListener(evt, (e) => {
    e.preventDefault();
    dropZone.classList.add('is-dragover');
  }));
['dragleave', 'dragend'].forEach(evt =>
  dropZone.addEventListener(evt, (e) => {
    // Ignore dragleave bubbling from children still inside the zone.
    if (evt === 'dragleave' && dropZone.contains(e.relatedTarget)) return;
    dropZone.classList.remove('is-dragover');
  }));
dropZone.addEventListener('drop', (e) => {
  e.preventDefault();
  dropZone.classList.remove('is-dragover');
  if (e.dataTransfer?.files?.length) addFiles(e.dataTransfer.files);
});

function addFiles(fileList) {
  const files = Array.from(fileList || []);
  if (!files.length) return;

  for (const file of files) {
    const relPath = file.webkitRelativePath || file.name;
    const prefix = dirPrefix(relPath);
    const item = {
      id: nextId++,
      file,
      relPath,
      prefix,
      state: 'pending',
      el: null,
    };
    queue.push(item);
    renderQueueItem(item);
    ensureGroup(prefix).items.push(item);
  }

  // Re-evaluate cover candidates and counts for affected groups.
  for (const file of files) refreshGroup(dirPrefix(file.webkitRelativePath || file.name));

  syncEmptyStates();
  startBtn.disabled = false;
  clearBtn.disabled = false;
  announce(`${queue.length} file${queue.length === 1 ? '' : 's'} in queue.`);
}

// Directory prefix: everything before the last path segment ("" for loose files).
function dirPrefix(relPath) {
  return relPath.split('/').slice(0, -1).join('/');
}

// ── Queue rendering ─────────────────────────────────────────────────────────
function renderQueueItem(item) {
  const li = document.createElement('li');
  li.className = 'queue-item';
  li.dataset.state = 'pending';

  const name = document.createElement('span');
  name.className = 'queue-item__name';
  name.textContent = item.relPath;            // textContent — never innerHTML with user data

  const size = document.createElement('span');
  size.className = 'queue-item__size';
  size.textContent = formatBytes(item.file.size);

  const bar = document.createElement('div');
  bar.className = 'queue-item__bar';
  // Expose the bar as an ARIA progressbar; setProgress keeps aria-valuenow in
  // sync with the visual fill width.
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
  // Keep the ARIA progressbar value in lockstep with the visual width.
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
    startBtn.disabled = false;
    if (running) pump();
  });
  item.el.msg.appendChild(btn);
}

// ── Album groups / cover cards ──────────────────────────────────────────────
function ensureGroup(prefix) {
  let g = groups.get(prefix);
  if (g) return g;
  g = {
    prefix,
    items: [],
    cover: null,        // best cover candidate File
    album: '',          // learned from a NEW-file upload response (first wins)
    artist: '',         // effective album-artist, paired with album
    poll: null,         // interval id
    el: null,           // card DOM refs
  };
  groups.set(prefix, g);
  renderCard(g);
  return g;
}

function refreshGroup(prefix) {
  const g = groups.get(prefix);
  if (!g) return;
  // Cover candidate: an image/jpeg|png whose stem is a known cover name; if
  // several, pick the largest by byte size.
  let best = null;
  for (const it of g.items) {
    const f = it.file;
    if (!IMAGE_MIMES.has(f.type)) continue;
    const stem = baseName(it.relPath).replace(/\.[^.]+$/, '').toLowerCase();
    if (!COVER_NAMES.has(stem)) continue;
    if (!best || f.size > best.size) best = f;
  }
  g.cover = best;
  updateCardMeta(g);
}

function baseName(relPath) {
  const parts = relPath.split('/');
  return parts[parts.length - 1];
}

function renderCard(g) {
  const card = document.createElement('article');
  card.className = 'cover-card';
  card.dataset.processing = 'false';

  const art = document.createElement('div');
  art.className = 'cover-card__art';
  const ph = document.createElement('span');
  ph.className = 'cover-card__placeholder';
  ph.textContent = '♫';                  // musical note placeholder
  ph.setAttribute('aria-hidden', 'true');
  const spinner = document.createElement('div');
  spinner.className = 'cover-card__spinner';
  spinner.setAttribute('aria-hidden', 'true');
  art.append(ph, spinner);

  const title = document.createElement('h3');
  title.className = 'cover-card__title';

  const meta = document.createElement('p');
  meta.className = 'cover-card__meta';

  const note = document.createElement('p');
  note.className = 'cover-card__note';

  const replaceWrap = document.createElement('div');
  replaceWrap.className = 'cover-card__replace';

  card.append(art, title, meta, note, replaceWrap);
  coverGrid.appendChild(card);
  g.el = { card, art, title, meta, note, replaceWrap };
  updateCardMeta(g);
}

function updateCardMeta(g) {
  if (!g.el) return;
  const audioCount = g.items.filter(it => isAudio(it.file)).length;
  const display = g.album || prettyName(g.prefix) || 'Loose files';
  g.el.title.textContent = display;
  g.el.title.title = display;

  const parts = [];
  if (g.artist) parts.push(g.artist);
  parts.push(`${audioCount} track${audioCount === 1 ? '' : 's'}`);
  g.el.meta.textContent = parts.join(' · ');
}

function isAudio(file) {
  return file.type.startsWith('audio/');
}

function prettyName(prefix) {
  if (!prefix) return '';
  const segs = prefix.split('/');
  return segs[segs.length - 1] || segs[0];
}

// ── Upload run ──────────────────────────────────────────────────────────────
startBtn.addEventListener('click', () => {
  running = true;
  startBtn.disabled = true;
  pump();
});

clearBtn.addEventListener('click', () => {
  // Remove only items not currently in flight; stop all polling.
  for (let i = queue.length - 1; i >= 0; i--) {
    if (queue[i].state !== 'uploading') queue.splice(i, 1);
  }
  queueList.querySelectorAll('.queue-item').forEach(li => {
    // Keep rows that are still uploading.
    const stillBusy = queue.some(it => it.el?.li === li);
    if (!stillBusy && li.dataset.state !== 'uploading') li.remove();
  });
  for (const g of groups.values()) stopPolling(g);
  groups.clear();
  coverGrid.replaceChildren();
  syncEmptyStates();
  clearBtn.disabled = queue.length === 0;
  startBtn.disabled = !queue.some(it => it.state === 'pending');
});

// pump fills available worker slots with pending files, up to workerCap.
function pump() {
  while (running && activeWorkers < workerCap) {
    const item = queue.find(it => it.state === 'pending');
    if (!item) break;
    activeWorkers++;
    uploadItem(item).finally(() => {
      activeWorkers--;
      // Continue or finish the run.
      if (queue.some(it => it.state === 'pending')) {
        pump();
      } else if (activeWorkers === 0) {
        finishRun();
      }
    });
  }
  // Nothing pending and nothing in flight at the moment of call.
  if (running && activeWorkers === 0 && !queue.some(it => it.state === 'pending')) {
    finishRun();
  }
}

function finishRun() {
  if (!running) return;                      // idempotent — guard double calls
  running = false;
  startBtn.disabled = !queue.some(it => it.state === 'pending');
  clearBtn.disabled = queue.length === 0 && groups.size === 0;
  // After uploads settle, attempt auto covers for completed groups.
  for (const g of groups.values()) maybeAutoCover(g);
  announce('Uploads finished.');
}

function uploadItem(item) {
  return new Promise((resolve) => {
    setItemState(item, 'uploading');
    setProgress(item, 0);

    const form = new FormData();
    // webkitRelativePath becomes the Content-Disposition filename so the server
    // sees the folder-qualified name.
    form.append('file', item.file, item.relPath);

    const xhr = new XMLHttpRequest();
    xhr.open('POST', `${API}/files/upload`);

    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) setProgress(item, (e.loaded / e.total) * 100);
    };

    xhr.onload = () => {
      let body = null;
      try { body = JSON.parse(xhr.responseText); } catch { /* non-JSON */ }

      if (xhr.status === 429) {
        // Backpressure: reduce workers (floor 1), re-queue at FRONT, log.
        handleBackoff(item);
        resolve();
        return;
      }

      if (xhr.status >= 200 && xhr.status < 300 && body?.ok !== false) {
        setProgress(item, 100);
        setItemState(item, 'done', body?.existed ? 'Already present' : 'Uploaded');
        // The upload response now carries the effective album + album-artist for
        // a NEW-file insert: {ok, existed, hash, filename, size, cover_found,
        // cover_processing, album, artist}. On the dedup/restore path (existed:
        // true) album+artist come back empty, so we only learn from fresh
        // uploads. We record the first non-empty {album, artist} pair onto the
        // group; that activates the real (tag-derived) auto-cover + status-poll
        // path instead of the directory-name fallback. See maybeAutoCover().
        const g = groups.get(item.prefix);
        if (g) {
          if (body?.album && !g.album) {
            g.album = body.album;
            g.artist = body.artist || '';
            updateCardMeta(g);          // prefer learned title over dir name
          }
          if (body?.cover_processing || body?.cover_found) {
            // Embedded cover was extracted server-side — poll for variants once
            // we know album+artist (now learnable from the response above).
            g.coverFromUpload = true;
          }
        }
        resolve();
        return;
      }

      // Per-file rejection / error.
      if (xhr.status === 415 || xhr.status === 400) {
        setItemState(item, 'rejected', body?.error || `Rejected (HTTP ${xhr.status})`);
      } else {
        setItemState(item, 'error', body?.error || `Upload failed (HTTP ${xhr.status})`);
        addRetry(item);
      }
      resolve();
    };

    xhr.onerror = () => {
      setItemState(item, 'error', 'Network error');
      addRetry(item);
      resolve();
    };

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
  // Visible transient toast for sighted users…
  showToast(`Server busy — workers reduced to ${workerCap}.`);
  // …and the precise count to the visually-hidden live region for SR users.
  announce(`Workers reduced to ${workerCap}.`);

  // Re-queue this file at the FRONT, reset to pending.
  setItemState(item, 'pending');
  setProgress(item, 0);
  const idx = queue.indexOf(item);
  if (idx !== -1) queue.splice(idx, 1);
  queue.unshift(item);
}

// ── Auto cover upload ────────────────────────────────────────────────────────
// We can only POST a cover when album+artist are known. The upload response now
// carries the effective album + album-artist for NEW-file inserts, so g.album/
// g.artist are populated for fresh uploads and this path fires for real. When a
// group is entirely deduped (existed:true) or untagged, album stays empty and we
// degrade gracefully to "no album info" rather than guessing.
function maybeAutoCover(g) {
  if (!g.el) return;
  const audioDone = g.items.filter(it => isAudio(it.file))
    .every(it => it.state === 'done' || it.state === 'error' || it.state === 'rejected');
  if (!audioDone) return;

  if (!g.cover || !canEditMeta || !g.album) {
    if (g.cover && g.album) {
      // Have a cover + album but no permission: silent grey placeholder.
    } else if (!g.album && g.items.some(it => it.state === 'done')) {
      g.el.note.textContent = 'No album info — cover not auto-set.';
    }
    // Still poll for embedded-cover variants if we somehow know album/artist.
    if (g.album) startPolling(g);
    return;
  }

  uploadCover(g, g.cover);
}

// uploadCover POSTs an image to the album cover endpoint, then polls for
// variant readiness. Shared by auto-cover and the manual "Replace cover".
async function uploadCover(g, file) {
  const album = effectiveAlbum(g);
  if (!album) { g.el.note.textContent = 'No album info — cannot set cover.'; return; }
  g.el.card.dataset.processing = 'true';
  g.el.note.textContent = 'Uploading cover…';
  try {
    const form = new FormData();
    form.append('image', file, file.name);
    const url = `${API}/api/albums/${encodeURIComponent(album)}/image?artist=${encodeURIComponent(g.artist)}`;
    const res = await fetch(url, { method: 'POST', body: form });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    g.el.note.textContent = '';
    startPolling(g);
  } catch (err) {
    console.error('Cover upload failed:', err);
    g.el.card.dataset.processing = 'false';
    g.el.note.textContent = 'Cover upload failed.';
  }
}

// ── Status polling ──────────────────────────────────────────────────────────
function startPolling(g) {
  if (!effectiveAlbum(g)) return;
  stopPolling(g);
  g.el.card.dataset.processing = 'true';
  const tick = () => pollStatus(g);
  tick();                                   // immediate first check
  g.poll = setInterval(tick, 2000);
}

function stopPolling(g) {
  if (g.poll) { clearInterval(g.poll); g.poll = null; }
}

async function pollStatus(g) {
  if (document.hidden) return;              // pause while tab hidden
  const album = effectiveAlbum(g);
  if (!album) { stopPolling(g); return; }
  try {
    const url = `${API}/api/albums/${encodeURIComponent(album)}/image/status?artist=${encodeURIComponent(g.artist)}`;
    const res = await fetch(url);
    if (res.status === 404) { stopPolling(g); g.el.card.dataset.processing = 'false'; return; }
    if (!res.ok) return;                    // transient; try again next tick
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
  img.alt = '';                             // decorative — title conveys the album
  img.loading = 'lazy';
  img.src = variantUrl.startsWith('http') ? variantUrl : `${API}${variantUrl}`;
  g.el.art.replaceChildren(img);
}

// ── Manual "Replace cover" wiring ───────────────────────────────────────────
// Shown only when the user has metadata.edit. The cover endpoints are addressed
// by album+artist. The upload response now supplies a tag-derived album+artist
// for fresh uploads (g.album/g.artist); the MANUAL button uses that when known
// and falls back to the directory name as a best-effort album otherwise, so the
// feature stays usable even when a group was fully deduped. Auto-cover
// (maybeAutoCover) stays strict and only fires on a learned (tag-derived) album.
function refreshReplaceButtons() {
  if (!canEditMeta) return;
  for (const g of groups.values()) {
    if (!g.el || g.el.replaceWrap.dataset.wired) continue;
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'btn';
    btn.textContent = 'Replace cover';
    btn.addEventListener('click', () => pickReplacement(g));
    g.el.replaceWrap.appendChild(btn);
    g.el.replaceWrap.dataset.wired = '1';
  }
}

// effectiveAlbum returns the album name to address the cover API with: the
// tag-derived album if learned, else the directory name as a best-effort.
function effectiveAlbum(g) {
  return g.album || prettyName(g.prefix);
}

function pickReplacement(g) {
  const input = document.createElement('input');
  input.type = 'file';
  input.accept = 'image/jpeg,image/png';
  input.addEventListener('change', () => {
    if (input.files?.length) uploadCover(g, input.files[0]);
  });
  input.click();
}

// Re-check whether replace buttons can be shown whenever cards change.
const cardObserver = new MutationObserver(refreshReplaceButtons);
cardObserver.observe(coverGrid, { childList: true });

// ── Helpers ─────────────────────────────────────────────────────────────────
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

function announce(msg) {
  srStatus.textContent = msg;
}

// showToast pops a transient, self-dismissing message in the toast stack. The
// container is role="status"/aria-live; the visually-hidden srStatus carries the
// authoritative announcement, so the toast text is purely visual reinforcement.
function showToast(msg) {
  if (!toastStack) return;
  const toast = document.createElement('div');
  toast.className = 'toast';
  toast.textContent = msg;            // textContent — no untrusted markup
  toastStack.appendChild(toast);
  // Trigger the enter transition on the next frame, then auto-dismiss.
  requestAnimationFrame(() => toast.classList.add('is-visible'));
  setTimeout(() => {
    toast.classList.remove('is-visible');
    toast.addEventListener('transitionend', () => toast.remove(), { once: true });
    // Fallback removal if transitions are disabled (reduced motion).
    setTimeout(() => toast.remove(), 300);
  }, 4000);
}

// Pause/resume polling on tab visibility changes.
document.addEventListener('visibilitychange', () => {
  if (!document.hidden) {
    for (const g of groups.values()) if (g.poll) pollStatus(g);
  }
});

syncEmptyStates();
