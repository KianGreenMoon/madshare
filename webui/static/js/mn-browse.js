// mn-browse.js — the merged-catalog browse machinery, shared by every page that
// renders madnetwork content: the landing view's lanes (/madnetwork), the
// A→Z drill-down behind "Browse all", and a single node's shelf
// (/madnetwork/node/<key>, docs/ui/madnetwork-nodes.md).
//
// It exists because a node page IS the shelf that used to be a mode of the
// landing page. Two pages rendering the same rows from two code paths is how a
// heart works in one place and not the other, so the row anatomy, the ⓘ
// source/versions panel and Materialize live here once and both pages import
// them.
//
// The drill itself is a FACTORY (createShelf) rather than module state: two
// shelves never coexist today, but a shelf is now tied to a page rather than to
// this module, and module-level `drill` was exactly what made the old
// single-node view impossible to give an address to.
import { getIdentity } from './auth.js';
import { getController } from './player-controller.js';
import { trackKey } from './favorites.js';
import { fmtTime } from './player.js';
import { showToast } from './shell.js';
import { quickAddItems } from './quick-add.js';
import {
  buildArtistRow, buildAlbumRow, buildTrackRow, mkDiscHeader,
  highlightPlayingRow, repaintHearts,
} from './browse-rows.js';
import { createVirtualList } from './virtual-list.js';

export const API = document.querySelector('meta[name="api-url"]')?.content || '';

// One page of Browse all. The merged catalog is a whole community's output, so
// the artist list is keyset-paged and windowed like the library's.
const ARTIST_PAGE_SIZE = 80;

// ── Small shared helpers ─────────────────────────────────────────────────────

export function mkSpan(cls, text) {
  const s = document.createElement('span');
  s.className = cls;
  s.textContent = text;
  return s;
}

export function fmtAgo(unix) {
  if (!unix) return 'never';
  const s = Math.floor(Date.now() / 1000) - unix;
  if (s < 90) return 'just now';
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return new Date(unix * 1000).toLocaleDateString(undefined, { dateStyle: 'medium' });
}

export function fmtDur(s) {
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

export async function fetchJSON(url) {
  const res = await fetch(url);
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  return res.json();
}

export function panelMessage(panel, html) {
  if (panel) panel.innerHTML = `<div class="panel-fade-in"><div class="panel-empty">${html}</div></div>`;
}

// canMaterialize mirrors the server gate on POST /api/madnetwork/download —
// UX only, the API still enforces it.
export function canMaterialize() {
  return !!getIdentity()?.permissions?.includes('file.upload');
}

// canSeeNetwork gates the holder → map links: /admin/network is admin ground and
// linking a user somewhere they will only be refused is worse than not linking.
// UX only — the admin API enforces the permission.
export function canSeeNetwork() {
  return !!getIdentity()?.permissions?.includes('federation.manage');
}

// mnKey is the appearance identity of a merged madnetwork track — the
// artist/album/title text, so the same audio under another album is a distinct
// row (click restarts, not pauses).
export function mnKey(artist, album, title) {
  return `mn:${artist || ''}␟${album || ''}␟${(title || '').toLowerCase()}`;
}

// nodeHref is the one address of a node: its public key. Everything that names a
// node on any madnetwork page links here (docs/ui/madnetwork-nodes.md §Why a
// node needs an address).
export function nodeHref(key) {
  return `/madnetwork/node/${encodeURIComponent(key)}`;
}

// ── Tracks → the play queue ──────────────────────────────────────────────────

// queueTrackOf maps a merged track to a controller queue object — the default
// pick is the most-held version's ladder-best rendition; a self-held version
// plays its direct local URL. Null when nothing is playable.
export function queueTrackOf(t, artist, album) {
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
    album: album || '',
    // The row's elected cover, for the OS media widget — the played FILE may
    // carry no embedded art at all (sidecar-cover libraries), and the widget
    // is exactly where that gap shows.
    artUrl: t.cover_hash ? `${API}/api/madnetwork/cover/${encodeURIComponent(t.cover_hash)}?size=medium` : null,
    dur: fmtDur(t.duration) || '—',
  };
}

