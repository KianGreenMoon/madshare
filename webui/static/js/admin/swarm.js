// Admin · Swarm — what this node moves over the madnetwork, and how fast.
// Design: docs/architecture/swarm-admin.md.
//
// A bespoke module rather than a file-list.js scope, and the reasons are
// specific: rows carry a progress bar and a live-updating rate cell, the list is
// a union of two origins with per-origin actions in one selection, and a
// two-second patch loop into windowed rows is a different rendering model from
// "fetch a page, render it". Bloating a component five surfaces share, to serve
// one, is the trade this avoids.
//
// Everything else is reused: virtual-list for the windowing, row-menu for the ⋯
// menu, the shared player + player bar, toast, the admin table/modal CSS, and
// share-depth.js for the scope vocabulary. It also mints NO endpoint that
// /admin/cache already owns — removal and claims are that page's routes, called
// from here. Two lenses on one node; a lens may duplicate a view, never an
// editor.
import { API, el, fmtBytes, fmtDate, shortHash, toast, handleAuthError, bootAdmin } from './shared.js';
import { createVirtualList } from '../virtual-list.js';
import { createPlayer } from '../player.js';
import { openRowMenu } from '../row-menu.js';
import { depthName } from '../share-depth.js';

const PAGE = 100;
const POLL_ACTIVE = 2000;   // something is moving: keep the bars honest
const POLL_IDLE = 10000;    // nothing is moving: just keep the totals fresh

let canUpload = false;      // file.upload — the Materialize gate (the API enforces it too)
let canManage = false;      // user.manage — the rate knobs and Forget stats
let federationOn = false;   // a node is running, so live figures and Materialize exist
let seeding = { enabled: true, cache: true };

const state = { scope: 'all', pill: '', q: '', sort: 'newest' };
let total = 0;
let vlist = null;
let pollTimer = null;
const expanded = new Set();   // hashes whose info panel is open
const details = new Map();    // hash → the detail payload once fetched

const listHost = document.getElementById('swarmList');
const countEl = document.getElementById('swarmCount');
const transfersHost = document.getElementById('swarmTransfers');

const dispName = f => f.title || f.filename || shortHash(f.hash);
// The second line only exists when it says something the first does not. An
// untagged cached blob is named by its hash, and repeating that hash underneath
// is noise — seen on a real cache, where most rows have no tags at all.
function dispSub(f) {
  const text = [f.artist, f.album].filter(Boolean).join(' — ');
  if (text) return text;
  return f.title && f.filename && f.filename !== f.title ? f.filename : '';
}

async function req(url, opts) {
  const res = await fetch(url, opts);
  if (handleAuthError(res)) throw new Error('Your session expired.');
  const data = await res.json().catch(() => ({}));
  if (!res.ok || data.ok === false) throw new Error(data.error || `HTTP ${res.status}`);
  return data;
}

// ── Player ───────────────────────────────────────────────────────────────────
// A library blob is served by /files; a cached one by the cache page's own audio
// endpoint, which — unlike the madnetwork relay — outlives federation being
// switched off, and that is exactly when someone is here looking at the disk.
function audioURL(f, download) {
  if (f.in_library && f.object_key) {
    return `${API}/files/${f.object_key}${download ? '?download=1' : ''}`;
  }
  return `${API}/api/admin/cache/${f.hash}/audio${download ? '?download=1' : ''}`;
}

const player = createPlayer({
  onError: () => toast('Couldn’t play this file.', 'error'),
});
function playRow(f) {
  player.load({ url: audioURL(f), title: dispName(f), artist: f.artist || '' });
}

// ── Confirm modal ────────────────────────────────────────────────────────────
const confirmModal = document.getElementById('confirmModal');
const confirmTitle = document.getElementById('confirmTitle');
const confirmBody = document.getElementById('confirmBody');
const confirmOK = document.getElementById('confirmOK');
const confirmCancel = document.getElementById('confirmCancel');
let pendingRun = null;

function confirm({ title, body, run }) {
  confirmTitle.textContent = title;
  confirmBody.textContent = body;
  pendingRun = run;
  confirmModal.classList.remove('hidden');
}
function closeConfirm() {
  confirmModal.classList.add('hidden');
  pendingRun = null;
}
confirmCancel.addEventListener('click', closeConfirm);
confirmModal.addEventListener('click', e => { if (e.target === confirmModal) closeConfirm(); });
confirmOK.addEventListener('click', async () => {
  const run = pendingRun;
  closeConfirm();
  if (!run) return;
  try { await run(); } catch (err) { toast(err.message, 'error'); }
});

