// Admin · Network — the madnetwork friendship page (federation F1). Shows this
// node's identity + exportable node card, imports friends' cards, and manages
// the trusted-peer list (accept / block / unblock / remove, local label, local
// user mapping). Gated on federation.manage (the API enforces it). Pairing is
// asynchronous — the page polls while anything is pending so state flips appear
// without a reload. Design: docs/architecture/federation.md.
import { bootAdmin, API, toast, handleAuthError, el } from './shared.js';
import { initMap, loadMap } from './network-map.js';

const disabledNote = document.getElementById('disabledNote');
const selfPanel    = document.getElementById('selfPanel');
const selfName     = document.getElementById('selfName');
const selfAddr     = document.getElementById('selfAddr');
const selfKey      = document.getElementById('selfKey');
const importForm   = document.getElementById('importForm');
const cardInput    = document.getElementById('cardInput');
const importBtn    = document.getElementById('importBtn');
const peersHeading = document.getElementById('peersHeading');
const peersList    = document.getElementById('peersList');

const cfModal   = document.getElementById('cfModal');
const cfTitle   = document.getElementById('cfModalTitle');
const cfBody    = document.getElementById('cfModalBody');
const cfConfirm = document.getElementById('cfConfirm');
const cfCancel  = document.getElementById('cfCancel');
const cfClose   = document.getElementById('cfClose');

const POLL_MS = 5000;
let pollTimer = null;
let ownCard = null;
let users = null;          // [{id, username}] or null when not loadable (no user.manage)
let lastPeersJSON = '';    // skip re-render (and select clobbering) when nothing changed
let reports = [];          // contradicted-claim findings, rendered on the peer they came from (F6)

importForm.addEventListener('submit', onImport);
document.getElementById('copyAddr').addEventListener('click', () => copyText(selfAddr.textContent, 'Mesh address copied.'));
document.getElementById('copyKey').addEventListener('click', () => copyText(selfKey.textContent, 'Public key copied.'));
document.getElementById('copyCard').addEventListener('click', () => copyText(JSON.stringify(ownCard, null, 2), 'Node card copied — send it to your friend.'));
document.getElementById('downloadCard').addEventListener('click', downloadCard);

// ── Load + poll ───────────────────────────────────────────────────────────────

async function loadStatus() {
  let data;
  try {
    const res = await fetch(`${API}/api/admin/federation`);
    if (handleAuthError(res)) return false;
    data = await res.json().catch(() => ({}));
    if (!res.ok) { toast(data.error || `Could not load federation status (HTTP ${res.status}).`, 'error'); return false; }
  } catch (err) { toast(`Could not load federation status: ${err.message}`, 'error'); return false; }

  if (!data.enabled) {
    disabledNote.hidden = false;
    return false;
  }
  ownCard = data.node.card;
  selfName.textContent = data.node.name || '(unnamed)';
  selfAddr.textContent = data.node.address;
  selfKey.textContent  = data.node.public_key;
  const inboundWarn = document.getElementById('inboundWarn');
  if (inboundWarn) {
    if (data.inbound_healthy === false) {
      inboundWarn.textContent = '⚠ This node’s inbound mesh path looks down — friends are unreachable until it recovers or the node is restarted (issue #398). The madnetwork browse is failing open (showing last-known catalogs).';
      inboundWarn.hidden = false;
    } else {
      inboundWarn.hidden = true;
    }
  }
  selfPanel.hidden = false;
  importForm.hidden = false;
  peersHeading.hidden = false;
  return true;
}

async function loadPeers() {
  let data;
  try {
    const res = await fetch(`${API}/api/admin/federation/peers`);
    if (handleAuthError(res)) { stopPolling(); return; }
    data = await res.json().catch(() => ({}));
    if (!res.ok) return; // transient — retry next poll
  } catch { return; }
  const peers = data.peers || [];
  await loadReports();
  const json = JSON.stringify(peers) + JSON.stringify(reports);
  if (json !== lastPeersJSON) {
    lastPeersJSON = json;
    renderPeers(peers);
  }
  // Keep watching while a pairing can flip on its own (our retry loop, or the
  // friend's admin acting); a settled list needs no polling.
  if (peers.some(p => p.state === 'pending_outgoing' || p.state === 'pending_incoming')) startPolling();
  else stopPolling();
}

