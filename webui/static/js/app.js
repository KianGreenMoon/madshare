import { openLoginModal } from './auth.js';
import { getController } from './player-controller.js';
import { fmtTime } from './player.js';
import { ensureLiked, isLiked, toggleLike, onLikedChange } from './favorites.js';
import { openRowMenu } from './row-menu.js';
import { showToast } from './shell.js';

// Read API base from HTML meta. Empty default => relative, same-origin URLs
// (the page and API share an origin in the bundled server). A non-empty value
// points a separately hosted UI at a remote API origin.
const API = document.querySelector('meta[name="api-url"]')?.content || '';

// Theme is owned by shell.js (persistent header, applied once across pages).

// Duration cache: shared with the queue panel and the playlists page so
// already-known durations show everywhere (dur-cache.js).
import { loadDurCache, saveDurCache } from './dur-cache.js';

// ── Library — drill-down state ────────────────────────────────────────────

// drill tracks which panel level is currently shown and what was selected.
// Browse fetches address entities by their stable id (artistId / albumId); the
// display names (artist / album) ride alongside only for the breadcrumb + track
// metadata text.
const drill = { level: 'artists', artist: null, album: null, artistId: null, albumId: null };

// Fetch and render the top-level artists panel.
async function loadArtists() {
  drill.level    = 'artists';
  drill.artist   = null;
  drill.album    = null;
  drill.artistId = null;
  drill.albumId  = null;

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

  if (!active) return; // navigated away while loading
  renderBreadcrumb();
  renderArtistList(artists);
}

// Fetch and render the albums panel for a given artist (addressed by id).
async function drillToAlbums(artistId, artistName) {
  drill.level    = 'albums';
  drill.artist   = artistName;
  drill.artistId = artistId;
  drill.album    = null;
  drill.albumId  = null;

  const panel = document.getElementById('libraryPanel');
  panel.innerHTML = '';

  let albums;
  try {
    const res = await fetch(`${API}/api/albums?artist_id=${encodeURIComponent(artistId)}`);
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    albums = await res.json();
  } catch (err) {
    console.error('Failed to load albums:', err);
    panel.innerHTML = '<div class="panel-empty" role="alert">Failed to load albums.</div>';
    return;
  }

  if (!active) return;
  renderBreadcrumb();
  renderAlbumList(albums);
}

// Fetch and render the tracks panel for a given album (addressed by id). The
// artist id/name ride along so the breadcrumb can step back up to the albums.
async function drillToTracks(albumId, artistId, artistName, albumTitle) {
  drill.level    = 'tracks';
  drill.artist   = artistName;
  drill.artistId = artistId;
  drill.album    = albumTitle;
  drill.albumId  = albumId;

  const panel = document.getElementById('libraryPanel');
  panel.innerHTML = '';

  let tracks;
  try {
    const res = await fetch(`${API}/api/tracks?album_id=${encodeURIComponent(albumId)}`);
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    tracks = await res.json();
  } catch (err) {
    console.error('Failed to load tracks:', err);
    panel.innerHTML = '<div class="panel-empty" role="alert">Failed to load tracks.</div>';
    return;
  }

  if (!active) return;
  renderBreadcrumb();
  renderTrackList(tracks);
}

// Rebuild the breadcrumb nav from current drill state.
// Uses addEventListener (not onclick) — module scripts don't share global scope.
//
// The breadcrumb holds only the drill path BELOW the section root: the "Music"
// subtab already labels the section and is the back-to-top affordance, so we never
// repeat it here. At the artists (top) level there's nothing to show, so the whole
// bar is hidden to avoid an empty strip.
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

  if (drill.level === 'albums') {
    bc.appendChild(mkCurrent(displayArtist));
  } else if (drill.level === 'tracks') {
    const capturedArtistId = drill.artistId;
    const capturedArtist   = drill.artist;
    bc.appendChild(mkLink(displayArtist, () => drillToAlbums(capturedArtistId, capturedArtist)));
    bc.appendChild(mkSep());
    bc.appendChild(mkCurrent(displayAlbum));
  }
  // 'artists' (top) level: empty — the Music subtab is the label and the way back.
  const bar = bc.closest('.library-bar');
  if (bar) bar.style.display = bc.children.length ? '' : 'none';
}

// ── Favorites & quick-add (Phase 5 step 4, docs/api/playlists.md) ─────────

