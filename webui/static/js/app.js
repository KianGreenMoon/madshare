// Read API base from HTML meta. Empty default => relative, same-origin URLs
// (the page and API share an origin in the bundled server). A non-empty value
// points a separately hosted UI at a remote API origin.
const API = document.querySelector('meta[name="api-url"]')?.content || '';

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
      `No music yet. <a href="/upload" id="openUploadEmpty">Upload files →</a>` +
      `</div></div>`;
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

  if (!tracks || tracks.length === 0) {
    playlist = [];
    playingFromSearch = false;
    currentIndex = -1;
    panel.innerHTML =
      `<div class="panel-fade-in"><div class="panel-empty">No tracks found.</div></div>`;
    return;
  }

  const durCache = loadDurCache();

  // Build a local list for this library view. We don't immediately clobber the
  // global playlist so that search-result playback continues uninterrupted while
  // the user browses the library without clicking a track.
  const libraryPlaylist = [];
  tracks.forEach(t => {
    const url = `${API}${t.url}`;
    const dur = t.duration_seconds
      ? fmtTime(t.duration_seconds)
      : (durCache[url] || '—');
    libraryPlaylist.push({
      url,
      title:  t.title  || t.filename || 'Unknown',
      artist: drill.artist || '',
      dur,
    });
  });

  // If not mid-search-playback, make this the active playlist immediately so
  // Next/Prev and the playing highlight reflect the library view.
  if (!playingFromSearch) {
    playlist = libraryPlaylist;
    if (currentIndex >= playlist.length) currentIndex = -1;
  }

  const wrap = document.createElement('div');
  wrap.className = 'panel-fade-in';

  tracks.forEach((t, i) => {
    const displayNum = t.track_number || (i + 1);
    const track      = libraryPlaylist[i];
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
    row.addEventListener('click', () => {
      playingFromSearch = false;
      playlist = libraryPlaylist;
      playIndex(i);
    });
    row.addEventListener('keydown', e => {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        playingFromSearch = false;
        playlist = libraryPlaylist;
        playIndex(i);
      }
    });
    wrap.appendChild(row);
  });

  panel.innerHTML = '';
  panel.appendChild(wrap);

  // Restore playing highlight only when in library playback mode.
  if (!playingFromSearch && currentIndex >= 0 && currentIndex < playlist.length) {
    wrap.querySelectorAll('.track-row').forEach(row => {
      row.classList.toggle('playing', Number(row.dataset.idx) === currentIndex);
    });
  }

  // Background fetch for any tracks still showing '—'.
  fetchMissingDurations(libraryPlaylist);
}