// ── Limits modal ─────────────────────────────────────────────────────────────
// The one editor for every transfer limit this node has: its two node-wide caps
// and the four member-budget bounds. Empty = inherit the config file,
// 0 = unlimited; all six are three-valued on the wire for the same reason.
const limitsModal = document.getElementById('limitsModal');
const limitUp = document.getElementById('limitUp');
const limitDown = document.getElementById('limitDown');
const limitWarn = document.getElementById('limitWarn');
// The member budget, keyed by the wire field each input writes — so the save
// path never repeats the mapping and a renamed field breaks in one place.
const memberFields = {
  member_rate_kib: document.getElementById('limitMemberRate'),
  per_member_rate_kib: document.getElementById('limitPerMemberRate'),
  member_max_transfers: document.getElementById('limitMemberMax'),
  per_member_max_transfers: document.getElementById('limitPerMemberMax'),
};

// The floor below which a node stops being useful rather than merely slow:
// peers' stall watchdogs fire, the swarm de-ranks it, and the bytes it did send
// are wasted. It WARNS and never refuses — the operator's line, their call.
const FLOOR_KIB = 256;

function checkFloor() {
  const vals = [limitUp.value, limitDown.value]
    .map(v => v.trim())
    .filter(v => v !== '')
    .map(Number)
    .filter(n => Number.isFinite(n) && n > 0 && n < FLOOR_KIB);
  if (!vals.length) {
    limitWarn.classList.add('hidden');
    return;
  }
  limitWarn.textContent =
    `Below ~${FLOOR_KIB} KiB/s this node is likely to be dropped by the swarm rather than just slow: `
    + 'one FLAC stream alone needs ~125 KiB/s, and every transfer shares this bucket. '
    + 'If the line cannot spare it, switching seeding off is the honest setting.';
  limitWarn.classList.remove('hidden');
}
limitUp.addEventListener('input', checkFloor);
limitDown.addEventListener('input', checkFloor);

