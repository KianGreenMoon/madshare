// Admin · Madnetwork cache — see what the swarm fetched, and clean it up.
// Design: docs/architecture/madnetwork-cache.md.
//
// A scope over the shared file-list.js component, not a new list: windowed
// infinite scroll, the search box, per-row checkboxes, the "Select all N
// matching" banner and the bulk bar all already live there, over exactly the
// paged envelope GET /api/admin/cache returns.
//
// What is deliberately switched OFF is as telling as what is on: no tag
// editors, no access editor, no Browse view. These are another node's tags
// about another node's bytes — the moment you want to change them you are
// materializing into the library, where the real editors are.
import { API, el, fmtBytes, fmtDate, toast, handleAuthError, bootAdmin } from './shared.js';
import { createFileList } from '../file-list.js';
import { createPlayer } from '../player.js';
import { TRASH_ICON, DOWNLOAD_ICON, MATERIALIZE_ICON, INFO_ICON } from '../icons.js';

let canUpload = false;     // file.upload — the Materialize gate (the API enforces it too)
let federationOn = false;  // a node is running, so materializing has an endpoint to hit
let fileList = null;

const dispName = f => f.title || f.filename || f.hash.slice(0, 12) + '…';
const dispSub = f => [f.artist, f.album].filter(Boolean).join(' — ');

async function req(url, opts) {
  const res = await fetch(url, opts);
  if (handleAuthError(res)) throw new Error('Your session expired.');
  const data = await res.json().catch(() => ({}));
  if (!res.ok || data.ok === false) throw new Error(data.error || `HTTP ${res.status}`);
  return data;
}

// ── Preview player ───────────────────────────────────────────────────────────
// The "what IS this" button. A cached blob streams straight off disk through
// the ordinary relay, so nothing special is needed to hear one.
let playCtx = null;
const player = createPlayer({
  onPrev: () => playAt(playCtx && playCtx.index > 0 ? playCtx.index - 1 : 0),
  onNext: () => playAt(playCtx ? playCtx.index + 1 : 0),
  onEnded: () => playAt(playCtx ? playCtx.index + 1 : 0),
  onError: () => toast('Couldn’t play this cached file.', 'error'),
});
// audioURL reads the cache file directly, NOT through the madnetwork relay:
// that relay only exists while a federation node runs, and this page has to keep
// working after federation is switched off — which is exactly when someone comes
// here to reclaim the disk.
const audioURL = (hash, download) => `${API}/api/admin/cache/${hash}/audio${download ? '?download=1' : ''}`;

function playRow(f, visible) {
  const items = (visible || [f]).map(x => ({
    url: audioURL(x.hash),
    title: dispName(x), artist: x.artist || '', key: x.hash,
  }));
  let index = items.findIndex(x => x.key === f.hash);
  if (index < 0) index = 0;
  playCtx = { items, index };
  playAt(index);
}
function playAt(i) {
  if (!playCtx || i < 0 || i >= playCtx.items.length) return;
  playCtx.index = i;
  const it = playCtx.items[i];
  player.load({ url: it.url, title: it.title, artist: it.artist });
  fileList?.setPlaying(it.key);
}

// ── Confirmation modal ───────────────────────────────────────────────────────
const delModal = document.getElementById('delModal');
const delTitle = document.getElementById('delModalTitle');
const delBody = document.getElementById('delModalBody');
const delConfirm = document.getElementById('delConfirm');
const delCancel = document.getElementById('delCancel');
let pendingRun = null;

function confirmRemove({ title, body, run }) {
  delTitle.textContent = title;
  delBody.textContent = body;
  pendingRun = run;
  delModal.classList.remove('hidden');
}
function closeConfirm() {
  delModal.classList.add('hidden');
  pendingRun = null;
}
delCancel.addEventListener('click', closeConfirm);
delModal.addEventListener('click', e => { if (e.target === delModal) closeConfirm(); });
delConfirm.addEventListener('click', async () => {
  const run = pendingRun;
  closeConfirm();
  if (!run) return;
  try { await run(); } catch (err) { toast(err.message, 'error'); }
});

// ── Claims modal (the rare view) ─────────────────────────────────────────────
const claimsModal = document.getElementById('claimsModal');
const claimsBody = document.getElementById('claimsBody');
document.getElementById('claimsClose').addEventListener('click', () => claimsModal.classList.add('hidden'));
claimsModal.addEventListener('click', e => { if (e.target === claimsModal) claimsModal.classList.add('hidden'); });

