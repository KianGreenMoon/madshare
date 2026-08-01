// Admin · Upgrades — renditions the madnetwork holds that outrank ours
// (federation F8 item 3, docs/architecture/federation.md §Quality upgrades).
//
// The findings are written by the catalog sweep, not by this page: opening it
// starts nothing and scans nothing. It reads, and it offers two decisions.
//
//   GET   /api/admin/upgrades?disposition=&limit=&offset=
//   PATCH /api/admin/upgrades/{id}          {"disposition": …}
//   POST  /api/madnetwork/download          {"hash": …}   ← Materialize
//
// Materialize deliberately goes through the ORDINARY download path rather than
// anything of this page's own: the bytes land in the review bucket and get
// re-fingerprinted locally, exactly like a track pulled from /madnetwork. There
// is one way into the library, and this is not a second one.
//
// Moderator-accessible; Materialize additionally needs file.upload.
import { API, el, fmtBytes, fmtDate, shortHash, toast, handleAuthError, bootAdmin } from './shared.js';

const results = document.getElementById('upgResults');
const loading = document.getElementById('upgLoading');
const countEl = document.getElementById('upgCount');
const tabs = [...document.querySelectorAll('.view-tab[data-disposition]')];

let disposition = 'new';
let canMaterialize = false;

// techLabel renders the tech fields the quality ladder ranks on. A dash where
// ffprobe never ran — on either side, since the remote figures are the origin's
// and it may not run ffprobe either.
function techLabel(r) {
  if (!r) return '—';
  const parts = [];
  if (r.codec) parts.push(String(r.codec).toUpperCase());
  const q = [];
  if (r.bit_depth) q.push(`${r.bit_depth}-bit`);
  if (r.sample_rate) q.push(`${(r.sample_rate / 1000).toFixed(r.sample_rate % 1000 ? 1 : 0)} kHz`);
  if (!q.length && r.bitrate) q.push(`${Math.round(r.bitrate / 1000)} kbps`);
  if (q.length) parts.push(q.join(' / '));
  if (r.byte_size) parts.push(fmtBytes(r.byte_size));
  return parts.join(' · ') || '—';
}

// evidenceLabel states how a remote rendition was tied to our recording. It is
// always shown: a page proposing to fetch tens of megabytes should say on what
// grounds, and the two grounds are genuinely different in strength.
function evidenceLabel(u) {
  return u.match === 'fingerprint'
    ? `fingerprint match · BER ${Number(u.ber || 0).toFixed(3)}`
    : 'they hold our exact bytes';
}

async function load() {
  loading.hidden = false;
  try {
    const res = await fetch(`${API}/api/admin/upgrades?disposition=${encodeURIComponent(disposition)}`);
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const data = await res.json();
    render(data.items || [], data.total || 0);
  } catch (err) {
    results.replaceChildren(el('p', { class: 'muted', text: `Couldn’t load upgrades: ${err.message}` }));
  } finally {
    loading.hidden = true;
  }
}

function render(items, total) {
  countEl.textContent = total ? `${total} finding${total === 1 ? '' : 's'}` : '';
  if (!items.length) {
    results.replaceChildren(el('p', { class: 'muted', text: disposition === 'new'
      ? 'Nothing to upgrade. Findings appear here as catalogs sync — there is nothing to start.'
      : 'No findings with that status.' }));
    return;
  }
  results.replaceChildren(el('div', { class: 'upg-list' }, items.map(row)));
}