// buildQueue turns a list of merged tracks into one play queue plus the index
// that maps a row to its slot, so prev/next stays tight when some rows are not
// playable.
export function buildQueue(tracks, artistOf, albumOf) {
  const queue = [];
  const qIndex = new Map();
  tracks.forEach((t, i) => {
    const qt = queueTrackOf(t, artistOf(t), albumOf(t));
    if (qt) {
      qIndex.set(i, queue.length);
      queue.push(qt);
    }
  });
  return { queue, qIndex };
}

// selfHeld reports whether this node already publishes the track (any version
// backed by the local library) — Materialize is then pointless and omitted.
export function selfHeld(t) {
  return (t.versions || []).some(v => v.url || (v.holders || []).some(h => h.self));
}

// ── Track rows ───────────────────────────────────────────────────────────────

// appendTrackRow renders one merged track through the shared row builder, then
// adds the madnetwork extras: the ⓘ source/versions toggle and the Materialize
// menu item. `queue` is the list's play queue and `qi` this track's slot in it
// (undefined when the track has no playable rendition).
//
// opts carries what a lane row needs and an album row does not: its own artist
// and album (a lane crosses both), the meta line that spells them out, and the
// `note` explaining why the row is in this lane at all.
export function appendTrackRow(wrap, t, i, queue, qi, opts = {}) {
  const playable = qi != null;
  const qt = playable ? queue[qi] : null;
  const artist = opts.artist;
  const album = opts.album;
  const controller = getController();

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
    if (detail.hidden) renderVersions(detail, t, artist);
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

function renderVersions(detail, t, artist) {
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
      // A holder with a key links to ITS PAGE — the node's own address, where
      // its shelf and its facts are. Tracking down whoever served something
      // starts from the content that exposed it (docs/ui/madnetwork-nodes.md);
      // the admin map is one further click, from the node page itself.
      const linked = !h.self && h.key;
      const holder = linked
        ? Object.assign(document.createElement('a'), {
            className: 'mn-holder mn-holder--linked',
            href: nodeHref(h.key),
            textContent: h.name || '(unnamed)',
          })
        : mkSpan('mn-holder', h.name || '(unnamed)');
      holder.title = h.self ? 'this server'
        : `last seen ${fmtAgo(h.last_seen)}` + (stale ? ' · not seen recently' : '')
          + (linked ? ' · open this node' : '');
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
      box.append(mkVersionActions(t, v, best, artist));
    }
    detail.append(box);
  });
}

// ── Version actions: play (relay or local) + materialize into the library ────