async function showClaims(f) {
  claimsBody.replaceChildren(el('p', { class: 'muted', text: 'Loading…' }));
  claimsModal.classList.remove('hidden');
  let claims = [];
  try {
    claims = (await req(`${API}/api/admin/cache/${f.hash}/claims`)).claims || [];
  } catch (err) {
    claimsBody.replaceChildren(el('p', { class: 'muted', text: err.message }));
    return;
  }
  if (!claims.length) {
    claimsBody.replaceChildren(
      el('p', { class: 'muted' }, [
        'No node currently advertises this file. That does not make it any less ',
        'usable — it plays, downloads and materializes from the tags in the file itself.',
      ]),
    );
    return;
  }
  claimsBody.replaceChildren(
    el('p', { class: 'muted', text: 'What each node advertising these bytes calls them right now:' }),
    el('ul', { class: 'cache-claims' }, claims.map(c => el('li', {}, [
      el('div', { class: 'cache-claim-title', text: c.title || '(untitled)' }),
      el('div', { class: 'cache-claim-sub muted', text: [c.artist, c.album].filter(Boolean).join(' — ') || '—' }),
      el('div', { class: 'cache-claim-src muted', text: `${c.source_name || 'unnamed node'} · ${(c.source_key || '').slice(0, 12)}…` }),
    ]))),
  );
}

// ── Actions ──────────────────────────────────────────────────────────────────
async function removeCached(hashes) {
  const data = await req(`${API}/api/admin/cache/bulk`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ action: 'remove', hashes }),
  });
  toast(`Removed ${data.removed} file${data.removed === 1 ? '' : 's'}, freed ${fmtBytes(data.bytes)}.`, 'success');
  return data.removed;
}
async function removeMatching(filter) {
  const data = await req(`${API}/api/admin/cache/bulk`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ action: 'remove', all: true, filter: { q: filter.q, field: filter.field } }),
  });
  toast(`Removed ${data.removed} file${data.removed === 1 ? '' : 's'}, freed ${fmtBytes(data.bytes)}.`, 'success');
  return data.removed;
}

// materialize brings one cached blob into the local library through the review
// bucket. It needs no live claim — the bytes are here, and staging reads the
// tags out of the file exactly as an upload does.
async function materialize(f) {
  try {
    await req(`${API}/api/madnetwork/download`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ hash: f.hash }),
    });
    toast(`“${dispName(f)}” is being added to the library — find it under My uploads.`, 'success');
  } catch (err) {
    toast(`Couldn’t materialize: ${err.message}`, 'error');
  }
}

// ── Summary strip ────────────────────────────────────────────────────────────
async function loadSummary() {
  const host = document.getElementById('cacheSummary');
  let s;
  try {
    s = await req(`${API}/api/admin/cache/summary`);
  } catch {
    host.replaceChildren(el('p', { class: 'muted', text: 'Couldn’t read the cache summary.' }));
    return;
  }
  federationOn = !!s.federation;

  const stats = [
    el('span', { class: 'cache-stat' }, [
      el('strong', { text: String(s.entries || 0) }), ` file${s.entries === 1 ? '' : 's'} · `,
      el('strong', { text: fmtBytes(s.bytes || 0) }),
    ]),
  ];
  const flight = s.in_flight || [];
  if (flight.length) {
    const done = flight.reduce((n, t) => n + (t.progress || 0), 0);
    const all = flight.reduce((n, t) => n + (t.size || 0), 0);
    stats.push(el('span', { class: 'cache-stat cache-stat--live' },
      [`${flight.length} downloading (${fmtBytes(done)} / ${fmtBytes(all)})`]));
  }
  if (s.seeding) {
    const on = s.seeding.enabled && s.seeding.cache;
    stats.push(el('span', { class: 'cache-stat muted' },
      [on ? 'seeding these files to the community' : 'not seeding the cache']));
  }

  const rows = [el('div', { class: 'cache-stats' }, stats)];

  // Abandoned partials: the scratch files of fetches that died. Startup sweeps
  // them, so a non-zero count here means fetches have died since this process
  // started — worth showing, and worth being able to clear without a restart.
  const p = s.partials || { count: 0, bytes: 0 };
  if (p.count > 0) {
    rows.push(el('div', { class: 'cache-partials' }, [
      el('span', {}, [`${p.count} abandoned partial download${p.count === 1 ? '' : 's'} · ${fmtBytes(p.bytes)}`]),
      el('button', {
        class: 'btn btn-neutral btn-sm', type: 'button',
        onclick: () => confirmRemove({
          title: 'Reclaim abandoned partials',
          body: `Delete ${p.count} unfinished download${p.count === 1 ? '' : 's'} and free ${fmtBytes(p.bytes)}? `
              + 'Downloads running right now are not touched.',
          run: async () => {
            const d = await req(`${API}/api/admin/cache/partials/reap`, { method: 'POST' });
            toast(`Reclaimed ${fmtBytes(d.bytes)} from ${d.removed} partial download${d.removed === 1 ? '' : 's'}.`, 'success');
            loadSummary();
          },
        }),
      }, ['Reclaim']),
    ]));
  }

  rows.push(el('div', { class: 'cache-tools' }, [
    el('button', {
      class: 'linklike', type: 'button',
      title: 'Re-read the cache directory — for a cache that was changed on disk underneath the server',
      onclick: async () => {
        try {
          const d = await req(`${API}/api/admin/cache/rescan`, { method: 'POST' });
          toast(d.added || d.dropped
            ? `Rescanned: ${d.added} added, ${d.dropped} stale entr${d.dropped === 1 ? 'y' : 'ies'} dropped.`
            : 'Rescanned — the index already matched the directory.', 'success');
          fileList?.reload();
          loadSummary();
        } catch (err) { toast(err.message, 'error'); }
      },
    }, ['Rescan the cache directory']),
  ]));

  host.replaceChildren(...rows);
}

