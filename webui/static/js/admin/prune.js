// Admin · Verify & Prune — drives the single, server-wide prune background job.
// The scan (deep especially) now runs detached on the server, so this page
// starts it, then polls GET /api/admin/prune/status for the shared state: any
// admin opening this page sees the same in-progress run (with a Cancel button)
// or the last-run summary. Requires file.delete. Design: docs/architecture/prune-job.md.
import { bootAdmin, API, shortHash, toast, handleAuthError, el } from './shared.js';

const previewBtn    = document.getElementById('previewPrune');
const pruneResults  = document.getElementById('pruneResults');
const pruneStatus   = document.getElementById('pruneStatus');
const pruneControls = document.getElementById('pruneControls');
const deepVerify    = document.getElementById('deepVerify');

const POLL_MS = 1500;
let pollTimer = null;

previewBtn.addEventListener('click', () => startRun({ confirm: false, deep: deepVerify.checked }));

// ── Status polling ───────────────────────────────────────────────────────────
function startPolling() {
  if (pollTimer) return;
  pollTimer = setInterval(refreshStatus, POLL_MS);
}
function stopPolling() {
  if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
}

async function refreshStatus() {
  try {
    const res = await fetch(`${API}/api/admin/prune/status`);
    if (handleAuthError(res)) { stopPolling(); return; }
    const snap = await res.json().catch(() => ({}));
    if (!res.ok) return; // transient — keep the current view, retry next tick
    render(snap);
  } catch { /* network blip — retry next tick */ }
}

