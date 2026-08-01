// Madnetwork — browsing the merged catalog of this node's community PLUS its own
// published library (federation F2/F3, docs/architecture/federation.md;
// UI design docs/ui/madnetwork-page.md). Built on the shared browse core: the
// same rows (hearts, "⋯" quick-add menus) and the same search view as the
// library page. The madnetwork-specific extras are the ⓘ source/versions panel
// and **Materialize** — fetching a remote track into this server's library
// (staged through the review bucket; the F3/F4 transfer machinery).
//
// The landing view is NOT the alphabet. On your own library you browse because
// you already know what is in it; on the network you have no memory to navigate,
// so the page leads with search and a set of lanes that each answer a question
// somebody actually arrives with — what can I get that I don't have, what
// appeared since I last looked, what does my community have, what is nearly
// gone, what did the people I chose personally bring, what does that one node
// have. The A→Z drill-down is still here, demoted to "Browse all" and windowed.
//
// Every lane is a plain fact about what THIS node has cached, and every row says
// why it is in the lane it is in. Nothing here is a recommendation.
//
// A track's default pick is its most-held version's ladder-best rendition;
// self-held tracks play the direct local /files/ URL (no relay hop) and carry
// their local tagset id, so hearts and playlists work on them like anywhere.
//
// Shell page module: NO page DOM at module-eval time — everything inside
// init() (the shell swaps <main> between navigations).
import { gatePage, PAGE_PERMS, getIdentity } from './auth.js';
import { getController } from './player-controller.js';
import { trackKey } from './favorites.js';
import { fmtTime } from './player.js';
import { showToast } from './shell.js';
import { quickAddItems } from './quick-add.js';
import {
  buildArtistRow, buildAlbumRow, buildTrackRow, mkDiscHeader,
  highlightPlayingRow, reflectPlayStateRows, repaintHearts,
} from './browse-rows.js';
import { createBrowseSearch } from './browse-search.js';
import { createVirtualList } from './virtual-list.js';

const API = document.querySelector('meta[name="api-url"]')?.content || '';

// Drill state for the current visit; reset on every init (the shell re-imports
// a cached module, so init must not rely on module state surviving).
//   level  'discover' (the landing lanes) | 'lane' (one lane, in full)
//          | 'artists' | 'albums' | 'tracks' (Browse all, the old drill-down)
//   source the node whose shelf we are browsing, or null for the merged view:
//          { id, name } with id 'self' for our own published library.
let drill = null;
let search = null;     // shared browse-search factory (per activation)
let abort = null;      // AbortController tied to the activation
let nodes = [];        // the summary's node list, reused by the "By node" lane

// The Browse-all artist scroller: the merged catalog is a whole community's
// output now, so it is paged and windowed like the library's rather than
// rendered whole (docs/ui/madnetwork-page.md §Scale stops being optional).
const ARTIST_PAGE_SIZE = 80;
let artistVList = null;
function destroyArtistVList() { if (artistVList) { artistVList.destroy(); artistVList = null; } }

// Shared player controller (singleton) + subscriptions so the track list can
// highlight what's playing, mirroring the local library. Wired in init(),
// released in teardown().
let controller = null;
let unsubTrackChange = null;
let unsubPlayState = null;

// In-flight materialize polls, keyed by hash (survive within a visit; cleared
// on teardown — the server job keeps running and the state is re-pollable).
const materializePolls = new Map();

export async function init() {
  if (!gatePage(PAGE_PERMS.madnetwork)) return;
  drill = { level: 'discover', artist: null, album: null, lane: null, source: null };
  abort = new AbortController();
  nodes = [];

  controller = getController();
  unsubTrackChange = controller.on('trackchange', t => highlightPlayingRow(t, controller.paused));
  unsubPlayState = controller.on('playstate', reflectPlayStateRows);

  search = createBrowseSearch({
    signal: abort.signal,
    fetchResults: async (q, fetchSignal) => {
      const res = await fetch(`${API}/api/madnetwork/search?q=${encodeURIComponent(q)}`, { signal: fetchSignal });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      return res.json();
    },
    onOpenArtist: a => showAlbums(a.name),
    onOpenAlbum:  a => showTracks(a.artist_name, a.title),
    albumArtUrl:  () => null, // the merged catalog carries no cover images
    buildQueueTrack: t => ({
      url: t.url ? `${API}${t.url}` : `${API}/api/madnetwork/stream/${t.hash}`,
      tagsetId: t.tagset_id || null,
      remoteLike: t.tagset_id ? null : {
        hash: t.hash, title: t.title || '', artist: t.artist_name || t.artist || '', album: t.album_title || '',
      },
      rowKey: mnKey(t.artist, t.album_title, t.title),
      title:  t.title || 'Unknown',
      artist: t.artist_name || t.artist || '',
    }),
  });

  loadStatus();
  showDiscover();
}

export function teardown() {
  abort?.abort();
  abort = null;
  search = null;
  destroyArtistVList();
  bulk = null; // stops the bulk watcher; server-side transfers finish on their own
  for (const timer of materializePolls.values()) clearTimeout(timer);
  materializePolls.clear();
  if (unsubTrackChange) { unsubTrackChange(); unsubTrackChange = null; }
  if (unsubPlayState) { unsubPlayState(); unsubPlayState = null; }
  controller = null;
  drill = null;
}