const heartSvg =
  `<svg width="15" height="15" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">` +
  `<path d="M12 21.35l-1.45-1.32C5.4 15.36 2 12.28 2 8.5 2 5.42 4.42 3 7.5 3c1.74 0 ` +
  `3.41.81 4.5 2.09C13.09 3.81 14.76 3 16.5 3 19.58 3 22 5.42 22 8.5c0 3.78-3.4 ` +
  `6.86-8.55 11.54L12 21.35z"/></svg>`;

// repaintHearts syncs every rendered heart with the shared liked set; runs on
// each render and whenever the set changes (any heart, any page, player bar).
function repaintHearts() {
  document.querySelectorAll('.row-heart[data-hash]').forEach(b => {
    const on = isLiked(b.dataset.hash);
    b.classList.toggle('liked', on);
    b.setAttribute('aria-pressed', String(on));
    const label = on ? 'Remove from Favorites' : 'Add to Favorites';
    b.setAttribute('aria-label', label);
    b.title = label;
  });
}
onLikedChange(repaintHearts);

// trackObjFromApi maps a browse-endpoint track to a controller track object.
function trackObjFromApi(t, artistName, durCache) {
  const url = `${API}${t.url}`;
  return {
    url,
    hash:   t.url.split('/')[2] || null,
    title:  t.title || 'Unknown',
    // Per-track performer when present; else the album/artist passed by the caller.
    artist: t.artist_name || artistName || '',
    dur:    t.duration_seconds ? fmtTime(t.duration_seconds) : (durCache[url] || '—'),
  };
}

// entityTracks collects the controller tracks for a whole album (albumId given)
// — or a whole artist (albumId == null → every album of artistId, in browse
// order). artistName is used only for the track objects' display text.
async function entityTracks(artistId, albumId, artistName) {
  const durCache = loadDurCache();
  const fetchAlbum = async id => {
    const res = await fetch(`${API}/api/tracks?album_id=${encodeURIComponent(id)}`);
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    return (await res.json()).map(t => trackObjFromApi(t, artistName, durCache));
  };
  if (albumId != null) return fetchAlbum(albumId);
  const res = await fetch(`${API}/api/albums?artist_id=${encodeURIComponent(artistId)}`);
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  const albums = await res.json();
  const lists = await Promise.all(albums.map(a => fetchAlbum(a.id)));
  return lists.flat();
}

// queueAdd runs a (possibly async) track collector and applies a queue action.
async function queueAdd(collect, how) {
  let tracks;
  try { tracks = await collect(); }
  catch { showToast('Failed to load tracks.', { type: 'error' }); return; }
  if (!tracks.length) return;
  if (how === 'next') controller.playNext(tracks);
  else controller.enqueue(tracks);
  showToast(`${tracks.length} track${tracks.length !== 1 ? 's' : ''} ${how === 'next' ? 'will play next' : 'added to queue'}.`,
    { type: 'success' });
}

// addToPlaylistMenu replaces the open row menu with a playlist picker (plus
// "New playlist…"), then posts the collected tracks' hashes.
async function addToPlaylistMenu(anchor, collect) {
  let lists, tracks;
  try {
    const res = await fetch(`${API}/api/playlists`);
    if (res.status === 401 || res.status === 403) { openLoginModal(); return; }
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    lists = await res.json();
    tracks = await collect();
  } catch { showToast('Failed to load playlists.', { type: 'error' }); return; }
  const hashes = tracks.map(t => t.hash).filter(Boolean);
  if (!hashes.length) return;

  const add = async (id, name) => {
    try {
      const res = await fetch(`${API}/api/playlists/${id}/items`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ hashes }),
      });
      if (!res.ok) throw new Error((await res.text().catch(() => '')).trim() || `HTTP ${res.status}`);
      const { added } = await res.json();
      showToast(`Added ${added} track${added !== 1 ? 's' : ''} to "${name}".`, { type: 'success' });
      if (added === 0 && hashes.length) showToast(`Already in "${name}".`, { type: 'status' });
    } catch (err) {
      showToast(`Couldn't add to "${name}": ${err.message}`, { type: 'error' });
    }
  };
  const items = lists.map(p => ({
    label: (p.kind === 'favorites' ? '♥ ' : '') + p.name,
    onClick: () => add(p.id, p.name),
  }));
  items.push({
    input: 'New playlist…',
    onSubmit: async name => {
      try {
        const res = await fetch(`${API}/api/playlists`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name, hashes }),
        });
        if (!res.ok) throw new Error((await res.text().catch(() => '')).trim() || `HTTP ${res.status}`);
        showToast(`Created "${name}" with ${hashes.length} track${hashes.length !== 1 ? 's' : ''}.`, { type: 'success' });
      } catch (err) {
        showToast(`Couldn't create playlist: ${err.message}`, { type: 'error' });
      }
    },
  });
  openRowMenu(anchor, items);
}