// Fetch audio metadata headers in the background for tracks still showing '—'.
// Runs 4 requests concurrently; saves results to localStorage so the next
// page load shows durations immediately without any fetch.
async function fetchMissingDurations(list) {
  const cache   = loadDurCache();
  const missing = list
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
let currentIndex     = -1;
let stopped          = false;
let shuffle          = false;
let playingFromSearch = false; // true while a search-result track is the active source

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
    const rowIdx = Number(row.dataset.idx);
    row.classList.toggle('playing', rowIdx === idx);
    if (rowIdx === idx) row.classList.remove('unavailable');
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

audio.addEventListener('error', () => {
  const failedRow = document.querySelector(`.track-row[data-idx="${currentIndex}"]`);
  if (failedRow) failedRow.classList.add('unavailable');
  if (shuffle && playlist.length > 1) {
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

// ── Search ───────────────────────────────────────────────────────────────

const searchInput = document.querySelector('.header__search-input');
const searchClear = document.querySelector('.header__search-clear');
const viewLibrary = document.getElementById('view-library');
const viewSearch  = document.getElementById('view-search');

let lastQuery   = '';
let searchTimer = null;
let searchAbort = null;

searchInput.addEventListener('input', () => {
  const q = searchInput.value.trim();
  searchClear.style.display = searchInput.value ? '' : 'none';
  if (q.length < 2) {
    showLibraryView();
    return;
  }
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => runSearch(q), 300);
});

searchInput.addEventListener('keydown', e => {
  if (e.key === 'Escape') clearSearch();
});

searchClear.addEventListener('click', clearSearch);

// Clear search when Library nav is clicked — prevents a full page reload and
// keeps SPA behaviour consistent with navigating back from search results.
document.querySelector('.nav-link[href="/"]')?.addEventListener('click', e => {
  e.preventDefault();
  clearSearch();
});

function clearSearch() {
  searchInput.value = '';
  searchClear.style.display = 'none';
  lastQuery = '';
  showLibraryView();
}

function showLibraryView() {
  viewLibrary.classList.add('view-panel--active');
  viewLibrary.classList.remove('view-panel--hidden');
  viewSearch.classList.add('view-panel--hidden');
  viewSearch.classList.remove('view-panel--active');
}

function showSearchView() {
  viewSearch.classList.add('view-panel--active');
  viewSearch.classList.remove('view-panel--hidden');
  viewLibrary.classList.add('view-panel--hidden');
  viewLibrary.classList.remove('view-panel--active');
}

async function runSearch(q) {
  if (q === lastQuery) return;
  lastQuery = q;

  // Cancel any in-flight request for an older query.
  if (searchAbort) searchAbort.abort();
  searchAbort = new AbortController();

  showSearchView();
  viewSearch.innerHTML = '<div class="search-loading-bar"></div>';

  let results;
  try {
    const res = await fetch(`${API}/api/search?q=${encodeURIComponent(q)}`, { signal: searchAbort.signal });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    results = await res.json();
    searchAbort = null;
  } catch (err) {
    if (err.name === 'AbortError') return; // superseded by a newer query — discard silently
    lastQuery = ''; // allow retry with the same query after a real error
    viewSearch.innerHTML =
      '<p style="color:var(--error);padding:16px;text-align:center">' +
      'Search failed — check your connection and try again.</p>';
    return;
  }

  renderSearchResults(results, q);
}

function renderSearchResults(results, q) {
  const { artists = [], albums = [], tracks = [] } = results;

  if (!artists.length && !albums.length && !tracks.length) {
    const qEsc = esc(q.length > 40 ? q.slice(0, 40) + '…' : q);
    viewSearch.innerHTML =
      `<div class="search-empty-state">` +
      `<div class="search-empty-state__query">No results for "<em>${qEsc}</em>"</div>` +
      `<div class="search-empty-state__hint">Try a different search term</div>` +
      `</div>`;
    return;
  }

  const frag = document.createDocumentFragment();

  if (artists.length) {
    const sec = document.createElement('section');
    sec.className = 'search-section';
    sec.innerHTML = '<h2 class="search-section__header">Artists</h2>';
    artists.forEach(a => {
      const row = document.createElement('div');
      row.className = 'search-row search-row--artist';
      row.tabIndex  = 0;
      row.setAttribute('role', 'button');
      row.setAttribute('aria-label', `Browse artist ${a.name}`);
      row.innerHTML =
        `<div class="search-row__avatar">${esc((a.name || '?')[0].toUpperCase())}</div>` +
        `<div class="search-row__title">${esc(a.name || 'Unknown Artist')}</div>`;
      row.addEventListener('click', () => { clearSearch(); drillToAlbums(a.name); });
      row.addEventListener('keydown', e => {
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); clearSearch(); drillToAlbums(a.name); }
      });
      sec.appendChild(row);
    });
    frag.appendChild(sec);
  }

  if (albums.length) {
    const noteSvg =
      `<svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">` +
      `<path d="M12 3v10.55A4 4 0 1 0 14 17V7h4V3h-6z"/></svg>`;
    const sec = document.createElement('section');
    sec.className = 'search-section';
    sec.innerHTML = '<h2 class="search-section__header">Albums</h2>';
    albums.forEach(a => {
      const artContent = a.has_image
        ? `<img src="${API}/api/albums/${encodeURIComponent(a.title || '')}/image?artist=${encodeURIComponent(a.artist_name || '')}" alt="" loading="lazy">`
        : noteSvg;
      const row = document.createElement('div');
      row.className = 'search-row search-row--album';
      row.tabIndex  = 0;
      row.setAttribute('role', 'button');
      row.setAttribute('aria-label', `Browse album ${a.title}`);
      row.innerHTML =
        `<div class="search-row__thumb">${artContent}</div>` +
        `<div class="search-row__body">` +
          `<div class="search-row__title">${esc(a.title || 'Other')}</div>` +
          `<div class="search-row__subtitle">${esc(a.artist_name || 'Unknown Artist')}</div>` +
        `</div>`;
      row.addEventListener('click', () => { clearSearch(); drillToTracks(a.artist_name, a.title); });
      row.addEventListener('keydown', e => {
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); clearSearch(); drillToTracks(a.artist_name, a.title); }
      });
      sec.appendChild(row);
    });
    frag.appendChild(sec);
  }

  if (tracks.length) {
    const noteSvg =
      `<svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor">` +
      `<path d="M12 3v10.55A4 4 0 1 0 14 17V7h4V3h-6z"/></svg>`;
    const playSvg =
      `<svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor">` +
      `<path d="M8 5v14l11-7z"/></svg>`;
    const sec = document.createElement('section');
    sec.className = 'search-section';
    sec.innerHTML = '<h2 class="search-section__header">Tracks</h2>';

    // Build a playlist from search results so the player can advance track-by-track.
    const searchPlaylist = tracks.map(t => ({
      url:    `${API}${t.url}`,
      title:  t.title       || 'Unknown',
      artist: t.artist_name || '',
    }));

    tracks.forEach((t, i) => {
      const dur      = t.duration_seconds ? fmtTime(t.duration_seconds) : '';
      const subtitle = [t.artist_name, t.album_title].filter(Boolean).join(' · ');
      const row      = document.createElement('div');
      row.className  = 'search-row search-row--track';
      row.tabIndex   = 0;
      row.setAttribute('role', 'button');
      row.setAttribute('aria-label', `Play ${t.title}`);
      row.innerHTML =
        `<div class="search-row__avatar search-row__avatar--note">${noteSvg}</div>` +
        `<div class="search-row__body">` +
          `<div class="search-row__title">${esc(t.title || 'Unknown')}</div>` +
          `<div class="search-row__subtitle">${esc(subtitle)}</div>` +
        `</div>` +
        (dur ? `<div class="search-row__duration">${esc(dur)}</div>` : '');

      // Swap note/play icon on hover to signal the row is playable.
      const avatar = row.querySelector('.search-row__avatar--note');
      row.addEventListener('mouseenter', () => {
        avatar.innerHTML = playSvg;
        avatar.style.color = 'var(--accent)';
      });
      row.addEventListener('mouseleave', () => {
        avatar.innerHTML = noteSvg;
        avatar.style.color = '';
      });

      row.addEventListener('click', () => {
        playingFromSearch = true;
        playlist = searchPlaylist;
        playIndex(i);
      });
      row.addEventListener('keydown', e => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          playingFromSearch = true;
          playlist = searchPlaylist;
          playIndex(i);
        }
      });
      sec.appendChild(row);
    });
    frag.appendChild(sec);
  }

  viewSearch.innerHTML = '';
  viewSearch.appendChild(frag);
}