// canMaterialize mirrors the server gate on POST /api/madnetwork/download —
// UX only, the API still enforces it.
function canMaterialize() {
  return !!getIdentity()?.permissions?.includes('file.upload');
}

// canSeeNetwork gates the holder → map links: /admin/network is admin ground and
// linking a user somewhere they will only be refused is worse than not linking.
// UX only — the admin API enforces the permission.
function canSeeNetwork() {
  return !!getIdentity()?.permissions?.includes('federation.manage');
}

// materializeAllItems yields the entity ⋯ menus' trailing "Materialize all"
// item (album given = that album; null = the whole artist).
function materializeAllItems(artist, album) {
  if (!canMaterialize()) return [];
  return [{ label: 'Materialize all', onClick: () => materializeAll(artist, album) }];
}

// mnKey is the appearance identity of a merged madnetwork track — the
// artist/album/title text, so the same audio under another album is a distinct
// row (click restarts, not pauses).
function mnKey(artist, album, title) {
  return `mn:${artist || ''}␟${album || ''}␟${(title || '').toLowerCase()}`;
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
  if (!box || !box.isConnected) return; // navigated away meanwhile

  const friends = data.friends || [];
  // The node list feeds the "By node" lane as well as the strip; our own
  // library is one shelf among them (it has no source row, hence the flag).
  nodes = friends.map(f => ({ ...f, self: false }));
  if (data.self_name) nodes.unshift({ name: data.self_name, self: true, entries: 0, last_seen: 0 });
  if (drill?.level === 'discover') refreshNodeLane();

  box.replaceChildren();
  // Fail-open banner: our own inbound mesh path looks dead, so nothing is being
  // hidden — the catalog shown is last-known, not live.
  if (data.inbound_healthy === false) {
    box.append(mkSpan('mn-status-warn',
      '⚠ This node can’t reach the mesh right now — showing the last-known catalog.'));
  }
  if (!friends.length && !data.tracks) {
    box.append(mkSpan('mn-status-main', 'No friends yet — the madnetwork view fills up once this node friends others on '),
      mkAdminLink());
    box.hidden = false;
    return;
  }
  // The strip lists every node we hold a catalog from: the ones this admin
  // friended, and the members of the wider community the frontier has reached.
  // They browse identically, so the count says "libraries", not "friends".
  box.append(mkSpan('mn-status-main',
    `${data.tracks} track${data.tracks === 1 ? '' : 's'} from ${friends.length} ` +
    `librar${friends.length === 1 ? 'y' : 'ies'}` +
    (data.self_name ? ' + this one' : '')));
  // Every chip opens that node's shelf — the strip already answered "who is
  // here", and "what do they have" is the next question a person asks.
  if (data.self_name) {
    const chip = mkNodeChip('mn-friend', data.self_name, 'this server',
      'This server’s own published library, merged into the view',
      { id: 'self', name: data.self_name });
    box.append(chip);
  }
  for (const f of friends) {
    const stale = f.reachable === false;
    const cls = 'mn-friend' + (f.entries ? '' : ' mn-friend--empty') + (stale ? ' mn-friend--stale' : '');
    box.append(mkNodeChip(cls, f.name || '(unnamed)', `seen ${fmtAgo(f.last_seen)}`,
      `${f.entries} entries · catalog synced ${fmtAgo(f.synced_at)}` +
      (f.friend ? ' · a node this server friended directly' : ' · reached through the community') +
      (stale ? ' · not seen recently — its tracks are hidden' : '') +
      ' · click to browse this node',
      { id: f.id, name: f.name || '(unnamed)' }));
  }
  box.hidden = false;
}

function mkNodeChip(cls, name, seen, title, source) {
  const chip = document.createElement('button');
  chip.type = 'button';
  chip.className = cls;
  chip.title = title;
  chip.append(mkSpan('mn-friend-name', name), mkSpan('mn-friend-seen', seen));
  chip.addEventListener('click', () => showArtists(source));
  return chip;
}

// refreshNodeLane redraws the By-node lane when the status poll lands after the
// landing view is already on screen (the two fetches race on every load).
function refreshNodeLane() {
  const panel = document.getElementById('mnPanel');
  const existing = panel?.querySelector('.mn-lane--nodes');
  const lane = buildNodeLane();
  if (!lane) return;
  lane.classList.add('mn-lane--nodes');
  if (existing) existing.replaceWith(lane);
  else panel?.querySelector('.panel-fade-in')?.append(lane);
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

  // Everything hangs off the landing view, and a node's shelf announces itself
  // as its own step so it is never mistaken for the merged catalog.
  const trail = [];
  if (drill.level !== 'discover') trail.push(mkLink('Madnetwork', () => showDiscover()));
  if (drill.level === 'lane') {
    trail.push(mkCurrent(laneTitle(drill.lane)));
  } else if (drill.level !== 'discover') {
    const rootLabel = drill.source ? drill.source.name : 'Browse all';
    const root = () => showArtists(drill.source);
    if (drill.level === 'artists') trail.push(mkCurrent(rootLabel));
    else trail.push(mkLink(rootLabel, root));
    if (drill.level === 'albums') trail.push(mkCurrent(drill.artist));
    if (drill.level === 'tracks') {
      trail.push(mkLink(drill.artist, () => showAlbums(drill.artist)), mkCurrent(drill.album));
    }
  }
  if (!trail.length) trail.push(mkCurrent('Madnetwork'));

  trail.forEach((el, i) => {
    if (i) bc.append(mkSep());
    bc.append(el);
  });
  renderBarActions();
}

