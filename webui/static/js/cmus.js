// library.js — Madshare 3-panel browser + player
// No frameworks, no build step. All fetch calls use the API base from <meta name="api-url">.
// Empty default => relative, same-origin URLs (bundled server). A non-empty
// value points a separately hosted UI at a remote API origin.
const API = document.querySelector('meta[name="api-url"]')?.content || '';

// Hide the Admin nav link for principals without admin rights. This page is a
// standalone view that doesn't use the shared auth module, so it checks /me
// directly. UX only — the admin API still enforces the permissions server-side.
(async function gateAdminLink() {
  const removeAdmin = () =>
    document.querySelectorAll('.main-nav a[href="/admin"]').forEach(a => a.remove());
  try {
    const res = await fetch(`${API}/api/auth/me`);
    const perms = (res.ok ? await res.json() : null)?.permissions || [];
    if (!['file.delete', 'user.manage'].some(p => perms.includes(p))) removeAdmin();
  } catch { removeAdmin(); }
})();

// ── State ─────────────────────────────────────────────────────────────────

const state = {
  artists: [],
  selectedArtist: null,   // artist name string ('' = Unknown Artist)
  albums: [],
  selectedAlbum: null,    // album title string (null = none selected; '' = "Other" bucket)
  tracks: [],
  currentTrackIndex: -1,  // index into state.tracks
  isPlaying: false,
};

const audio = document.getElementById('audio');

// ── DOM refs ──────────────────────────────────────────────────────────────

const artistsList   = document.getElementById('artists-list');
const albumsList    = document.getElementById('albums-list');
const tracksList    = document.getElementById('tracks-list');
const artistsCount  = document.getElementById('artists-count');
const albumsCount   = document.getElementById('albums-count');
const tracksCount   = document.getElementById('tracks-count');

const playerTitle   = document.getElementById('player-title');
const playerArtist  = document.getElementById('player-artist');
const playerArt     = document.getElementById('player-art');
const playerTime    = document.getElementById('player-time');
const scrubber      = document.getElementById('scrubber');
const volSlider     = document.getElementById('volume-slider');
const btnPlay       = document.getElementById('btn-play');
const btnPrev       = document.getElementById('btn-prev');
const btnNext       = document.getElementById('btn-next');
const btnMute       = document.getElementById('btn-mute');
const iconPlay      = document.getElementById('icon-play');
const iconPause     = document.getElementById('icon-pause');
const playerControls    = document.getElementById('player-controls');
const playerScrubWrap   = document.getElementById('player-scrubber-wrap');
const playerVolWrap     = document.getElementById('player-volume-wrap');

// Image upload inputs
const artistImageInput = document.getElementById('artist-image-input');
const albumImageInput  = document.getElementById('album-image-input');

// Pending image upload targets (set before programmatic click)
let pendingArtistImageName = null;
let pendingAlbumImageData  = null; // { artist, title }

// ── Utilities ─────────────────────────────────────────────────────────────

/**
 * URL-encode an artist/album name for use in query strings and path segments.
 * An empty string is a valid value (the "Unknown" / "Other" bucket).
 */
function encodeParam(s) {
  return encodeURIComponent(s);
}

/**
 * Format seconds into M:SS (or H:MM:SS for tracks > 1 hour).
 * Returns '' for null/undefined/non-finite inputs.
 */
function formatDuration(seconds) {
  if (seconds == null || !isFinite(seconds) || seconds < 0) return '';
  const h   = Math.floor(seconds / 3600);
  const m   = Math.floor((seconds % 3600) / 60);
  const sec = String(Math.floor(seconds % 60)).padStart(2, '0');
  return h > 0 ? `${h}:${String(m).padStart(2, '0')}:${sec}` : `${m}:${sec}`;
}

/**
 * Safe text setter — never assign user strings via innerHTML.
 */
function setText(el, text) {
  el.textContent = text;
}