// ── Start a run (scan or prune) ──────────────────────────────────────────────
async function startRun(body) {
  setControlsBusy(true);
  let snap, status;
  try {
    const res = await fetch(`${API}/api/admin/prune`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (handleAuthError(res)) return;
    status = res.status;
    snap = await res.json().catch(() => ({}));
  } catch (err) {
    toast(`Prune failed to start: ${err.message}`, 'error');
    setControlsBusy(false);
    return;
  }

  // 409 = someone is already running (busy) or there is no scan to prune.
  if (status === 409) {
    if (snap.state === 'running') {
      toast('A prune is already running.', 'info');
      render(snap); // show their in-progress run
    } else {
      toast(snap.error || 'Run a scan first.', 'error');
      render(snap);
    }
    return;
  }
  if (status !== 202) {
    toast(snap.error || `Could not start (HTTP ${status})`, 'error');
    setControlsBusy(false);
    return;
  }
  render(snap); // 202 — render the freshly started run and begin polling
}

async function cancelRun() {
  try {
    const res = await fetch(`${API}/api/admin/prune/cancel`, { method: 'POST' });
    if (handleAuthError(res)) return;
    toast('Cancelling…', 'info');
  } catch (err) {
    toast(`Cancel failed: ${err.message}`, 'error');
    return;
  }
  refreshStatus(); // reflect the new state promptly
}

// ── Render from a status snapshot ────────────────────────────────────────────
function render(snap) {
  if (snap.state === 'running') {
    renderRunning(snap);
    startPolling();
  } else {
    stopPolling();
    renderIdle(snap);
  }
}

function setControlsBusy(busy) {
  previewBtn.disabled = busy;
  deepVerify.disabled = busy;
  previewBtn.setAttribute('aria-busy', busy ? 'true' : 'false');
}

function renderRunning(snap) {
  setControlsBusy(true);
  pruneControls.hidden = true;
  pruneResults.replaceChildren();

  const verb = snap.phase === 'pruning' ? 'Pruning' : (snap.deep ? 'Deep scanning' : 'Scanning');
  const p = snap.progress || {};
  const counted = p.total ? `${p.scanned} of ${p.total}` : `${p.scanned || 0}`;
  const head = el('div', { class: 'result-panel-head' }, [
    el('span', { class: 'prune-spinner', 'aria-hidden': 'true' }),
    el('span', { text: `${verb}… (${counted})` }),
  ]);

  const bar = el('div', { class: 'prune-progress', role: 'progressbar', 'aria-label': verb });
  const fill = el('div', { class: 'prune-progress-fill' });
  if (p.total) fill.style.width = Math.round((p.scanned / p.total) * 100) + '%';
  bar.appendChild(fill);

  const meta = el('p', { class: 'prune-meta', text:
    `Started${snap.started_by ? ` by ${snap.started_by}` : ''}${snap.started_at ? ` · ${fmtWhen(snap.started_at)}` : ''}` });

  pruneStatus.replaceChildren(el('div', { class: 'result-panel is-running' }, [
    head, bar, meta,
    el('button', { class: 'btn btn-neutral prune-cancel', text: 'Cancel', onclick: cancelRun }),
  ]));
}

function renderIdle(snap) {
  setControlsBusy(false);
  pruneControls.hidden = false;
  pruneStatus.replaceChildren(buildLastRunLines(snap));

  // Held detail of the most recent in-process run (gone after a restart).
  const r = snap.last_result;
  if (!r) { pruneResults.replaceChildren(); return; }
  if (r.kind === 'scan') {
    if ((r.dangling_count || 0) === 0) {
      renderPrunePanel('success', 'All records verified',
        `${r.scanned} file${r.scanned === 1 ? '' : 's'} checked, nothing to prune.`);
    } else {
      renderDanglingPanel(r);
    }
  } else {
    renderPruneCommitResult(r);
  }
}

// Compact "last scan / last prune" lines with their date — survive a restart.
function buildLastRunLines(snap) {
  const lines = [];
  if (snap.last_scan) {
    const s = snap.last_scan;
    lines.push(lastRunLine('Last scan',
      `${s.dangling_count || 0} dangling of ${s.scanned} checked` + (s.deep ? ' · deep' : ''), s));
  }
  if (snap.last_prune) {
    const s = snap.last_prune;
    let summary = `${s.pruned_count || 0} record${s.pruned_count === 1 ? '' : 's'} removed`;
    if (s.failed_count) summary += `, ${s.failed_count} failed`;
    lines.push(lastRunLine('Last prune', summary + (s.deep ? ' · deep' : ''), s));
  }
  return el('div', { class: 'prune-lastrun' }, lines);
}

function lastRunLine(label, summary, s) {
  const tail = [];
  if (s.outcome && s.outcome !== 'completed') tail.push(s.outcome);
  if (s.by) tail.push(`by ${s.by}`);
  if (s.finished_at) tail.push(fmtWhen(s.finished_at));
  return el('p', { class: 'prune-lastrun-line' }, [
    el('span', { class: 'prune-lastrun-label', text: label + ':' }),
    el('span', { text: ` ${summary}` }),
    tail.length ? el('span', { class: 'prune-lastrun-meta', text: ` · ${tail.join(' · ')}` }) : null,
  ]);
}

function fmtWhen(iso) {
  const d = new Date(iso);
  return isNaN(d) ? '' : d.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
}

// ── Result panels (idle detail) ──────────────────────────────────────────────
function renderPrunePanel(kind, title, detail) {
  const children = [
    el('div', { class: 'result-panel-head' }, [
      el('span', { class: 'result-panel-icon', 'aria-hidden': 'true', text: kind === 'success' ? '✓' : kind === 'error' ? '✕' : '⚠' }),
      el('span', { text: title }),
    ]),
  ];
  if (detail) children.push(el('p', { text: detail }));
  pruneResults.replaceChildren(el('div', { class: 'result-panel is-' + kind }, children));
}

function renderDanglingPanel(data) {
  const n = data.dangling_count;
  pruneResults.replaceChildren(el('div', { class: 'result-panel is-warning' }, [
    el('div', { class: 'result-panel-head' }, [
      el('span', { class: 'result-panel-icon', 'aria-hidden': 'true', text: '⚠' }),
      el('span', { text: `${n} dangling record${n === 1 ? '' : 's'} found (of ${data.scanned} checked).` }),
    ]),
    el('p', { text: data.deep
      ? 'These files are missing from disk or their contents are corrupted.'
      : 'These point to files missing from disk.' }),
    buildDanglingList(data.dangling),
    el('button', {
      class: 'btn btn-destructive-solid', text: `Prune ${n} record${n === 1 ? '' : 's'}`,
      onclick: () => openPruneModal(n),
    }),
  ]));
}

function buildDanglingList(entries) {
  const items = (entries || []).map(entry => {
    const fn = Array.isArray(entry.filenames) && entry.filenames.length
      ? entry.filenames.join(', ')
      : '(no filename)';
    const li = el('li', {}, [
      el('span', { class: 'dangling-name', text: fn }),
      el('span', { class: 'dangling-hash', title: entry.hash || '', text: shortHash(entry.hash) }),
    ]);
    if (entry.reason) li.appendChild(el('span', { class: 'dangling-reason is-' + entry.reason, text: entry.reason }));
    return li;
  });
  return el('ul', { class: 'dangling-list' }, items);
}

// ── Prune confirmation modal (focus trap, Esc to close) ──────────────────────
const pruneModal      = document.getElementById('pruneModal');
const pruneModalBody  = document.getElementById('pruneModalBody');
const confirmPruneBtn = document.getElementById('confirmPrune');
const cancelPruneBtn  = document.getElementById('cancelPrune');
const closePruneBtn   = document.getElementById('closePruneModal');

let lastFocusBeforeModal = null;

function openPruneModal(count) {
  lastFocusBeforeModal = document.activeElement;
  document.getElementById('pruneModalTitle').textContent =
    `Prune ${count} record${count === 1 ? '' : 's'}?`;
  pruneModalBody.textContent =
    `This permanently removes ${count} database record${count === 1 ? '' : 's'} whose ` +
    `file${count === 1 ? ' is' : 's are'} already gone. This cannot be undone.`;
  confirmPruneBtn.textContent = `Prune ${count} record${count === 1 ? '' : 's'}`;
  confirmPruneBtn.disabled = false;
  confirmPruneBtn.removeAttribute('aria-busy');

  pruneModal.classList.remove('hidden');
  cancelPruneBtn.focus();
}

function closePruneModal() {
  pruneModal.classList.add('hidden');
  lastFocusBeforeModal?.focus?.();
}

cancelPruneBtn.addEventListener('click', closePruneModal);
closePruneBtn.addEventListener('click', closePruneModal);
pruneModal.addEventListener('click', e => { if (e.target === pruneModal) closePruneModal(); });
confirmPruneBtn.addEventListener('click', commitPrune);

pruneModal.addEventListener('keydown', e => {
  if (e.key === 'Escape') { closePruneModal(); return; }
  if (e.key !== 'Tab') return;
  const focusable = pruneModal.querySelectorAll(
    'button:not([disabled]), [href], input, [tabindex]:not([tabindex="-1"])'
  );
  if (!focusable.length) return;
  const first = focusable[0];
  const last  = focusable[focusable.length - 1];
  if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
  else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
});

// commitPrune starts the prune job (deletes exactly the reviewed set the server
// still holds from the last scan); the running state is then shown via polling.
async function commitPrune() {
  closePruneModal();
  await startRun({ confirm: true });
}

// ── Boot ────────────────────────────────────────────────────────────────────
(async function boot() {
  if (!await bootAdmin({ require: 'file.delete' })) return;
  await refreshStatus(); // shared state first: maybe a run is already going
})();
