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

// ── Duration cache ────────────────────────────────────────────────────────
// Persists fetched durations across page loads so headers aren't re-fetched.
const DUR_CACHE_KEY = 'madshare-durations';
function loadDurCache() {
  try { return JSON.parse(localStorage.getItem(DUR_CACHE_KEY) || '{}'); }
  catch { return {}; }
}
function saveDurCache(c) {
  try { localStorage.setItem(DUR_CACHE_KEY, JSON.stringify(c)); }
  catch {} // quota exceeded — not fatal
}

// ── Library — drill-down state ────────────────────────────────────────────

// drill tracks which panel level is currently shown and what was selected.
// playlist is shared with the player — playIndex() reads it directly.
const drill = { level: 'artists', artist: null, album: null };
let playlist = [];

// Fetch and render the top-level artists panel.
async function loadArtists() {
  drill.level  = 'artists';
  drill.artist = null;
  drill.album  = null;

  const panel = document.getElementById('libraryPanel');
  panel.innerHTML = '<div class="panel-loading" aria-live="polite" role="status"></div>';

  let artists;
  try {
    const res = await fetch(`${API}/api/artists`);
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    artists = await res.json();
  } catch (err) {
    console.error('Failed to load artists:', err);
    panel.innerHTML = '<div class="panel-empty" role="alert">Failed to load library.</div>';
    return;
  }

  renderBreadcrumb();
  renderArtistList(artists);
}

// Fetch and render the albums panel for a given artist.
async function drillToAlbums(artistName) {
  drill.level  = 'albums';
  drill.artist = artistName;
  drill.album  = null;

  const panel = document.getElementById('libraryPanel');
  panel.innerHTML = '';

  let albums;
  try {
    const res = await fetch(`${API}/api/albums?artist=${encodeURIComponent(artistName || '')}`);
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    albums = await res.json();
  } catch (err) {
    console.error('Failed to load albums:', err);
    panel.innerHTML = '<div class="panel-empty" role="alert">Failed to load albums.</div>';
    return;
  }

  renderBreadcrumb();
  renderAlbumList(albums);
}

// Fetch and render the tracks panel for a given artist + album.
async function drillToTracks(artistName, albumTitle) {
  drill.level  = 'tracks';
  drill.artist = artistName;
  drill.album  = albumTitle;

  const panel = document.getElementById('libraryPanel');
  panel.innerHTML = '';

  let tracks;
  try {
    const res = await fetch(
      `${API}/api/tracks?artist=${encodeURIComponent(artistName || '')}&album=${encodeURIComponent(albumTitle || '')}`
    );
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    tracks = await res.json();
  } catch (err) {
    console.error('Failed to load tracks:', err);
    panel.innerHTML = '<div class="panel-empty" role="alert">Failed to load tracks.</div>';
    return;
  }

  renderBreadcrumb();
  renderTrackList(tracks);
}

// Rebuild the breadcrumb nav from current drill state.
// Uses addEventListener (not onclick) — module scripts don't share global scope.
function renderBreadcrumb() {
  const bc = document.getElementById('breadcrumb');
  bc.innerHTML = '';

  const displayArtist = drill.artist || 'Unknown Artist';
  const displayAlbum  = drill.album  || 'Other';

  function mkLink(label, handler) {
    const btn = document.createElement('button');
    btn.className = 'bc-item bc-link';
    btn.textContent = label;
    btn.addEventListener('click', handler);
    return btn;
  }
  function mkSep() {
    const s = document.createElement('span');
    s.className = 'bc-sep';
    s.textContent = '›';
    return s;
  }
  function mkCurrent(label) {
    const s = document.createElement('span');
    s.className = 'bc-item bc-current';
    s.textContent = label;
    return s;
  }

  if (drill.level === 'artists') {
    bc.appendChild(mkCurrent('Library'));
  } else if (drill.level === 'albums') {
    bc.appendChild(mkLink('Library', loadArtists));
    bc.appendChild(mkSep());
    bc.appendChild(mkCurrent(displayArtist));
  } else {
    const capturedArtist = drill.artist;
    bc.appendChild(mkLink('Library', loadArtists));
    bc.appendChild(mkSep());
    bc.appendChild(mkLink(displayArtist, () => drillToAlbums(capturedArtist)));
    bc.appendChild(mkSep());
    bc.appendChild(mkCurrent(displayAlbum));
  }
}