/**
 * Build 6 skeleton placeholder rows for a loading column.
 * heightClass: 'skeleton-row--artist' | 'skeleton-row--album' | 'skeleton-row--track'
 */
function buildSkeletons(container, heightClass, count = 6) {
  container.innerHTML = '';
  for (let i = 0; i < count; i++) {
    const row = document.createElement('div');
    row.className = `skeleton-row ${heightClass}`;
    container.appendChild(row);
  }
}

// ── Artists ───────────────────────────────────────────────────────────────

async function loadArtists() {
  buildSkeletons(artistsList, 'skeleton-row--artist');
  setText(artistsCount, '');

  let artists;
  try {
    const res = await fetch(`${API}/api/artists`);
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    artists = await res.json();
  } catch (err) {
    console.error('Failed to load artists:', err);
    artistsList.innerHTML = '';
    const msg = document.createElement('div');
    msg.className = 'col-empty-state';
    setText(msg, 'Failed to load artists.');
    artistsList.appendChild(msg);
    return;
  }

  state.artists = Array.isArray(artists) ? artists : [];
  renderArtists();
}

function renderArtists() {
  artistsList.innerHTML = '';
  setText(artistsCount, state.artists.length > 0 ? String(state.artists.length) : '');

  if (state.artists.length === 0) {
    const empty = document.createElement('div');
    empty.className = 'col-empty-state';
    // Build link safely — no innerHTML with user data
    const text = document.createTextNode('No music yet. ');
    const link = document.createElement('a');
    link.href = '/upload';
    setText(link, 'Upload files →');
    empty.appendChild(text);
    empty.appendChild(link);
    artistsList.appendChild(empty);
    return;
  }

  const fragment = document.createDocumentFragment();
  state.artists.forEach(artist => {
    fragment.appendChild(makeArtistItem(artist));
  });
  artistsList.appendChild(fragment);
  artistsList.classList.add('list-fade-in');
}

function makeArtistItem(artist) {
  const displayName = artist.name || 'Unknown Artist';

  const item = document.createElement('div');
  item.className = 'artist-item';
  item.setAttribute('role', 'option');
  item.setAttribute('aria-selected', 'false');
  item.setAttribute('tabindex', '0');
  item.dataset.artistName = artist.name; // raw value, may be ''

  // Art placeholder (click to upload image)
  // Per spec: clicking artist name text area triggers image upload in v0.
  // We attach the upload trigger to the entire item for simplicity, but only
  // to the art placeholder element conceptually; here we wire it via separate click.

  const nameEl = document.createElement('span');
  nameEl.className = 'artist-name';
  setText(nameEl, displayName);

  const chevron = document.createElement('span');
  chevron.className = 'artist-chevron';
  chevron.setAttribute('aria-hidden', 'true');
  setText(chevron, '›');

  item.appendChild(nameEl);
  item.appendChild(chevron);

  // Select on click
  item.addEventListener('click', () => selectArtist(artist.name));
  item.addEventListener('keydown', e => {
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); selectArtist(artist.name); }
    handleColumnKeyNav(e, artistsList, '.artist-item');
  });

  return item;
}

function selectArtist(name) {
  state.selectedArtist = name;
  state.selectedAlbum  = null;

  // Update artist selection UI
  artistsList.querySelectorAll('.artist-item').forEach(el => {
    const selected = el.dataset.artistName === name;
    el.setAttribute('aria-selected', String(selected));
  });

  // Clear tracks column
  renderTracksEmpty('Select an album');

  // Load albums
  loadAlbums(name);
}

// Mark the artist item that contains the currently playing track with has-playing
function updateArtistPlayingIndicator() {
  if (state.currentTrackIndex < 0) return;
  const playingTrack = state.tracks[state.currentTrackIndex];
  if (!playingTrack) return;

  artistsList.querySelectorAll('.artist-item').forEach(el => {
    el.classList.toggle('has-playing', el.dataset.artistName === state.selectedArtist);
  });
}

// ── Albums ────────────────────────────────────────────────────────────────