// Bar actions are per view: the landing keeps the alphabet one click away
// (demoted, not deleted), and the tracks view carries the visible "Materialize
// all" — an album-level bulk action shouldn't hide in a menu.
function renderBarActions() {
  const el = document.getElementById('mnBarActions');
  if (!el) return;
  el.replaceChildren();
  if (drill?.level === 'discover') {
    const btn = document.createElement('button');
    btn.className = 'btn btn-neutral';
    btn.textContent = 'Browse all A→Z';
    btn.title = 'Every artist in the merged catalog, alphabetically';
    btn.addEventListener('click', () => showArtists(null));
    el.append(btn);
    return;
  }
  if (drill?.level !== 'tracks' || !canMaterialize()) return;
  const { artist, album } = drill;
  const btn = document.createElement('button');
  btn.className = 'btn btn-neutral mn-bulk-btn';
  btn.textContent = '⬇ Materialize all';
  btn.title = 'Fetch every track of this album into this server’s library';
  btn.addEventListener('click', () => materializeAll(artist, album));
  el.append(btn);
}

// ── Data fetching ─────────────────────────────────────────────────────────────

function panelMessage(html) {
  const panel = document.getElementById('mnPanel');
  panel.innerHTML = `<div class="panel-fade-in"><div class="panel-empty">${html}</div></div>`;
}

async function fetchJSON(url) {
  const res = await fetch(url);
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  return res.json();
}

// queueTrackOf maps a merged track to a controller queue object — the default
// pick is the most-held version's ladder-best rendition; a self-held version
// plays its direct local URL. Null when nothing is playable.
function queueTrackOf(t, artist, album) {
  const v0 = t.versions?.[0];
  const best = v0?.renditions?.[0];
  if (!best || !best.hash) return null;
  return {
    url: v0.url ? `${API}${v0.url}` : `${API}/api/madnetwork/stream/${best.hash}`,
    tagsetId: t.tagset_id || null,
    // Remote-only tracks are likable/playlistable by hash, carrying the
    // display text captured at add time (docs/ui/madnetwork-page.md).
    remoteLike: t.tagset_id ? null : {
      hash: best.hash, title: t.title || '', artist: t.artist || artist || '', album: album || '',
    },
    rowKey: mnKey(artist, album, t.title),
    title: t.title || 'Unknown',
    artist: t.artist || artist || '',
    dur: fmtDur(t.duration) || '—',
  };
}

// entityTracks collects the playable queue tracks of a whole album — or a
// whole artist (album == null → every album, in browse order). Feeds the
// artist/album "⋯" quick-add menus.
async function entityTracks(artist, album) {
  const fetchAlbum = async al => {
    const data = await fetchJSON(`${API}/api/madnetwork/tracks?artist=${encodeURIComponent(artist)}&album=${encodeURIComponent(al)}${sourceQS()}`);
    return (data.tracks || []).map(t => queueTrackOf(t, artist, al)).filter(Boolean);
  };
  if (album != null) return fetchAlbum(album);
  const data = await fetchJSON(`${API}/api/madnetwork/albums?artist=${encodeURIComponent(artist)}${sourceQS()}`);
  const lists = await Promise.all((data.albums || []).map(al => fetchAlbum(al.title)));
  return lists.flat();
}

// ── Landing view: the lanes ───────────────────────────────────────────────────

// Lane titles are also the fallbacks for a lane the server names but this build
// doesn't know — the server sends its own title with every lane.
const LANE_TITLES = {
  missing: 'Not in your library',
  new:     'New on the network',
  held:    'Most held here',
  rare:    'Only one node has it',
  friends: 'From your direct friends',
};
function laneTitle(name) { return LANE_TITLES[name] || 'Lane'; }

// laneNote is the one-line answer to "why is this row here" — the rule that
// every lane row is explainable, made literal. It reads facts the server sent,
// never an inference.
function laneNote(lane, t) {
  const holders = t.holders || 0;
  const nodes_ = n => `${n} node${n === 1 ? '' : 's'}`;
  // The weighted lanes rank by branches, so when the two counts disagree the
  // note says so: this row sits where it does because of independent agreement,
  // not because of how many keys claimed it. Said only when it is news —
  // "5 nodes · 5 branches" would be on every row and mean nothing.
  const held = () => {
    const b = t.branches || 0;
    return b > 0 && b < holders
      ? `held by ${nodes_(holders)} · ${b} branch${b === 1 ? '' : 'es'}`
      : `held by ${nodes_(holders)}`;
  };
  switch (lane) {
    case 'rare':    return t.source_name ? `only ${t.source_name} has it` : 'only one node has it';
    case 'new':     return t.first_seen ? `first seen ${fmtAgo(t.first_seen)}` : 'new here';
    case 'held':    return `${held()} here`;
    case 'missing': return held();
    case 'friends': return `held by ${nodes_(holders)}`;
    default:        return '';
  }
}