function startPolling() { if (!pollTimer) pollTimer = setInterval(loadPeers, POLL_MS); }
function stopPolling()  { if (pollTimer) { clearInterval(pollTimer); pollTimer = null; } }

// Contradicted identity claims (federation F6): findings this node made by
// checking a peer's advertised fingerprints against bytes it holds itself. They
// render on the peer card they belong to, beside the Block action.
//
// Nothing here is automatic and the wording must keep it that way: a finding is a
// CONTRADICTION with innocent explanations (a different chromaprint build, a peer
// that grouped a rendition wrongly, a relay repeating someone else's claim), never
// an accusation the UI has already accepted.
async function loadReports() {
  try {
    const res = await fetch(`${API}/api/admin/federation/reports`);
    if (!res.ok) { reports = []; return; }
    const data = await res.json().catch(() => ({}));
    reports = data.reports || [];
  } catch { reports = []; }
}

const short = (h, n = 10) => (h && h.length > n ? `${h.slice(0, n)}…` : (h || '—'));

function claimHeadline(r) {
  const pct = `${(r.ber * 100).toFixed(0)}% of bits differ`;
  if (r.kind === 'grouping') {
    return `Claims ${short(r.hash)} and ${short(r.other_hash)} are the same recording, `
      + `but our own fingerprints of those two blobs disagree (${pct}).`;
  }
  return `Advertises ${short(r.hash)} with a fingerprint that does not match our own copy `
    + `of those exact bytes (${pct}).`;
}

// The measurement and its provenance — what was compared, over how much, and how
// each side was obtained. A version difference is called out because it is the
// most common innocent explanation: chromaprint output is build-sensitive.
function claimEvidence(r) {
  const parts = [`compared ${r.words} fingerprint words`];
  if (r.kind === 'grouping') {
    parts.push('both fingerprints computed here');
  } else {
    parts.push(`ours ${short(r.our_head, 12)} vs advertised ${short(r.their_head, 12)}`);
  }
  if (r.our_version || r.their_version) {
    const same = r.our_version === r.their_version;
    parts.push(same
      ? `both fingerprinted by ${r.our_version || 'an unknown build'}`
      : `different fingerprinter builds (ours ${r.our_version || '?'}, theirs ${r.their_version || '?'})`);
  }
  return parts.join(' · ');
}

// Findings are matched by KEY, not by peer id: since F7 item 5 a report belongs
// to a cached catalog, and most of those come from members with no peer row at
// all. renderOrphanClaims below is where those land.
function renderClaims(p) {
  return claimBox(reports.filter(r => r.peer_key === p.public_key));
}

function claimBox(mine) {
  if (!mine.length) return null;
  const box = el('div', { class: 'peer-claims' }, [
    el('div', { class: 'peer-claims-head' }, [
      `⚠ ${mine.length} contradicted claim${mine.length > 1 ? 's' : ''} — checked against bytes we hold. `,
      el('span', { class: 'peer-claims-note' }, ['Nothing has been acted on; blocking is always your call.']),
    ]),
  ]);
  for (const r of mine) {
    box.append(el('div', { class: 'peer-claim' }, [
      el('div', { class: 'peer-claim-what', text: claimHeadline(r) }),
      el('div', { class: 'peer-claim-evidence', text: claimEvidence(r) }),
      el('div', { class: 'peer-claim-actions' }, [
        el('button', {
          class: 'btn btn-neutral btn-sm',
          title: 'An innocent explanation, or not worth acting on — stop showing this',
          onclick: () => disposeClaim(r, 'dismissed'),
        }, ['Dismiss']),
        el('button', {
          class: 'btn btn-neutral btn-sm',
          title: 'You have taken this up with the peer, or blocked it',
          onclick: () => disposeClaim(r, 'acted'),
        }, ['Mark handled']),
      ]),
    ]));
  }
  return box;
}

