import { getController } from './player-controller.js';
import { fmtTime } from './player.js';
import { ensureLiked } from './favorites.js';
import { quickAddItems } from './quick-add.js';
import {
  buildArtistRow, buildAlbumRow, buildTrackRow, mkDiscHeader,
  playKeyOf, highlightPlayingRow, reflectPlayStateRows, markUnavailableRows,
  repaintHearts,
} from './browse-rows.js';
import { createBrowseSearch } from './browse-search.js';
import { discKey, discLabel, isMultiDisc } from './disc.js';
import { createVirtualList } from './virtual-list.js';

// Read API base from HTML meta. Empty default => relative, same-origin URLs
// (the page and API share an origin in the bundled server). A non-empty value
// points a separately hosted UI at a remote API origin.
const API = document.querySelector('meta[name="api-url"]')?.content || '';

// Theme is owned by shell.js (persistent header, applied once across pages).

// Row building, quick-add menus, and the search view live in the shared browse
// core (browse-rows.js / quick-add.js / browse-search.js — shared with the
// madnetwork page, docs/ui/madnetwork-page.md); this module owns the library's
// drill state, data fetching, and page lifecycle.

// Duration cache: shared with the queue panel and the playlists page so
// already-known durations show everywhere (dur-cache.js).
import { loadDurCache, saveDurCache } from './dur-cache.js';

// ── Library — drill-down state ────────────────────────────────────────────

// drill tracks which panel level is currently shown and what was selected.
// Browse fetches address entities by their stable id (artistId / albumId); the
// display names (artist / album) ride alongside only for the breadcrumb + track
// metadata text.
const drill = { level: 'artists', artist: null, album: null, artistId: null, albumId: null };

// The artist list is the unbounded browse surface, so it's cursor-paginated and
// virtualized (window-scroll): only on-screen rows live in the DOM and more pages
// stream in as you scroll (docs/architecture/infinite-scroll-virtualization.md).
// Albums/tracks per entity are bounded and render whole. artistVList holds the
// live scroller so navigating away (or re-entering) can tear it down.
const ARTIST_PAGE_SIZE = 80;
let artistVList = null;
function destroyArtistVList() { if (artistVList) { artistVList.destroy(); artistVList = null; } }
function spacerDiv(px) {
  const d = document.createElement('div');
  d.style.height = `${Math.max(0, px)}px`;
  d.setAttribute('aria-hidden', 'true');
  return d;
}

// Fetch and render the top-level artists panel.
async function loadArtists() {
  drill.level    = 'artists';
  drill.artist   = null;
  drill.album    = null;
  drill.artistId = null;
  drill.albumId  = null;

  destroyArtistVList();
  const panel = document.getElementById('libraryPanel');
  panel.innerHTML = '<div class="panel-loading" aria-live="polite" role="status"></div>';

  let page;
  try {
    const res = await fetch(`${API}/api/artists?limit=${ARTIST_PAGE_SIZE}`);
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    page = await res.json();   // { items, next_cursor }
  } catch (err) {
    console.error('Failed to load artists:', err);
    panel.innerHTML = '<div class="panel-empty" role="alert">Failed to load library.</div>';
    return;
  }

  if (!active) return; // navigated away while loading
  renderBreadcrumb();
  renderArtistList(page);
}