function openLimits(limits) {
  limitUp.value = limits?.up?.override_kib ?? '';
  limitDown.value = limits?.down?.override_kib ?? '';
  // Only an OVERRIDE prefills a field: a config value shown in the box would be
  // saved back as an override on the next Save, quietly pinning a number the
  // operator only ever read. The placeholder says where the blank comes from.
  for (const [name, input] of Object.entries(memberFields)) {
    const side = limits?.[name];
    input.value = (name.endsWith('_kib') ? side?.override_kib : side?.override) ?? '';
  }
  checkFloor();
  limitsModal.classList.remove('hidden');
}
document.getElementById('limitsCancel').addEventListener('click', () => limitsModal.classList.add('hidden'));
limitsModal.addEventListener('click', e => { if (e.target === limitsModal) limitsModal.classList.add('hidden'); });
document.getElementById('limitsSave').addEventListener('click', async () => {
  // An empty field is an explicit null — "go back to the config file" — which is
  // a different request from omitting the field.
  const parse = input => (input.value.trim() === '' ? null : Math.max(0, Math.round(Number(input.value))));
  const body = { up_kib: parse(limitUp), down_kib: parse(limitDown) };
  for (const [name, input] of Object.entries(memberFields)) body[name] = parse(input);
  try {
    await req(`${API}/api/admin/swarm/limits`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    limitsModal.classList.add('hidden');
    toast('Transfer limits updated.', 'success');
    loadSummary();
  } catch (err) { toast(err.message, 'error'); }
});

// ── Summary strip ────────────────────────────────────────────────────────────
let lastLimits = null;

function rateText(side) {
  if (!side) return '—';
  const kib = side.effective_kib || 0;
  const where = side.source === 'override' ? 'set here' : 'from config';
  return kib > 0 ? `${kib} KiB/s (${where})` : `unlimited (${where})`;
}

// The member budget as one line. Four numbers, all usually unlimited, would be
// wallpaper — so the line names only the bounds that are actually set, and says
// plainly when none are. Friends are named every time: a reader seeing "members"
// throttled should not have to remember who that excludes.
function memberText(limits) {
  const val = name => (name.endsWith('_kib')
    ? limits?.[name]?.effective_kib
    : limits?.[name]?.effective) || 0;
  const parts = [];
  if (val('member_rate_kib')) parts.push(`${val('member_rate_kib')} KiB/s together`);
  if (val('per_member_rate_kib')) parts.push(`${val('per_member_rate_kib')} KiB/s each`);
  if (val('member_max_transfers')) parts.push(`${val('member_max_transfers')} transfers together`);
  if (val('per_member_max_transfers')) parts.push(`${val('per_member_max_transfers')} transfers each`);
  return parts.length
    ? `Non-friends — ${parts.join(' · ')} (friends are exempt)`
    : 'Non-friends — no budget set; friends are exempt from any';
}

async function loadSummary() {
  const host = document.getElementById('swarmSummary');
  let s;
  try { s = await req(`${API}/api/admin/swarm/summary`); }
  catch { host.replaceChildren(el('p', { class: 'muted', text: 'Couldn’t read the swarm summary.' })); return; }

  federationOn = !!s.federation;
  lastLimits = s.limits || null;
  if (s.seeding) seeding = s.seeding;

  const all = s.all_time || {};
  const sess = s.session || {};
  const rows = [
    el('div', { class: 'swarm-stats' }, [
      el('span', { class: 'swarm-stat' }, [
        el('strong', { text: `▲ ${fmtBytes(all.up_bytes || 0)}` }), ' up · ',
        el('strong', { text: `▼ ${fmtBytes(all.down_bytes || 0)}` }), ' down',
        el('span', { class: 'muted', text: '  all time' }),
      ]),
      el('span', { class: 'swarm-stat muted' }, [
        `▲ ${fmtBytes(sess.up_bytes || 0)} · ▼ ${fmtBytes(sess.down_bytes || 0)} this session`,
        ...(sess.wasted_bytes ? [` · ${fmtBytes(sess.wasted_bytes)} wasted`] : []),
      ]),
    ]),
  ];

  const limitsLine = el('div', { class: 'swarm-limits' }, [
    el('span', {}, [
      `Limits — up: ${rateText(s.limits?.up)} · down: ${rateText(s.limits?.down)}`,
      el('br'),
      el('span', { class: 'muted', text: memberText(s.limits) }),
    ]),
    ...(canManage ? [el('button', {
      class: 'btn btn-neutral btn-sm', type: 'button',
      onclick: () => openLimits(lastLimits),
    }, ['Change…'])] : []),
  ]);
  rows.push(limitsLine);

  const seedText = !federationOn
    ? 'federation is off — nothing is being served or fetched right now'
    : (seeding.enabled
      ? `seeding: on${seeding.cache ? ' · cache seeding: on' : ' · cache seeding: off'}`
      : 'seeding: off — this node consumes without serving');
  const peers = (s.peers || []).filter(p => p.up_bytes > 0);
  rows.push(el('div', { class: 'swarm-stats muted' }, [
    // Not a .swarm-stat: that class holds figures together with nowrap, and this
    // is a sentence. Wearing it, it set a 418px floor under the whole page and
    // pushed a phone into sideways scrolling.
    el('span', {}, [seedText]),
    ...(peers.length ? [el('span', { class: 'swarm-stat' },
      [`${peers.length} node${peers.length === 1 ? '' : 's'} pulled from us this session`])] : []),
  ]));

  host.replaceChildren(...rows);
  renderTransfers(s.active || []);
}

// ── Live transfers (the pinned block) ────────────────────────────────────────
// Partials are in no table and must not enter one — unverified bytes are never
// described or advertised — so they render above the paged list rather than in
// it (docs/architecture/swarm-admin.md §Partials).
function renderTransfers(active) {
  if (!active.length) {
    transfersHost.hidden = true;
    transfersHost.replaceChildren();
    return;
  }
  transfersHost.hidden = false;
  transfersHost.replaceChildren(
    el('h3', { class: 'swarm-transfers-title', text: `Downloading now (${active.length})` }),
    ...active.map(t => {
      const pct = t.size > 0 ? Math.min(100, Math.round((t.progress / t.size) * 100)) : 0;
      return el('div', { class: 'swarm-transfer' }, [
        el('div', { class: 'swarm-transfer-head' }, [
          el('code', { class: 'swarm-hash', text: shortHash(t.hash) }),
          el('span', { class: 'muted', text: `${t.mode || 'starting'} · ${fmtBytes(t.progress)} / ${fmtBytes(t.size)}` }),
          ...(t.chunks ? [el('span', { class: 'muted', text: `${t.chunks_done}/${t.chunks} chunks` })] : []),
        ]),
        progressBar(pct, true),
      ]);
    }),
  );
}

// progressBar is the graphical fill. A complete blob is 100% by definition: the
// swarm verifies the whole-file hash before the file gets its final name, so a
// partial one never wears a finished name.
function progressBar(pct, live) {
  return el('div', {
    class: `swarm-bar${live ? ' swarm-bar--live' : ''}`,
    role: 'progressbar', 'aria-valuenow': String(pct),
    'aria-valuemin': '0', 'aria-valuemax': '100',
    title: `${pct}%`,
  }, [el('span', { class: 'swarm-bar-fill', style: `width:${pct}%` })]);
}

// ── Who we trade with ────────────────────────────────────────────────────────
// All-time per counterparty (mig 042), the companion to the member quotas:
// those bound what a member may cost us, this says what one has. Collapsed by
// default and loaded when opened — it is a question you go and ask, not one the
// page should answer over the file list.
const peersHost = document.getElementById('swarmPeers');
const peersBody = document.getElementById('swarmPeersBody');
const peersSummary = document.getElementById('swarmPeersSummary');
let peersLoadedAt = 0;

// What a node is to us NOW, not when the bytes moved. `gone` is not an error
// state: an unfriended node, or one the discovery rotation has evicted, keeps
// its history — what it cost us does not stop being true.
const KIND_LABEL = {
  friend: 'friend',
  member: 'member',
  blocked: 'blocked',
  pending_outgoing: 'pending',
  pending_incoming: 'pending',
  gone: 'no longer known',
  unplaced: 'guests and listener devices',
};

function peerName(p) {
  if (p.key === '') return 'Unnamed requesters';
  return p.name || shortHash(p.key);
}

function renderPeerRow(p) {
  const up = (p.up_bytes || 0) + (p.session?.up_bytes || 0);
  const down = (p.down_bytes || 0) + (p.session?.down_bytes || 0);
  const kids = [
    el('div', { class: 'swarm-peer-name' }, [
      el('span', { class: 'swarm-peer-title', text: peerName(p) }),
      el('span', { class: 'swarm-peer-kind muted', text: KIND_LABEL[p.kind] || p.kind || '' }),
    ]),
    el('div', { class: 'swarm-row-traffic' }, [
      el('span', { class: 'swarm-up', text: `▲ ${fmtBytes(up)}` }),
      el('span', { class: 'swarm-down', text: `▼ ${fmtBytes(down)}` }),
    ]),
    el('span', { class: 'muted swarm-peer-when', text:
      p.last_at ? fmtDate(p.last_at) : (p.session ? 'this session' : '') }),
  ];
  // Forgetting is an explicit act with a cost, so it is worded like one — and
  // it does NOT touch any blob's history, because the two ledgers count the same
  // bytes without either being derived from the other.
  if (canManage) {
    kids.push(el('button', {
      class: 'btn btn-neutral btn-sm', type: 'button',
      onclick: () => confirm({
        title: 'Forget this history',
        body: p.key === ''
          ? 'Forget what unnamed requesters have moved? The bytes really did move. '
            + 'Per-file traffic is a separate record and is not touched.'
          : `Forget what “${peerName(p)}” has moved with this node? The bytes really did move, `
            + 'and this does not change any file’s own traffic — the two are separate records.',
        run: async () => {
          await req(`${API}/api/admin/swarm/peers/forget`, {
            method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ keys: [p.key] }),
          });
          toast('Forgotten.', 'success');
          loadPeers(true);
        },
      }),
    }, ['Forget']));
  }
  return el('div', { class: `swarm-peer-row${p.key === '' ? ' swarm-peer-row--bucket' : ''}` }, kids);
}