async function showDiscover() {
  drill = { level: 'discover', artist: null, album: null, lane: null, source: null };
  destroyArtistVList();
  renderBreadcrumb();
  const panel = document.getElementById('mnPanel');
  panel.innerHTML = '<div class="panel-loading" aria-live="polite" role="status"></div>';

  let data;
  try {
    data = await fetchJSON(`${API}/api/madnetwork/discover`);
  } catch { panelMessage('Could not load the madnetwork.'); return; }
  if (!drill || drill.level !== 'discover') return; // navigated on meanwhile

  const lanes = data.lanes || [];
  const wrap = document.createElement('div');
  wrap.className = 'panel-fade-in';
  for (const lane of lanes) {
    wrap.append(buildLaneBlock(lane));
  }
  const nodeLane = buildNodeLane();
  if (nodeLane) {
    nodeLane.classList.add('mn-lane--nodes');
    wrap.append(nodeLane);
  }

  if (!wrap.childElementCount) {
    panelMessage('Nothing here yet — libraries appear once this node friends others on '
      + '<a href="/admin/network">Admin › Network</a>.');
    return;
  }
  panel.replaceChildren(wrap);
  repaintHearts();
  const cur = controller?.current?.();
  if (cur && cur.track) highlightPlayingRow(cur.track, controller.paused);
}

// buildLaneBlock renders one lane's digest: a heading, up to eight ordinary
// track rows, and "See all" when there is more behind it.
function buildLaneBlock(lane) {
  const box = document.createElement('section');
  box.className = 'mn-lane';

  const head = document.createElement('div');
  head.className = 'mn-lane-head';
  head.append(mkSpan('mn-lane-title', lane.title || laneTitle(lane.name)));
  if (lane.more) {
    const more = document.createElement('button');
    more.className = 'mn-lane-more';
    more.textContent = 'See all →';
    more.addEventListener('click', () => showLane(lane.name));
    head.append(more);
  }
  box.append(head);

  const rows = document.createElement('div');
  rows.className = 'mn-lane-rows';
  const tracks = lane.tracks || [];
  // One queue per lane: playing a lane row continues down that lane, which is
  // the only continuation that makes sense for a list assembled by a ranking.
  const queue = [];
  const qIndex = new Map();
  tracks.forEach((t, i) => {
    const qt = queueTrackOf(t, t.group_artist, t.album);
    if (qt) {
      qIndex.set(i, queue.length);
      queue.push(qt);
    }
  });
  tracks.forEach((t, i) => {
    appendTrackRow(rows, t, i, queue, qIndex.get(i), {
      artist: t.group_artist,
      album: t.album,
      meta: [t.artist || t.group_artist, t.album].filter(Boolean).join(' · '),
      note: laneNote(lane.name, t),
      num: i + 1,
    });
  });
  box.append(rows);
  return box;
}

// buildNodeLane is "By node": every library we hold a catalog from, our own
// included. Opening one enters Browse all restricted to that node — the shelf
// an admin looks at after a report, where a node's offering is complete and
// uncorroborated entries included (docs/ui/madnetwork-page.md).
function buildNodeLane() {
  if (!nodes.length) return null;
  const box = document.createElement('section');
  box.className = 'mn-lane';
  const head = document.createElement('div');
  head.className = 'mn-lane-head';
  head.append(mkSpan('mn-lane-title', 'By node'));
  box.append(head);

  const rows = document.createElement('div');
  rows.className = 'mn-lane-rows';
  for (const n of nodes) {
    rows.append(buildArtistRow({
      name: n.name || '(unnamed)',
      meta: n.self ? 'this server’s own published library'
        : `${n.entries} entr${n.entries === 1 ? 'y' : 'ies'} · seen ${fmtAgo(n.last_seen)}`
          + (n.friend ? ' · direct friend' : ''),
      onOpen: () => showArtists({ id: n.self ? 'self' : n.id, name: n.name || '(unnamed)' }),
    }));
  }
  box.append(rows);
  return box;
}

// ── One lane, in full ─────────────────────────────────────────────────────────

// showLane is "See all": the same ranking without the digest's per-source cap,
// paged. The tail of a lane is reachable here and through search — a lane ranks,
// it never hides.
async function showLane(name, offset = 0) {
  drill = { level: 'lane', artist: null, album: null, lane: name, source: null };
  destroyArtistVList();
  renderBreadcrumb();
  const panel = document.getElementById('mnPanel');
  panel.innerHTML = '<div class="panel-loading" aria-live="polite" role="status"></div>';

  let data;
  try {
    data = await fetchJSON(`${API}/api/madnetwork/lane?name=${encodeURIComponent(name)}&offset=${offset}`);
  } catch { panelMessage('Could not load this lane.'); return; }
  if (!drill || drill.level !== 'lane' || drill.lane !== name) return;

  const lane = data.lane || {};
  const tracks = lane.tracks || [];
  if (!tracks.length) { panelMessage('Nothing in this lane right now.'); return; }

  const wrap = document.createElement('div');
  wrap.className = 'panel-fade-in';
  const queue = [];
  const qIndex = new Map();
  tracks.forEach((t, i) => {
    const qt = queueTrackOf(t, t.group_artist, t.album);
    if (qt) {
      qIndex.set(i, queue.length);
      queue.push(qt);
    }
  });
  tracks.forEach((t, i) => {
    appendTrackRow(wrap, t, i, queue, qIndex.get(i), {
      artist: t.group_artist,
      album: t.album,
      meta: [t.artist || t.group_artist, t.album].filter(Boolean).join(' · '),
      note: laneNote(name, t),
      num: offset + i + 1,
    });
  });

  if (offset > 0 || lane.more) {
    const nav = document.createElement('div');
    nav.className = 'mn-lane-nav';
    if (offset > 0) {
      const prev = document.createElement('button');
      prev.className = 'btn btn-neutral';
      prev.textContent = '← Previous';
      prev.addEventListener('click', () => showLane(name, Math.max(0, offset - (data.limit || 50))));
      nav.append(prev);
    }
    if (lane.more) {
      const next = document.createElement('button');
      next.className = 'btn btn-neutral';
      next.textContent = 'Next →';
      next.addEventListener('click', () => showLane(name, offset + (data.limit || 50)));
      nav.append(next);
    }
    wrap.append(nav);
  }
  panel.replaceChildren(wrap);
  repaintHearts();
  const cur = controller?.current?.();
  if (cur && cur.track) highlightPlayingRow(cur.track, controller.paused);
}

