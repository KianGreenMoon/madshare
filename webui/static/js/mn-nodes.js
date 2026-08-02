// mn-nodes.js — the vocabulary and the rows every node surface shares: the
// Nodes lane on /madnetwork, the directory at /madnetwork/nodes, and the card at
// the top of /madnetwork/node/<key> (docs/ui/madnetwork-nodes.md).
//
// One module because the three surfaces must agree about the same node. A row
// that says "2 hops · member" in a lane and "direct friend" on the node's own
// page would be two answers about one node, and the reader has no way to tell
// which is the real one.
//
// Names are the weak fact here and the key is the strong one: beyond our own
// friends a name is what a node claims about itself, relayed. That is why every
// row shows the key beside the name and why the key — never the source id — is
// what a node page is addressed by.
import { API, fmtAgo, mkSpan, fetchJSON, canSeeNetwork, nodeHref } from './mn-browse.js';
import { esc } from './browse-rows.js';

export { nodeHref };

// fetchNodes returns every node this server holds a catalog from, our own
// included, already in display order (hops, then the alphabet — the server sorts
// so that no surface can sort differently).
export async function fetchNodes() {
  const data = await fetchJSON(`${API}/api/madnetwork/summary`);
  return {
    nodes: data.nodes || [],
    tracks: data.tracks || 0,
    inboundHealthy: data.inbound_healthy !== false,
  };
}

export function nodeName(n) {
  return n.name || '(unnamed)';
}

// shortKey is the key as a row shows it. Enough to tell two nodes apart at a
// glance; the full value is on the node's own page, where it can be copied.
export function shortKey(key) {
  return key ? `${key.slice(0, 8)}…` : '';
}

// hopsText spells out the distance the lists are ordered by. "Distance unknown"
// is a real answer, not a placeholder: we hold this node's catalog but no chain
// of friendships has placed it yet, which is ordinary while the graph fills in.
export function hopsText(n) {
  if (n.self) return 'this server';
  if (n.hops == null) return 'distance unknown';
  return n.hops === 1 ? '1 hop' : `${n.hops} hops`;
}

// classText is how this node came to be in our view: a decision an admin made
// here, or the community's own shape.
export function classText(n) {
  if (n.self) return 'this server';
  return n.friend ? 'direct friend' : 'member';
}

// freshnessText is the availability vocabulary, unchanged from the chips it
// replaces: when we last had contact, and when its catalog was last pulled.
export function freshnessText(n) {
  if (n.self) return 'always available';
  const bits = [`seen ${fmtAgo(n.last_seen)}`];
  if (n.synced_at) bits.push(`synced ${fmtAgo(n.synced_at)}`);
  return bits.join(' · ');
}

export function entriesText(n) {
  const count = n.entries || 0;
  return `${count.toLocaleString()} ${count === 1 ? 'entry' : 'entries'}`;
}

// buildNodeRow is one node in a list: an anchor, so a node can be opened in a
// tab and its link copied — the whole point of giving a node an address.
export function buildNodeRow(n) {
  const row = document.createElement('a');
  row.className = 'panel-row artist-row mn-node-row';
  row.href = nodeHref(n.key);
  row.setAttribute('aria-label', `Open node ${nodeName(n)}`);
  if (n.self) row.classList.add('mn-node-row--self');
  if (!n.self && n.reachable === false) row.classList.add('mn-node--stale');
  row.innerHTML =
    `<span class="row-name">${esc(nodeName(n))}` +
      `<span class="mn-node-key">${esc(shortKey(n.key))}</span></span>` +
    `<span class="row-meta">${esc([hopsText(n), classText(n), entriesText(n), freshnessText(n)]
      .filter((v, i, a) => v && a.indexOf(v) === i).join(' · '))}</span>` +
    `<span class="row-chevron" aria-hidden="true">›</span>`;
  return row;
}

// buildNodeCard is the header of a node's own page: the same facts with room to
// breathe, the full key with a copy control, and — for an admin only — the way
// on to the network map.
export function buildNodeCard(n, { onCopy } = {}) {
  const card = document.createElement('section');
  card.className = 'mn-node-card';
  if (n.self) card.classList.add('mn-node-card--self');
  if (!n.self && n.reachable === false) card.classList.add('mn-node--stale');

  const head = document.createElement('div');
  head.className = 'mn-node-card-head';
  head.append(mkSpan('mn-node-card-name', nodeName(n)));
  const chips = document.createElement('div');
  chips.className = 'mn-node-chips';
  chips.append(mkSpan('mn-node-chip', classText(n)));
  if (!n.self) chips.append(mkSpan('mn-node-chip', hopsText(n)));
  head.append(chips);
  card.append(head);

  const keyLine = document.createElement('div');
  keyLine.className = 'mn-node-keyline';
  const key = mkSpan('mn-node-fullkey', n.key || '(no key)');
  key.title = 'This node’s public key — its identity, and this page’s address';
  keyLine.append(key);
  if (n.key && navigator.clipboard) {
    const copy = document.createElement('button');
    copy.type = 'button';
    copy.className = 'mn-node-copy';
    copy.textContent = '⧉';
    copy.title = 'Copy the node key';
    copy.setAttribute('aria-label', 'Copy the node key');
    copy.addEventListener('click', async () => {
      try {
        await navigator.clipboard.writeText(n.key);
        onCopy?.(true);
      } catch { onCopy?.(false); }
    });
    keyLine.append(copy);
  }
  card.append(keyLine);

  const facts = mkSpan('mn-node-facts', `${entriesText(n)} · ${freshnessText(n)}`);
  card.append(facts);

  // The map is admin ground, so the link is offered only to an admin — the same
  // rule the holder links follow. It is the second click, not the first: the
  // node page answers "what does it have", the map answers "how does it reach
  // us", and only one of those is an admin question.
  if (!n.self && n.key && canSeeNetwork()) {
    const map = document.createElement('a');
    map.className = 'mn-node-maplink';
    map.href = `/admin/network#node=${encodeURIComponent(n.key)}`;
    map.target = '_blank';
    map.rel = 'noopener';
    map.textContent = 'open on the network map →';
    card.append(map);
  }
  return card;
}