async function loadPeers(force) {
  if (!force && Date.now() - peersLoadedAt < 8000) return;
  let data;
  try { data = await req(`${API}/api/admin/swarm/peers`); }
  catch { peersBody.replaceChildren(el('p', { class: 'muted', text: 'Couldn’t read the peer totals.' })); return; }
  peersLoadedAt = Date.now();

  const rows = data.peers || [];
  const bucket = data.unplaced;
  const t = data.totals || {};
  peersSummary.textContent = rows.length || bucket
    ? `Who we trade with — ${rows.length} node${rows.length === 1 ? '' : 's'}`
      + ` · ▲ ${fmtBytes(t.up_bytes || 0)} · ▼ ${fmtBytes(t.down_bytes || 0)} all time`
    : 'Who we trade with';

  if (!rows.length && !bucket) {
    peersBody.replaceChildren(el('p', { class: 'muted', text:
      'Nothing yet — no node has pulled from this one, and it has fetched from none.' }));
    return;
  }
  peersBody.replaceChildren(
    ...rows.map(renderPeerRow),
    ...(bucket ? [renderPeerRow(bucket)] : []),
  );
}

peersHost.addEventListener('toggle', () => { if (peersHost.open) loadPeers(true); });

// ── Rows ─────────────────────────────────────────────────────────────────────
function chips(f) {
  const out = [];
  if (f.in_library) out.push(el('span', { class: 'swarm-chip', text: 'library' }));
  if (f.in_cache) out.push(el('span', { class: 'swarm-chip swarm-chip--cache', text: 'cached' }));
  if (f.trashed) out.push(el('span', { class: 'swarm-chip swarm-chip--warn', text: 'trashed' }));
  else if (f.review_state && f.review_state !== 'approved') {
    out.push(el('span', { class: 'swarm-chip swarm-chip--warn', text: 'in review' }));
  }
  if (f.in_library) {
    out.push(el('span', { class: 'swarm-chip swarm-chip--scope', text: depthName(f.share_depth) }));
  }
  if (!f.seedable) out.push(el('span', { class: 'swarm-chip swarm-chip--off', text: 'not shared' }));
  return out;
}

