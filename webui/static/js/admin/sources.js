// Admin · Data sources — manage in-place "symlink" imports. Lists each symlink
// source (root, scan summary, status), the links-storage health (count / broken /
// external bytes), and an Add form constrained to the configured symlink_roots
// (hidden when none). A freshly added source scans in the background, so this page
// polls GET /api/admin/sources until no source is scanning. Moderator-accessible
// (the API gates POST/GET on content.moderate). Design: docs/architecture/data-sources.md.
import { bootAdmin, API, fmtBytes, fmtDate, toast, handleAuthError, el } from './shared.js';

const addForm      = document.getElementById('addForm');
const disabledNote = document.getElementById('disabledNote');
const nameInput    = document.getElementById('srcName');
const rootSelect   = document.getElementById('srcRoot');
const subInput     = document.getElementById('srcSub');
const addBtn       = document.getElementById('addBtn');
const linksHealth  = document.getElementById('linksHealth');
const sourcesList  = document.getElementById('sourcesList');

const POLL_MS = 1500;
let pollTimer = null;

addForm.addEventListener('submit', onAdd);

// ── Load + poll ───────────────────────────────────────────────────────────────
function startPolling() { if (!pollTimer) pollTimer = setInterval(load, POLL_MS); }
function stopPolling()  { if (pollTimer) { clearInterval(pollTimer); pollTimer = null; } }

async function load() {
  let data;
  try {
    const res = await fetch(`${API}/api/admin/sources`);
    if (handleAuthError(res)) { stopPolling(); return; }
    if (res.status === 503) { stopPolling(); renderUnavailable(); return; }
    data = await res.json().catch(() => ({}));
    if (!res.ok) return; // transient — keep the current view, retry next tick
  } catch { return; } // network blip — retry next tick
  render(data);
}

function render(data) {
  if (data.enabled) {
    fillRoots(data.roots || []);
    addForm.hidden = false;
    disabledNote.hidden = true;
  } else {
    addForm.hidden = true;
    disabledNote.hidden = false;
  }
  renderHealth(data.links);
  renderSources(data.sources || []);
  // Keep polling while any source is mid-scan; stop once all are settled.
  if ((data.sources || []).some(s => s.status === 'scanning')) startPolling();
  else stopPolling();
}

// fillRoots rebuilds the root <select> only when the option set changed, so a
// background poll never clobbers the operator's current selection mid-edit.
function fillRoots(roots) {
  const current = Array.from(rootSelect.options).map(o => o.value).join('\n');
  if (current === roots.join('\n')) return;
  rootSelect.replaceChildren(...roots.map(r => el('option', { value: r, text: r })));
}

// ── Render: links health ──────────────────────────────────────────────────────
function renderHealth(links) {
  if (!links) { linksHealth.hidden = true; return; }
  linksHealth.hidden = false;
  const broken = links.broken || 0;
  const parts = [
    el('span', { class: 'lh-stat', text: `${links.count || 0} link${links.count === 1 ? '' : 's'}` }),
    el('span', { class: 'lh-sep', 'aria-hidden': 'true', text: '·' }),
    el('span', { class: 'lh-stat', text: `${fmtBytes(links.external_bytes || 0)} external` }),
  ];
  if (broken > 0) {
    parts.push(el('span', { class: 'lh-sep', 'aria-hidden': 'true', text: '·' }));
    parts.push(el('span', { class: 'lh-broken', text: `${broken} broken link${broken === 1 ? '' : 's'}` }));
  }
  linksHealth.className = 'links-health' + (broken > 0 ? ' is-warning' : '');
  // Build children in an array and filter nulls: replaceChildren is native and,
  // unlike el(), would insert a bare null as the literal text "null".
  const children = [
    el('span', { class: 'lh-label', text: 'Links storage' }),
    el('span', { class: 'lh-stats' }, parts),
  ];
  if (broken > 0) {
    children.push(el('a', { class: 'lh-action', href: '/admin/prune', text: 'Prune broken links →' }));
  }
  linksHealth.replaceChildren(...children);
}