// ── Browse all: the alphabet, demoted and windowed ───────────────────────────

// sourceQS is the "&source=" this view is restricted to, if any — appended to
// every browse fetch so a node's shelf stays a node's shelf all the way down.
function sourceQS() {
  return drill?.source ? `&source=${encodeURIComponent(drill.source.id)}` : '';
}

async function showArtists(source = null) {
  drill = { level: 'artists', artist: null, album: null, lane: null, source };
  destroyArtistVList();
  renderBreadcrumb();
  const panel = document.getElementById('mnPanel');
  panel.innerHTML = '<div class="panel-loading" aria-live="polite" role="status"></div>';

  let page;
  try {
    page = await fetchJSON(`${API}/api/madnetwork/artists?limit=${ARTIST_PAGE_SIZE}${sourceQS()}`);
  } catch { panelMessage('Could not load the madnetwork catalog.'); return; }
  if (!drill || drill.level !== 'artists') return; // user drilled on meanwhile

  const artists = page.artists || [];
  if (!artists.length) {
    panelMessage(source
      ? 'This node is not offering anything we can see right now.'
      : 'Nothing here yet — catalogs appear after the first sync with a friend.');
    return;
  }

  const wrap = document.createElement('div');
  wrap.className = 'panel-fade-in';
  panel.replaceChildren(wrap);

  // Windowed + cursor-paged: only the on-screen rows exist in the DOM and the
  // next page streams in as the window nears the end (same machinery as the
  // library page's artist scroller).
  let cursor = page.next_cursor || null;
  const qs = sourceQS();
  artistVList = createVirtualList({
    sizerEl: wrap,
    windowScroll: true,
    makeSpacer: spacerDiv,
    renderRow: artistRow,
    estimateHeight: 56,
    fetchMore: async () => {
      if (!cursor) return { items: [], done: true };
      try {
        const d = await fetchJSON(
          `${API}/api/madnetwork/artists?limit=${ARTIST_PAGE_SIZE}&cursor=${encodeURIComponent(cursor)}${qs}`);
        cursor = d.next_cursor || null;
        return { items: d.artists || [], done: !cursor };
      } catch {
        return { items: [], done: true };
      }
    },
  });
  artistVList.setItems(artists);
}

function spacerDiv(px) {
  const d = document.createElement('div');
  d.style.height = `${Math.max(0, px)}px`;
  d.setAttribute('aria-hidden', 'true');
  return d;
}

function artistRow(a) {
  return buildArtistRow({
    name: a.name,
    meta: `${a.albums} album${a.albums === 1 ? '' : 's'} · ${a.tracks} track${a.tracks === 1 ? '' : 's'}`,
    onOpen: () => showAlbums(a.name),
    makeMenuItems: btn => quickAddItems(btn, () => entityTracks(a.name, null),
      { extraItems: materializeAllItems(a.name, null) }),
  });
}

async function showAlbums(artist) {
  drill = { ...drill, level: 'albums', artist, album: null };
  destroyArtistVList();
  renderBreadcrumb();
  let data;
  try {
    data = await fetchJSON(`${API}/api/madnetwork/albums?artist=${encodeURIComponent(artist)}${sourceQS()}`);
  } catch { panelMessage('Could not load albums.'); return; }
  if (!drill || drill.level !== 'albums' || drill.artist !== artist) return;

  const albums = data.albums || [];
  if (!albums.length) { panelMessage('No albums found.'); return; }
  const wrap = document.createElement('div');
  wrap.className = 'panel-fade-in';
  for (const al of albums) {
    const yearPrefix = al.year ? `${al.year} · ` : '';
    wrap.append(buildAlbumRow({
      title: al.title,
      meta: `${yearPrefix}${al.tracks} track${al.tracks === 1 ? '' : 's'}`,
      artUrl: null, // no cover images in the merged catalog
      onOpen: () => showTracks(artist, al.title),
      makeMenuItems: btn => quickAddItems(btn, () => entityTracks(artist, al.title),
        { extraItems: materializeAllItems(artist, al.title) }),
    }));
  }
  document.getElementById('mnPanel').replaceChildren(wrap);
}