// whyNotSeeding is the sentence the page exists to be able to say. Row facts
// first, then the node-level switches, which are not facts about the row.
function whyNotSeeding(f) {
  if (f.trashed) return 'In the trash — trashed blobs are never served.';
  if (f.review_state && f.review_state !== 'approved') {
    return 'Still in review — nothing is published before a moderator approves it.';
  }
  if (f.in_library && !f.seedable) return 'Scoped Local — this recording never leaves the machine.';
  if (!seeding.enabled) return 'Seeding is switched off for the whole node (Settings).';
  if (f.in_cache && !f.in_library && !seeding.cache) return 'Cache seeding is switched off (Settings).';
  if (!federationOn) return 'Federation is off, so nothing is being served right now.';
  return '';
}

function renderRow(f) {
  const open = expanded.has(f.hash);
  const tr = f.transfer;
  const pct = tr && tr.size > 0 ? Math.min(100, Math.round((tr.progress / tr.size) * 100)) : 100;
  const up = (f.up_bytes || 0) + (f.session?.up_bytes || 0);
  const down = (f.down_bytes || 0) + (f.session?.down_bytes || 0);

  const row = el('div', { class: `swarm-row${open ? ' swarm-row--open' : ''}`, 'data-hash': f.hash }, [
    el('div', { class: 'swarm-row-main' }, [
      el('button', {
        class: 'swarm-row-name', type: 'button',
        title: 'Show details',
        onclick: () => toggleExpand(f),
      }, [
        el('span', { class: 'swarm-title', text: dispName(f) }),
        ...(dispSub(f) ? [el('span', { class: 'swarm-sub muted', text: dispSub(f) })] : []),
      ]),
      el('div', { class: 'swarm-row-bar' }, [progressBar(pct, !!tr)]),
      el('div', { class: 'swarm-row-traffic' }, [
        el('span', { class: 'swarm-up', text: `▲ ${fmtBytes(up)}` }),
        el('span', { class: 'swarm-down', text: `▼ ${fmtBytes(down)}` }),
        el('span', { class: 'muted swarm-size', text: fmtBytes(f.byte_size) }),
      ]),
      el('div', { class: 'swarm-row-chips' }, chips(f)),
      el('button', {
        class: 'icon-btn swarm-menu-btn', type: 'button',
        title: 'More actions', 'aria-label': `Actions for ${dispName(f)}`,
        onclick: e => { e.stopPropagation(); openMenu(e.currentTarget, f); },
      }, ['⋯']),
    ]),
  ]);
  if (open) row.appendChild(renderDetail(f));
  return row;
}