// ── The list ─────────────────────────────────────────────────────────────────
async function loadCachePage({ limit, offset, q, field, sort }) {
  const params = new URLSearchParams({ limit: String(limit), offset: String(offset) });
  if (q) params.set('q', q);
  if (field) params.set('field', field);
  if (sort) params.set('sort', sort);
  const data = await req(`${API}/api/admin/cache?${params.toString()}`);
  return { total: data.total || 0, items: data.items || [] };
}

function cacheScope() {
  return {
    title: 'Cached files',
    emptyText: 'Nothing cached. Files land here when something on this node plays or fetches from the madnetwork.',
    columns: ['check', 'title', 'artist', 'album', 'size', 'fetched', 'used', 'actions'],
    cells: {
      fetched: { label: 'Fetched', cls: 'col-when', render: f => el('td', { class: 'cell-when', 'data-label': 'Fetched' }, [fmtDate(f.fetched_at)]) },
      // The column that previews what a retention sweep would take first.
      used: { label: 'Last used', cls: 'col-when', render: f => el('td', { class: 'cell-when', 'data-label': 'Last used' }, [fmtDate(f.last_used_at)]) },
    },
    rowKey: f => f.hash,
    paged: true,
    pageSize: 100,
    apiBase: API,
    loadPage: loadCachePage,
    // Every row is removable; nothing here is editable, so there is no partial
    // selectability to express.
    selectable: () => true,
    sorts: [
      { id: 'newest', label: 'Newest fetched' },
      { id: 'oldest', label: 'Oldest fetched' },
      { id: 'lru', label: 'Least recently used' },
      { id: 'largest', label: 'Largest' },
      { id: 'smallest', label: 'Smallest' },
    ],
    rowActions: [
      // Materialize goes through the madnetwork download endpoint, which only
      // exists while a node runs. Playing, downloading and removing do not, so
      // this is the one action that disappears with federation switched off.
      ...(canUpload && federationOn ? [{
        id: 'materialize', label: 'Materialize into the library', icon: MATERIALIZE_ICON,
        run: async f => { await materialize(f); return false; },
      }] : []),
      {
        id: 'download', label: 'Download to this device', icon: DOWNLOAD_ICON,
        run: f => { window.location.href = audioURL(f.hash, true); return false; },
      },
      {
        id: 'claims', label: 'What the network calls this', icon: INFO_ICON,
        run: f => { showClaims(f); return false; },
      },
      {
        id: 'remove', label: 'Remove from cache', icon: TRASH_ICON, kind: 'danger',
        run: f => {
          confirmRemove({
            title: 'Remove from cache',
            body: `Remove “${dispName(f)}” (${fmtBytes(f.byte_size)})? `
                + 'This node stops seeding it, and it can be fetched again while somebody holds it.',
            run: async () => { await removeCached([f.hash]); fileList.reload(); loadSummary(); },
          });
          return false; // the modal owns the run and the reload
        },
      },
    ],
    bulkActions: [{
      id: 'remove', label: 'Remove from cache', kind: 'danger',
      run: keys => new Promise(resolve => {
        confirmRemove({
          title: 'Remove from cache',
          body: `Remove ${keys.length} cached file${keys.length === 1 ? '' : 's'}? `
              + 'This node stops seeding them, and they can be fetched again while somebody holds them.',
          run: async () => { await removeCached(keys); loadSummary(); resolve(true); },
        });
        delCancel.addEventListener('click', () => resolve(false), { once: true });
      }),
      // "Select all N matching" — the server resolves the set from the same
      // filter the page is showing, so it can never act on a different one.
      runAll: filter => new Promise(resolve => {
        confirmRemove({
          title: 'Remove every matching file',
          body: (filter.q ? `Remove every cached file matching “${filter.q}”? ` : 'Remove EVERY cached file? ')
              + 'This node stops seeding them, and they can be fetched again while somebody holds them.',
          run: async () => { await removeMatching(filter); loadSummary(); resolve(true); },
        });
        delCancel.addEventListener('click', () => resolve(false), { once: true });
      }),
    }],
    onPlay: playRow,
    toast, handleAuthError,
  };
}

// ── Boot ─────────────────────────────────────────────────────────────────────
(async function init() {
  const identity = await bootAdmin({ require: 'file.delete' });
  if (!identity) return;
  canUpload = (identity.permissions || []).includes('file.upload');

  // The summary first: it reports whether a federation node is running, and the
  // scope's action set depends on that. Offering a Materialize button that 404s
  // is worse than not offering one.
  await loadSummary();
  fileList = createFileList(cacheScope());
  fileList.mount(document.getElementById('cacheList'));
})();