async function showTracks(artist, album) {
  drill = { ...drill, level: 'tracks', artist, album };
  destroyArtistVList();
  renderBreadcrumb();
  let data;
  try {
    data = await fetchJSON(`${API}/api/madnetwork/tracks?artist=${encodeURIComponent(artist)}&album=${encodeURIComponent(album)}${sourceQS()}`);
  } catch { panelMessage('Could not load tracks.'); return; }
  if (!drill || drill.level !== 'tracks' || drill.album !== album) return;

  const tracks = data.tracks || [];
  if (!tracks.length) { panelMessage('No tracks found.'); return; }

  // Build the album's play queue once. Unplayable tracks (no rendition hash)
  // are kept out of the queue; the map lets each row find its queue slot so
  // prev/next stays tight.
  const queue = [];
  const qIndex = new Map();
  tracks.forEach((t, i) => {
    const qt = queueTrackOf(t, artist, album);
    if (qt) {
      qIndex.set(i, queue.length);
      queue.push(qt);
    }
  });

  const wrap = document.createElement('div');
  wrap.className = 'panel-fade-in';
  let shownDisc;
  const multiDisc = new Set(tracks.map(t => t.disc_number ?? null)).size > 1;
  tracks.forEach((t, i) => {
    const disc = t.disc_number ?? null;
    if (multiDisc && disc !== shownDisc) {
      shownDisc = disc;
      wrap.append(mkDiscHeader(disc === null ? 'No disc' : `Disc ${disc}`));
    }
    appendTrackRow(wrap, t, i, queue, qIndex.get(i));
  });
  document.getElementById('mnPanel').replaceChildren(wrap);

  // Re-highlight whatever is playing if its row is in this view.
  const cur = controller?.current?.();
  if (cur && cur.track) highlightPlayingRow(cur.track, controller.paused);
  repaintHearts();
}

// ── Track rows ────────────────────────────────────────────────────────────────

// selfHeld reports whether this node already publishes the track (any version
// backed by the local library) — Materialize is then pointless and omitted.
function selfHeld(t) {
  return (t.versions || []).some(v => v.url || (v.holders || []).some(h => h.self));
}

// appendTrackRow renders one merged track through the shared row builder, then
// adds the madnetwork extras: the ⓘ source/versions toggle and the Materialize
// menu item. `queue` is the list's play queue and `qi` this track's slot in it
// (undefined when the track has no playable rendition).
//
// opts carries what a lane row needs and an album row does not: its own artist
// and album (a lane crosses both), the meta line that spells them out, and the
// `note` explaining why the row is in this lane at all.
function appendTrackRow(wrap, t, i, queue, qi, opts = {}) {
  const playable = qi != null;
  const qt = playable ? queue[qi] : null;
  const artist = opts.artist ?? drill?.artist;
  const album = opts.album ?? drill?.album;

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

  const play = () => {
    if (!playable) { toggle(); return; } // nothing to play — open the panel
    const cur = controller?.current();
    const curKey = cur ? (cur.track.rowKey || cur.track.url) : null;
    if (curKey === qt.rowKey) controller.toggle();
    else controller?.setQueue(queue, qi);
  };

  const row = buildTrackRow({
    num: opts.num ?? t.track_number ?? (i + 1),
    title: t.title || 'Unknown',
    meta: opts.meta ?? (t.artist || artist || ''),
    dur: fmtDur(t.duration) || '—',
    rowKey: playable ? qt.rowKey : null,
    url: playable ? qt.url : null,
    likeKey: playable ? trackKey(qt) : (t.tagset_id || null),
    likeMeta: qt?.remoteLike || null,
    onPlay: play,
    makeMenuItems: btn => {
      const extraItems = [];
      if (playable && canMaterialize() && !selfHeld(t)) {
        const hash = t.versions[0].renditions[0].hash;
        extraItems.push({
          label: 'Materialize',
          onClick: () => startMaterialize(hash, { title: t.title }),
        });
      }
      return quickAddItems(btn, () => (qt ? [qt] : []), { extraItems });
    },
  });
  row.classList.add('mn-track');

  const toggle = () => {
    if (detail.hidden) renderVersions(detail, t);
    detail.hidden = !detail.hidden;
    infoBtn.setAttribute('aria-expanded', String(!detail.hidden));
    row.classList.toggle('mn-track--open', !detail.hidden);
  };
  infoBtn.addEventListener('click', e => { e.stopPropagation(); toggle(); });
  const durEl = row.querySelector('.track-dur');
  // The lane's reason sits next to the row's own facts rather than in a
  // tooltip: a person must be able to see why a row was put in front of them.
  if (opts.note) {
    const why = mkSpan('mn-why', opts.note);
    why.title = 'Why this row is here';
    row.insertBefore(why, durEl);
  }
  row.insertBefore(infoBtn, durEl);

  wrap.append(row, detail);
}