// Render a list of artist rows into #libraryPanel.
function renderArtistList(artists) {
  const panel = document.getElementById('libraryPanel');

  if (!artists || artists.length === 0) {
    panel.innerHTML =
      `<div class="panel-fade-in"><div class="panel-empty">` +
      `No music yet. <a href="#" id="openUploadEmpty">Upload files →</a>` +
      `</div></div>`;
    document.getElementById('openUploadEmpty')?.addEventListener('click', e => {
      e.preventDefault();
      openModal();
    });
    return;
  }

  // Build all rows as a fragment for a single DOM write.
  const wrap = document.createElement('div');
  wrap.className = 'panel-fade-in';

  artists.forEach(artist => {
    const displayName = artist.name || 'Unknown Artist';
    const count       = artist.track_count ?? 0;
    const row         = document.createElement('div');
    row.className     = 'panel-row artist-row';
    row.tabIndex      = 0;
    row.setAttribute('role', 'button');
    row.setAttribute('aria-label', `Browse ${displayName}`);
    row.innerHTML =
      `<span class="row-name">${esc(displayName)}</span>` +
      `<span class="row-meta">${count} track${count !== 1 ? 's' : ''}</span>` +
      `<span class="row-chevron" aria-hidden="true">›</span>`;
    row.addEventListener('click', () => drillToAlbums(artist.name));
    row.addEventListener('keydown', e => {
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); drillToAlbums(artist.name); }
    });
    wrap.appendChild(row);
  });

  panel.innerHTML = '';
  panel.appendChild(wrap);
}

// Render a list of album cards into #libraryPanel.
function renderAlbumList(albums) {
  const panel = document.getElementById('libraryPanel');

  if (!albums || albums.length === 0) {
    panel.innerHTML =
      `<div class="panel-fade-in"><div class="panel-empty">No albums found.</div></div>`;
    return;
  }

  const wrap = document.createElement('div');
  wrap.className = 'panel-fade-in';

  // Music note SVG used as art placeholder
  const noteSvg =
    `<svg width="20" height="20" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">` +
    `<path d="M12 3v10.55A4 4 0 1 0 14 17V7h4V3h-6z"/>` +
    `</svg>`;

  albums.forEach(album => {
    const title      = album.title       || 'Other';
    const count      = album.track_count ?? 0;
    const yearPrefix = album.year        ? `${album.year} · ` : '';
    const artContent = album.has_image
      ? `<img src="${API}/api/albums/${encodeURIComponent(album.title || '')}/image?artist=${encodeURIComponent(album.artist_name || '')}" alt="" loading="lazy">`
      : noteSvg;

    const row = document.createElement('div');
    row.className = 'panel-row album-row';
    row.tabIndex  = 0;
    row.setAttribute('role', 'button');
    row.setAttribute('aria-label', `Browse album ${title}`);
    row.innerHTML =
      `<div class="row-art">${artContent}</div>` +
      `<div class="row-body">` +
        `<div class="row-name">${esc(title)}</div>` +
        `<div class="row-meta">${esc(yearPrefix)}${count} track${count !== 1 ? 's' : ''}</div>` +
      `</div>` +
      `<span class="row-chevron" aria-hidden="true">›</span>`;
    row.addEventListener('click', () => drillToTracks(album.artist_name, album.title));
    row.addEventListener('keydown', e => {
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); drillToTracks(album.artist_name, album.title); }
    });
    wrap.appendChild(row);
  });

  panel.innerHTML = '';
  panel.appendChild(wrap);
}