// ── Render: source list ───────────────────────────────────────────────────────
function renderSources(sources) {
  if (!sources.length) {
    sourcesList.replaceChildren(el('p', { class: 'sources-empty', text: 'No imported directories yet.' }));
    return;
  }
  sourcesList.replaceChildren(...sources.map(sourceCard));
}

function sourceCard(s) {
  const head = el('div', { class: 'source-card-head' }, [
    el('span', { class: 'source-name', text: s.name || '(unnamed)' }),
    statusBadge(s.status),
  ]);
  const root = el('code', { class: 'source-root', title: s.root || '', text: s.root || '—' });

  const meta = [];
  const sum = s.summary;
  if (s.status === 'scanning') {
    meta.push(el('span', { class: 'source-meta', text: 'Scanning…' }));
  } else if (sum) {
    meta.push(el('span', { class: 'source-meta' }, [
      el('strong', { text: String(sum.linked || 0) }), ' linked',
      summaryTail(' · ', sum.skipped, 'skipped'),
      summaryTail(' · ', sum.failed, 'failed', sum.failed > 0),
      `  (of ${sum.scanned || 0} scanned)`,
    ]));
  }
  if (s.scanned_at) {
    meta.push(el('span', { class: 'source-when', text: `Last scan ${fmtDate(s.scanned_at)}` }));
  }

  // Per-source actions: Refresh (additive rescan) + Remove (relation-aware).
  // Both are disabled mid-scan; `el` sets attributes literally, so use the DOM
  // .disabled property rather than a (truthy) disabled="false" attribute.
  const scanning = s.status === 'scanning';
  const refreshBtn = el('button', {
    class: 'btn btn-sm btn-neutral', text: 'Refresh',
    title: 'Re-scan this folder for newly added files', onclick: () => onRefresh(s),
  });
  const removeBtn = el('button', {
    class: 'btn btn-sm btn-destructive-outline', text: 'Remove',
    title: 'Remove this source (unlinks its tracks; never touches your originals)',
    onclick: () => onRemove(s),
  });
  refreshBtn.disabled = scanning;
  removeBtn.disabled = scanning;
  const actions = el('div', { class: 'source-card-actions' }, [refreshBtn, removeBtn]);

  return el('div', { class: 'source-card is-' + (s.status || 'active') }, [
    head, root, el('div', { class: 'source-card-meta' }, meta), actions,
  ]);
}

function summaryTail(sep, n, label, warn = false) {
  return el('span', { class: warn ? 'sum-warn' : '' }, [sep, `${n || 0} ${label}`]);
}

function statusBadge(status) {
  if (status === 'scanning') {
    return el('span', { class: 'src-badge is-scanning' }, [
      el('span', { class: 'src-spinner', 'aria-hidden': 'true' }), 'scanning',
    ]);
  }
  if (status === 'error') return el('span', { class: 'src-badge is-error', text: 'error' });
  return el('span', { class: 'src-badge is-active', text: 'active' });
}

// ── Add a source ──────────────────────────────────────────────────────────────
async function onAdd(e) {
  e.preventDefault();
  const name = nameInput.value.trim();
  const base = rootSelect.value;
  const sub  = subInput.value.trim().replace(/^\/+/, '');
  if (!name) { toast('A name is required.', 'error'); nameInput.focus(); return; }
  if (!base) { toast('Choose an allowed root.', 'error'); return; }
  const root = sub ? base.replace(/\/+$/, '') + '/' + sub : base;

  addBtn.disabled = true;
  addBtn.setAttribute('aria-busy', 'true');
  try {
    const res = await fetch(`${API}/api/admin/sources`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ kind: 'symlink', name, root }),
    });
    if (handleAuthError(res)) return;
    const body = await res.json().catch(() => ({}));
    if (res.status === 201) {
      toast(`Scanning “${name}”…`, 'info');
      nameInput.value = '';
      subInput.value = '';
      load(); // show the new scanning source and begin polling
      return;
    }
    const msg = {
      403: body.error || 'That folder is not under an allowed root.',
      409: 'A scan is already running — try again when it finishes.',
      503: 'Symlink imports are not configured.',
    }[res.status] || body.error || `Could not add source (HTTP ${res.status}).`;
    toast(msg, 'error');
  } catch (err) {
    toast(`Add failed: ${err.message}`, 'error');
  } finally {
    addBtn.disabled = false;
    addBtn.removeAttribute('aria-busy');
  }
}

