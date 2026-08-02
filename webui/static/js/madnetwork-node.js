// madnetwork-node.js — one node's page at /madnetwork/node/<key>
// (docs/ui/madnetwork-nodes.md §One node).
//
// The page IS the shelf that used to be a mode of /madnetwork, with the node's
// facts above it. That is the whole point: a shelf held in a JS variable could
// not be linked, and the id it was keyed by is a local row number the discovery
// rotation recycles. The key in this URL is the node's identity everywhere.
//
// Shell page module: NO page DOM at module-eval time — everything inside init().
import { gatePage, PAGE_PERMS } from './auth.js';
import { getController } from './player-controller.js';
import { showToast } from './shell.js';
import { createBrowseSearch } from './browse-search.js';
import { highlightPlayingRow, reflectPlayStateRows } from './browse-rows.js';
import {
  API, fetchJSON, mkSpan, mnKey, createShelf,
  clearMaterializePolls, stopBulkWatch,
} from './mn-browse.js';
import { buildNodeCard, nodeName } from './mn-nodes.js';

let abort = null;
let shelf = null;
let search = null;
let controller = null;
let unsubTrackChange = null;
let unsubPlayState = null;

// keyFromPath reads the node key out of the URL. It is the page's only input —
// the server renders the same document for every node.
function keyFromPath() {
  const m = location.pathname.match(/\/madnetwork\/node\/([^/]+)/);
  return m ? decodeURIComponent(m[1]) : '';
}

export async function init() {
  if (!gatePage(PAGE_PERMS.madnetwork)) return;
  abort = new AbortController();

  controller = getController();
  unsubTrackChange = controller.on('trackchange', t => highlightPlayingRow(t, controller.paused));
  unsubPlayState = controller.on('playstate', reflectPlayStateRows);

  const key = keyFromPath();
  let data;
  try {
    data = await fetchJSON(`${API}/api/madnetwork/nodes/${encodeURIComponent(key)}`);
  } catch {
    // The three failure answers are the server's and they mean different
    // things; a fetch that never got one is our own problem.
    renderMissing(key);
    return;
  }
  if (!abort || abort.signal.aborted) return;

  const node = data.node || {};
  renderCard(node);
  document.title = `${nodeName(node)} — Madshare`;

  if (data.no_catalog) {
    // Placeable on the graph, never pulled from: the shelf is empty because
    // nothing has been fetched, not because the node offers nothing. Saying so
    // is the difference between "wait" and "there is nothing here".
    document.getElementById('mnPanel').innerHTML =
      '<div class="panel-fade-in"><div class="panel-empty">This server holds no catalog from this node yet. '
      + 'It appears in the community, and the next discovery round may fetch its library.</div></div>';
    return;
  }

  mountShelf(node);
}

export function teardown() {
  abort?.abort();
  abort = null;
  shelf?.destroy();
  shelf = null;
  search = null;
  stopBulkWatch();
  clearMaterializePolls();
  if (unsubTrackChange) { unsubTrackChange(); unsubTrackChange = null; }
  if (unsubPlayState) { unsubPlayState(); unsubPlayState = null; }
  controller = null;
}

function renderCard(node) {
  const box = document.getElementById('mnNodeCard');
  if (!box) return;
  box.replaceChildren(buildNodeCard(node, {
    onCopy: ok => showToast(ok ? 'Node key copied.' : 'Could not copy the key.',
      { type: ok ? 'success' : 'error' }),
  }));
}

// renderMissing is the answer for a key nothing in our view knows, and for one
// that is not a key at all. It echoes what was asked for: someone who arrived
// here from a link needs to be able to compare it.
function renderMissing(key) {
  const box = document.getElementById('mnNodeCard');
  if (box) {
    const card = document.createElement('section');
    card.className = 'mn-node-card mn-node-card--missing';
    card.append(mkSpan('mn-node-card-name', 'This node is not in view'));
    card.append(mkSpan('mn-node-fullkey', key || '(no key in the address)'));
    card.append(mkSpan('mn-node-facts',
      'No catalog, and no chain of friendships from here reaches it.'));
    box.replaceChildren(card);
  }
  const panel = document.getElementById('mnPanel');
  if (panel) panel.replaceChildren();
}

function mountShelf(node) {
  const source = node.self ? 'self' : node.key;
  shelf = createShelf({
    panel: document.getElementById('mnPanel'),
    trail: document.getElementById('mnBreadcrumb'),
    actions: document.getElementById('mnBarActions'),
    source,
    rootLabel: nodeName(node),
    exitHref: '/madnetwork/nodes',
    exitLabel: 'Nodes',
  });

  // Search is scoped to this node — the same shared search view as the library
  // and the merged catalog, pointed at one shelf. On a node page "search"
  // meaning "search everything" would be a trapdoor out of the page.
  search = createBrowseSearch({
    signal: abort.signal,
    fetchResults: async (q, fetchSignal) => {
      const res = await fetch(
        `${API}/api/madnetwork/search?q=${encodeURIComponent(q)}&source=${encodeURIComponent(source)}`,
        { signal: fetchSignal });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      return res.json();
    },
    onOpenArtist: a => shelf.showAlbums(a.name),
    onOpenAlbum: a => shelf.showTracks(a.artist_name, a.title),
    albumArtUrl: () => null, // the merged catalog carries no cover images
    buildQueueTrack: t => ({
      url: t.url ? `${API}${t.url}` : `${API}/api/madnetwork/stream/${t.hash}`,
      tagsetId: t.tagset_id || null,
      remoteLike: t.tagset_id ? null : {
        hash: t.hash, title: t.title || '', artist: t.artist_name || t.artist || '', album: t.album_title || '',
      },
      rowKey: mnKey(t.artist, t.album_title, t.title),
      title: t.title || 'Unknown',
      artist: t.artist_name || t.artist || '',
    }),
  });

  shelf.showArtists();
}