async function loadAlbums(artistName) {
  buildSkeletons(albumsList, 'skeleton-row--album');
  setText(albumsCount, '');

  let albums;
  try {
    const res = await fetch(`${API}/api/albums?artist=${encodeParam(artistName)}`);
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    albums = await res.json();
  } catch (err) {
    console.error('Failed to load albums:', err);
    albumsList.innerHTML = '';
    const msg = document.createElement('div');
    msg.className = 'col-empty-state';
    setText(msg, 'Failed to load albums.');
    albumsList.appendChild(msg);
    return;
  }

  state.albums = Array.isArray(albums) ? albums : [];
  renderAlbums();
}

function renderAlbums() {
  albumsList.innerHTML = '';
  setText(albumsCount, state.albums.length > 0 ? String(state.albums.length) : '');

  if (state.albums.length === 0) {
    const empty = document.createElement('div');
    empty.className = 'col-empty-state';
    setText(empty, 'No albums found.');
    albumsList.appendChild(empty);
    return;
  }

  const fragment = document.createDocumentFragment();
  state.albums.forEach(album => {
    fragment.appendChild(makeAlbumItem(album));
  });
  albumsList.appendChild(fragment);
  albumsList.classList.add('list-fade-in');
}

function makeAlbumItem(album) {
  // Empty title = "Other" bucket
  const displayTitle = album.title || 'Other';
  const isOther      = !album.title;

  const item = document.createElement('div');
  item.className = 'album-item';
  item.setAttribute('role', 'option');
  item.setAttribute('aria-selected', 'false');
  item.setAttribute('tabindex', '0');
  item.dataset.albumTitle  = album.title;       // raw, may be ''
  item.dataset.artistName  = album.artist_name;

  // Art placeholder
  const art = document.createElement('div');
  art.className = 'art-placeholder album-art';
  art.setAttribute('title', 'Click to upload album art');
  if (album.has_image) {
    const img = document.createElement('img');
    img.src = `${API}/api/albums/${encodeParam(album.title)}/image?artist=${encodeParam(album.artist_name)}`;
    img.alt = `${displayTitle} album art`;
    art.appendChild(img);
  } else {
    const note = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    note.setAttribute('width', '16');
    note.setAttribute('height', '16');
    note.setAttribute('viewBox', '0 0 24 24');
    note.setAttribute('fill', 'currentColor');
    note.setAttribute('aria-hidden', 'true');
    note.innerHTML = '<path d="M12 3v10.55c-.59-.34-1.27-.55-2-.55-2.21 0-4 1.79-4 4s1.79 4 4 4 4-1.79 4-4V7h4V3h-6z"/>';
    art.appendChild(note);
  }
  // Trigger image upload on art click (stop propagation so item doesn't also select)
  art.addEventListener('click', e => {
    e.stopPropagation();
    pendingAlbumImageData = { artist: album.artist_name, title: album.title };
    albumImageInput.click();
  });

  // Text block
  const text = document.createElement('div');
  text.className = 'album-text';

  const titleEl = document.createElement('div');
  titleEl.className = 'album-title';
  setText(titleEl, displayTitle);

  const metaEl = document.createElement('div');
  metaEl.className = 'album-meta';
  if (isOther) {
    setText(metaEl, `${album.track_count} tracks, no album`);
  } else {
    const parts = [];
    if (album.year) parts.push(String(album.year));
    parts.push(`${album.track_count} ${album.track_count === 1 ? 'track' : 'tracks'}`);
    setText(metaEl, parts.join(' · '));
  }

  text.appendChild(titleEl);
  text.appendChild(metaEl);

  const chevron = document.createElement('span');
  chevron.className = 'album-chevron';
  chevron.setAttribute('aria-hidden', 'true');
  setText(chevron, '›');

  item.appendChild(art);
  item.appendChild(text);
  item.appendChild(chevron);

  item.addEventListener('click', () => selectAlbum(album.artist_name, album.title));
  item.addEventListener('keydown', e => {
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); selectAlbum(album.artist_name, album.title); }
    handleColumnKeyNav(e, albumsList, '.album-item');
  });

  return item;
}

