const API = 'http://localhost:3000';

// ── Theme ─────────────────────────────────────────────────────────────────

const html      = document.documentElement;
const themeDots = document.querySelectorAll('.theme-dot');

applyTheme(localStorage.getItem('madshare-theme') || 'dark');

themeDots.forEach(dot => dot.addEventListener('click', () => applyTheme(dot.dataset.theme)));

function applyTheme(name) {
  html.dataset.theme = name;
  localStorage.setItem('madshare-theme', name);
  themeDots.forEach(d => {
    const on = d.dataset.theme === name;
    d.classList.toggle('active', on);
    d.setAttribute('aria-pressed', String(on));
  });
}

// ── Library ──────────────────────────────────────────────────────────────

let playlist = [];

async function loadLibrary() {
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

  if (!tracks || tracks.length === 0) return;

  empty.remove();
  playlist = [];

  tracks.forEach((t, i) => {
    const title  = t.title  || t.filename;
    const artist = t.artist || '';
    const meta   = [artist, t.album, t.year || null].filter(Boolean).join(' · ');
    const dur    = t.duration ? fmtTime(t.duration) : '—';

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
  const prev = currentIndex > 0 ? currentIndex - 1 : playlist.length - 1;
  playIndex(prev);
});

document.getElementById('btnNext').addEventListener('click', () => {
  const next = currentIndex < playlist.length - 1 ? currentIndex + 1 : 0;
  playIndex(next);
});

btnPlay.addEventListener('click', () => {
  if (stopped && currentIndex >= 0) { playIndex(currentIndex); return; }
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
    let next;
    do { next = Math.floor(Math.random() * playlist.length); } while (next === currentIndex);
    playIndex(next);
  } else if (currentIndex < playlist.length - 1) {
    playIndex(currentIndex + 1);
  } else {
    stopped = true;
    syncPlayIcon();
  }
});

function syncPlayIcon() {
  const playing = !audio.paused && !stopped;
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
  const file = e.dataTransfer.files[0];
  if (file) uploadFile(file);
});

fileInput.addEventListener('change', () => {
  const file = fileInput.files[0];
  if (file) uploadFile(file);
  fileInput.value = '';
});

async function uploadFile(file) {
  setStatus('Uploading "' + file.name + '"…', '');
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
  document.getElementById('trackList').innerHTML = '';
  playlist = [];
  await loadLibrary();
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