// ── Upload modal ─────────────────────────────────────────────────────────

const modal     = document.getElementById('uploadModal');
const dropZone  = document.getElementById('dropZone');
const fileInput = document.getElementById('fileInput');
const status    = document.getElementById('uploadStatus');

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
    if (res.status === 401 || res.status === 403) {
      setStatus('Uploading requires signing in.', 'error');
      openLoginModal();
      return;
    }
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

// ── Auth ─────────────────────────────────────────────────────────────────

const userArea    = document.getElementById('userArea');
const userName    = document.getElementById('userName');
const signInBtn   = document.getElementById('signInBtn');
const logoutBtn   = document.getElementById('logoutBtn');
const changePassBtn = document.getElementById('changePassBtn');
const loginModal  = document.getElementById('loginModal');
const loginForm   = document.getElementById('loginForm');
const loginUser   = document.getElementById('loginUser');
const loginPass   = document.getElementById('loginPass');
const loginError  = document.getElementById('loginError');
const loginClose  = document.getElementById('loginClose');
const loginCancel = document.getElementById('loginCancel');

let currentUser = null;

async function fetchIdentity() {
  try {
    const res = await fetch(`${API}/api/auth/me`);
    if (!res.ok) return null;
    return await res.json();
  } catch {
    return null;
  }
}

function showSignedIn(identity) {
  currentUser = identity;
  userArea.hidden = false;
  userName.textContent = identity.username;
  signInBtn.hidden = true;
  if (identity.password_change_required) openPassModal(true);
}

function showSignedOut() {
  currentUser = null;
  userArea.hidden = true;
  signInBtn.hidden = false;
}

