// Madnetwork — the merged catalog of this node's community PLUS its own
// published library (federation F2/F3, docs/architecture/federation.md;
// UI design docs/ui/madnetwork-page.md). Built on the shared browse core: the
// same rows (hearts, "⋯" quick-add menus) and the same search view as the
// library page. The madnetwork-specific extras — the ⓘ source/versions panel,
// Materialize, and the A→Z shelf — live in mn-browse.js, shared with the
// per-node pages.
//
// The landing view is NOT the alphabet. On your own library you browse because
// you already know what is in it; on the network you have no memory to navigate,
// so the page leads with search and a set of lanes that each answer a question
// somebody actually arrives with — what does this server already have, what can
// I get that it doesn't, what appeared since I last looked, what does the
// community have, what is nearly gone, what did the nodes this admin chose
// bring, and which libraries are behind all of it. The A→Z drill-down is still
// here, demoted to "Browse all" and windowed.
//
// Every lane is a plain fact about what THIS node has cached, and every row says
// why it is in the lane it is in. Nothing here is a recommendation.
//
// Shell page module: NO page DOM at module-eval time — everything inside
// init() (the shell swaps <main> between navigations).
import { gatePage, PAGE_PERMS } from './auth.js';
import { getController } from './player-controller.js';
import { createBrowseSearch } from './browse-search.js';
import { highlightPlayingRow, reflectPlayStateRows, repaintHearts } from './browse-rows.js';
import {
  API, fetchJSON, panelMessage, mkSpan, fmtAgo, mnKey,
  buildQueue, appendTrackRow, createShelf,
  clearMaterializePolls, stopBulkWatch,
} from './mn-browse.js';
import { fetchNodes, buildNodeRow } from './mn-nodes.js';

// The landing view's own state. The A→Z drill is a shelf object now (see
// mn-browse.js) rather than more fields here, which is what let a single node's
// shelf move to a page of its own.
//   level  'discover' (the landing lanes) | 'lane' (one lane, in full)
//          | 'shelf'  (Browse all, the old drill-down)
let level = 'discover';
let laneName = null;
let shelf = null;
let search = null;     // shared browse-search factory (per activation)
let abort = null;      // AbortController tied to the activation
let nodes = [];        // the node list, shared by the status line and the Nodes lane

// How many nodes the Nodes lane shows before "See all" — the same eight as a
// track lane's digest, because it is the same kind of thing: a screenful, with
// the rest one click away.
const NODE_DIGEST = 8;

let controller = null;
let unsubTrackChange = null;
let unsubPlayState = null;

export async function init() {
  if (!gatePage(PAGE_PERMS.madnetwork)) return;
  level = 'discover';
  laneName = null;
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
    onOpenArtist: a => openShelf(s => s.showAlbums(a.name)),
    onOpenAlbum:  a => openShelf(s => s.showTracks(a.artist_name, a.title)),
    albumArtUrl:  a => a.cover_hash
      ? `${API}/api/madnetwork/cover/${encodeURIComponent(a.cover_hash)}?size=small`
      : null, // no source claims art for this album
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
  destroyShelf();
  stopBulkWatch(); // server-side transfers finish on their own
  clearMaterializePolls();
  if (unsubTrackChange) { unsubTrackChange(); unsubTrackChange = null; }
  if (unsubPlayState) { unsubPlayState(); unsubPlayState = null; }
  controller = null;
  level = 'discover';
}

function destroyShelf() {
  shelf?.destroy();
  shelf = null;
}

// ── Status line ───────────────────────────────────────────────────────────────

// The strip is two things about the VIEW: how much is in it, and whether it can
// be trusted to be current. It no longer lists the nodes — that was a phone book
// growing without bound (F7 item 5 made the sweep pull from members too), and it
// is a lane plus a page of its own now (docs/ui/madnetwork-nodes.md).
async function loadStatus() {
  const box = document.getElementById('mnStatus');
  let data;
  try {
    data = await fetchNodes();
  } catch { return; }
  if (!box || !box.isConnected) return; // navigated away meanwhile

  nodes = data.nodes;
  if (level === 'discover') refreshNodeLane();

  box.replaceChildren();
  // Fail-open banner: our own inbound mesh path looks dead, so nothing is being
  // hidden — the catalog shown is last-known, not live.
  if (!data.inboundHealthy) {
    box.append(mkSpan('mn-status-warn',
      '⚠ This node can’t reach the mesh right now — showing the last-known catalog.'));
  }
  const others = nodes.filter(n => !n.self).length;
  if (!others && !data.tracks) {
    box.append(mkSpan('mn-status-main', 'No friends yet — the madnetwork view fills up once this node friends others on '),
      mkAdminLink());
    box.hidden = false;
    return;
  }
  const self = nodes.some(n => n.self);
  const tracks = `${data.tracks} track${data.tracks === 1 ? '' : 's'}`;
  // "from 0 libraries + this one" is arithmetic, not a sentence. A node that is
  // alone on the network is a normal state — one this page should say plainly
  // rather than report as a count of nothing.
  box.append(mkSpan('mn-status-main', others
    ? `${tracks} from ${others} librar${others === 1 ? 'y' : 'ies'}` + (self ? ' + this one' : '')
    : `${tracks} — only this server’s library so far`));
  box.hidden = false;
}