function selectAlbum(artistName, albumTitle) {
  state.selectedAlbum = albumTitle;

  albumsList.querySelectorAll('.album-item').forEach(el => {
    const selected = el.dataset.artistName === artistName && el.dataset.albumTitle === albumTitle;
    el.setAttribute('aria-selected', String(selected));
  });

  loadTracks(artistName, albumTitle);
}

// ── Tracks ────────────────────────────────────────────────────────────────

async function loadTracks(artistName, albumTitle) {
  buildSkeletons(tracksList, 'skeleton-row--track');
  setText(tracksCount, '');

  let tracks;
  try {
    const res = await fetch(
      `${API}/api/tracks?artist=${encodeParam(artistName)}&album=${encodeParam(albumTitle)}`
    );
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    tracks = await res.json();
  } catch (err) {
    console.error('Failed to load tracks:', err);
    tracksList.innerHTML = '';
    const msg = document.createElement('div');
    msg.className = 'col-empty-state';
    setText(msg, 'Failed to load tracks.');
    tracksList.appendChild(msg);
    return;
  }

  state.tracks = Array.isArray(tracks) ? tracks : [];
  renderTracks();
}

function renderTracks() {
  tracksList.innerHTML = '';
  setText(tracksCount, state.tracks.length > 0 ? String(state.tracks.length) : '');

  if (state.tracks.length === 0) {
    const empty = document.createElement('div');
    empty.className = 'col-empty-state';
    setText(empty, 'No tracks found.');
    tracksList.appendChild(empty);
    return;
  }

  const fragment = document.createDocumentFragment();
  state.tracks.forEach((track, i) => {
    fragment.appendChild(makeTrackItem(track, i));
  });
  tracksList.appendChild(fragment);
  tracksList.classList.add('list-fade-in');

  // Re-apply playing indicator if current track is in this set
  if (state.currentTrackIndex >= 0) {
    highlightPlayingTrack(state.currentTrackIndex);
  }
}

function makeTrackItem(track, index) {
  const item = document.createElement('div');
  item.className = 'track-item';
  item.setAttribute('role', 'option');
  item.setAttribute('aria-selected', 'false');
  item.setAttribute('tabindex', '0');
  item.dataset.trackIndex = index;

  // Track number cell
  const numEl = document.createElement('span');
  numEl.className = 'track-num';
  setText(numEl, track.track_number != null ? String(track.track_number) : String(index + 1));

  // EQ bars (shown when playing, replaces number)
  const eqBars = document.createElement('span');
  eqBars.className = 'eq-bars';
  eqBars.setAttribute('aria-hidden', 'true');
  for (let b = 0; b < 3; b++) {
    const bar = document.createElement('span');
    bar.className = 'eq-bar';
    eqBars.appendChild(bar);
  }

  // Title cell
  const titleEl = document.createElement('span');
  titleEl.className = 'track-title';
  setText(titleEl, track.title || 'Untitled');

  // Duration cell
  const durEl = document.createElement('span');
  durEl.className = 'track-dur';
  setText(durEl, formatDuration(track.duration_seconds));

  item.appendChild(numEl);
  item.appendChild(eqBars);
  item.appendChild(titleEl);
  item.appendChild(durEl);

  item.addEventListener('click', () => playTrack(index));
  item.addEventListener('keydown', e => {
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); playTrack(index); }
    handleColumnKeyNav(e, tracksList, '.track-item');
  });

  return item;
}

function renderTracksEmpty(message) {
  state.tracks = [];
  state.currentTrackIndex = -1;
  tracksList.innerHTML = '';
  setText(tracksCount, '');
  const empty = document.createElement('div');
  empty.className = 'col-empty-state';
  setText(empty, message);
  tracksList.appendChild(empty);
}