function fmtDur(s) {
  if (!s || !isFinite(s)) return '';
  return fmtTime(s);
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
      const stale = !h.self && h.reachable === false;
      // A holder with a key links to its place on the network map, for an admin
      // who can act on what they find there. Tracking down whoever served
      // something starts from the content that exposed it, not from remembering
      // to go and look at a diagram (federation.md §The network map).
      const linked = !h.self && h.key && canSeeNetwork();
      const holder = linked
        ? Object.assign(document.createElement('a'), {
            className: 'mn-holder mn-holder--linked',
            href: `/admin/network#node=${encodeURIComponent(h.key)}`,
            target: '_blank',
            rel: 'noopener',
            textContent: h.name || '(unnamed)',
          })
        : mkSpan('mn-holder', h.name || '(unnamed)');
      holder.title = h.self ? 'this server'
        : `last seen ${fmtAgo(h.last_seen)}` + (stale ? ' · not seen recently' : '')
          + (linked ? ' · open on the network map' : '');
      if (h.self) holder.classList.add('mn-holder--self');
      if (stale) holder.classList.add('mn-holder--stale');
      hs.append(holder);
    });
    // Versions are ordered by independent voices, not by how many nodes claim
    // them. The server sends the count only when it is lower than the holder
    // count — i.e. when several of these names reach us through one friendship
    // and this list looks broader than the agreement behind it.
    if (v.voices) {
      const note = mkSpan('mn-voices', ` (${v.voices} branch${v.voices === 1 ? '' : 'es'})`);
      note.title = 'these holders reach us through ' + (v.voices === 1 ? 'a single friend' : `${v.voices} friends`)
        + ' — one branch counts once';
      hs.append(note);
    }
    box.append(hs);

    // Actions on the version's ladder-best rendition (renditions[0] — the
    // server sorts them by the quality ladder).
    const best = (v.renditions || [])[0];
    if (best && best.hash) {
      box.append(mkVersionActions(t, v, best));
    }
    detail.append(box);
  });
}

// ── Version actions: play (relay or local) + materialize into the library ────

function mkVersionActions(t, v, rd) {
  const bar = document.createElement('div');
  bar.className = 'mn-actions';

  const track = {
    url: v.url ? `${API}${v.url}` : `${API}/api/madnetwork/stream/${rd.hash}`,
    tagsetId: t.tagset_id || null,
    title: t.title || 'Unknown',
    artist: t.artist || t.group_artist || drill?.artist || '',
  };

  const play = document.createElement('button');
  play.className = 'btn btn-neutral mn-action';
  play.textContent = '▶ Play';
  play.title = v.url ? 'Play from this server’s library'
    : 'Stream from the madnetwork (relayed through this server)';
  play.addEventListener('click', () => getController().setQueue([track], 0));

  const queue = document.createElement('button');
  queue.className = 'btn btn-neutral mn-action';
  queue.textContent = '+ Queue';
  queue.title = 'Add to the play queue';
  queue.addEventListener('click', () => {
    getController().enqueue([track]);
    showToast('Added to queue.', { type: 'success' });
  });

  bar.append(play, queue);

  // Materialize this specific version — omitted when it is already local.
  if (canMaterialize() && !v.url) {
    const mat = document.createElement('button');
    mat.className = 'btn btn-neutral mn-action';
    mat.textContent = '⬇ Materialize';
    mat.title = 'Fetch into this server’s library (staged for review)';
    mat.addEventListener('click', () => startMaterialize(rd.hash, { btn: mat, title: t.title }));
    bar.append(mat);
  }
  return bar;
}

// ── Materialize (POST /api/madnetwork/download + transfer poll) ──────────────

// startMaterialize kicks off the fetch-into-library flow. With a `btn` (the ⓘ
// panel's version action) the button carries the progress; from the ⋯ menu
// (no surviving element) progress is toast-only.
async function startMaterialize(hash, { btn, title } = {}) {
  if (btn) { btn.disabled = true; btn.textContent = 'Materializing…'; }
  else showToast(`Materializing “${title || 'track'}”…`, { type: 'status' });
  let data;
  try {
    const res = await fetch(`${API}/api/madnetwork/download`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ hash }),
    });
    data = await res.json().catch(() => ({}));
    if (res.status === 401 || res.status === 403) {
      showToast('You need upload permission to materialize into the library.', { type: 'error' });
      resetMaterializeBtn(btn); return;
    }
    if (!res.ok && !data.started) {
      showToast(data.error || 'Materialize failed to start.', { type: 'error' });
      resetMaterializeBtn(btn); return;
    }
  } catch {
    showToast('Materialize failed to start.', { type: 'error' });
    resetMaterializeBtn(btn); return;
  }
  if (data.existed) {
    showToast(data.attached
      ? 'Bytes already in the library — the tagset was staged as a new appearance.'
      : 'Already in the library (nothing new to add).', { type: 'success' });
    resetMaterializeBtn(btn, data.attached ? 'Staged' : 'In library');
    return;
  }
  pollMaterialize(btn, hash);
}

function pollMaterialize(btn, hash) {
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
        showToast('Materialized — staged in My uploads for review.', { type: 'success' });
        resetMaterializeBtn(btn, 'Staged'); materializePolls.delete(hash); return;
      case 'approved':
        showToast('Materialized into the library.', { type: 'success' });
        resetMaterializeBtn(btn, 'In library'); materializePolls.delete(hash); return;
      case 'failed':
        showToast(`Materialize failed: ${data.error || 'unknown error'}`, { type: 'error' });
        resetMaterializeBtn(btn); materializePolls.delete(hash); return;
      default: {
        if (btn && data.size > 0 && data.progress >= 0 && btn.isConnected) {
          btn.textContent = `${Math.floor((data.progress / data.size) * 100)} %`;
        }
        schedule();
      }
    }
  };
  const schedule = () => materializePolls.set(hash, setTimeout(tick, 1500));
  schedule();
}