// quickAddItems builds the "⋯" menu for a row. collect yields the row's tracks
// (a single track, an album, or a whole artist).
function quickAddItems(anchor, collect, { likeHash } = {}) {
  const items = [
    { label: 'Play next',       onClick: () => queueAdd(collect, 'next') },
    { label: 'Add to queue',    onClick: () => queueAdd(collect, 'queue') },
    { label: 'Add to playlist…', keepOpen: true, onClick: () => addToPlaylistMenu(anchor, collect) },
  ];
  if (likeHash) {
    items.push({
      label: isLiked(likeHash) ? 'Remove from Favorites' : 'Add to Favorites',
      onClick: () => toggleLike(likeHash),
    });
  }
  return items;
}

// mkMoreBtn returns the "⋯" row button wired to the quick-add menu.
function mkMoreBtn(label, makeItems) {
  const btn = document.createElement('button');
  btn.className = 'row-more';
  btn.setAttribute('aria-label', label);
  btn.setAttribute('aria-haspopup', 'menu');
  btn.title = 'More actions';
  btn.textContent = '⋯';
  btn.addEventListener('click', e => {
    e.stopPropagation();
    openRowMenu(btn, makeItems(btn));
  });
  return btn;
}

// mkHeartBtn returns a heart button for a track row (state via repaintHearts).
function mkHeartBtn(hash) {
  const btn = document.createElement('button');
  btn.className = 'row-heart';
  btn.dataset.hash = hash || '';
  btn.setAttribute('aria-pressed', 'false');
  btn.setAttribute('aria-label', 'Add to Favorites');
  btn.title = 'Add to Favorites';
  btn.innerHTML = heartSvg;
  btn.addEventListener('click', e => {
    e.stopPropagation();
    if (hash) toggleLike(hash);
  });
  return btn;
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
    row.insertBefore(
      mkMoreBtn(`More actions for ${displayName}`,
        btn => quickAddItems(btn, () => entityTracks(artist.id, null, artist.name))),
      row.querySelector('.row-chevron'));
    row.addEventListener('click', () => drillToAlbums(artist.id, artist.name));
    row.addEventListener('keydown', e => {
      if (e.target !== row) return; // buttons inside the row handle their own keys
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); drillToAlbums(artist.id, artist.name); }
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
      ? `<img src="${API}/api/albums/${encodeURIComponent(album.id)}/image" alt="" loading="lazy">`
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
    row.insertBefore(
      mkMoreBtn(`More actions for ${title}`,
        btn => quickAddItems(btn, () => entityTracks(null, album.id, album.artist_name))),
      row.querySelector('.row-chevron'));
    row.addEventListener('click', () => drillToTracks(album.id, album.artist_id, album.artist_name, album.title));
    row.addEventListener('keydown', e => {
      if (e.target !== row) return;
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); drillToTracks(album.id, album.artist_id, album.artist_name, album.title); }
    });
    wrap.appendChild(row);
  });

  panel.innerHTML = '';
  panel.appendChild(wrap);
}