async function disposeClaim(r, disposition) {
  try {
    const res = await fetch(`${API}/api/admin/federation/reports/${r.id}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ disposition }),
    });
    if (handleAuthError(res)) return;
    if (!res.ok) {
      const data = await res.json().catch(() => ({}));
      toast(data.error || `Could not update the finding (HTTP ${res.status}).`, 'error');
      return;
    }
  } catch (err) {
    toast(`Could not update the finding: ${err.message}`, 'error');
    return;
  }
  toast(disposition === 'dismissed' ? 'Finding dismissed.' : 'Finding marked handled.', 'info');
  lastPeersJSON = '';
  refresh();
}

// Users for the mapping dropdown; needs user.manage — degrade to plain text
// without it.
async function loadUsers() {
  try {
    const res = await fetch(`${API}/api/admin/users`);
    if (!res.ok) return;
    const data = await res.json().catch(() => ({}));
    users = data.users || data || null;
    if (users && !Array.isArray(users)) users = null;
  } catch { /* mapping select simply not offered */ }
}

// ── Rendering ─────────────────────────────────────────────────────────────────

const STATE_LABEL = {
  friend:           'Friend',
  pending_outgoing: 'Waiting for their side',
  pending_incoming: 'Wants to pair',
  blocked:          'Blocked',
};

function stateClass(state) {
  if (state === 'friend') return 'is-friend';
  if (state === 'blocked') return 'is-blocked';
  return 'is-pending';
}

function fmtLastSeen(unix) {
  if (!unix) return 'never seen';
  const s = Math.floor(Date.now() / 1000) - unix;
  if (s < 90) return 'seen just now';
  if (s < 3600) return `seen ${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `seen ${Math.floor(s / 3600)}h ago`;
  return 'seen ' + new Date(unix * 1000).toLocaleDateString(undefined, { dateStyle: 'medium' });
}

function renderPeers(peers) {
  const orphans = renderOrphanClaims(peers);
  if (!peers.length && !orphans) {
    peersList.replaceChildren(el('p', { class: 'net-empty', text: 'No known nodes yet — import a friend’s node card above.' }));
    return;
  }
  peersList.replaceChildren(...peers.map(renderPeer));
  if (orphans) peersList.append(orphans);
}

// Contradicted claims from nodes that are not peers of ours — members of the
// community whose catalogs this node caches since F7 item 5. They have no card
// to sit on, and leaving them out would let the dashboard count findings an
// admin cannot reach. Block by key is the same act the network map offers.
function renderOrphanClaims(peers) {
  const known = new Set(peers.map(p => p.public_key));
  const orphaned = reports.filter(r => r.peer_key && !known.has(r.peer_key));
  if (!orphaned.length) return null;
  const box = el('div', { class: 'peer-card peer-card--orphan-claims' }, [
    el('div', { class: 'peer-head' }, [
      el('span', { class: 'peer-name', text: 'Nodes you have not friended' }),
    ]),
    el('p', { class: 'peer-sub', text: 'Members of your community whose catalogs this node caches. You have made no decision about them; these are findings, not verdicts.' }),
  ]);
  const byKey = new Map();
  for (const r of orphaned) {
    if (!byKey.has(r.peer_key)) byKey.set(r.peer_key, []);
    byKey.get(r.peer_key).push(r);
  }
  for (const [key, mine] of byKey) {
    const node = { key, name: mine[0].peer_name || '' };
    const group = el('div', { class: 'peer-claim-group' }, [
      el('div', { class: 'peer-head' }, [
        el('span', { class: 'peer-name', text: node.name || key.slice(0, 12) }),
        el('code', { class: 'peer-key', text: key }),
      ]),
    ]);
    const claims = claimBox(mine);
    if (claims) group.append(claims);
    group.append(el('div', { class: 'peer-actions' }, [
      el('button', {
        class: 'btn btn-destructive-solid btn-sm',
        text: 'Block…',
        onclick: () => blockMapNode(node),
      }),
    ]));
    box.append(group);
  }
  return box;
}

function renderPeer(p) {
  const nameSpan = el('span', { class: 'peer-name', text: peerLabel(p) });
  const head = el('div', { class: 'peer-head' }, [
    nameSpan,
    el('button', { class: 'peer-rename', title: 'Rename (local label)', 'aria-label': 'Rename', onclick: () => startRename(p, nameSpan) }, ['✎']),
    el('span', { class: `peer-badge ${stateClass(p.state)}`, text: STATE_LABEL[p.state] || p.state }),
    el('span', { class: 'peer-when', text: fmtLastSeen(p.last_seen) }),
  ]);

  const meta = el('div', { class: 'peer-meta' });
  meta.append(el('span', {}, [p.address ? `mesh ${p.address}` : '']));
  // What the node calls itself, shown whenever our label is hiding it — the
  // label always wins, but it must never make the peer's own name unreadable.
  if (p.heard_name && p.name && p.heard_name !== p.name) {
    meta.append(el('span', { class: 'peer-heard', title: 'The name this node gives itself, refreshed on every contact' },
      [`calls itself “${p.heard_name}”`]));
  }
  meta.append(renderUserMapping(p));

  const actions = el('div', { class: 'peer-actions' });
  if (p.state === 'pending_incoming') {
    actions.append(el('button', { class: 'btn btn-neutral', onclick: () => acceptPeer(p) }, ['Accept…']));
  }
  if (p.state === 'blocked') {
    actions.append(el('button', { class: 'btn btn-neutral', onclick: () => unblockPeer(p) }, ['Unblock']));
  } else {
    actions.append(el('button', { class: 'btn btn-neutral', onclick: () => blockPeer(p) }, ['Block…']));
  }
  actions.append(el('button', { class: 'btn btn-destructive-outline', onclick: () => removePeer(p) }, ['Remove…']));

  const card = el('div', { class: `peer-card ${stateClass(p.state)}` }, [
    head,
    el('code', { class: 'peer-key', text: p.public_key, title: 'The node’s public key — its identity' }),
    meta,
  ]);
  const pairing = renderPairing(p);
  if (pairing) card.append(pairing);
  // Findings sit directly above the actions, so the evidence and the Block
  // button an admin might reach for are read in one movement.
  const claims = renderClaims(p);
  if (claims) card.append(claims);
  card.append(actions);
  return card;
}

// What our last pairing attempt did (federation.PairAttempt). A pairing that
// does not converge is the one federation failure an admin cannot see from the
// outside — "pending_outgoing" looks identical whether the node is switched off,
// refusing us, or simply waiting on its own admin — so the card says which.
//
// Only for pending_outgoing: once a peer is a friend the attempt is history, and
// for pending_incoming the contact came the other way (last_seen covers it).
function renderPairing(p) {
  if (p.state !== 'pending_outgoing') return null;
  const a = p.last_attempt;
  if (!a) {
    return el('p', { class: 'peer-pairing' }, ['Contacting the node… (retried every minute)']);
  }
  const when = fmtAgo(a.at);
  if (a.error) {
    return el('p', { class: 'peer-pairing is-bad' }, [`Last try ${when}: ${a.error}`]);
  }
  if (a.result === 'friend') {
    // Their side is already mutual; ours flips on the next sweep.
    return el('p', { class: 'peer-pairing is-good' }, [`This node considers you friends (${when}) — settling on our side.`]);
  }
  return el('p', { class: 'peer-pairing is-good' }, [
    `Request delivered ${when} — waiting for their admin to accept it. Nothing more to do on this side.`,
  ]);
}

function fmtAgo(unix) {
  if (!unix) return 'just now';
  const s = Math.max(0, Math.floor(Date.now() / 1000) - unix);
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return new Date(unix * 1000).toLocaleDateString(undefined, { dateStyle: 'medium' });
}

// Mapping a personal (madplayer) node to a local account: all existing ACLs
// then apply to its owner. Select needs the users list (user.manage); without
// it the mapping is shown read-only.
function renderUserMapping(p) {
  if (!users) {
    return el('span', {}, [p.username ? `account: ${p.username}` : '']);
  }
  const sel = el('select', { class: 'peer-user-select', title: 'Map this node to a local user account' });
  sel.append(el('option', { value: '', text: '— no linked account —' }));
  for (const u of users) {
    const opt = el('option', { value: String(u.id), text: u.username });
    if (p.user_id === u.id) opt.selected = true;
    sel.append(opt);
  }
  sel.addEventListener('change', async () => {
    const userID = sel.value === '' ? null : Number(sel.value);
    const ok = await patchPeer(p.id, { user_id: userID });
    if (ok) toast(userID === null ? 'Account link cleared.' : 'Node linked to local account.', 'info');
    refresh();
  });
  return el('label', {}, ['account: ', sel]);
}

// peerLabel is the client half of federation.Peer.Label: the admin's own label
// wins, then what the node calls itself, then its short key — a name is never
// blank, and never the only thing identifying a node (the key is right below it).
function peerLabel(p) {
  return p.name || p.heard_name || p.public_key.slice(0, 12);
}

function startRename(p, nameSpan) {
  // The field edits the LOCAL LABEL only, so it starts empty for a peer this
  // admin never named and the peer's own name sits in the placeholder: saving
  // nothing keeps following that name, and clearing the field returns to it.
  //
  // Mirrors the server cap (federation.MaxPeerNameRunes), so a rename is never
  // silently truncated on save. maxlength counts UTF-16 units rather than runes,
  // making it marginally stricter for astral characters like emoji — stopping
  // the field early is better than accepting text the server would cut.
  const input = el('input', {
    class: 'peer-name-input',
    value: p.name || '',
    placeholder: p.heard_name || 'local label',
    title: p.heard_name ? `This node calls itself “${p.heard_name}”` : 'A label only this node sees',
    maxlength: '64',
  });
  nameSpan.replaceWith(input);
  input.focus();
  input.select();
  let done = false;
  const finish = async save => {
    if (done) return;
    done = true;
    const next = input.value.trim();
    if (save && next !== (p.name || '')) {
      if (await patchPeer(p.id, { name: next })) {
        toast(next ? 'Renamed.' : 'Label cleared — showing the name this node gives itself.', 'info');
      }
    }
    lastPeersJSON = '';
    refresh();
  };
  input.addEventListener('keydown', e => {
    if (e.key === 'Enter') finish(true);
    if (e.key === 'Escape') finish(false);
  });
  input.addEventListener('blur', () => finish(true));
}

// ── Actions ───────────────────────────────────────────────────────────────────

// The field takes either form an admin can have a node in: a full node card, or
// just its public key — which is what the network map hands out, and what a
// friend-of-a-friend is knowable by without their admin exporting anything.
// Identity is the key; a card adds only a claimed name.
const KEY_RE = /^[0-9a-f]{64}$/i;

function importPayload(raw) {
  if (raw.startsWith('{')) {
    try {
      return { payload: { card: JSON.parse(raw) } };
    } catch {
      return { error: 'That is not valid JSON — paste the card exactly as exported, or paste just the node’s public key.' };
    }
  }
  if (KEY_RE.test(raw)) return { payload: { public_key: raw.toLowerCase() } };
  // A mesh address is derived from the key and cannot be turned back into one,
  // so it is not enough to friend a node with — say that instead of "invalid".
  if (raw.includes(':') && /^[0-9a-f:]+$/i.test(raw)) {
    return { error: 'A mesh address is not enough to friend a node — an address cannot be turned back into a key. Paste the node’s public key (64 hex characters) or its card.' };
  }
  return { error: 'Paste a node card (JSON) or a node’s public key (64 hex characters).' };
}

async function onImport(e) {
  e.preventDefault();
  const { payload, error } = importPayload(cardInput.value.trim());
  if (error) { toast(error, 'error'); return; }
  importBtn.disabled = true;
  try {
    const res = await fetch(`${API}/api/admin/federation/peers`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    if (handleAuthError(res)) return;
    const body = await res.json().catch(() => ({}));
    if (!res.ok) { toast(body.error || `Import failed (HTTP ${res.status}).`, 'error'); return; }
    cardInput.value = '';
    toast(body.peer?.state === 'friend'
      ? `Friendship with “${peerLabel(body.peer)}” established.`
      : 'Contacting the node — its admin has to accept before you are friends.', 'info');
    refresh();
  } catch (err) {
    toast(`Import failed: ${err.message}`, 'error');
  } finally {
    importBtn.disabled = false;
  }
}

async function acceptPeer(p) {
  const ok = await confirmModal({
    title: 'Accept pairing request?',
    bodyNodes: [
      el('p', {}, [`“${peerLabel(p)}” asks to become a friend. Verify that this key matches the node card its admin sent you out-of-band:`]),
      el('code', { class: 'modal-key', text: p.public_key }),
      el('p', {}, ['A friend node can browse and fetch the parts of this library you share with the madnetwork.']),
    ],
    confirmLabel: 'Accept as friend',
    danger: false,
  });
  if (ok) await peerOp(p, 'accept', 'Friend added.');
}

// Blocking is a PUBLIC act: the block is published as a signed distrust mark
// that relays across the whole network and is readable by the node being
// blocked (docs/architecture/federation.md §Friend-list gossip). The modal has
// to say so plainly and collect the reason, because a mark without one is an
// anonymous downvote nobody downstream can act on.
//
// Shared by the peer list and the network map, so a node blocked from the graph
// is asked for exactly as much as one blocked from the peer card.
async function askBlockReason(label) {
  const reason = el('input', {
    type: 'text',
    class: 'modal-reason',
    maxlength: '280',
    placeholder: 'e.g. advertised a hash with a contradicting fingerprint',
  });
  const ok = await confirmModal({
    title: 'Block this node?',
    bodyNodes: [
      el('p', {}, [`Block “${label}”? It loses all madnetwork service from this node immediately. You can unblock it later.`]),
      el('p', { class: 'modal-note' }, ['This block is published to the network as a distrust mark — everyone, including the blocked node, can see it and read the reason. Unblocking withdraws it again.']),
      el('p', { class: 'modal-note' }, ['Whatever you could only see through this node leaves the map too, and is forgotten rather than merely hidden. Nodes another friend also vouches for stay. Unblocking re-learns the rest on the next sync.']),
      el('label', { class: 'modal-label' }, ['Reason (shown to everyone)']),
      reason,
    ],
    confirmLabel: 'Block and publish',
    danger: true,
  });
  return ok ? reason.value.trim() : null;
}

async function blockPeer(p) {
  const reason = await askBlockReason(peerLabel(p));
  if (reason === null) return;
  await peerOp(p, 'block', 'Node blocked; distrust mark published.', { reason });
}

// Blocking from the map goes by KEY: most nodes there are strangers with no
// peer row, which is the whole reason the map is worth having.
async function blockMapNode(n) {
  const reason = await askBlockReason(n.name || n.key.slice(0, 12));
  if (reason === null) return;
  try {
    const res = await fetch(`${API}/api/admin/federation/block`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ public_key: n.key, name: n.name || '', reason }),
    });
    if (handleAuthError(res)) return;
    const body = await res.json().catch(() => ({}));
    if (!res.ok) { toast(body.error || `Block failed (HTTP ${res.status}).`, 'error'); return; }
    toast('Node blocked; distrust mark published.', 'info');
  } catch (err) {
    toast(`Block failed: ${err.message}`, 'error');
  }
  refresh();
}

// Friending from the map goes by KEY, for the same reason blocking does: the
// nodes worth acting on there are the ones we have no row for. A friend of a
// friend is knowable — the graph carries its key — without its admin exporting a
// card, and the trust graph is a graph, so nothing about being two hops away
// makes this a lesser friendship than the one it was discovered through.
//
// This sends the request and no more. The far node records a pending request its
// admin has to accept, exactly as a card import does — friending stays mutual.
async function friendMapNode(n) {
  const label = n.name || n.key.slice(0, 12);
  // Not `body`: a fetch options object below has a `body` key, and the two names
  // sitting next to each other is how the bug this file just fixed reads.
  const bodyNodes = [
    el('p', {}, [`Send “${label}” a pairing request. Its admin has to accept before you are friends, and nothing of your library is shared until they do.`]),
    el('code', { class: 'modal-key', text: n.key }),
  ];
  if (n.named === 'heard') {
    bodyNodes.push(el('p', { class: 'modal-note' }, [
      'That name is only what the network says this node calls itself. The key above is the identity — check it against the person you mean to friend.',
    ]));
  }
  const ok = await confirmModal({
    title: 'Ask this node to be friends?',
    bodyNodes,
    confirmLabel: 'Send request',
    danger: false,
  });
  if (!ok) return;
  try {
    const res = await fetch(`${API}/api/admin/federation/peers`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ public_key: n.key, name: n.name || '' }),
    });
    if (handleAuthError(res)) return;
    const data = await res.json().catch(() => ({}));
    if (!res.ok) { toast(data.error || `Request failed (HTTP ${res.status}).`, 'error'); return; }
    toast(data.peer?.state === 'friend'
      ? `Friendship with “${peerLabel(data.peer)}” established — they had already asked.`
      : 'Request sent — waiting for their admin to accept it.', 'info');
  } catch (err) {
    toast(`Request failed: ${err.message}`, 'error');
  }
  refresh();
}

