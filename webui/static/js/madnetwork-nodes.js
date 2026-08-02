// madnetwork-nodes.js — the node directory at /madnetwork/nodes
// (docs/ui/madnetwork-nodes.md §The directory).
//
// The list this page shows used to be the landing page's status strip: every
// node at once, as chips, with no room for the facts that distinguish them. It
// is a page now because the list grew — since F7 item 5 the sweep pulls from
// members nobody here chose, so "everyone we hold a catalog from" is a
// community-sized list rather than a handful of friendships.
//
// Ordering is the server's (hops, then the alphabet) and this page does not
// re-sort: one rule, one place. The filter box narrows, it never reorders.
//
// Shell page module: NO page DOM at module-eval time — everything inside init().
import { gatePage, PAGE_PERMS } from './auth.js';
import { fetchNodes, buildNodeRow, nodeName } from './mn-nodes.js';
import { mkSpan } from './mn-browse.js';

let abort = null;
let nodes = [];

export async function init() {
  if (!gatePage(PAGE_PERMS.madnetwork)) return;
  abort = new AbortController();
  nodes = [];
  wireFilter();
  await load();
}

export function teardown() {
  abort?.abort();
  abort = null;
  nodes = [];
}

async function load() {
  const list = document.getElementById('mnNodeList');
  if (!list) return;
  list.innerHTML = '<div class="panel-loading" aria-live="polite" role="status"></div>';
  let data;
  try {
    data = await fetchNodes();
  } catch {
    list.innerHTML = '<div class="panel-empty">Could not load the node list.</div>';
    return;
  }
  if (!abort || abort.signal.aborted) return; // navigated away meanwhile
  nodes = data.nodes;
  renderStatus(data);
  render('');
}

// The header line counts what the list is, in the vocabulary the rest of the
// section uses: libraries we hold a catalog from, how many an admin here chose
// personally, and how many are currently out of contact.
function renderStatus({ tracks, inboundHealthy }) {
  const box = document.getElementById('mnStatus');
  if (!box) return;
  box.replaceChildren();
  if (!inboundHealthy) {
    box.append(mkSpan('mn-status-warn',
      '⚠ This node can’t reach the mesh right now — freshness below is last-known.'));
  }
  const friends = nodes.filter(n => n.friend && !n.self).length;
  const stale = nodes.filter(n => !n.self && n.reachable === false).length;
  const bits = [`${nodes.length} librar${nodes.length === 1 ? 'y' : 'ies'}`];
  if (friends) bits.push(`${friends} direct friend${friends === 1 ? '' : 's'}`);
  if (stale) bits.push(`${stale} not seen recently`);
  bits.push(`${tracks.toLocaleString()} track${tracks === 1 ? '' : 's'} between them`);
  box.append(mkSpan('mn-status-main', bits.join(' · ')));
  box.hidden = false;
}

function render(query) {
  const list = document.getElementById('mnNodeList');
  if (!list) return;
  const q = query.trim().toLowerCase();
  // Name AND key, because they are trusted differently: a name beyond our own
  // friends is what a node claims about itself, and someone checking up on a
  // node arrives holding the key.
  const shown = q
    ? nodes.filter(n => nodeName(n).toLowerCase().includes(q) || (n.key || '').toLowerCase().includes(q))
    : nodes;

  const wrap = document.createElement('div');
  wrap.className = 'panel-fade-in';
  if (!shown.length) {
    wrap.innerHTML = nodes.length
      ? `<div class="panel-empty">No node matches “${escapeText(query)}”.</div>`
      : '<div class="panel-empty">No libraries yet — they appear once this node friends others on '
        + '<a href="/admin/network">Admin › Network</a>.</div>';
    list.replaceChildren(wrap);
    return;
  }
  for (const n of shown) wrap.append(buildNodeRow(n));
  list.replaceChildren(wrap);
}

function escapeText(s) {
  const d = document.createElement('div');
  d.textContent = s;
  return d.innerHTML;
}

function wireFilter() {
  const input = document.getElementById('mnNodeFilter');
  const clear = document.querySelector('.library-search__clear');
  if (!input) return;
  input.addEventListener('input', () => {
    if (clear) clear.style.display = input.value ? '' : 'none';
    render(input.value);
  }, { signal: abort.signal });
  clear?.addEventListener('click', () => {
    input.value = '';
    clear.style.display = 'none';
    render('');
    input.focus();
  }, { signal: abort.signal });
}