function openLoginModal() {
  loginForm.reset();
  loginError.hidden = true;
  loginModal.classList.remove('hidden');
  loginUser.focus();
}

function closeLoginModal() {
  loginModal.classList.add('hidden');
}

signInBtn.addEventListener('click', openLoginModal);
loginClose.addEventListener('click', closeLoginModal);
loginCancel.addEventListener('click', closeLoginModal);
loginModal.addEventListener('click', e => { if (e.target === loginModal) closeLoginModal(); });
loginModal.addEventListener('keydown', e => { if (e.key === 'Escape') closeLoginModal(); });

loginForm.addEventListener('submit', async e => {
  e.preventDefault();
  loginError.hidden = true;
  try {
    const res = await fetch(`${API}/api/auth/login`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username: loginUser.value, password: loginPass.value }),
    });
    if (!res.ok) {
      loginError.textContent = res.status === 401
        ? 'Invalid username or password.'
        : `Sign-in failed (HTTP ${res.status}).`;
      loginError.hidden = false;
      return;
    }
    loginPass.value = '';
    location.reload();
  } catch (err) {
    loginError.textContent = `Sign-in failed: ${err.message}`;
    loginError.hidden = false;
  }
});

logoutBtn.addEventListener('click', async () => {
  try { await fetch(`${API}/api/auth/logout`, { method: 'POST' }); } catch {}
  location.reload();
});

// ── Change password ───────────────────────────────────────────────────────

const passModal   = document.getElementById('passModal');
const passForm    = document.getElementById('passForm');
const oldPass     = document.getElementById('oldPass');
const newPass     = document.getElementById('newPass');
const confirmPass = document.getElementById('confirmPass');
const passError   = document.getElementById('passError');
const passForced  = document.getElementById('passForced');
const passCancel  = document.getElementById('passCancel');
const passClose   = document.getElementById('passClose');

let passIsForced = false;

function openPassModal(forced) {
  passIsForced = forced;
  passForm.reset();
  passError.hidden = true;
  passForced.hidden = !forced;
  passCancel.hidden = forced;
  passClose.hidden = forced;
  passModal.classList.remove('hidden');
  oldPass.focus();
}

function closePassModal() {
  if (passIsForced) return;
  passModal.classList.add('hidden');
}

changePassBtn.addEventListener('click', () => openPassModal(false));
passCancel.addEventListener('click', closePassModal);
passClose.addEventListener('click', closePassModal);
passModal.addEventListener('click', e => { if (e.target === passModal) closePassModal(); });
passModal.addEventListener('keydown', e => { if (e.key === 'Escape') closePassModal(); });

passForm.addEventListener('submit', async e => {
  e.preventDefault();
  passError.hidden = true;
  if (newPass.value !== confirmPass.value) {
    passError.textContent = 'New passwords do not match.';
    passError.hidden = false;
    return;
  }
  if (newPass.value.length < 8) {
    passError.textContent = 'New password must be at least 8 characters.';
    passError.hidden = false;
    return;
  }
  try {
    const res = await fetch(`${API}/api/auth/password`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ old_password: oldPass.value, new_password: newPass.value }),
    });
    if (!res.ok) {
      const msg = (await res.text()).trim();
      if (res.status === 401 && /authentication required/i.test(msg)) {
        passIsForced = false;
        passModal.classList.add('hidden');
        showSignedOut();
        return;
      }
      passError.textContent = res.status === 401
        ? 'Current password is incorrect.'
        : `Couldn't change password: ${msg || `HTTP ${res.status}`}`;
      passError.hidden = false;
      return;
    }
    passIsForced = false;
    passModal.classList.add('hidden');
    toast('Password changed.', 'success');
    const identity = await fetchIdentity();
    if (identity) { currentUser = identity; }
  } catch (err) {
    passError.textContent = `Couldn't change password: ${err.message}`;
    passError.hidden = false;
  }
});

// ── Toasts ───────────────────────────────────────────────────────────────

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
  if (type !== 'error') setTimeout(() => el.remove(), 4000);
}

// ── Utilities ────────────────────────────────────────────────────────────

function esc(s) {
  return String(s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;')
    .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

// ── Boot ─────────────────────────────────────────────────────────────────
(async function boot() {
  const identity = await fetchIdentity();
  if (identity) showSignedIn(identity);
  loadArtists();
})();