// pullMapNode asks the refresh loop to fetch one node's catalog now, rather than
// when the frontier rotation reaches it (F7 item 5). No confirmation: it costs
// one request and reveals nothing about us that browsing the map did not.
async function pullMapNode(n) {
  try {
    const res = await fetch(`${API}/api/admin/federation/discover`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ public_key: n.key }),
    });
    if (handleAuthError(res)) return;
    const data = await res.json().catch(() => ({}));
    if (!res.ok) { toast(data.error || `Request failed (HTTP ${res.status}).`, 'error'); return; }
    toast('Asked for this node’s library — it appears on /madnetwork once it answers.', 'info');
  } catch (err) {
    toast(`Request failed: ${err.message}`, 'error');
  }
}

async function unblockPeer(p) {
  await peerOp(p, 'unblock', 'Node unblocked; the distrust mark is withdrawn.');
}

async function removePeer(p) {
  const ok = await confirmModal({
    title: 'Remove this node?',
    bodyNodes: [
      el('p', {}, [`Forget “${peerLabel(p)}” entirely? A new card import (or a fresh pairing request from its side) starts over from scratch.`]),
      el('p', { class: 'modal-note' }, ['Everything you knew only through this node is forgotten with it — the map may lose more than one dot. Nodes another friend also vouches for stay.']),
    ],
    confirmLabel: 'Remove',
    danger: true,
  });
  if (!ok) return;
  try {
    const res = await fetch(`${API}/api/admin/federation/peers/${p.id}`, { method: 'DELETE' });
    if (handleAuthError(res)) return;
    const body = await res.json().catch(() => ({}));
    if (!res.ok) { toast(body.error || `Remove failed (HTTP ${res.status}).`, 'error'); return; }
    toast('Node removed.', 'info');
  } catch (err) {
    toast(`Remove failed: ${err.message}`, 'error');
  }
  refresh();
}