// refreshNodeLane redraws the Nodes lane when the node fetch lands after the
// landing view is already on screen (the two fetches race on every load).
function refreshNodeLane() {
  const panel = document.getElementById('mnPanel');
  const existing = panel?.querySelector('.mn-lane--nodes');
  const lane = buildNodeLane();
  if (!lane) return;
  if (existing) existing.replaceWith(lane);
  else panel?.querySelector('.panel-fade-in')?.append(lane);
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
  if (!bc) return;
  bc.replaceChildren();
  const mkLink = (label, handler) => {
    const btn = document.createElement('button');
    btn.className = 'bc-item bc-link';
    btn.textContent = label;
    btn.addEventListener('click', handler);
    return btn;
  };
  const mkCurrent = label => mkSpan('bc-item bc-current', label);

  const trail = [];
  if (level === 'lane') {
    trail.push(mkLink('Madnetwork', () => showDiscover()), mkSpan('bc-sep', '›'),
      mkCurrent(laneTitle(laneName)));
  } else {
    trail.push(mkCurrent('Madnetwork'));
  }
  trail.forEach(el => bc.append(el));
  renderBarActions();
}

// The landing keeps the alphabet one click away (demoted, not deleted).
function renderBarActions() {
  const el = document.getElementById('mnBarActions');
  if (!el) return;
  el.replaceChildren();
  if (level !== 'discover') return;
  const btn = document.createElement('button');
  btn.className = 'btn btn-neutral';
  btn.textContent = 'Browse all A→Z';
  btn.title = 'Every artist in the merged catalog, alphabetically';
  btn.addEventListener('click', () => openShelf(s => s.showArtists()));
  el.append(btn);
}

// ── Browse all: the alphabet, demoted and windowed ───────────────────────────

// openShelf hands the panel to the shared drill-down over the merged catalog.
// Its breadcrumb owns the trail from there on, with a step back to the lanes.
function openShelf(run) {
  destroyShelf();
  level = 'shelf';
  laneName = null;
  document.getElementById('mnBarActions')?.replaceChildren();
  shelf = createShelf({
    panel: document.getElementById('mnPanel'),
    trail: document.getElementById('mnBreadcrumb'),
    actions: document.getElementById('mnBarActions'),
    source: null,
    rootLabel: 'Browse all',
    onExit: () => showDiscover(),
    exitLabel: 'Madnetwork',
  });
  run(shelf);
}

// ── Landing view: the lanes ───────────────────────────────────────────────────

// Lane titles are the fallbacks for a lane the server names but this build
// doesn't know — the server sends its own title with every lane.
const LANE_TITLES = {
  local:   'Local library',
  missing: 'Missing here',
  new:     'New on the network',
  held:    'Most held here',
  rare:    'Only one node has it',
  friends: 'From direct friends',
};
function laneTitle(name) { return LANE_TITLES[name] || 'Lane'; }

// LANE_SEE_ALL_HREF is where a lane's "See all" LEAVES the page instead of
// opening the lane's own view. Only the local library has one: its tail is the
// library page, and a second full view of it inside the network page would be
// two answers to one question.
const LANE_SEE_ALL_HREF = { local: '/library' };

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
    // The local lane's rows need no explanation: they are here because this
    // server published them, which the heading already says.
    default:        return '';
  }
}