// Fetch and render the albums panel for a given artist (addressed by id).
async function drillToAlbums(artistId, artistName) {
  drill.level    = 'albums';
  drill.artist   = artistName;
  drill.artistId = artistId;
  drill.album    = null;
  drill.albumId  = null;

  destroyArtistVList();   // leaving the artists level
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
async function drillToTracks(albumId, artistId, artistName, albumTitle, hasImage) {
  drill.level    = 'tracks';
  drill.artist   = artistName;
  drill.artistId = artistId;
  drill.album    = albumTitle;
  drill.albumId  = albumId;
  drill.albumHasImage = !!hasImage;

  destroyArtistVList();   // can be reached straight from a search hit
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
// Once drilled in, the path is rooted at a clickable "Library" crumb (mirroring
// /madnetwork's "Madnetwork › …") so the whole library is one click away on the
// same line as the artist step-back. At the artists (top) level there's nothing
// to step back to, so the whole bar is hidden to avoid an empty strip.
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
    bc.appendChild(mkLink('Library', () => loadArtists()));
    bc.appendChild(mkSep());
    bc.appendChild(mkCurrent(displayArtist));
  } else if (drill.level === 'tracks') {
    const capturedArtistId = drill.artistId;
    const capturedArtist   = drill.artist;
    bc.appendChild(mkLink('Library', () => loadArtists()));
    bc.appendChild(mkSep());
    bc.appendChild(mkLink(displayArtist, () => drillToAlbums(capturedArtistId, capturedArtist)));
    bc.appendChild(mkSep());
    bc.appendChild(mkCurrent(displayAlbum));
  }
  // 'artists' (top) level: empty — already at the full library, nothing to step back to.
  const bar = bc.closest('.library-bar');
  if (bar) bar.style.display = bc.children.length ? '' : 'none';
}

// ── Track collectors for the quick-add menus ──────────────────────────────

