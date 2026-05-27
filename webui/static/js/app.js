// BUG-14: read API base from HTML meta so non-local deployments work
const API = document.querySelector('meta[name="api-url"]')?.content || 'http://localhost:3000';

// ── Theme ─────────────────────────────────────────────────────────────────

const VALID_THEMES = new Set(['dark', 'light', 'ocean', 'sunset']); // BUG-07
const html      = document.documentElement;
const themeDots = document.querySelectorAll('.theme-dot');

applyTheme(localStorage.getItem('madshare-theme') || 'dark');

themeDots.forEach(dot => dot.addEventListener('click', () => applyTheme(dot.dataset.theme)));

function applyTheme(name) {
  if (!VALID_THEMES.has(name)) name = 'dark'; // BUG-07: reject unknown values
  html.dataset.theme = name;
  localStorage.setItem('madshare-theme', name);
  themeDots.forEach(d => {
    const on = d.dataset.theme === name;
    d.classList.toggle('active', on);
    d.setAttribute('aria-pressed', String(on));
  });
}

// ── Library ──────────────────────────────────────────────────────────────

let playlist             = [];
let libraryLoading       = false; // BUG-03: concurrent call guard
let libraryReloadPending = false; // BUG-03: queue at most one extra reload

async function loadLibrary() {
  if (libraryLoading) { libraryReloadPending = true; return; } // BUG-03
  libraryLoading = true;
  try {
    let tracks;
    try {
      const res = await fetch(`${API}/api/files`);
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      tracks = await res.json();
    } catch (err) {
      console.error('Failed to load library:', err);
      return;
    }

    const list  = document.getElementById('trackList');
    const empty = document.getElementById('emptyState');

    // BUG-08/01: remove only track rows; leave #emptyState in the DOM
    list.querySelectorAll('.track-row').forEach(el => el.remove());
    playlist = [];

    if (!tracks || tracks.length === 0) {
      if (empty) empty.style.display = ''; // BUG-13: show empty state again
      return;
    }

    if (empty) empty.style.display = 'none'; // BUG-08: hide, never remove

    tracks.forEach((t, i) => {
      const title  = t.title  || t.filename;
      const artist = t.artist || '';
      const meta   = [artist, t.album, t.year || null].filter(Boolean).join(' · ');
      const dur    = t.duration ? fmtTime(t.duration) : '—'; // BUG-09: stays '—' until API exposes duration

      playlist.push({ url: `${API}${t.url}`, title, artist });

      const li = document.createElement('li');
      li.className = 'track-row';
      li.tabIndex  = 0;
      li.dataset.idx = i;
      li.setAttribute('role', 'button');
      li.setAttribute('aria-label', `Play ${title}`);
      li.innerHTML = `
        <span class="track-num">${i + 1}</span>
        <span class="track-icon-playing" aria-hidden="true">
          <svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor"><path d="M8 5v14l11-7z"/></svg>
        </span>
        <div class="track-info">
          <div class="track-title">${esc(title)}</div>
          <div class="track-meta">${esc(meta)}</div>
        </div>
        <span class="track-dur">${esc(dur)}</span>
      `;
      li.addEventListener('click', () => playIndex(i));
      li.addEventListener('keydown', e => {
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); playIndex(i); }
      });
      list.appendChild(li);
    });

    // BUG-02: restore playing highlight if audio is still running after reload
    if (currentIndex >= 0 && currentIndex < playlist.length) {
      list.querySelectorAll('.track-row')[currentIndex]?.classList.add('playing');
    }
  } finally {
    libraryLoading = false;
    if (libraryReloadPending) { // BUG-03: flush one queued reload
      libraryReloadPending = false;
      loadLibrary();
    }
  }
}

// ── Player ───────────────────────────────────────────────────────────────

let currentIndex = -1;
let stopped      = false;
let shuffle      = false;

const audio        = document.getElementById('audio');
const playerBar    = document.getElementById('player-bar');
const playerTitle  = document.getElementById('playerTitle');
const playerArtist = document.getElementById('playerArtist');
const playerTime   = document.getElementById('playerTime');
const progressBar  = document.getElementById('progressBar');
const progressFill = document.getElementById('progressFill');
const btnPlay      = document.getElementById('btnPlay');
const btnShuffle   = document.getElementById('btnShuffle');
const iconPlay     = document.getElementById('iconPlay');
const iconPause    = document.getElementById('iconPause');
const volumeSlider = document.getElementById('volume-slider');

function playIndex(idx) {
  if (idx < 0 || idx >= playlist.length) return;
  currentIndex = idx;
  stopped      = false;

  const track = playlist[idx];
  audio.src = track.url;
  audio.play().catch(() => {});

  playerTitle.textContent  = track.title;
  playerArtist.textContent = track.artist;
  playerBar.classList.remove('hidden');

  document.querySelectorAll('.track-row').forEach((row, i) => {
    row.classList.toggle('playing', i === idx);
  });
}

document.getElementById('btnPrev').addEventListener('click', () => {
  if (currentIndex < 0) return; // BUG-04: nothing playing yet
  playIndex(currentIndex > 0 ? currentIndex - 1 : playlist.length - 1);
});

document.getElementById('btnNext').addEventListener('click', () => {
  if (currentIndex < 0) return; // BUG-04: nothing playing yet
  playIndex(currentIndex < playlist.length - 1 ? currentIndex + 1 : 0);
});