function mkVersionActions(t, v, rd, artist) {
  const bar = document.createElement('div');
  bar.className = 'mn-actions';

  const track = {
    url: v.url ? `${API}${v.url}` : `${API}/api/madnetwork/stream/${rd.hash}`,
    tagsetId: t.tagset_id || null,
    title: t.title || 'Unknown',
    artist: t.artist || t.group_artist || artist || '',
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

// In-flight materialize polls, keyed by hash (survive within a visit; cleared on
// teardown — the server job keeps running and the state is re-pollable).
const materializePolls = new Map();

export function clearMaterializePolls() {
  for (const timer of materializePolls.values()) clearTimeout(timer);
  materializePolls.clear();
}

// startMaterialize kicks off the fetch-into-library flow. With a `btn` (the ⓘ
// panel's version action) the button carries the progress; from the ⋯ menu
// (no surviving element) progress is toast-only.
export async function startMaterialize(hash, { btn, title } = {}) {
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

// stopBulkWatch drops the watcher on page teardown; server-side transfers finish
// on their own.
export function stopBulkWatch() { bulk = null; }

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
async function collectMaterializeTargets(artist, album, qs) {
  const targets = { pending: [], local: 0 };
  const scan = async al => {
    const data = await fetchJSON(`${API}/api/madnetwork/tracks?artist=${encodeURIComponent(artist)}&album=${encodeURIComponent(al)}${qs}`);
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
    const data = await fetchJSON(`${API}/api/madnetwork/albums?artist=${encodeURIComponent(artist)}${qs}`);
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
// artist (album null), within the shelf's source scope.
export async function materializeAll(artist, album, qs = '') {
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
    targets = await collectMaterializeTargets(artist, album, qs);
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

// materializeAllItems yields the entity ⋯ menus' trailing "Materialize all"
// item (album given = that album; null = the whole artist).
function materializeAllItems(artist, album, qs) {
  if (!canMaterialize()) return [];
  return [{ label: 'Materialize all', onClick: () => materializeAll(artist, album, qs) }];
}

// ── The shelf: artists → albums → tracks over one source (or the merge) ──────

// createShelf builds a drill-down over the merged catalog, optionally restricted
// to one node (`source`, a node key or "self"). The caller owns the DOM: it
// passes the panel to render into and the trail element to write a breadcrumb
// to, so the same shelf works inside the landing page and as the whole body of
// a node page.
//
//   panel      element the views render into
//   trail      element the breadcrumb writes to (optional)
//   actions    element the view's bar actions write to (optional)
//   source     node key | "self" | null (the merged catalog)
//   rootLabel  what the shelf's own root step is called
//   onExit     called when the breadcrumb's step ABOVE the root is clicked
//              (null = no step above: the shelf is the whole page)
//   exitHref   that step as a LINK instead — used when leaving the shelf means
//              leaving the page, so the step behaves like the navigation it is
//              (middle-click, copy link); the shell router intercepts it
export function createShelf({ panel, trail, actions, source = null, rootLabel = 'Browse all', onExit = null, exitHref = null, exitLabel = 'Madnetwork' }) {
  // level: 'artists' | 'albums' | 'tracks'
  let level = 'artists', artist = null, album = null;
  let vlist = null;
  let alive = true;
  const qs = source ? `&source=${encodeURIComponent(source)}` : '';
  const controller = getController();

  const destroyVList = () => { if (vlist) { vlist.destroy(); vlist = null; } };

  function renderTrail() {
    if (!trail) return;
    trail.replaceChildren();
    const mkLink = (label, handler) => {
      const btn = document.createElement('button');
      btn.className = 'bc-item bc-link';
      btn.textContent = label;
      btn.addEventListener('click', handler);
      return btn;
    };
    const mkSep = () => mkSpan('bc-sep', '›');
    const mkCurrent = label => mkSpan('bc-item bc-current', label);

    const mkExit = () => {
      const a = document.createElement('a');
      a.className = 'bc-item bc-link';
      a.href = exitHref;
      a.textContent = exitLabel;
      return a;
    };

    const steps = [];
    if (exitHref) steps.push(mkExit());
    else if (onExit) steps.push(mkLink(exitLabel, onExit));
    if (level === 'artists') steps.push(mkCurrent(rootLabel));
    else steps.push(mkLink(rootLabel, () => showArtists()));
    if (level === 'albums') steps.push(mkCurrent(artist));
    if (level === 'tracks') {
      steps.push(mkLink(artist, () => showAlbums(artist)), mkCurrent(album));
    }
    steps.forEach((el, i) => {
      if (i) trail.append(mkSep());
      trail.append(el);
    });
    renderActions();
  }

  // The tracks view carries the visible "Materialize all" — an album-level bulk
  // action shouldn't hide in a menu.
  function renderActions() {
    if (!actions) return;
    actions.replaceChildren();
    if (level !== 'tracks' || !canMaterialize()) return;
    const btn = document.createElement('button');
    btn.className = 'btn btn-neutral mn-bulk-btn';
    btn.textContent = '⬇ Materialize all';
    btn.title = 'Fetch every track of this album into this server’s library';
    btn.addEventListener('click', () => materializeAll(artist, album, qs));
    actions.append(btn);
  }

  // entityTracks collects the playable queue tracks of a whole album — or a
  // whole artist (album == null → every album, in browse order). Feeds the
  // artist/album "⋯" quick-add menus.
  async function entityTracks(a, al) {
    const fetchAlbum = async name => {
      const data = await fetchJSON(`${API}/api/madnetwork/tracks?artist=${encodeURIComponent(a)}&album=${encodeURIComponent(name)}${qs}`);
      return (data.tracks || []).map(t => queueTrackOf(t, a, name)).filter(Boolean);
    };
    if (al != null) return fetchAlbum(al);
    const data = await fetchJSON(`${API}/api/madnetwork/albums?artist=${encodeURIComponent(a)}${qs}`);
    const lists = await Promise.all((data.albums || []).map(x => fetchAlbum(x.title)));
    return lists.flat();
  }

  function spacerDiv(px) {
    const d = document.createElement('div');
    d.style.height = `${Math.max(0, px)}px`;
    d.setAttribute('aria-hidden', 'true');
    return d;
  }

  async function showArtists() {
    level = 'artists'; artist = null; album = null;
    destroyVList();
    renderTrail();
    panel.innerHTML = '<div class="panel-loading" aria-live="polite" role="status"></div>';

    let page;
    try {
      page = await fetchJSON(`${API}/api/madnetwork/artists?limit=${ARTIST_PAGE_SIZE}${qs}`);
    } catch { panelMessage(panel, 'Could not load the madnetwork catalog.'); return; }
    if (!alive || level !== 'artists') return; // user drilled on meanwhile

    const artists = page.artists || [];
    if (!artists.length) {
      panelMessage(panel, source
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
    vlist = createVirtualList({
      sizerEl: wrap,
      windowScroll: true,
      makeSpacer: spacerDiv,
      renderRow: a => buildArtistRow({
        name: a.name,
        meta: `${a.albums} album${a.albums === 1 ? '' : 's'} · ${a.tracks} track${a.tracks === 1 ? '' : 's'}`,
        onOpen: () => showAlbums(a.name),
        makeMenuItems: btn => quickAddItems(btn, () => entityTracks(a.name, null),
          { extraItems: materializeAllItems(a.name, null, qs) }),
      }),
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
    vlist.setItems(artists);
  }

  async function showAlbums(a) {
    level = 'albums'; artist = a; album = null;
    destroyVList();
    renderTrail();
    let data;
    try {
      data = await fetchJSON(`${API}/api/madnetwork/albums?artist=${encodeURIComponent(a)}${qs}`);
    } catch { panelMessage(panel, 'Could not load albums.'); return; }
    if (!alive || level !== 'albums' || artist !== a) return;

    const albums = data.albums || [];
    if (!albums.length) { panelMessage(panel, 'No albums found.'); return; }
    const wrap = document.createElement('div');
    wrap.className = 'panel-fade-in';
    for (const al of albums) {
      const yearPrefix = al.year ? `${al.year} · ` : '';
      wrap.append(buildAlbumRow({
        title: al.title,
        meta: `${yearPrefix}${al.tracks} track${al.tracks === 1 ? '' : 's'}`,
        // The elected cover (covers-federation M4), relayed by this server
        // from whichever node holds it. Hash-addressed and immutable, so the
        // browser caches it as hard as a local variant.
        artUrl: al.cover_hash ? `${API}/api/madnetwork/cover/${encodeURIComponent(al.cover_hash)}?size=small` : null,
        onOpen: () => showTracks(a, al.title),
        makeMenuItems: btn => quickAddItems(btn, () => entityTracks(a, al.title),
          { extraItems: materializeAllItems(a, al.title, qs) }),
      }));
    }
    panel.replaceChildren(wrap);
  }

  async function showTracks(a, al) {
    level = 'tracks'; artist = a; album = al;
    destroyVList();
    renderTrail();
    let data;
    try {
      data = await fetchJSON(`${API}/api/madnetwork/tracks?artist=${encodeURIComponent(a)}&album=${encodeURIComponent(al)}${qs}`);
    } catch { panelMessage(panel, 'Could not load tracks.'); return; }
    if (!alive || level !== 'tracks' || album !== al) return;

    const tracks = data.tracks || [];
    if (!tracks.length) { panelMessage(panel, 'No tracks found.'); return; }

    const { queue, qIndex } = buildQueue(tracks, () => a, () => al);
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
      appendTrackRow(wrap, t, i, queue, qIndex.get(i), { artist: a, album: al });
    });
    panel.replaceChildren(wrap);

    // Re-highlight whatever is playing if its row is in this view.
    const cur = controller?.current?.();
    if (cur && cur.track) highlightPlayingRow(cur.track, controller.paused);
    repaintHearts();
  }

  return {
    showArtists, showAlbums, showTracks,
    get level() { return level; },
    destroy() { alive = false; destroyVList(); },
  };
}