// trackObjFromApi maps a browse-endpoint track to a controller track object.
// tagsetId is the listening identity (hearts, playlists, renditions); the url
// is the server-resolved best rendition (recording-tagsets P1).
function trackObjFromApi(t, artistName, durCache) {
  const url = `${API}${t.url}`;
  return {
    url,
    tagsetId: t.tagset_id || null,
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

// ── Rendering (shared row builders from browse-rows.js) ───────────────────

// artistRow builds one artist row via the shared builder. Used by the windowed
// scroller's renderRow and reused as items scroll into view.
function artistRow(artist) {
  const displayName = artist.name || 'Unknown Artist';
  const count       = artist.track_count ?? 0;
  return buildArtistRow({
    name: displayName,
    meta: `${count} track${count !== 1 ? 's' : ''}`,
    onOpen: () => drillToAlbums(artist.id, artist.name),
    makeMenuItems: btn => quickAddItems(btn, () => entityTracks(artist.id, null, artist.name)),
  });
}

// Render the artists panel as a virtualized, infinite-scrolling list. `page` is
// the first { items, next_cursor } response; further pages stream in via the
// scroller's fetchMore as the window nears the end.
function renderArtistList(page) {
  const panel = document.getElementById('libraryPanel');
  const items = (page && page.items) || [];

  if (items.length === 0) {
    destroyArtistVList();
    panel.innerHTML =
      `<div class="panel-fade-in"><div class="panel-empty">` +
      `No music yet. <a href="/upload" id="openUploadEmpty">Upload files →</a>` +
      `</div></div>`;
    return;
  }

  destroyArtistVList();
  const wrap = document.createElement('div');
  wrap.className = 'panel-fade-in';
  panel.innerHTML = '';
  panel.appendChild(wrap);

  let cursor = page.next_cursor || null;
  artistVList = createVirtualList({
    sizerEl: wrap,
    windowScroll: true,
    makeSpacer: spacerDiv,
    renderRow: artistRow,
    estimateHeight: 56,         // artist-row height; corrected on measure
    fetchMore: async () => {
      if (!cursor) return { items: [], done: true };
      try {
        const res = await fetch(`${API}/api/artists?limit=${ARTIST_PAGE_SIZE}&cursor=${encodeURIComponent(cursor)}`);
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const d = await res.json();
        cursor = d.next_cursor || null;
        return { items: d.items || [], done: !cursor };
      } catch (err) {
        console.error('Failed to load more artists:', err);
        return { items: [], done: true };
      }
    },
  });
  artistVList.setItems(items);
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

  albums.forEach(album => {
    const title      = album.title       || 'Other';
    const count      = album.track_count ?? 0;
    const yearPrefix = album.year        ? `${album.year} · ` : '';
    wrap.appendChild(buildAlbumRow({
      title,
      meta: `${yearPrefix}${count} track${count !== 1 ? 's' : ''}`,
      artUrl: album.has_image
        ? `${API}/api/albums/${encodeURIComponent(album.id)}/image?size=small`
        : null,
      onOpen: () => drillToTracks(album.id, album.artist_id, album.artist_name, album.title, album.has_image),
      makeMenuItems: btn => quickAddItems(btn, () => entityTracks(null, album.id, album.artist_name)),
    }));
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
      // The tagset id rides on the track for the queue panel's "Save as
      // playlist", the hearts, and the quality-dropdown renditions fetch.
      tagsetId: t.tagset_id || null,
      // rowKey is the APPEARANCE identity used to match the playing row and to
      // decide pause-vs-restart on click: the tagset (appearance) when known, so
      // a different appearance of the same audio (different tagset, same url)
      // restarts rather than toggling pause.
      rowKey: t.tagset_id ? `ts:${t.tagset_id}` : `url:${url}`,
      title:  t.title  || t.filename || 'Unknown',
      // Per-track performer (matches the playlists page); falls back to the
      // album-grouping artist from the breadcrumb when a row has no performer.
      artist: t.artist_name || drill.artist || '',
      album:  drill.album || '',
      // The album's cover, for the OS media widget (media-session.js). medium:
      // the widget renders bigger than a list thumb.
      artUrl: drill.albumHasImage
        ? `${API}/api/albums/${encodeURIComponent(drill.albumId)}/image?size=medium`
        : null,
      dur,
    });
  });

  const wrap = document.createElement('div');
  wrap.className = 'panel-fade-in';

  // Multi-disc albums (more than one distinct disc key — untagged/0/N each count)
  // get a "Disc N" subheading before each disc; the queue stays one flat ordered
  // list, so the headers are purely visual and don't shift track indices.
  // disc.js is the shared rule (docs/architecture/disc-numbering.md).
  const multiDisc = isMultiDisc(tracks);
  let shownDisc, discTrackNo = 0;   // shownDisc starts undefined: no real key equals it

  tracks.forEach((t, i) => {
    const disc = discKey(t.disc_number);
    if (multiDisc && disc !== shownDisc) {
      shownDisc = disc;
      discTrackNo = 0;
      wrap.appendChild(mkDiscHeader(discLabel(disc)));
    }
    discTrackNo++;
    const displayNum = t.track_number || (multiDisc ? discTrackNo : i + 1);
    const track      = libraryPlaylist[i];
    // Click the playing row to pause/resume it; click any other row (including a
    // different appearance of the same audio) to start it fresh.
    const play = () => {
      const cur = controller.current();
      if (cur && playKeyOf(cur.track) === track.rowKey) controller.toggle();
      else controller.setQueue(libraryPlaylist, i);
    };
    wrap.appendChild(buildTrackRow({
      num: displayNum,
      title: track.title,
      meta: track.artist,
      dur: track.dur,
      rowKey: track.rowKey,
      url: track.url,   // duration write-back / unavailable marking
      idx: i,           // used by the background duration fetch
      likeKey: track.tagsetId,
      onPlay: play,
      makeMenuItems: btn => quickAddItems(btn, () => [track], {
        extraItems: [{ label: 'Download', onClick: () => downloadTrack(track) }],
      }),
    }));
  });

  panel.innerHTML = '';
  panel.appendChild(wrap);

  // Re-highlight whatever is currently playing if its row is in this view.
  const cur = controller.current();
  if (cur) highlightPlayingRow(cur.track, controller.paused);
  repaintHearts();

  // Background fetch for any tracks still showing '—'.
  fetchMissingDurations(libraryPlaylist);
}

