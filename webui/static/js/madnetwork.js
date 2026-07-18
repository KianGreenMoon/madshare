// Madnetwork — browsing the merged catalog of this node's friends (federation
// F2, docs/architecture/federation.md §Catalog). A drill-down mirroring the
// local library (artist → album → track); the same tagset text offered by
// several friends is ONE row, and a track expands into its "versions" — the
// distinct claimed recordings behind that text — each with its renditions and
// which friends hold it. Browse-only in F2: playing and downloading remote
// content arrive with F3 (direct transfer).
//
// Shell page module: NO page DOM at module-eval time — everything inside
// init() (the shell swaps <main> between navigations).
import { gatePage, PAGE_PERMS } from './auth.js';

const API = document.querySelector('meta[name="api-url"]')?.content || '';

// Drill state for the current visit; reset on every init (the shell re-imports
// a cached module, so init must not rely on module state surviving).
let drill = null;      // { level: 'artists'|'albums'|'tracks', artist, album }
let searchTimer = null;

export async function init() {
  if (!gatePage(PAGE_PERMS.madnetwork)) return;
  drill = { level: 'artists', artist: null, album: null };

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
  drill = null;
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
    const yearPrefix = al.year ? `${al.year} · ` : '';
    wrap.append(mkRow('album-row', al.title,
      `${yearPrefix}${al.tracks} track${al.tracks === 1 ? '' : 's'}`,
      () => showTracks(artist, al.title)));
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

  const wrap = document.createElement('div');
  wrap.className = 'panel-fade-in';
  const list = document.createElement('ul');
  list.className = 'track-list';
  let lastDisc;
  const multiDisc = new Set(tracks.map(t => t.disc_number ?? null)).size > 1;
  for (const t of tracks) {
    const disc = t.disc_number ?? null;
    if (multiDisc && disc !== lastDisc) {
      lastDisc = disc;
      const hd = document.createElement('li');
      hd.className = 'track-disc-header';
      hd.textContent = disc === null ? 'No disc' : disc === 0 ? 'Disc 0' : `Disc ${disc}`;
      list.append(hd);
    }
    list.append(mkTrackRow(t));
  }
  wrap.append(list);
  document.getElementById('mnPanel').replaceChildren(wrap);
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

function mkTrackRow(t) {
  const li = document.createElement('li');
  const row = document.createElement('div');
  row.className = 'track-row mn-track';
  row.tabIndex = 0;

  const num = mkSpan('track-num', t.track_number ?? '');
  const body = document.createElement('div');
  body.className = 'mn-track-body';
  body.append(mkSpan('track-title', t.title));
  const holders = new Set();
  for (const v of t.versions || []) for (const h of v.holders || []) holders.add(h.name);
  const metaBits = [];
  if (t.duration) metaBits.push(fmtDur(t.duration));
  metaBits.push(`${holders.size} friend${holders.size === 1 ? '' : 's'}`);
  if ((t.versions || []).length > 1) metaBits.push(`${t.versions.length} versions`);
  body.append(mkSpan('mn-track-meta', metaBits.join(' · ')));

  row.append(num, body, mkSpan('row-chevron mn-expand', '▾'));
  li.append(row);

  // Expansion: the versions detail (renditions + holders). Toggled per row.
  const detail = document.createElement('div');
  detail.className = 'mn-versions';
  detail.hidden = true;
  li.append(detail);
  const toggle = () => {
    if (detail.hidden) renderVersions(detail, t);
    detail.hidden = !detail.hidden;
    row.classList.toggle('mn-track--open', !detail.hidden);
  };
  row.addEventListener('click', toggle);
  row.addEventListener('keydown', e => {
    if (e.target !== row) return;
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggle(); }
  });
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
    detail.append(box);
  });
  const note = document.createElement('div');
  note.className = 'mn-fetch-note';
  note.textContent = 'Playing and downloading from the madnetwork arrives with the transfer milestone.';
  detail.append(note);
}