// Render a list of track rows into #libraryPanel. Builds the queue this view
// would play, but does not touch the controller's active queue — that changes
// only when the user clicks a track (controller.setQueue).
function renderTrackList(tracks) {
  const panel = document.getElementById('libraryPanel');

  if (!tracks || tracks.length === 0) {
    panel.innerHTML =
      `<div class="panel-fade-in"><div class="panel-empty">No tracks found.</div></div>`;
    return;
  }

  const durCache = loadDurCache();

  const libraryPlaylist = [];
  tracks.forEach(t => {
    const url = `${API}${t.url}`;
    const dur = t.duration_seconds
      ? fmtTime(t.duration_seconds)
      : (durCache[url] || '—');
    libraryPlaylist.push({
      url,
      // hash rides on the track for the queue panel's "Save as playlist"
      // (t.url is /files/<hash>/<filename>).
      hash:   t.url.split('/')[2] || null,
      title:  t.title  || t.filename || 'Unknown',
      // Per-track performer (matches the playlists page); falls back to the
      // album-grouping artist from the breadcrumb when a row has no performer.
      artist: t.artist_name || drill.artist || '',
      dur,
    });
  });

  const wrap = document.createElement('div');
  wrap.className = 'panel-fade-in';

  // Multi-disc albums (more than one distinct disc number, untagged = disc 1)
  // get a "Disc N" subheading before each disc; the queue stays one flat ordered
  // list, so the headers are purely visual and don't shift track indices.
  const multiDisc = new Set(tracks.map(t => t.disc_number || 1)).size > 1;
  let shownDisc = null, discTrackNo = 0;

  tracks.forEach((t, i) => {
    const disc = t.disc_number || 1;
    if (multiDisc && disc !== shownDisc) {
      shownDisc = disc;
      discTrackNo = 0;
      const hdr = document.createElement('div');
      hdr.className = 'track-disc-header';
      hdr.textContent = `Disc ${disc}`;
      wrap.appendChild(hdr);
    }
    discTrackNo++;
    const displayNum = t.track_number || (multiDisc ? discTrackNo : i + 1);
    const track      = libraryPlaylist[i];
    const row        = document.createElement('div');
    row.className    = 'track-row';
    row.tabIndex     = 0;
    row.dataset.idx  = i;          // used by the background duration fetch
    row.dataset.url  = track.url;  // stable key for the playing highlight
    row.setAttribute('role', 'button');
    row.setAttribute('aria-label', `Play ${track.title}`);
    row.innerHTML =
      `<span class="track-num">${displayNum}</span>` +
      `<span class="track-icon-playing" aria-hidden="true">` +
        `<svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor"><path d="M8 5v14l11-7z"/></svg>` +
      `</span>` +
      `<div class="track-info">` +
        `<div class="track-title">${esc(track.title)}</div>` +
        `<div class="track-meta">${esc(track.artist)}</div>` +
      `</div>` +
      `<span class="track-dur">${esc(track.dur)}</span>`;
    const durEl = row.querySelector('.track-dur');
    row.insertBefore(mkHeartBtn(track.hash), durEl);
    row.insertBefore(
      mkMoreBtn(`More actions for ${track.title}`,
        btn => quickAddItems(btn, () => [track], { likeHash: track.hash })),
      durEl);
    const play = () => controller.setQueue(libraryPlaylist, i);
    row.addEventListener('click', play);
    row.addEventListener('keydown', e => {
      if (e.target !== row) return;
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); play(); }
    });
    wrap.appendChild(row);
  });

  panel.innerHTML = '';
  panel.appendChild(wrap);

  // Re-highlight whatever is currently playing if its row is in this view.
  const cur = controller.current();
  if (cur) highlightPlaying(cur.track.url);
  repaintHearts();

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

// ── Player (thin caller over player-controller.js) ─────────────────────────
// The controller is the SHARED singleton (created by shell.js, same instance
// here); it owns the <audio>, the player-bar, and the play QUEUE. The page
// builds queues (controller.setQueue on a track click) and reflects state — row
// highlighting and duration write-back — through the subscriptions below
// (module-scoped: they run once and persist, and are harmless on other pages
// since they match rows by data-url). Auth-expiry and the queue-replaced undo
// toast are shell concerns, wired in shell.js. The queue is stable: browsing
// never changes it, only an explicit play or a manual queue edit does.

const controller = getController();
controller.on('trackchange', track => highlightPlaying(track.url));
controller.on('duration', writeDuration);
controller.on('error', track => markUnavailable(track.url));

// highlightPlaying marks the track row whose URL is playing (and clears the rest).
function highlightPlaying(url) {
  document.querySelectorAll('.track-row').forEach(row => {
    const on = row.dataset.url === url;
    row.classList.toggle('playing', on);
    if (on) row.classList.remove('unavailable');
  });
}

function markUnavailable(url) {
  document.querySelectorAll('.track-row').forEach(row => {
    if (row.dataset.url === url) row.classList.add('unavailable');
  });
}

// writeDuration fills a newly-known duration into the track object, the cache,
// and every rendered row for that URL (library .track-dur or search duration).
function writeDuration(track, durSeconds) {
  if (!track || (track.dur && track.dur !== '—')) return; // already known
  const s = fmtTime(durSeconds);
  track.dur = s;
  const cache = loadDurCache();
  cache[track.url] = s;
  saveDurCache(cache);
  document.querySelectorAll('[data-url]').forEach(row => {
    if (row.dataset.url !== track.url) return;
    const el = row.querySelector('.track-dur') || row.querySelector('.search-row__duration');
    if (el) el.textContent = s;
  });
}

// ── Search ───────────────────────────────────────────────────────────────

// These elements live in swappable DOM (inside <main>, above the view panels), so
// they're re-queried and re-wired by wireSearch() on each init() and removed via
// the AbortController on teardown(). Nav is owned by shell.js now — the old
// "clear search on Library click" hack is gone (re-entering the library is a
// shell swap, which resets to the artists view).
let searchInput = null;
let searchClear = null;
let viewLibrary = null;
let viewSearch  = null;

