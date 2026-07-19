// Madnetwork — browsing the merged catalog of this node's friends (federation
// F2, docs/architecture/federation.md §Catalog). A drill-down mirroring the
// local library (artist → album → track); the same tagset text offered by
// several friends is ONE row, and a track expands into its "versions" — the
// distinct claimed recordings behind that text — each with its renditions and
// which friends hold it. F3 (direct transfer) adds the version actions: Play
// (cache-through streaming relay, /api/madnetwork/stream) and Download to
// library (fetch + stage through the review bucket).
//
// Shell page module: NO page DOM at module-eval time — everything inside
// init() (the shell swaps <main> between navigations).
import { gatePage, PAGE_PERMS } from './auth.js';
import { getController } from './player-controller.js';
import { showToast } from './shell.js';

const API = document.querySelector('meta[name="api-url"]')?.content || '';

// Drill state for the current visit; reset on every init (the shell re-imports
// a cached module, so init must not rely on module state surviving).
let drill = null;      // { level: 'artists'|'albums'|'tracks', artist, album }
let searchTimer = null;

// Shared player controller (singleton) + the trackchange subscription so the
// track list can highlight what's playing, mirroring the local library. Wired
// in init(), released in teardown().
let controller = null;
let unsubTrackChange = null;
let unsubPlayState = null;

// In-flight download polls, keyed by hash (survive within a visit; cleared on
// teardown — the server job keeps running and the state is re-pollable).
const downloadPolls = new Map();

// Art placeholder for album rows — the merged catalog carries no cover images,
// so every remote album falls back to this note icon (same glyph the library
// shows for cover-less albums).
const noteSvg =
  `<svg width="20" height="20" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">` +
  `<path d="M12 3v10.55A4 4 0 1 0 14 17V7h4V3h-6z"/></svg>`;

export async function init() {
  if (!gatePage(PAGE_PERMS.madnetwork)) return;
  drill = { level: 'artists', artist: null, album: null };

  controller = getController();
  unsubTrackChange = controller.on('trackchange', t => highlightPlaying(t));
  unsubPlayState = controller.on('playstate', reflectPlayState);

  const input = document.getElementById('mnSearchInput');
  input.addEventListener('input', () => {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => { if (drill.level === 'artists') showArtists(input.value); }, 250);
  });

  loadStatus();
  showArtists('');
}

export function teardown() {
  clearTimeout(searchTimer);
  for (const timer of downloadPolls.values()) clearTimeout(timer);
  downloadPolls.clear();
  if (unsubTrackChange) { unsubTrackChange(); unsubTrackChange = null; }
  if (unsubPlayState) { unsubPlayState(); unsubPlayState = null; }
  controller = null;
  drill = null;
}

// highlightPlaying marks the row of the playing appearance (matched by data-key
// = the artist/album/title identity, so the same audio under another album is a
// distinct row) and reflects the pause/resume indicator, mirroring the library.
function highlightPlaying(track) {
  const key = track ? (track.rowKey || track.url) : null;
  const paused = controller?.paused;
  document.querySelectorAll('.mn-track').forEach(row => {
    const on = !!key && row.dataset.key === key;
    row.classList.toggle('playing', on);
    row.classList.toggle('paused', on && paused);
  });
}

// reflectPlayState flips the pause/resume indicator on the current row on every
// play/pause of the shared player.
function reflectPlayState(playing) {
  document.querySelectorAll('.mn-track.playing')
    .forEach(row => row.classList.toggle('paused', !playing));
}

// ── Status strip ──────────────────────────────────────────────────────────────

function fmtAgo(unix) {
  if (!unix) return 'never';
  const s = Math.floor(Date.now() / 1000) - unix;
  if (s < 90) return 'just now';
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return new Date(unix * 1000).toLocaleDateString(undefined, { dateStyle: 'medium' });
}