function highlightPlayingTrack(index) {
  tracksList.querySelectorAll('.track-item').forEach(el => {
    const idx = Number(el.dataset.trackIndex);
    el.classList.toggle('is-playing', idx === index);
    el.classList.toggle('is-paused',  idx === index && !state.isPlaying);
    el.setAttribute('aria-selected', idx === index ? 'true' : 'false');
    if (idx === index) el.classList.remove('unavailable');
  });
}

// ── Playback ──────────────────────────────────────────────────────────────

function playTrack(index) {
  if (index < 0 || index >= state.tracks.length) return;
  state.currentTrackIndex = index;

  const track = state.tracks[index];
  audio.src = `${API}${track.url}`;
  audio.play().catch(err => console.error('Playback failed:', err));

  updatePlayerBar(track);
  highlightPlayingTrack(index);
  updateArtistPlayingIndicator();
  enablePlayerControls();
}

function updatePlayerBar(track) {
  playerTitle.textContent = track.title || 'Untitled';
  playerTitle.classList.add('has-track');
  playerArtist.textContent = state.selectedArtist || '';
}

function enablePlayerControls() {
  playerControls.removeAttribute('aria-disabled');
  playerScrubWrap.removeAttribute('aria-disabled');
  playerVolWrap.removeAttribute('aria-disabled');
}

function syncPlayIcon() {
  const playing = !audio.paused;
  state.isPlaying = playing;
  iconPlay.style.display  = playing ? 'none' : '';
  iconPause.style.display = playing ? '' : 'none';
  btnPlay.setAttribute('aria-label', playing ? 'Pause' : 'Play');
  btnPlay.title = playing ? 'Pause' : 'Play';

  // Pause/resume EQ animation
  tracksList.querySelectorAll('.track-item.is-playing').forEach(el => {
    el.classList.toggle('is-paused', !playing);
  });
}

// Player controls
btnPlay.addEventListener('click', () => {
  if (state.currentTrackIndex < 0) return;
  if (audio.paused) audio.play().catch(() => {});
  else              audio.pause();
});

btnPrev.addEventListener('click', () => {
  if (state.currentTrackIndex < 0) return;
  const prev = state.currentTrackIndex > 0
    ? state.currentTrackIndex - 1
    : state.tracks.length - 1;
  playTrack(prev);
});

btnNext.addEventListener('click', () => {
  if (state.currentTrackIndex < 0) return;
  const next = state.currentTrackIndex < state.tracks.length - 1
    ? state.currentTrackIndex + 1
    : 0;
  playTrack(next);
});

// Mute toggle
let preMuteVolume = 1;
btnMute.addEventListener('click', () => {
  if (audio.volume > 0) {
    preMuteVolume = audio.volume;
    audio.volume = 0;
    volSlider.value = '0';
  } else {
    audio.volume = preMuteVolume;
    volSlider.value = String(preMuteVolume);
  }
  updateVolumeFill();
});

// Audio events
audio.addEventListener('play',  syncPlayIcon);
audio.addEventListener('pause', syncPlayIcon);

audio.addEventListener('ended', () => {
  // Auto-advance to next track
  if (state.currentTrackIndex < state.tracks.length - 1) {
    playTrack(state.currentTrackIndex + 1);
  } else {
    // End of album — reset icon
    state.isPlaying = false;
    iconPlay.style.display  = '';
    iconPause.style.display = 'none';
    btnPlay.setAttribute('aria-label', 'Play');
    tracksList.querySelectorAll('.track-item').forEach(el => el.classList.remove('is-playing', 'is-paused'));
  }
});

audio.addEventListener('error', () => {
  const failedEl = tracksList.querySelector(`.track-item[data-track-index="${state.currentTrackIndex}"]`);
  if (failedEl) failedEl.classList.add('unavailable');
  if (state.currentTrackIndex < state.tracks.length - 1) {
    playTrack(state.currentTrackIndex + 1);
  } else {
    state.isPlaying = false;
    iconPlay.style.display  = '';
    iconPause.style.display = 'none';
    btnPlay.setAttribute('aria-label', 'Play');
    tracksList.querySelectorAll('.track-item').forEach(el => el.classList.remove('is-playing', 'is-paused'));
  }
});