function renderDetail(f) {
  const d = details.get(f.hash);
  const host = el('div', { class: 'swarm-detail' });
  if (!d) {
    host.replaceChildren(el('p', { class: 'muted', text: 'Loading…' }));
    return host;
  }
  const facts = [
    ['Hash', f.hash],
    ['Size', fmtBytes(f.byte_size)],
    ['Filename', f.filename || '—'],
    ['Added', fmtDate(f.added_at)],
    ['Where', [f.in_library ? 'library' : null, f.in_cache ? 'cache' : null].filter(Boolean).join(' + ')],
  ];
  const traffic = [
    ['Uploaded', fmtBytes(d.up_bytes || 0)],
    ['Downloaded', fmtBytes(d.down_bytes || 0)],
    ...(d.wasted_bytes ? [['Wasted', fmtBytes(d.wasted_bytes)]] : []),
    ...(d.last_at ? [['Last active', fmtDate(d.last_at)]] : []),
  ];
  const kids = [
    el('div', { class: 'swarm-detail-cols' }, [
      el('dl', { class: 'swarm-facts' }, facts.flatMap(([k, v]) => [
        el('dt', { text: k }), el('dd', { text: String(v) }),
      ])),
      el('dl', { class: 'swarm-facts' }, traffic.flatMap(([k, v]) => [
        el('dt', { text: k }), el('dd', { text: String(v) }),
      ])),
    ]),
  ];

  const why = whyNotSeeding(f);
  if (why) kids.push(el('p', { class: 'swarm-why muted', text: why }));

  const tr = d.transfer;
  if (tr) {
    kids.push(el('div', { class: 'swarm-live' }, [
      el('h4', { text: 'Fetching now' }),
      el('p', { class: 'muted', text:
        `${tr.mode || '—'} · ${tr.chunks_done || 0}/${tr.chunks || 0} chunks`
        + (tr.first_byte_ms ? ` · first byte ${tr.first_byte_ms} ms` : '')
        + (tr.failovers ? ` · ${tr.failovers} failover${tr.failovers === 1 ? '' : 's'}` : '')
        + (tr.stalls ? ` · ${tr.stalls} stall${tr.stalls === 1 ? '' : 's'}` : '')
        + (tr.corrupt ? ` · ${tr.corrupt} corrupt chunk${tr.corrupt === 1 ? '' : 's'}` : '') }),
      ...(tr.providers || []).map(p => el('div', { class: 'swarm-provider muted', text:
        `${p.name || shortHash(p.key || '')} — ${fmtBytes(p.bytes || 0)}`
        + (p.chunks ? `, ${p.chunks} chunks` : '')
        + (p.failures ? `, ${p.failures} failed` : '')
        + (p.dropped ? ' (dropped)' : '') })),
    ]));
  }

  const holders = d.holders || [];
  if (holders.length) {
    kids.push(el('div', { class: 'swarm-holders' }, [
      el('h4', { text: `Who else has this (${holders.length})` }),
      ...holders.map(hd => el('div', { class: 'swarm-holder muted', text:
        `${hd.name || 'unnamed node'} · ${shortHash(hd.key || '')}`
        + (hd.title ? ` — calls it “${hd.title}”` : '') })),
    ]));
  } else if (federationOn) {
    kids.push(el('p', { class: 'muted', text: 'No node currently advertises these bytes.' }));
  }

  host.replaceChildren(...kids);
  return host;
}

async function toggleExpand(f) {
  if (expanded.has(f.hash)) {
    expanded.delete(f.hash);
    vlist?.refresh();
    return;
  }
  expanded.add(f.hash);
  vlist?.refresh();
  if (!details.has(f.hash)) {
    try {
      details.set(f.hash, await req(`${API}/api/admin/swarm/${f.hash}`));
    } catch {
      details.set(f.hash, { ...f });
    }
    vlist?.refresh();
  }
}