async function loadStatus() {
  const box = document.getElementById('mnStatus');
  let data;
  try {
    const res = await fetch(`${API}/api/madnetwork/summary`);
    if (!res.ok) return;
    data = await res.json();
  } catch { return; }
  if (!box) return; // navigated away meanwhile

  const friends = data.friends || [];
  box.replaceChildren();
  if (!friends.length) {
    box.append(mkSpan('mn-status-main', 'No friends yet — the madnetwork view fills up once this node friends others on '),
      mkAdminLink());
    box.hidden = false;
    return;
  }
  box.append(mkSpan('mn-status-main',
    `${data.tracks} track${data.tracks === 1 ? '' : 's'} from ${friends.length} friend${friends.length === 1 ? '' : 's'}`));
  for (const f of friends) {
    const chip = document.createElement('span');
    chip.className = 'mn-friend' + (f.entries ? '' : ' mn-friend--empty');
    chip.title = `${f.entries} entries · catalog synced ${fmtAgo(f.synced_at)}`;
    chip.append(mkSpan('mn-friend-name', f.name || '(unnamed)'),
      mkSpan('mn-friend-seen', `seen ${fmtAgo(f.last_seen)}`));
    box.append(chip);
  }
  box.hidden = false;
}

function mkSpan(cls, text) {
  const s = document.createElement('span');
  s.className = cls;
  s.textContent = text;
  return s;
}

function mkAdminLink() {
  const a = document.createElement('a');
  a.href = '/admin/network';
  a.textContent = 'Admin › Network';
  return a;
}

// ── Breadcrumb ────────────────────────────────────────────────────────────────

function renderBreadcrumb() {
  const bc = document.getElementById('mnBreadcrumb');
  bc.replaceChildren();
  const mkLink = (label, handler) => {
    const btn = document.createElement('button');
    btn.className = 'bc-item bc-link';
    btn.textContent = label;
    btn.addEventListener('click', handler);
    return btn;
  };
  const mkSep = () => mkSpan('bc-sep', '›');
  const mkCurrent = label => mkSpan('bc-item bc-current', label);

  if (drill.level === 'artists') {
    bc.append(mkCurrent('Madnetwork'));
  } else if (drill.level === 'albums') {
    bc.append(mkLink('Madnetwork', () => showArtists(currentQuery())), mkSep(), mkCurrent(drill.artist));
  } else {
    bc.append(mkLink('Madnetwork', () => showArtists(currentQuery())), mkSep(),
      mkLink(drill.artist, () => showAlbums(drill.artist)), mkSep(), mkCurrent(drill.album));
  }
  // The artist filter applies to the artist list only.
  document.getElementById('mnSearch').hidden = drill.level !== 'artists';
}

function currentQuery() {
  return document.getElementById('mnSearchInput')?.value || '';
}

// ── Views ─────────────────────────────────────────────────────────────────────

function panelMessage(html) {
  const panel = document.getElementById('mnPanel');
  panel.innerHTML = `<div class="panel-fade-in"><div class="panel-empty">${html}</div></div>`;
}

async function fetchJSON(url) {
  const res = await fetch(url);
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  return res.json();
}

async function showArtists(q) {
  drill = { level: 'artists', artist: null, album: null };
  renderBreadcrumb();
  let data;
  try {
    data = await fetchJSON(`${API}/api/madnetwork/artists?q=${encodeURIComponent(q || '')}`);
  } catch { panelMessage('Could not load the madnetwork catalog.'); return; }
  if (!drill || drill.level !== 'artists') return; // user drilled on meanwhile

  const artists = data.artists || [];
  if (!artists.length) {
    panelMessage(q ? 'No artists match the filter.'
      : 'Nothing here yet — catalogs appear after the first sync with a friend.');
    return;
  }
  const wrap = document.createElement('div');
  wrap.className = 'panel-fade-in';
  for (const a of artists) {
    wrap.append(mkRow('artist-row', a.name,
      `${a.albums} album${a.albums === 1 ? '' : 's'} · ${a.tracks} track${a.tracks === 1 ? '' : 's'}`,
      () => showAlbums(a.name)));
  }
  const panel = document.getElementById('mnPanel');
  panel.replaceChildren(wrap);
}