async function peerOp(p, op, doneMsg, body = null) {
  try {
    const res = await fetch(`${API}/api/admin/federation/peers/${p.id}/${op}`, {
      method: 'POST',
      ...(body ? { headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) } : {}),
    });
    if (handleAuthError(res)) return;
    // NOT `body` — that is this function's request parameter, read a few lines
    // up. Re-declaring the name here put that read in a temporal dead zone, so
    // every accept, block and unblock threw before the request was ever sent.
    const data = await res.json().catch(() => ({}));
    if (!res.ok) { toast(data.error || `Operation failed (HTTP ${res.status}).`, 'error'); return; }
    toast(doneMsg, 'info');
  } catch (err) {
    toast(`Operation failed: ${err.message}`, 'error');
  }
  refresh();
}

async function patchPeer(id, patch) {
  try {
    const res = await fetch(`${API}/api/admin/federation/peers/${id}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(patch),
    });
    if (handleAuthError(res)) return false;
    const body = await res.json().catch(() => ({}));
    if (!res.ok) { toast(body.error || `Update failed (HTTP ${res.status}).`, 'error'); return false; }
    return true;
  } catch (err) {
    toast(`Update failed: ${err.message}`, 'error');
    return false;
  }
}

function refresh() {
  lastPeersJSON = '';
  loadPeers();
  loadMap();
}

// ── Helpers ───────────────────────────────────────────────────────────────────

async function copyText(text, doneMsg) {
  try {
    await navigator.clipboard.writeText(text);
    toast(doneMsg, 'info');
  } catch {
    toast('Copy failed — select and copy manually.', 'error');
  }
}

function downloadCard() {
  const blob = new Blob([JSON.stringify(ownCard, null, 2) + '\n'], { type: 'application/json' });
  const a = el('a', { href: URL.createObjectURL(blob), download: 'madshare-node-card.json' });
  a.click();
  URL.revokeObjectURL(a.href);
}

function confirmModal({ title, bodyNodes, confirmLabel, danger = true }) {
  return new Promise(resolve => {
    cfTitle.textContent = title;
    cfBody.replaceChildren(...bodyNodes);
    cfConfirm.textContent = confirmLabel;
    cfConfirm.className = 'btn ' + (danger ? 'btn-destructive-solid' : 'btn-neutral');
    cfModal.classList.remove('hidden');
    cfConfirm.focus();
    const cleanup = () => {
      cfConfirm.removeEventListener('click', onOk);
      cfCancel.removeEventListener('click', onCancel);
      cfClose.removeEventListener('click', onCancel);
      cfModal.removeEventListener('click', onBackdrop);
      document.removeEventListener('keydown', onKey);
    };
    const onOk       = () => { cfModal.classList.add('hidden'); cleanup(); resolve(true); };
    const onCancel   = () => { cfModal.classList.add('hidden'); cleanup(); resolve(false); };
    const onBackdrop = e => { if (e.target === cfModal) onCancel(); };
    const onKey      = e => { if (e.key === 'Escape' && !cfModal.classList.contains('hidden')) onCancel(); };
    cfConfirm.addEventListener('click', onOk);
    cfCancel.addEventListener('click', onCancel);
    cfClose.addEventListener('click', onCancel);
    cfModal.addEventListener('click', onBackdrop);
    document.addEventListener('keydown', onKey);
  });
}

// ── Boot ──────────────────────────────────────────────────────────────────────
(async function boot() {
  if (!await bootAdmin({ require: 'federation.manage' })) return;
  if (!await loadStatus()) return;
  await loadUsers();
  await loadPeers();
  initMap({ onBlockNode: blockMapNode, onFriendNode: friendMapNode, onPullNode: pullMapNode });
  await loadMap();
})();
