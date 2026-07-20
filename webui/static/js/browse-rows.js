// browse-rows.js — the shared row builders for the browse drill-downs (library
// and madnetwork pages, docs/ui/madnetwork-page.md §Shared browse core).
// Extracted from app.js: artist / album / track rows, the inline favorites
// heart, disc headers, and the playing-row highlight helpers. The rows are
// presentation-only — data fetching, drill state, and menu contents stay with
// the calling page, passed in as callbacks.
import { isLiked, toggleLike, onLikedChange } from './favorites.js';
import { mkMoreBtn } from './quick-add.js';

export function esc(s) {
  return String(s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;')
    .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

// Music note SVG used as the album-art placeholder.
export const noteSvg =
  `<svg width="20" height="20" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">` +
  `<path d="M12 3v10.55A4 4 0 1 0 14 17V7h4V3h-6z"/>` +
  `</svg>`;

const heartSvg =
  `<svg width="15" height="15" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">` +
  `<path d="M12 21.35l-1.45-1.32C5.4 15.36 2 12.28 2 8.5 2 5.42 4.42 3 7.5 3c1.74 0 ` +
  `3.41.81 4.5 2.09C13.09 3.81 14.76 3 16.5 3 19.58 3 22 5.42 22 8.5c0 3.78-3.4 ` +
  `6.86-8.55 11.54L12 21.35z"/></svg>`;

// repaintHearts syncs every rendered heart with the shared liked set; runs on
// each render and whenever the set changes (any heart, any page, player bar).
export function repaintHearts() {
  document.querySelectorAll('.row-heart[data-tagset]').forEach(b => {
    const on = isLiked(b.dataset.tagset);
    b.classList.toggle('liked', on);
    b.setAttribute('aria-pressed', String(on));
    const label = on ? 'Remove from Favorites' : 'Add to Favorites';
    b.setAttribute('aria-label', label);
    b.title = label;
  });
}
onLikedChange(repaintHearts);

// mkHeartBtn returns a heart button for a track row (state via repaintHearts),
// keyed by the track's tagset id. The heart is THE favorites control — there is
// no menu duplicate.
export function mkHeartBtn(tagsetId) {
  const btn = document.createElement('button');
  btn.className = 'row-heart';
  btn.dataset.tagset = tagsetId || '';
  btn.setAttribute('aria-pressed', 'false');
  btn.setAttribute('aria-label', 'Add to Favorites');
  btn.title = 'Add to Favorites';
  btn.innerHTML = heartSvg;
  btn.addEventListener('click', e => {
    e.stopPropagation();
    if (tagsetId) toggleLike(tagsetId);
  });
  return btn;
}

// openOnActivate wires click + Enter/Space on a row (buttons inside the row
// handle their own keys).
function openOnActivate(row, onOpen) {
  row.addEventListener('click', onOpen);
  row.addEventListener('keydown', e => {
    if (e.target !== row) return;
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onOpen(); }
  });
}

// buildArtistRow builds one artist row. name/meta are display-ready text;
// makeMenuItems (optional) yields the ⋯ menu for the row.
export function buildArtistRow({ name, meta, onOpen, makeMenuItems }) {
  const row = document.createElement('div');
  row.className = 'panel-row artist-row';
  row.tabIndex = 0;
  row.setAttribute('role', 'button');
  row.setAttribute('aria-label', `Browse ${name}`);
  row.innerHTML =
    `<span class="row-name">${esc(name)}</span>` +
    `<span class="row-meta">${esc(meta)}</span>` +
    `<span class="row-chevron" aria-hidden="true">›</span>`;
  if (makeMenuItems) {
    row.insertBefore(mkMoreBtn(`More actions for ${name}`, makeMenuItems),
      row.querySelector('.row-chevron'));
  }
  openOnActivate(row, onOpen);
  return row;
}

// buildAlbumRow builds one album row with the cover-art column. artUrl null →
// the note placeholder (cover-less albums, remote catalogs).
export function buildAlbumRow({ title, meta, artUrl, onOpen, makeMenuItems }) {
  const artContent = artUrl
    ? `<img src="${artUrl}" alt="" loading="lazy">`
    : noteSvg;
  const row = document.createElement('div');
  row.className = 'panel-row album-row';
  row.tabIndex = 0;
  row.setAttribute('role', 'button');
  row.setAttribute('aria-label', `Browse album ${title}`);
  row.innerHTML =
    `<div class="row-art">${artContent}</div>` +
    `<div class="row-body">` +
      `<div class="row-name">${esc(title)}</div>` +
      `<div class="row-meta">${esc(meta)}</div>` +
    `</div>` +
    `<span class="row-chevron" aria-hidden="true">›</span>`;
  if (makeMenuItems) {
    row.insertBefore(mkMoreBtn(`More actions for ${title}`, makeMenuItems),
      row.querySelector('.row-chevron'));
  }
  openOnActivate(row, onOpen);
  return row;
}

// buildTrackRow builds one track row (num / playing icon / title+meta / heart /
// ⋯ / duration). rowKey is the appearance identity for the playing highlight;
// url and idx land on the dataset when given (duration write-back, unavailable
// marking).
export function buildTrackRow({ num, title, meta, dur, rowKey, url, idx, tagsetId, onPlay, makeMenuItems }) {
  const row = document.createElement('div');
  row.className = 'track-row';
  row.tabIndex = 0;
  if (idx != null) row.dataset.idx = idx;
  if (url) row.dataset.url = url;
  if (rowKey) row.dataset.key = rowKey;
  row.setAttribute('role', 'button');
  row.setAttribute('aria-label', `Play ${title}`);
  row.innerHTML =
    `<span class="track-num">${esc(num)}</span>` +
    `<span class="track-icon-playing" aria-hidden="true">` +
      `<svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor"><path d="M8 5v14l11-7z"/></svg>` +
    `</span>` +
    `<div class="track-info">` +
      `<div class="track-title">${esc(title)}</div>` +
      `<div class="track-meta">${esc(meta)}</div>` +
    `</div>` +
    `<span class="track-dur">${esc(dur)}</span>`;
  const durEl = row.querySelector('.track-dur');
  row.insertBefore(mkHeartBtn(tagsetId), durEl);
  if (makeMenuItems) {
    row.insertBefore(mkMoreBtn(`More actions for ${title}`, makeMenuItems), durEl);
  }
  openOnActivate(row, onPlay);
  return row;
}

// mkDiscHeader returns the "Disc N" subheading between disc groups.
export function mkDiscHeader(label) {
  const hdr = document.createElement('div');
  hdr.className = 'track-disc-header';
  hdr.textContent = label;
  return hdr;
}

// ── Playing-row reflection ───────────────────────────────────────────────────

// playKeyOf is the appearance identity used to match the playing row (rowKey
// when the queue carries it, else tagset/url from an older or foreign queue).
export function playKeyOf(track) {
  if (!track) return null;
  return track.rowKey || (track.tagsetId ? `ts:${track.tagsetId}` : `url:${track.url}`);
}

// highlightPlayingRow marks the row of the playing APPEARANCE (and clears the
// rest), plus the .paused state so the indicator shows pause (playing) vs play
// (resume).
export function highlightPlayingRow(track, paused) {
  const key = playKeyOf(track);
  document.querySelectorAll('.track-row').forEach(row => {
    const on = !!key && row.dataset.key === key;
    row.classList.toggle('playing', on);
    row.classList.toggle('paused', on && paused);
    if (on) row.classList.remove('unavailable');
  });
}

// reflectPlayStateRows flips the pause/resume indicator on the current row
// without re-scanning identity — fired on every play/pause of the shared player.
export function reflectPlayStateRows(playing) {
  document.querySelectorAll('.track-row.playing')
    .forEach(row => row.classList.toggle('paused', !playing));
}

// markUnavailableRows flags every row of a URL the player failed to load.
export function markUnavailableRows(url) {
  document.querySelectorAll('.track-row').forEach(row => {
    if (row.dataset.url === url) row.classList.add('unavailable');
  });
}