async function showAlbums(artist) {
  drill = { level: 'albums', artist, album: null };
  renderBreadcrumb();
  let data;
  try {
    data = await fetchJSON(`${API}/api/madnetwork/albums?artist=${encodeURIComponent(artist)}`);
  } catch { panelMessage('Could not load albums.'); return; }
  if (!drill || drill.level !== 'albums' || drill.artist !== artist) return;

  const albums = data.albums || [];
  if (!albums.length) { panelMessage('No albums found.'); return; }
  const wrap = document.createElement('div');
  wrap.className = 'panel-fade-in';
  for (const al of albums) {
    wrap.append(mkAlbumRow(al, () => showTracks(artist, al.title)));
  }
  document.getElementById('mnPanel').replaceChildren(wrap);
}

async function showTracks(artist, album) {
  drill = { level: 'tracks', artist, album };
  renderBreadcrumb();
  let data;
  try {
    data = await fetchJSON(`${API}/api/madnetwork/tracks?artist=${encodeURIComponent(artist)}&album=${encodeURIComponent(album)}`);
  } catch { panelMessage('Could not load tracks.'); return; }
  if (!drill || drill.level !== 'tracks' || drill.album !== album) return;

  const tracks = data.tracks || [];
  if (!tracks.length) { panelMessage('No tracks found.'); return; }

  // Build the album's play queue once (default = each track's most-held version,
  // ladder-best rendition). Unplayable tracks (no rendition hash) are kept out of
  // the queue; the map lets each row find its queue slot so prev/next stays tight.
  const queue = [];
  const qIndex = new Map();
  tracks.forEach((t, i) => {
    const best = t.versions?.[0]?.renditions?.[0];
    if (best && best.hash) {
      qIndex.set(i, queue.length);
      queue.push({
        url: `${API}/api/madnetwork/stream/${best.hash}`,
        // Appearance identity: the artist/album/title text, so the same audio
        // under another album is a distinct row (click restarts, not pauses).
        rowKey: `mn:${drill.artist}␟${drill.album}␟${(t.title || '').toLowerCase()}`,
        title: t.title || 'Unknown',
        artist: t.artist || drill.artist || '',
        dur: fmtDur(t.duration) || '—',
      });
    }
  });

  const wrap = document.createElement('div');
  wrap.className = 'panel-fade-in';
  const list = document.createElement('ul');
  list.className = 'track-list';
  let lastDisc;
  const multiDisc = new Set(tracks.map(t => t.disc_number ?? null)).size > 1;
  tracks.forEach((t, i) => {
    const disc = t.disc_number ?? null;
    if (multiDisc && disc !== lastDisc) {
      lastDisc = disc;
      const hd = document.createElement('li');
      hd.className = 'track-disc-header';
      hd.textContent = disc === null ? 'No disc' : disc === 0 ? 'Disc 0' : `Disc ${disc}`;
      list.append(hd);
    }
    list.append(mkTrackRow(t, i, queue, qIndex.get(i)));
  });
  wrap.append(list);
  document.getElementById('mnPanel').replaceChildren(wrap);

  // Re-highlight whatever is playing if its row is in this view.
  const cur = controller?.current?.();
  if (cur && cur.track) highlightPlaying(cur.track);
}

// ── Row builders ──────────────────────────────────────────────────────────────

function mkRow(kind, name, meta, onOpen) {
  const row = document.createElement('div');
  row.className = `panel-row ${kind}`;
  row.tabIndex = 0;
  row.setAttribute('role', 'button');
  row.setAttribute('aria-label', `Browse ${name}`);
  row.append(mkSpan('row-name', name), mkSpan('row-meta', meta), mkSpan('row-chevron', '›'));
  row.addEventListener('click', onOpen);
  row.addEventListener('keydown', e => {
    if (e.target !== row) return;
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onOpen(); }
  });
  return row;
}