// ── The ⋯ menu ───────────────────────────────────────────────────────────────
// Under a menu to keep the row quiet, as asked. Library blobs are deliberately
// NOT deletable here — deletion of our own content stays where the recording
// context makes it a safe decision, and the menu links there instead.
function openMenu(anchor, f) {
  const items = [
    { label: 'Play', onClick: () => playRow(f) },
    { label: expanded.has(f.hash) ? 'Hide details' : 'Details', onClick: () => toggleExpand(f) },
    { label: 'Download to this device', onClick: () => { window.location.href = audioURL(f, true); } },
  ];
  if (f.in_cache && canUpload && federationOn) {
    items.push({ label: 'Materialize into the library', onClick: () => materialize(f) });
  }
  if (f.in_library && f.recording_id) {
    items.push({
      label: 'Open in library',
      onClick: () => { window.location.href = `/admin/library#recordings-${f.recording_id}`; },
    });
  }
  if (f.in_cache) {
    items.push({
      label: 'Remove from cache',
      onClick: () => confirm({
        title: 'Remove from cache',
        body: `Remove “${dispName(f)}” (${fmtBytes(f.byte_size)}) from the download cache? `
          + 'This node stops seeding it, and it can be fetched again while somebody holds it.',
        run: async () => {
          // The cache page's remover — this page mints no second one.
          const d = await req(`${API}/api/admin/cache/bulk`, {
            method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ action: 'remove', hashes: [f.hash] }),
          });
          toast(`Removed, freed ${fmtBytes(d.bytes)}.`, 'success');
          reload(); loadSummary();
        },
      }),
    });
  }
  if (canManage && (f.up_bytes || f.down_bytes)) {
    items.push({
      label: 'Forget its traffic history',
      onClick: () => confirm({
        title: 'Forget traffic history',
        body: `Forget what “${dispName(f)}” has moved? The bytes really did move, so this also `
          + 'lowers this node’s all-time totals. The file itself is not touched.',
        run: async () => {
          await req(`${API}/api/admin/swarm/stats/forget`, {
            method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ hashes: [f.hash] }),
          });
          toast('Traffic history forgotten.', 'success');
          reload(); loadSummary();
        },
      }),
    });
  }
  openRowMenu(anchor, items);
}

async function materialize(f) {
  try {
    await req(`${API}/api/madnetwork/download`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ hash: f.hash }),
    });
    toast(`“${dispName(f)}” is being added to the library — find it under My uploads.`, 'success');
  } catch (err) { toast(`Couldn’t materialize: ${err.message}`, 'error'); }
}

// ── Loading ──────────────────────────────────────────────────────────────────
function params(offset) {
  const p = new URLSearchParams({ limit: String(PAGE), offset: String(offset), scope: state.scope });
  if (state.pill) p.set('state', state.pill);
  if (state.q) p.set('q', state.q);
  if (state.sort) p.set('sort', state.sort);
  return p;
}

async function fetchPage(offset) {
  return req(`${API}/api/admin/swarm?${params(offset).toString()}`);
}

async function reload() {
  let first;
  try { first = await fetchPage(0); }
  catch (err) {
    listHost.replaceChildren(el('p', { class: 'error', text: `Failed to load: ${err.message}` }));
    return;
  }
  total = first.total || 0;
  countEl.textContent = total
    ? `${total} file${total === 1 ? '' : 's'} · ${fmtBytes(first.bytes || 0)}`
    : '';
  if (!total) {
    listHost.replaceChildren(el('p', { class: 'muted', text: emptyText() }));
    return;
  }
  vlist.setItems(first.items || [], { keepScroll: true });
}

// The empty message has to distinguish "you have none of these" from "none
// match what you asked for". Saying "no files in the library yet" while a filter
// is hiding sixty of them is simply false.
const PILL_EMPTY = {
  live: 'Nothing published — no approved, untrashed file here.',
  review: 'Nothing waiting for review.',
  private: 'Nothing scoped Local — every file here is shared at some depth.',
  trashed: 'Nothing in the trash.',
};

function emptyText() {
  if (state.q) return 'Nothing matches that search.';
  if (state.pill) return PILL_EMPTY[state.pill] || 'Nothing matches that filter.';
  if (state.scope === 'cache') return 'Nothing cached yet. Files land here when this node fetches from the madnetwork.';
  if (state.scope === 'library') return 'No files in the library yet.';
  return 'No files yet — nothing uploaded and nothing fetched.';
}