async function showDiscover() {
  destroyShelf();
  level = 'discover';
  laneName = null;
  renderBreadcrumb();
  const panel = document.getElementById('mnPanel');
  panel.innerHTML = '<div class="panel-loading" aria-live="polite" role="status"></div>';

  let data;
  try {
    data = await fetchJSON(`${API}/api/madnetwork/discover`);
  } catch { panelMessage(panel, 'Could not load the madnetwork.'); return; }
  if (level !== 'discover') return; // navigated on meanwhile

  const lanes = data.lanes || [];
  const wrap = document.createElement('div');
  wrap.className = 'panel-fade-in';
  for (const lane of lanes) {
    wrap.append(buildLaneBlock(lane));
  }
  const nodeLane = buildNodeLane();
  if (nodeLane) wrap.append(nodeLane);

  if (!wrap.childElementCount) {
    panelMessage(panel, 'Nothing here yet — libraries appear once this node friends others on '
      + '<a href="/admin/network">Admin › Network</a>.');
    return;
  }
  panel.replaceChildren(wrap);
  repaintHearts();
  const cur = controller?.current?.();
  if (cur && cur.track) highlightPlayingRow(cur.track, controller.paused);
}

// laneHead builds a lane's heading plus its trailing "See all", which is either
// a link off the page or the lane's own full view.
function laneHead(name, title, more) {
  const head = document.createElement('div');
  head.className = 'mn-lane-head';
  head.append(mkSpan('mn-lane-title', title));
  const href = LANE_SEE_ALL_HREF[name];
  if (href) {
    const link = document.createElement('a');
    link.className = 'mn-lane-more';
    link.href = href;
    link.textContent = 'See all →';
    head.append(link);
  } else if (more) {
    const btn = document.createElement('button');
    btn.className = 'mn-lane-more';
    btn.textContent = 'See all →';
    btn.addEventListener('click', () => showLane(name));
    head.append(btn);
  }
  return head;
}

// buildLaneBlock renders one lane's digest: a heading, up to eight ordinary
// track rows, and "See all" when there is more behind it.
function buildLaneBlock(lane) {
  const box = document.createElement('section');
  box.className = 'mn-lane';
  box.append(laneHead(lane.name, lane.title || laneTitle(lane.name), lane.more));

  const rows = document.createElement('div');
  rows.className = 'mn-lane-rows';
  const tracks = lane.tracks || [];
  // One queue per lane: playing a lane row continues down that lane, which is
  // the only continuation that makes sense for a list assembled by a ranking.
  const { queue, qIndex } = buildQueue(tracks, t => t.group_artist, t => t.album);
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

// buildNodeLane is "Nodes": the first few libraries we hold a catalog from, our
// own included, in the order every node list uses (hops, then the alphabet —
// docs/ui/madnetwork-nodes.md §Ordering). Each row opens that node's page.
//
// Its "See all" is always offered, unlike a track lane's: the directory is not
// the tail of a ranking but a different surface, carrying keys, freshness and a
// filter that a digest row has no room for.
function buildNodeLane() {
  if (!nodes.length) return null;
  const box = document.createElement('section');
  box.className = 'mn-lane mn-lane--nodes';
  const head = document.createElement('div');
  head.className = 'mn-lane-head';
  head.append(mkSpan('mn-lane-title', 'Nodes'));
  const more = document.createElement('a');
  more.className = 'mn-lane-more';
  more.href = '/madnetwork/nodes';
  more.textContent = 'See all →';
  head.append(more);
  box.append(head);

  const rows = document.createElement('div');
  rows.className = 'mn-lane-rows';
  for (const n of nodes.slice(0, NODE_DIGEST)) rows.append(buildNodeRow(n));
  box.append(rows);
  return box;
}

// ── One lane, in full ─────────────────────────────────────────────────────────

// showLane is "See all": the same ranking without the digest's per-source cap,
// paged. The tail of a lane is reachable here and through search — a lane ranks,
// it never hides.
async function showLane(name, offset = 0) {
  destroyShelf();
  level = 'lane';
  laneName = name;
  renderBreadcrumb();
  const panel = document.getElementById('mnPanel');
  panel.innerHTML = '<div class="panel-loading" aria-live="polite" role="status"></div>';

  let data;
  try {
    data = await fetchJSON(`${API}/api/madnetwork/lane?name=${encodeURIComponent(name)}&offset=${offset}`);
  } catch { panelMessage(panel, 'Could not load this lane.'); return; }
  if (level !== 'lane' || laneName !== name) return;

  const lane = data.lane || {};
  const tracks = lane.tracks || [];
  if (!tracks.length) { panelMessage(panel, 'Nothing in this lane right now.'); return; }

  const wrap = document.createElement('div');
  wrap.className = 'panel-fade-in';
  const { queue, qIndex } = buildQueue(tracks, t => t.group_artist, t => t.album);
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