audio.addEventListener('timeupdate', () => {
  if (!audio.duration) return;
  const progress = audio.currentTime / audio.duration;
  scrubber.value = String(progress);
  updateScrubberFill(progress);
  playerTime.textContent = `${formatDuration(audio.currentTime)} / ${formatDuration(audio.duration)}`;
});

// Scrubber seek
scrubber.addEventListener('input', () => {
  if (!audio.duration) return;
  const t = Number(scrubber.value) * audio.duration;
  audio.currentTime = t;
  updateScrubberFill(Number(scrubber.value));
});

// Volume
volSlider.addEventListener('input', () => {
  audio.volume = Number(volSlider.value);
  updateVolumeFill();
});

/**
 * Drive the CSS gradient for the scrubber track fill.
 * The CSS uses var(--scrubber-progress) on .player-scrubber.
 */
function updateScrubberFill(ratio) {
  const pct = `${(ratio * 100).toFixed(1)}%`;
  document.documentElement.style.setProperty('--scrubber-progress', pct);
}

/**
 * Drive the CSS gradient for the volume track fill.
 */
function updateVolumeFill() {
  const pct = `${(Number(volSlider.value) * 100).toFixed(1)}%`;
  document.documentElement.style.setProperty('--volume-level', pct);
}

// ── Keyboard navigation within a column ──────────────────────────────────

/**
 * ArrowUp / ArrowDown move focus between items in the same column.
 * The column container captures focus so scrolling with keys feels natural.
 */
function handleColumnKeyNav(e, container, selector) {
  if (e.key !== 'ArrowUp' && e.key !== 'ArrowDown') return;
  e.preventDefault();
  const items = [...container.querySelectorAll(selector)];
  const current = document.activeElement;
  const idx = items.indexOf(current);
  if (idx < 0) return;
  const next = e.key === 'ArrowDown'
    ? items[Math.min(idx + 1, items.length - 1)]
    : items[Math.max(idx - 1, 0)];
  next.focus();
}

// ── Image uploads ─────────────────────────────────────────────────────────

artistImageInput.addEventListener('change', async () => {
  const file = artistImageInput.files[0];
  artistImageInput.value = '';
  if (!file || pendingArtistImageName === null) return;

  const artistName = pendingArtistImageName;
  pendingArtistImageName = null;

  const fd = new FormData();
  fd.append('image', file);

  try {
    const res = await fetch(
      `${API}/api/artists/${encodeParam(artistName)}/image`,
      { method: 'POST', body: fd }
    );
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    // Refresh artists to pick up has_image change
    await loadArtists();
    // Re-select the artist so albums column stays in sync
    if (state.selectedArtist !== null) selectArtist(state.selectedArtist);
  } catch (err) {
    console.error('Failed to upload artist image:', err);
  }
});

albumImageInput.addEventListener('change', async () => {
  const file = albumImageInput.files[0];
  albumImageInput.value = '';
  if (!file || !pendingAlbumImageData) return;

  const { artist, title } = pendingAlbumImageData;
  pendingAlbumImageData = null;

  const fd = new FormData();
  fd.append('image', file);

  try {
    const res = await fetch(
      `${API}/api/albums/${encodeParam(title)}/image?artist=${encodeParam(artist)}`,
      { method: 'POST', body: fd }
    );
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    // Refresh albums for selected artist
    if (state.selectedArtist !== null) await loadAlbums(state.selectedArtist);
    // Re-select album so tracks stay visible
    if (state.selectedAlbum !== null) selectAlbum(artist, state.selectedAlbum);
  } catch (err) {
    console.error('Failed to upload album image:', err);
  }
});

// ── Boot ──────────────────────────────────────────────────────────────────

// Initialise volume fill on load
updateVolumeFill();

loadArtists();