function row(u) {
  const head = el('div', { class: 'upg-head' }, [
    el('div', { class: 'upg-name' }, [
      el('div', { class: 'upg-title', text: u.title || `Recording #${u.recording_id}` }),
      el('div', { class: 'upg-sub', text: u.artist || '—' }),
    ]),
    el('a', { class: 'linklike upg-link', href: `/admin/library#recordings-${u.recording_id}`,
      text: `recording #${u.recording_id}` }),
  ]);

  const compare = el('table', { class: 'upg-compare' }, [
    el('thead', {}, el('tr', {}, [
      el('th', { text: '' }), el('th', { text: 'Rendition' }), el('th', { text: 'Blob' }),
    ])),
    el('tbody', {}, [
      el('tr', {}, [
        el('td', { class: 'upg-side', text: 'Yours' }),
        el('td', { text: techLabel(u.ours) }),
        el('td', { class: 'mono', text: u.ours && u.ours.hash ? shortHash(u.ours.hash) : '—' }),
      ]),
      el('tr', { class: 'is-offered' }, [
        el('td', { class: 'upg-side', text: 'Offered' }),
        el('td', {}, [techLabel(u.offered), el('span', { class: 'upg-claimed', text: 'claimed' })]),
        el('td', { class: 'mono', text: shortHash(u.remote_hash) }),
      ]),
    ]),
  ]);

  const meta = el('div', { class: 'upg-meta' }, [
    el('span', { class: 'upg-evidence', text: evidenceLabel(u) }),
    u.source ? el('span', {
      class: `upg-source${u.source_reachable ? '' : ' upg-source--stale'}`,
      text: u.source, title: u.source_reachable ? u.source_key : `${u.source_key} — not seen recently`,
    }) : el('span', { class: 'upg-source upg-source--stale', text: 'no source cached' }),
    el('span', { class: 'upg-seen', text: `first seen ${fmtDate(u.first_seen)}` }),
  ]);

  return el('div', { class: `upg-card is-${u.disposition}` }, [head, compare, meta, actions(u)]);
}

function actions(u) {
  const bar = el('div', { class: 'upg-actions' });
  if (u.disposition === 'new') {
    const fetchBtn = el('button', { class: 'btn btn-sm btn-primary', text: 'Materialize',
      title: 'Fetch it into the review bucket and add it to this recording',
      onclick: () => materialize(u, fetchBtn) });
    fetchBtn.disabled = !canMaterialize;
    if (!canMaterialize) fetchBtn.title = 'Needs the file.upload permission';
    bar.append(fetchBtn);
    bar.append(el('button', { class: 'btn btn-sm btn-neutral', text: 'Dismiss',
      title: 'Do not raise this again on the next sync', onclick: () => decide(u, 'dismissed') }));
  } else {
    bar.append(el('span', { class: 'upg-status', text: u.disposition === 'materialized'
      ? 'Fetched — check the review queue' : 'Dismissed' }));
    bar.append(el('button', { class: 'btn btn-sm btn-neutral', text: 'Reopen',
      onclick: () => decide(u, 'new') }));
  }
  return bar;
}

async function decide(u, next) {
  try {
    const res = await fetch(`${API}/api/admin/upgrades/${u.id}`, {
      method: 'PATCH', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ disposition: next }),
    });
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    toast(next === 'dismissed' ? 'Dismissed — the next sync will not raise it again.' : 'Reopened.', 'success');
    load();
  } catch (err) { toast(err.message, 'error'); }
}

// materialize hands the hash to the ordinary download path and only then marks
// the finding. Marking first would claim something that had not happened yet.
async function materialize(u, btn) {
  btn.disabled = true;
  btn.textContent = 'Fetching…';
  try {
    const res = await fetch(`${API}/api/madnetwork/download`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ hash: u.remote_hash }),
    });
    if (handleAuthError(res)) return;
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || `HTTP ${res.status}`);
    await decide(u, 'materialized');
    toast('Fetching — it lands in the review queue, and nothing of yours was replaced.', 'success');
  } catch (err) {
    toast(err.message, 'error');
    btn.disabled = false;
    btn.textContent = 'Materialize';
  }
}

tabs.forEach(tab => tab.addEventListener('click', () => {
  tabs.forEach(t => t.classList.toggle('is-active', t === tab));
  disposition = tab.dataset.disposition;
  load();
}));

bootAdmin({ require: 'content.moderate' }).then(identity => {
  if (!identity) return;
  canMaterialize = (identity.permissions || []).includes('file.upload');
  load();
});