// Render a list of track rows into #libraryPanel and populate playlist[].
function renderTrackList(tracks) {
  const panel = document.getElementById('libraryPanel');

  // Always reset playlist when entering a track list so player reflects current album.
  playlist = [];

  if (!tracks || tracks.length === 0) {
    panel.innerHTML =
      `<div class="panel-fade-in"><div class="panel-empty">No tracks found.</div></div>`;
    return;
  }

  const durCache = loadDurCache();

  // Populate shared playlist array first so playIndex() works immediately.
  tracks.forEach(t => {
    const url = `${API}${t.url}`;
    const dur = t.duration_seconds
      ? fmtTime(t.duration_seconds)
      : (durCache[url] || '—');
    playlist.push({
      url,
      title:  t.title  || t.filename || 'Unknown',
      artist: drill.artist || '',
      dur,
    });
  });

  const wrap = document.createElement('div');
  wrap.className = 'panel-fade-in';

  tracks.forEach((t, i) => {
    const displayNum = t.track_number || (i + 1);
    const track      = playlist[i];
    const row        = document.createElement('div');
    row.className    = 'track-row';
    row.tabIndex     = 0;
    row.dataset.idx  = i;
    row.setAttribute('role', 'button');
    row.setAttribute('aria-label', `Play ${track.title}`);
    row.innerHTML =
      `<span class="track-num">${displayNum}</span>` +
      `<span class="track-icon-playing" aria-hidden="true">` +
        `<svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor"><path d="M8 5v14l11-7z"/></svg>` +
      `</span>` +
      `<div class="track-info">` +
        `<div class="track-title">${esc(track.title)}</div>` +
        `<div class="track-meta">${esc(drill.artist || '')}</div>` +
      `</div>` +
      `<span class="track-dur">${esc(track.dur)}</span>`;
    row.addEventListener('click', () => playIndex(i));
    row.addEventListener('keydown', e => {
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); playIndex(i); }
    });
    wrap.appendChild(row);
  });

  panel.innerHTML = '';
  panel.appendChild(wrap);

  // Restore playing highlight if we're still on the same playlist.
  if (currentIndex >= 0 && currentIndex < playlist.length) {
    wrap.querySelectorAll('.track-row').forEach(row => {
      row.classList.toggle('playing', Number(row.dataset.idx) === currentIndex);
    });
  }

  // Background fetch for any tracks still showing '—'.
  fetchMissingDurations();
}

// Fetch audio metadata headers in the background for tracks still showing '—'.
// Runs 4 requests concurrently; saves results to localStorage so the next
// page load shows durations immediately without any fetch.
async function fetchMissingDurations() {
  const cache   = loadDurCache();
  const missing = playlist
    .map((track, i) => ({ track, i }))
    .filter(({ track }) => track.dur === '—');

  if (!missing.length) return;

  function fetchOne({ track, i }) {
    return new Promise(resolve => {
      const a   = new Audio();
      a.preload = 'metadata';
      a.src     = track.url;
      const done = () => {
        if (isFinite(a.duration) && a.duration > 0) {
          const dur        = fmtTime(a.duration);
          track.dur        = dur;
          cache[track.url] = dur;
          // Update every rendered row for this index
          document.querySelectorAll(`.track-row[data-idx="${i}"] .track-dur`)
            .forEach(el => { el.textContent = dur; });
        }
        a.src = ''; // release
        resolve();
      };
      a.addEventListener('loadedmetadata', done, { once: true });
      a.addEventListener('error',          done, { once: true });
    });
  }

  const CONCURRENCY = 4;
  for (let j = 0; j < missing.length; j += CONCURRENCY) {
    await Promise.all(missing.slice(j, j + CONCURRENCY).map(fetchOne));
    saveDurCache(cache);
  }
}

// Reload whichever drill level is currently shown — called after a successful upload.
async function reloadCurrentLevel() {
  if (drill.level === 'artists')     loadArtists();
  else if (drill.level === 'albums') drillToAlbums(drill.artist);
  else                               drillToTracks(drill.artist, drill.album);
}

// ── Player ───────────────────────────────────────────────────────────────

// playlist is declared in the library section above — shared here.
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

  // Match by data-idx so grouped views (non-sequential DOM order) work correctly
  document.querySelectorAll('.track-row').forEach(row => {
    row.classList.toggle('playing', Number(row.dataset.idx) === idx);
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

// When a track loads into the player, update its duration in the list immediately.
audio.addEventListener('loadedmetadata', () => {
  if (currentIndex < 0 || !isFinite(audio.duration) || audio.duration <= 0) return;
  const track = playlist[currentIndex];
  if (track.dur !== '—') return; // already known
  const dur = fmtTime(audio.duration);
  track.dur = dur;
  const cache = loadDurCache();
  cache[track.url] = dur;
  saveDurCache(cache);
  document.querySelectorAll(`.track-row[data-idx="${currentIndex}"] .track-dur`)
    .forEach(el => { el.textContent = dur; });
});
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
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = String(Math.floor(s % 60)).padStart(2, '0');
  return h > 0 ? `${h}:${String(m).padStart(2, '0')}:${sec}` : `${m}:${sec}`;
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
  await reloadCurrentLevel();
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
loadArtists();