// ── Live poll ────────────────────────────────────────────────────────────────
// Patches the strip, the pinned block and the traffic cells of VISIBLE rows.
// It never re-renders the list, which would fight the virtual window and lose
// the open panels.
async function poll() {
  if (document.hidden) return schedule(POLL_IDLE);
  const items = vlist ? vlist.getItems() : [];
  const p = new URLSearchParams();
  for (const f of items.slice(0, 200)) p.append('hash', f.hash);
  let live;
  try { live = await req(`${API}/api/admin/swarm/live?${p.toString()}`); }
  catch { return schedule(POLL_IDLE); }

  renderTransfers(live.active || []);
  const active = new Map((live.active || []).map(t => [t.hash, t]));
  let changed = false;
  for (const f of items) {
    const sess = live.rows?.[f.hash];
    const tr = active.get(f.hash);
    const wasActive = !!f.transfer;
    if (sess) f.session = sess;
    f.transfer = tr || undefined;
    if (sess || tr || wasActive) changed = true;
  }
  if (changed) vlist.refresh();
  // An open peers panel keeps up, throttled to its own interval: those figures
  // are all-time and move slowly, so polling them at the progress bars' rate
  // would be a request per two seconds to say the same thing.
  if (peersHost.open) loadPeers(false);
  schedule(active.size ? POLL_ACTIVE : POLL_IDLE);
}

function schedule(ms) {
  clearTimeout(pollTimer);
  pollTimer = setTimeout(poll, ms);
}
document.addEventListener('visibilitychange', () => { if (!document.hidden) schedule(500); });

// ── Controls ─────────────────────────────────────────────────────────────────
function buildControls() {
  const scopes = [
    ['all', 'All files'],
    ['library', 'In library'],
    ['cache', 'Cached'],
  ];
  const scopeHost = document.getElementById('swarmScope');
  scopeHost.replaceChildren(...scopes.map(([id, label]) => el('button', {
    class: `view-tab${state.scope === id ? ' view-tab--active' : ''}`,
    type: 'button', role: 'tab', 'aria-selected': String(state.scope === id),
    onclick: () => { state.scope = id; buildControls(); reload(); },
  }, [label])));

  const pills = [
    ['', 'Any state'],
    ['live', 'Published'],
    ['review', 'In review'],
    ['private', 'Local only'],
    ['trashed', 'Trashed'],
  ];
  const pillHost = document.getElementById('swarmPills');
  // The state pills describe the library half, so they are meaningless — and
  // would silently empty the list — while the Cached scope is selected.
  pillHost.hidden = state.scope === 'cache';
  pillHost.replaceChildren(...pills.map(([id, label]) => el('button', {
    class: `swarm-pill${state.pill === id ? ' swarm-pill--active' : ''}`,
    type: 'button',
    onclick: () => { state.pill = id; buildControls(); reload(); },
  }, [label])));
}

function wireControls() {
  const qEl = document.getElementById('swarmQ');
  let debounce = null;
  qEl.addEventListener('input', () => {
    clearTimeout(debounce);
    debounce = setTimeout(() => { state.q = qEl.value.trim(); reload(); }, 250);
  });
  document.getElementById('swarmSort').addEventListener('change', e => {
    state.sort = e.target.value;
    reload();
  });
}

// ── Boot ─────────────────────────────────────────────────────────────────────
(async function init() {
  const identity = await bootAdmin({ require: 'file.delete' });
  if (!identity) return;
  const perms = identity.permissions || [];
  canUpload = perms.includes('file.upload');
  canManage = perms.includes('user.manage');

  vlist = createVirtualList({
    windowScroll: true,
    sizerEl: listHost,
    makeSpacer: px => el('div', { style: `height:${px}px` }),
    renderRow,
    estimateHeight: f => (expanded.has(f.hash) ? 320 : 56),
    buffer: 6,
    fetchMore: async () => {
      const have = vlist.count();
      if (have >= total) return { items: [], done: true };
      const page = await fetchPage(have);
      total = page.total || total;
      return { items: page.items || [], done: have + (page.items || []).length >= total };
    },
  });

  buildControls();
  wireControls();
  await loadSummary();   // reports whether a node runs, which the menu depends on
  await reload();
  schedule(POLL_ACTIVE);
})();