// downloadTrack saves the track's resolved rendition file to the user's
// device — an anchor download of the same-origin /files/ URL; the browser
// names the file after the URL's filename segment. (Fetching a remote
// madnetwork track into the LIBRARY is "Materialize", on /madnetwork.)
function downloadTrack(track) {
  const a = document.createElement('a');
  a.href = track.url;
  a.download = '';
  document.body.appendChild(a);
  a.click();
  a.remove();
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
// highlighting (browse-rows.js helpers) and duration write-back — through the
// subscriptions below (module-scoped: they run once and persist, and are
// harmless on other pages since they match rows by data-key). Auth-expiry and
// the queue-replaced undo toast are shell concerns, wired in shell.js. The
// queue is stable: browsing never changes it, only an explicit play or a manual
// queue edit does.

const controller = getController();
controller.on('trackchange', track => highlightPlayingRow(track, controller.paused));
controller.on('playstate', reflectPlayStateRows);
controller.on('duration', writeDuration);
controller.on('error', track => markUnavailableRows(track.url));

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

// ── Search (shared view machinery from browse-search.js) ──────────────────

// The search elements live in swappable DOM (inside <main>, above the view
// panels), so the factory is created fresh on each init() and its listeners
// are removed via the AbortController on teardown(). Nav is owned by shell.js —
// re-entering the library is a shell swap, which resets to the artists view.
let search = null;

function createSearch(signal) {
  return createBrowseSearch({
    signal,
    fetchResults: async (q, fetchSignal) => {
      const res = await fetch(`${API}/api/search?q=${encodeURIComponent(q)}`, { signal: fetchSignal });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      return res.json();
    },
    onOpenArtist: a => drillToAlbums(a.id, a.name),
    onOpenAlbum:  a => drillToTracks(a.id, a.artist_id, a.artist_name, a.title, a.has_image),
    albumArtUrl:  a => a.has_image
      ? `${API}/api/albums/${encodeURIComponent(a.id)}/image?size=small`
      : null,
    buildQueueTrack: t => ({
      url:    `${API}${t.url}`,
      tagsetId: t.tagset_id || null,
      title:  t.title       || 'Unknown',
      artist: t.artist_name || '',
    }),
    // The artist scroller may have rendered against a hidden (zero-rect) sizer
    // while search was up; re-derive its window for the visible scroll position.
    onLibraryView: () => artistVList?.refresh(),
  });
}

// ── Lifecycle (driven by shell.js) ─────────────────────────────────────────
// init() runs on first load and on every navigation back to the library; it
// re-wires the swappable DOM (search bar, views) and (re)loads the artists.
// teardown() runs before navigating away: it removes those listeners, cancels
// timers and aborts in-flight fetches. The player/controller is NOT torn down —
// it lives in the persistent shell so playback survives navigation.
let abort  = null;     // AbortController for this activation's listeners
let active = false;    // guards late async renders after teardown

// Hardware back-button hook for the Android app: the native MainActivity calls
// window.__madshareBack first and only does its own history/root handling when
// this returns false. Here it pops the library's in-page state (which has no
// browser-history entry): close an open search, else drill one level up. Returns
// false at the artists root so native can take over. A plain browser never calls it.
function handleBack() {
  if (search?.isSearching()) { search.clear(); return true; }
  if (drill.level === 'tracks') { drillToAlbums(drill.artistId, drill.artist); return true; }
  if (drill.level === 'albums') { loadArtists(); return true; }
  return false;
}

export function init() {
  active = true;
  abort = new AbortController();
  search = createSearch(abort.signal);
  window.__madshareBack = handleBack;   // app-only; harmless/no-op in a browser
  ensureLiked(); // hearts repaint via onLikedChange once the set arrives
  loadArtists();
}

export function teardown() {
  active = false;
  abort?.abort();         // also cancels the search factory's timers/fetches
  abort = null;
  search = null;
  destroyArtistVList();   // drop the window scroll/resize listeners
  if (window.__madshareBack === handleBack) window.__madshareBack = null;
}
