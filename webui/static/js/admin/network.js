// Admin · Network — the madnetwork friendship page (federation F1). Shows this
// node's identity + exportable node card, imports friends' cards, and manages
// the trusted-peer list (accept / block / unblock / remove, local label, local
// user mapping). Gated on federation.manage (the API enforces it). Pairing is
// asynchronous — the page polls while anything is pending so state flips appear
// without a reload. Design: docs/architecture/federation.md.
import { bootAdmin, API, toast, handleAuthError, el } from './shared.js';

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
  const json = JSON.stringify(peers);
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
  if (!peers.length) {
    peersList.replaceChildren(el('p', { class: 'net-empty', text: 'No known nodes yet — import a friend’s node card above.' }));
    return;
  }
  peersList.replaceChildren(...peers.map(renderPeer));
}

function renderPeer(p) {
  const nameSpan = el('span', { class: 'peer-name', text: p.name || '(unnamed node)' });
  const head = el('div', { class: 'peer-head' }, [
    nameSpan,
    el('button', { class: 'peer-rename', title: 'Rename (local label)', 'aria-label': 'Rename', onclick: () => startRename(p, nameSpan) }, ['✎']),
    el('span', { class: `peer-badge ${stateClass(p.state)}`, text: STATE_LABEL[p.state] || p.state }),
    el('span', { class: 'peer-when', text: fmtLastSeen(p.last_seen) }),
  ]);

  const meta = el('div', { class: 'peer-meta' });
  meta.append(el('span', {}, [p.address ? `mesh ${p.address}` : '']));
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

  return el('div', { class: `peer-card ${stateClass(p.state)}` }, [
    head,
    el('code', { class: 'peer-key', text: p.public_key, title: 'The node’s public key — its identity' }),
    meta,
    actions,
  ]);
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

function startRename(p, nameSpan) {
  const input = el('input', { class: 'peer-name-input', value: p.name || '', maxlength: '100' });
  nameSpan.replaceWith(input);
  input.focus();
  input.select();
  let done = false;
  const finish = async save => {
    if (done) return;
    done = true;
    if (save && input.value.trim() !== (p.name || '')) {
      if (await patchPeer(p.id, { name: input.value.trim() })) toast('Renamed.', 'info');
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

async function onImport(e) {
  e.preventDefault();
  let card;
  try {
    card = JSON.parse(cardInput.value);
  } catch {
    toast('That is not valid JSON — paste the card exactly as exported.', 'error');
    return;
  }
  importBtn.disabled = true;
  try {
    const res = await fetch(`${API}/api/admin/federation/peers`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ card }),
    });
    if (handleAuthError(res)) return;
    const body = await res.json().catch(() => ({}));
    if (!res.ok) { toast(body.error || `Import failed (HTTP ${res.status}).`, 'error'); return; }
    cardInput.value = '';
    toast(body.peer?.state === 'friend'
      ? `Friendship with “${body.peer.name || body.peer.public_key.slice(0, 12)}” established.`
      : 'Card imported — contacting the node…', 'info');
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
      el('p', {}, [`“${p.name || '(unnamed node)'}” asks to become a friend. Verify that this key matches the node card its admin sent you out-of-band:`]),
      el('code', { class: 'modal-key', text: p.public_key }),
      el('p', {}, ['A friend node can browse and fetch the parts of this library you share with the madnetwork.']),
    ],
    confirmLabel: 'Accept as friend',
    danger: false,
  });
  if (ok) await peerOp(p, 'accept', 'Friend added.');
}

async function blockPeer(p) {
  const ok = await confirmModal({
    title: 'Block this node?',
    bodyNodes: [el('p', {}, [`Block “${p.name || p.public_key.slice(0, 12)}”? It loses all madnetwork service from this node immediately. You can unblock it later.`])],
    confirmLabel: 'Block',
    danger: true,
  });
  if (ok) await peerOp(p, 'block', 'Node blocked.');
}

async function unblockPeer(p) {
  await peerOp(p, 'unblock', 'Node unblocked.');
}

async function removePeer(p) {
  const ok = await confirmModal({
    title: 'Remove this node?',
    bodyNodes: [el('p', {}, [`Forget “${p.name || p.public_key.slice(0, 12)}” entirely? A new card import (or a fresh pairing request from its side) starts over from scratch.`])],
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

async function peerOp(p, op, doneMsg) {
  try {
    const res = await fetch(`${API}/api/admin/federation/peers/${p.id}/${op}`, { method: 'POST' });
    if (handleAuthError(res)) return;
    const body = await res.json().catch(() => ({}));
    if (!res.ok) { toast(body.error || `Operation failed (HTTP ${res.status}).`, 'error'); return; }
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
})();