btnPlay.addEventListener('click', () => {
  if (stopped && currentIndex >= 0) {
    // BUG-10: resume from seeked position — don't reassign audio.src
    stopped = false;
    audio.play().catch(() => {});
    syncPlayIcon();
    return;
  }
  if (audio.paused) audio.play().catch(() => {});
  else              audio.pause();
});

btnShuffle.addEventListener('click', () => {
  shuffle = !shuffle;
  btnShuffle.classList.toggle('active', shuffle);
  const label = shuffle ? 'Shuffle on' : 'Shuffle off';
  btnShuffle.setAttribute('aria-label', label);
  btnShuffle.title = label;
});

audio.addEventListener('play',  syncPlayIcon);
audio.addEventListener('pause', syncPlayIcon);
audio.addEventListener('ended', () => {
  if (shuffle && playlist.length > 1) {
    // BUG-05: build pool of other indices — no infinite loop possible
    const others = playlist.map((_, i) => i).filter(i => i !== currentIndex);
    playIndex(others[Math.floor(Math.random() * others.length)]);
  } else if (currentIndex < playlist.length - 1) {
    playIndex(currentIndex + 1);
  } else {
    stopped = true;
    syncPlayIcon();
  }
});

function syncPlayIcon() {
  // BUG-11: derive state from audio element only — stopped flag is for button logic only
  const playing = !audio.paused;
  iconPlay.style.display  = playing ? 'none' : '';
  iconPause.style.display = playing ? ''     : 'none';
  btnPlay.setAttribute('aria-label', playing ? 'Pause' : 'Play');
  btnPlay.title = playing ? 'Pause' : 'Play';
}

// Progress
audio.addEventListener('timeupdate', () => {
  if (!audio.duration) return;
  const pct = (audio.currentTime / audio.duration) * 100;
  progressFill.style.width = pct + '%';
  progressBar.setAttribute('aria-valuenow', Math.round(pct));
  playerTime.textContent = fmtTime(audio.currentTime) + ' / ' + fmtTime(audio.duration);
});

progressBar.addEventListener('click', e => {
  if (!audio.duration) return;
  const r = progressBar.getBoundingClientRect();
  audio.currentTime = ((e.clientX - r.left) / r.width) * audio.duration;
});

progressBar.addEventListener('keydown', e => {
  if (!audio.duration) return;
  if (e.key === 'ArrowRight') audio.currentTime = Math.min(audio.duration, audio.currentTime + 5);
  if (e.key === 'ArrowLeft')  audio.currentTime = Math.max(0, audio.currentTime - 5);
});

// Volume
volumeSlider.addEventListener('input', () => { audio.volume = volumeSlider.value; });

function fmtTime(s) {
  if (!isFinite(s)) return '0:00';
  return Math.floor(s / 60) + ':' + String(Math.floor(s % 60)).padStart(2, '0');
}

// ── Upload modal ─────────────────────────────────────────────────────────

const modal     = document.getElementById('uploadModal');
const dropZone  = document.getElementById('dropZone');
const fileInput = document.getElementById('fileInput');
const status    = document.getElementById('uploadStatus');

document.getElementById('openUpload').addEventListener('click', openModal);
document.getElementById('openUploadEmpty')?.addEventListener('click', openModal);
document.getElementById('closeModal').addEventListener('click', closeModal);
modal.addEventListener('click', e => { if (e.target === modal) closeModal(); });
document.addEventListener('keydown', e => { if (e.key === 'Escape') closeModal(); });

function openModal()  { modal.classList.remove('hidden'); fileInput.focus(); }
function closeModal() { modal.classList.add('hidden'); setStatus('', ''); }

dropZone.addEventListener('dragover',  e  => { e.preventDefault(); dropZone.classList.add('dragover'); });
dropZone.addEventListener('dragleave', () => dropZone.classList.remove('dragover'));
dropZone.addEventListener('drop', e => {
  e.preventDefault();
  dropZone.classList.remove('dragover');
  const files = Array.from(e.dataTransfer.files); // BUG-12: handle all dropped files
  if (files.length) uploadFiles(files);
});

fileInput.addEventListener('change', () => {
  const files = Array.from(fileInput.files); // BUG-12
  if (files.length) uploadFiles(files);
  fileInput.value = '';
});

async function uploadFiles(files) { // BUG-12: upload sequentially, one reload at the end
  for (const file of files) {
    await uploadFile(file);
  }
}

async function uploadFile(file) {
  setStatus('Uploading "' + file.name + '"…', ''); // uses textContent — safe (BUG-06)
  const fd = new FormData();
  fd.append('file', file);

  let data;
  try {
    const res = await fetch(`${API}/files/upload`, { method: 'POST', body: fd });
    if (!res.ok) {
      const msg = await res.text().catch(() => res.statusText);
      setStatus('Upload failed: ' + msg, 'error');
      return;
    }
    data = await res.json();
  } catch (err) {
    setStatus('Upload error: ' + err.message, 'error');
    return;
  }

  setStatus((data.existed ? 'Already in library' : 'Uploaded') + ': ' + file.name, 'success');
  await loadLibrary(); // BUG-08/01/13: loadLibrary now owns clearing rows internally
}

function setStatus(msg, type) {
  status.textContent = msg;
  status.className   = 'upload-status' + (type ? ' ' + type : '');
}

// ── Utilities ────────────────────────────────────────────────────────────

function esc(s) {
  return String(s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;')
    .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

// ── Boot ─────────────────────────────────────────────────────────────────
loadLibrary();