function resetMaterializeBtn(btn, label) {
  if (!btn || !btn.isConnected) return;
  btn.textContent = label || '⬇ Materialize';
  btn.disabled = !!label;
}

// ── Materialize all (docs/ui/madnetwork-page.md phase 5) ─────────────────────
// The per-entity bulk action: fetch every track of an album (or a whole
// artist) into the library. Submissions are strictly sequential — one download
// at a time, waiting out each transfer (the server swarm-fetches each blob's
// CHUNKS in parallel internally) — so a big album never floods the mesh.
// Already-local tracks are skipped. Progress lives in the persistent #mnBulk
// line (toasts can't update in place); one completion toast sums it up.

let bulk = null; // { label, pending, done, local, failed } — one run at a time

function updateBulkUI() {
  const el = document.getElementById('mnBulk');
  if (!el || !el.isConnected) return;
  if (!bulk) { el.hidden = true; el.textContent = ''; return; }
  let text = `Materializing “${bulk.label}” — ${bulk.done + bulk.failed}/${bulk.pending} fetched`;
  if (bulk.local) text += ` · ${bulk.local} already local`;
  if (bulk.failed) text += ` · ${bulk.failed} failed`;
  el.textContent = text;
  el.hidden = false;
}

// collectMaterializeTargets walks the entity's merged tracks: already-local
// tracks count as `local`, everything else contributes its default-version
// best rendition hash.
async function collectMaterializeTargets(artist, album) {
  const targets = { pending: [], local: 0 };
  const scan = async al => {
    const data = await fetchJSON(`${API}/api/madnetwork/tracks?artist=${encodeURIComponent(artist)}&album=${encodeURIComponent(al)}${sourceQS()}`);
    for (const t of data.tracks || []) {
      const best = t.versions?.[0]?.renditions?.[0];
      if (!best || !best.hash) continue; // nothing fetchable
      if (selfHeld(t)) targets.local++;
      else targets.pending.push(best.hash);
    }
  };
  if (album != null) {
    await scan(album);
  } else {
    const data = await fetchJSON(`${API}/api/madnetwork/albums?artist=${encodeURIComponent(artist)}${sourceQS()}`);
    for (const al of data.albums || []) await scan(al.title);
  }
  return targets;
}

// materializeOne submits one download and waits for its terminal state.
// Resolves 'done' | 'local' (bytes were already in the library) | 'failed'.
function materializeOne(hash) {
  return new Promise(resolve => {
    (async () => {
      let data;
      try {
        const res = await fetch(`${API}/api/madnetwork/download`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ hash }),
        });
        data = await res.json().catch(() => ({}));
        if (!res.ok && !data.started && !data.existed) { resolve('failed'); return; }
      } catch { resolve('failed'); return; }
      if (data.existed) { resolve('local'); return; }
      const tick = async () => {
        if (!bulk) { resolve('failed'); return; } // page torn down — stop watching
        let st;
        try {
          const res = await fetch(`${API}/api/madnetwork/transfers/${hash}`);
          if (!res.ok) throw new Error();
          st = await res.json();
        } catch { setTimeout(tick, 2000); return; }
        switch (st.state) {
          case 'staged': case 'attached': case 'approved': resolve('done'); return;
          case 'failed': resolve('failed'); return;
          default: setTimeout(tick, 2000);
        }
      };
      tick();
    })();
  });
}

// materializeAll runs the bulk flow for one album (album given) or a whole
// artist (album null).
async function materializeAll(artist, album) {
  if (!canMaterialize()) return;
  if (bulk) {
    showToast('A bulk materialize is already running — wait for it to finish.', { type: 'status' });
    return;
  }
  const label = album != null ? album : artist;
  bulk = { label, pending: 0, done: 0, local: 0, failed: 0 };
  updateBulkUI();

  let targets;
  try {
    targets = await collectMaterializeTargets(artist, album);
  } catch {
    bulk = null;
    updateBulkUI();
    showToast('Could not load the tracks to materialize.', { type: 'error' });
    return;
  }
  bulk.pending = targets.pending.length;
  bulk.local = targets.local;
  if (!bulk.pending) {
    bulk = null;
    updateBulkUI();
    showToast(`Nothing to fetch — “${label}” is already in the library.`, { type: 'status' });
    return;
  }
  updateBulkUI();

  for (const hash of targets.pending) {
    if (!bulk) return; // torn down mid-run; server-side transfers finish on their own
    const outcome = await materializeOne(hash);
    if (!bulk) return;
    if (outcome === 'done') bulk.done++;
    else if (outcome === 'local') bulk.local++;
    else bulk.failed++;
    updateBulkUI();
  }

  const { done, local, failed } = bulk;
  bulk = null;
  updateBulkUI();
  let msg = `Materialized ${done} track${done !== 1 ? 's' : ''} from “${label}”`;
  if (local) msg += ` (${local} already local)`;
  if (failed) msg += ` — ${failed} failed`;
  showToast(msg + '.', { type: failed ? 'error' : 'success' });
}