let lastQuery   = '';
let searchTimer = null;
let searchAbort = null;

function wireSearch(signal) {
  searchInput = document.querySelector('.library-search__input');
  searchClear = document.querySelector('.library-search__clear');
  viewLibrary = document.getElementById('view-library');
  viewSearch  = document.getElementById('view-search');
  if (!searchInput) return;

  searchInput.addEventListener('input', () => {
    const q = searchInput.value.trim();
    searchClear.style.display = searchInput.value ? '' : 'none';
    if (q.length < 2) { showLibraryView(); return; }
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => runSearch(q), 300);
  }, { signal });

  searchInput.addEventListener('keydown', e => {
    if (e.key === 'Escape') clearSearch();
  }, { signal });

  searchClear.addEventListener('click', clearSearch, { signal });
}

function clearSearch() {
  if (searchInput) searchInput.value = '';
  if (searchClear) searchClear.style.display = 'none';
  lastQuery = '';
  showLibraryView();
}

function showLibraryView() {
  if (!viewLibrary || !viewSearch) return;
  viewLibrary.classList.add('view-panel--active');
  viewLibrary.classList.remove('view-panel--hidden');
  viewSearch.classList.add('view-panel--hidden');
  viewSearch.classList.remove('view-panel--active');
}

function showSearchView() {
  if (!viewLibrary || !viewSearch) return;
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
    if (viewSearch) viewSearch.innerHTML =
      '<p style="color:var(--error);padding:16px;text-align:center">' +
      'Search failed — check your connection and try again.</p>';
    return;
  }

  if (!active) return; // navigated away while searching
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
      row.addEventListener('click', () => { clearSearch(); drillToAlbums(a.id, a.name); });
      row.addEventListener('keydown', e => {
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); clearSearch(); drillToAlbums(a.id, a.name); }
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
        ? `<img src="${API}/api/albums/${encodeURIComponent(a.id)}/image" alt="" loading="lazy">`
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
      row.addEventListener('click', () => { clearSearch(); drillToTracks(a.id, a.artist_id, a.artist_name, a.title); });
      row.addEventListener('keydown', e => {
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); clearSearch(); drillToTracks(a.id, a.artist_id, a.artist_name, a.title); }
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

    // Build the queue a search-result click would play (controller.setQueue).
    const searchPlaylist = tracks.map(t => ({
      url:    `${API}${t.url}`,
      hash:   t.url.split('/')[2] || null,
      title:  t.title       || 'Unknown',
      artist: t.artist_name || '',
    }));

    tracks.forEach((t, i) => {
      const dur      = t.duration_seconds ? fmtTime(t.duration_seconds) : '';
      const subtitle = [t.artist_name, t.album_title].filter(Boolean).join(' · ');
      const row      = document.createElement('div');
      row.className  = 'search-row search-row--track';
      row.tabIndex   = 0;
      row.dataset.url = searchPlaylist[i].url; // stable key for the playing highlight
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

      // Heart between the row body and the duration (matches the library rows).
      row.insertBefore(mkHeartBtn(searchPlaylist[i].hash),
        row.querySelector('.search-row__duration'));

      const play = () => controller.setQueue(searchPlaylist, i);
      row.addEventListener('click', play);
      row.addEventListener('keydown', e => {
        if (e.target !== row) return;
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); play(); }
      });
      sec.appendChild(row);
    });
    frag.appendChild(sec);
  }

  viewSearch.innerHTML = '';
  viewSearch.appendChild(frag);
  repaintHearts();
}

// ── Utilities ────────────────────────────────────────────────────────────

function esc(s) {
  return String(s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;')
    .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

// ── Lifecycle (driven by shell.js) ─────────────────────────────────────────
// init() runs on first load and on every navigation back to the library; it
// re-wires the swappable DOM (search bar, views) and (re)loads the artists.
// teardown() runs before navigating away: it removes those listeners, cancels
// timers and aborts in-flight fetches. The player/controller is NOT torn down —
// it lives in the persistent shell so playback survives navigation.
let abort  = null;     // AbortController for this activation's listeners
let active = false;    // guards late async renders after teardown

export function init() {
  active = true;
  abort = new AbortController();
  wireSearch(abort.signal);
  ensureLiked(); // hearts repaint via onLikedChange once the set arrives
  loadArtists();
}

export function teardown() {
  active = false;
  abort?.abort();
  abort = null;
  clearTimeout(searchTimer);
  if (searchAbort) { searchAbort.abort(); searchAbort = null; }
}