// ── Refresh (additive rescan) ───────────────────────────────────────────────────
async function onRefresh(s) {
  try {
    const res = await fetch(`${API}/api/admin/sources/${encodeURIComponent(s.id)}/rescan`, { method: 'POST' });
    if (handleAuthError(res)) return;
    const body = await res.json().catch(() => ({}));
    if (res.ok) {
      toast(`Rescanning “${s.name}”…`, 'info');
      load(); // reflect the scanning status and begin polling
      return;
    }
    const msg = {
      404: 'That source no longer exists.',
      403: body.error || 'That folder is no longer under an allowed root.',
      409: 'A scan is already running — try again when it finishes.',
      503: 'Symlink imports are not configured.',
    }[res.status] || body.error || `Could not rescan (HTTP ${res.status}).`;
    toast(msg, 'error');
  } catch (err) {
    toast(`Rescan failed: ${err.message}`, 'error');
  }
}

// ── Remove (relation-aware) ─────────────────────────────────────────────────────
// Dry-run the removal first so the confirm dialog states the real consequences
// (how many records are deleted vs kept because another source/local upload still
// references them). Originals are never touched — only our symlinks.
async function onRemove(s) {
  let prev;
  try {
    const res = await fetch(`${API}/api/admin/sources/${encodeURIComponent(s.id)}/removal-preview`);
    if (handleAuthError(res)) return;
    prev = await res.json().catch(() => ({}));
    if (!res.ok) { toast(prev.error || `Could not prepare removal (HTTP ${res.status}).`, 'error'); return; }
  } catch (err) {
    toast(`Could not prepare removal: ${err.message}`, 'error');
    return;
  }

  const remove = prev.will_remove || 0;
  const keep = prev.will_keep || 0;
  let msg = `Remove the source “${s.name}”?\n\n`;
  if (remove === 0) {
    msg += keep > 0
      ? `No tracks will be deleted — all ${keep} are still referenced elsewhere (another source or a local upload).`
      : 'No tracks are attributed to this source; only the source record is removed.';
  } else {
    msg += `${remove} track${remove === 1 ? '' : 's'} will be removed (their links only — your original files are never touched).`;
    if (keep > 0) msg += `\n${keep} shared/owned track${keep === 1 ? '' : 's'} will be kept.`;
  }
  if (!confirm(msg)) return;

  try {
    const res = await fetch(`${API}/api/admin/sources/${encodeURIComponent(s.id)}`, { method: 'DELETE' });
    if (handleAuthError(res)) return;
    const body = await res.json().catch(() => ({}));
    if (res.ok) {
      toast(`Removed “${s.name}” — ${body.removed || 0} removed, ${body.kept || 0} kept.`, 'info');
      load();
      return;
    }
    const m = {
      404: 'That source no longer exists.',
      409: 'A scan is running — try again when it finishes.',
    }[res.status] || body.error || `Could not remove source (HTTP ${res.status}).`;
    toast(m, 'error');
  } catch (err) {
    toast(`Remove failed: ${err.message}`, 'error');
  }
}

function renderUnavailable() {
  addForm.hidden = true;
  disabledNote.hidden = true;
  linksHealth.hidden = true;
  sourcesList.replaceChildren(el('p', { class: 'sources-empty', text: 'Data sources are unavailable on this server.' }));
}

// ── Boot ──────────────────────────────────────────────────────────────────────
(async function boot() {
  if (!await bootAdmin({ require: 'content.moderate' })) return;
  await load();
})();