// Album row with the library's cover-art column (a note placeholder — the merged
// catalog carries no images) so the drill-down looks like the local library.
function mkAlbumRow(al, onOpen) {
  const row = document.createElement('div');
  row.className = 'panel-row album-row';
  row.tabIndex = 0;
  row.setAttribute('role', 'button');
  row.setAttribute('aria-label', `Browse album ${al.title}`);

  const art = document.createElement('div');
  art.className = 'row-art';
  art.innerHTML = noteSvg;

  const body = document.createElement('div');
  body.className = 'row-body';
  const name = document.createElement('div');
  name.className = 'row-name';
  name.textContent = al.title;
  const meta = document.createElement('div');
  meta.className = 'row-meta';
  const yearPrefix = al.year ? `${al.year} · ` : '';
  meta.textContent = `${yearPrefix}${al.tracks} track${al.tracks === 1 ? '' : 's'}`;
  body.append(name, meta);

  const chevron = mkSpan('row-chevron', '›');
  chevron.setAttribute('aria-hidden', 'true');

  row.append(art, body, chevron);
  row.addEventListener('click', onOpen);
  row.addEventListener('keydown', e => {
    if (e.target !== row) return;
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onOpen(); }
  });
  return row;
}

function fmtDur(s) {
  if (!s || !isFinite(s)) return '';
  const m = Math.floor(s / 60), sec = Math.round(s % 60);
  return `${m}:${String(sec).padStart(2, '0')}`;
}

function fmtQuality(rd) {
  const bits = [];
  if (rd.codec) bits.push(rd.codec.toUpperCase());
  if (rd.bitrate) bits.push(`${Math.round(rd.bitrate / 1000)} kbps`);
  if (rd.sample_rate) bits.push(`${(rd.sample_rate / 1000).toLocaleString(undefined, { maximumFractionDigits: 1 })} kHz`);
  if (rd.size) bits.push(fmtSize(rd.size));
  return bits.join(' · ') || 'unknown quality';
}

