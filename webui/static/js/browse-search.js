// browse-search.js — the shared debounced search view for the browse pages
// (library and madnetwork, docs/ui/madnetwork-page.md §Search). Extracted from
// app.js. One input, 2+ chars, 300 ms debounce, a search view with Artists /
// Albums / Tracks sections; hit = drill (via the page's callbacks) or play.
//
// createBrowseSearch is called from a page's init() (the elements live in
// swappable DOM); its listeners are bound to the given AbortController signal,
// and aborting that signal also cancels timers and in-flight fetches — the
// page's teardown() needs nothing else.
import { getController } from './player-controller.js';
import { fmtTime } from './player.js';
import { trackKey } from './favorites.js';
import { esc, mkHeartBtn, repaintHearts } from './browse-rows.js';

export function createBrowseSearch({
  signal,            // AbortController signal (page activation lifetime)
  fetchResults,      // async (q, fetchSignal) => { artists, albums, tracks }
  onOpenArtist,      // (a) => void — drill to the artist (search already cleared)
  onOpenAlbum,       // (a) => void — drill to the album (search already cleared)
  albumArtUrl,       // (a) => cover URL or null (null → note placeholder)
  buildQueueTrack,   // (t) => controller track { url, tagsetId, title, artist }
  onLibraryView,     // () => void — after switching back (e.g. vlist refresh)
  inputSel = '.library-search__input',
  clearSel = '.library-search__clear',
  libraryViewSel = '#view-library',
  searchViewSel = '#view-search',
}) {
  const searchInput = document.querySelector(inputSel);
  const searchClear = document.querySelector(clearSel);
  const viewLibrary = document.querySelector(libraryViewSel);
  const viewSearch = document.querySelector(searchViewSel);

  let lastQuery = '';
  let searchTimer = null;
  let searchAbort = null;
  let disposed = false;

  signal.addEventListener('abort', () => {
    disposed = true;
    clearTimeout(searchTimer);
    if (searchAbort) { searchAbort.abort(); searchAbort = null; }
  });

  function isSearching() {
    return (viewSearch && viewSearch.classList.contains('view-panel--active'))
      || !!(searchInput && searchInput.value);
  }

  function clear() {
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
    onLibraryView?.();
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
      results = await fetchResults(q, searchAbort.signal);
      searchAbort = null;
    } catch (err) {
      if (err.name === 'AbortError') return; // superseded by a newer query — discard silently
      lastQuery = ''; // allow retry with the same query after a real error
      if (viewSearch) viewSearch.innerHTML =
        '<p style="color:var(--error);padding:16px;text-align:center">' +
        'Search failed — check your connection and try again.</p>';
      return;
    }

    if (disposed) return; // navigated away while searching
    renderResults(results, q);
  }

  function renderResults(results, q) {
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
        row.tabIndex = 0;
        row.setAttribute('role', 'button');
        row.setAttribute('aria-label', `Browse artist ${a.name}`);
        row.innerHTML =
          `<div class="search-row__avatar">${esc((a.name || '?')[0].toUpperCase())}</div>` +
          `<div class="search-row__title">${esc(a.name || 'Unknown Artist')}</div>`;
        const open = () => { clear(); onOpenArtist(a); };
        row.addEventListener('click', open);
        row.addEventListener('keydown', e => {
          if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); open(); }
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
        const artUrl = albumArtUrl(a);
        const artContent = artUrl
          ? `<img src="${artUrl}" alt="" loading="lazy">`
          : noteSvg;
        const row = document.createElement('div');
        row.className = 'search-row search-row--album';
        row.tabIndex = 0;
        row.setAttribute('role', 'button');
        row.setAttribute('aria-label', `Browse album ${a.title}`);
        row.innerHTML =
          `<div class="search-row__thumb">${artContent}</div>` +
          `<div class="search-row__body">` +
            `<div class="search-row__title">${esc(a.title || 'Other')}</div>` +
            `<div class="search-row__subtitle">${esc(a.artist_name || 'Unknown Artist')}</div>` +
          `</div>`;
        const open = () => { clear(); onOpenAlbum(a); };
        row.addEventListener('click', open);
        row.addEventListener('keydown', e => {
          if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); open(); }
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
      const searchPlaylist = tracks.map(buildQueueTrack);

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
        row.insertBefore(mkHeartBtn(trackKey(searchPlaylist[i]), searchPlaylist[i].remoteLike),
          row.querySelector('.search-row__duration'));

        const play = () => getController().setQueue(searchPlaylist, i);
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

  if (searchInput) {
    searchInput.addEventListener('input', () => {
      const q = searchInput.value.trim();
      searchClear.style.display = searchInput.value ? '' : 'none';
      if (q.length < 2) { showLibraryView(); return; }
      clearTimeout(searchTimer);
      searchTimer = setTimeout(() => runSearch(q), 300);
    }, { signal });

    searchInput.addEventListener('keydown', e => {
      if (e.key === 'Escape') clear();
    }, { signal });

    searchClear.addEventListener('click', clear, { signal });
  }

  return { clear, isSearching };
}