function fmtSize(n) {
  if (n < 1024 * 1024) return `${Math.round(n / 1024)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

// A track row laid out like the local library's (number / title+artist /
// duration), clicking to play. The madnetwork-only extras are a Download pill
// and a subtle ⓘ toggle that reveals the source panel (holders / versions /
// quality) — rare info kept out of the way. `queue` is the album's play queue
// and `qi` this track's slot in it (undefined when the track has no playable
// rendition).
function mkTrackRow(t, i, queue, qi) {
  const li = document.createElement('li');
  const row = document.createElement('div');
  row.className = 'track-row mn-track';
  row.tabIndex = 0;
  row.setAttribute('role', 'button');

  const playable = qi != null;
  if (playable) {
    row.dataset.url = queue[qi].url;
    row.dataset.key = queue[qi].rowKey; // appearance identity for the playing highlight
  }

  const num = mkSpan('track-num', t.track_number ?? (i + 1));
  const icon = document.createElement('span');
  icon.className = 'track-icon-playing';
  icon.setAttribute('aria-hidden', 'true');
  icon.innerHTML = '<svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor"><path d="M8 5v14l11-7z"/></svg>';

  // Title over artist, stacked — the library uses block <div>s here (spans would
  // run onto one line).
  const info = document.createElement('div');
  info.className = 'track-info';
  const title = document.createElement('div');
  title.className = 'track-title';
  title.textContent = t.title || 'Unknown';
  const meta = document.createElement('div');
  meta.className = 'track-meta';
  meta.textContent = t.artist || drill?.artist || '';
  info.append(title, meta);

  row.append(num, icon, info);

  // Download — the madnetwork-only action, on the default version's best rendition.
  const best = t.versions?.[0]?.renditions?.[0];
  if (best && best.hash) {
    const dl = document.createElement('button');
    dl.type = 'button';
    dl.className = 'mn-dl';
    dl.textContent = '⬇ Download';
    dl.title = 'Fetch into this server’s library (staged for review)';
    dl.addEventListener('click', e => { e.stopPropagation(); startDownload(dl, best.hash); });
    row.append(dl);
  }

  // Source & versions — a subtle info toggle for the provenance panel.
  const detail = document.createElement('div');
  detail.className = 'mn-versions';
  detail.hidden = true;
  const infoBtn = document.createElement('button');
  infoBtn.type = 'button';
  infoBtn.className = 'mn-info';
  infoBtn.textContent = 'ⓘ';
  infoBtn.title = 'Source & versions';
  infoBtn.setAttribute('aria-label', `Source and versions for ${t.title || 'this track'}`);
  infoBtn.setAttribute('aria-expanded', 'false');
  const toggle = () => {
    if (detail.hidden) renderVersions(detail, t);
    detail.hidden = !detail.hidden;
    infoBtn.setAttribute('aria-expanded', String(!detail.hidden));
    row.classList.toggle('mn-track--open', !detail.hidden);
  };
  infoBtn.addEventListener('click', e => { e.stopPropagation(); toggle(); });
  row.append(infoBtn, mkSpan('track-dur', fmtDur(t.duration)));

  li.append(row, detail);

  if (playable) {
    // Click the playing row to pause/resume it; any other row starts fresh.
    const play = () => {
      const cur = controller?.current();
      const curKey = cur ? (cur.track.rowKey || cur.track.url) : null;
      if (curKey === queue[qi].rowKey) controller.toggle();
      else controller?.setQueue(queue, qi);
    };
    row.setAttribute('aria-label', `Play ${t.title || 'track'}`);
    row.addEventListener('click', play);
    row.addEventListener('keydown', e => {
      if (e.target !== row) return;
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); play(); }
    });
  } else {
    row.setAttribute('aria-label', t.title || 'Unknown');
    // Nothing to play — the ⓘ panel is still reachable via its own button.
    row.addEventListener('keydown', e => {
      if (e.target !== row) return;
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggle(); }
    });
  }
  return li;
}

function renderVersions(detail, t) {
  detail.replaceChildren();
  (t.versions || []).forEach((v, i) => {
    const box = document.createElement('div');
    box.className = 'mn-version';
    const head = document.createElement('div');
    head.className = 'mn-version-head';
    if ((t.versions || []).length > 1) head.append(mkSpan('mn-version-label', `Version ${i + 1}`));
    if (v.license) head.append(mkSpan('mn-version-license', v.license));
    if (v.guest_playable) head.append(mkSpan('mn-version-guest', 'guest'));
    if (head.childElementCount) box.append(head);

    const rds = document.createElement('ul');
    rds.className = 'mn-renditions';
    for (const rd of v.renditions || []) {
      const li = document.createElement('li');
      li.append(mkSpan('mn-rendition-quality', fmtQuality(rd)));
      li.title = rd.hash;
      rds.append(li);
    }
    box.append(rds);

    const hs = document.createElement('div');
    hs.className = 'mn-holders';
    hs.append(mkSpan('mn-holders-label', 'held by '));
    (v.holders || []).forEach((h, j) => {
      if (j) hs.append(document.createTextNode(', '));
      const holder = mkSpan('mn-holder', h.name || '(unnamed)');
      holder.title = `last seen ${fmtAgo(h.last_seen)}`;
      hs.append(holder);
    });
    box.append(hs);

    // F3 actions on the version's ladder-best rendition (renditions[0] — the
    // server sorts them by the quality ladder).
    const best = (v.renditions || [])[0];
    if (best && best.hash) {
      box.append(mkVersionActions(t, best));
    }
    detail.append(box);
  });
}

// ── F3 version actions: play (streamed relay) + download to library ──────────

function mkVersionActions(t, rd) {
  const bar = document.createElement('div');
  bar.className = 'mn-actions';

  const play = document.createElement('button');
  play.className = 'btn btn-neutral mn-action';
  play.textContent = '▶ Play';
  play.title = 'Stream from the madnetwork (relayed through this server)';
  play.addEventListener('click', () => {
    getController().setQueue([{
      url: `${API}/api/madnetwork/stream/${rd.hash}`,
      title: t.title || 'Unknown',
      artist: t.artist || drill?.artist || '',
    }], 0);
  });

  const queue = document.createElement('button');
  queue.className = 'btn btn-neutral mn-action';
  queue.textContent = '+ Queue';
  queue.title = 'Add to the play queue';
  queue.addEventListener('click', () => {
    getController().enqueue([{
      url: `${API}/api/madnetwork/stream/${rd.hash}`,
      title: t.title || 'Unknown',
      artist: t.artist || drill?.artist || '',
    }]);
    showToast('Added to queue.', { type: 'success' });
  });

  const dl = document.createElement('button');
  dl.className = 'btn btn-neutral mn-action';
  dl.textContent = '⬇ Download';
  dl.title = 'Fetch into this server’s library (staged for review)';
  dl.addEventListener('click', () => startDownload(dl, rd.hash));

  bar.append(play, queue, dl);
  return bar;
}

async function startDownload(btn, hash) {
  btn.disabled = true;
  btn.textContent = 'Downloading…';
  let data;
  try {
    const res = await fetch(`${API}/api/madnetwork/download`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ hash }),
    });
    data = await res.json().catch(() => ({}));
    if (res.status === 401 || res.status === 403) {
      showToast('You need upload permission to download into the library.', { type: 'error' });
      resetDownloadBtn(btn); return;
    }
    if (!res.ok && !data.started) {
      showToast(data.error || 'Download failed to start.', { type: 'error' });
      resetDownloadBtn(btn); return;
    }
  } catch {
    showToast('Download failed to start.', { type: 'error' });
    resetDownloadBtn(btn); return;
  }
  if (data.existed) {
    showToast(data.attached
      ? 'Bytes already in the library — the tagset was staged as a new appearance.'
      : 'Already in the library (nothing new to add).', { type: 'success' });
    resetDownloadBtn(btn, data.attached ? 'Staged' : 'In library');
    return;
  }
  pollDownload(btn, hash);
}

function pollDownload(btn, hash) {
  const tick = async () => {
    let data;
    try {
      const res = await fetch(`${API}/api/madnetwork/transfers/${hash}`);
      if (!res.ok) throw new Error();
      data = await res.json();
    } catch { schedule(); return; }
    switch (data.state) {
      case 'staged':
      case 'attached':
        showToast('Downloaded — staged in My uploads for review.', { type: 'success' });
        resetDownloadBtn(btn, 'Staged'); downloadPolls.delete(hash); return;
      case 'approved':
        showToast('Downloaded into the library.', { type: 'success' });
        resetDownloadBtn(btn, 'In library'); downloadPolls.delete(hash); return;
      case 'failed':
        showToast(`Download failed: ${data.error || 'unknown error'}`, { type: 'error' });
        resetDownloadBtn(btn); downloadPolls.delete(hash); return;
      default: {
        if (data.size > 0 && data.progress >= 0 && btn.isConnected) {
          btn.textContent = `${Math.floor((data.progress / data.size) * 100)} %`;
        }
        schedule();
      }
    }
  };
  const schedule = () => downloadPolls.set(hash, setTimeout(tick, 1500));
  schedule();
}

function resetDownloadBtn(btn, label) {
  if (!btn.isConnected) return;
  btn.textContent = label || '⬇ Download';
  btn.disabled = !!label;
}
