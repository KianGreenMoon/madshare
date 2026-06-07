// Admin · Verify & Prune — non-destructive scan for dangling DB records, then a
// confirmed prune. Requires file.delete.
import { bootAdmin, API, shortHash, toast, handleAuthError, el } from './shared.js';

const previewBtn   = document.getElementById('previewPrune');
const pruneResults = document.getElementById('pruneResults');
const deepVerify   = document.getElementById('deepVerify');

// Capture the scan mode used for the preview so the commit prunes exactly what
// was previewed even if the checkbox is toggled afterwards.
let lastPruneDeep = false;

previewBtn.addEventListener('click', runPreview);

async function runPreview() {
  previewBtn.disabled = true;
  previewBtn.setAttribute('aria-busy', 'true');
  const original = previewBtn.textContent;
  lastPruneDeep = deepVerify.checked;
  previewBtn.textContent = lastPruneDeep ? 'Verifying…' : 'Scanning…';

  let data;
  try {
    const res = await fetch(`${API}/api/admin/prune`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ confirm: false, deep: lastPruneDeep }),
    });
    if (handleAuthError(res)) return;
    data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
  } catch (err) {
    renderPrunePanel('error', 'Prune scan failed', err.message);
    toast(`Prune scan failed: ${err.message}`, 'error');
    return;
  } finally {
    previewBtn.disabled = false;
    previewBtn.removeAttribute('aria-busy');
    previewBtn.textContent = original;
  }

  if ((data.dangling_count || 0) === 0) {
    renderPrunePanel('success', 'All records verified',
      `${data.scanned} file${data.scanned === 1 ? '' : 's'} checked, nothing to prune.`);
    return;
  }
  renderDanglingPanel(data);
}

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

async function commitPrune() {
  confirmPruneBtn.disabled = true;
  cancelPruneBtn.disabled = true;
  confirmPruneBtn.setAttribute('aria-busy', 'true');
  const original = confirmPruneBtn.textContent;
  confirmPruneBtn.textContent = 'Pruning…';

  let data;
  try {
    const res = await fetch(`${API}/api/admin/prune`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ confirm: true, deep: lastPruneDeep }),
    });
    if (handleAuthError(res)) { closePruneModal(); return; }
    data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
  } catch (err) {
    cancelPruneBtn.disabled = false;
    confirmPruneBtn.disabled = false;
    confirmPruneBtn.removeAttribute('aria-busy');
    confirmPruneBtn.textContent = original;
    toast(`Prune failed: ${err.message}`, 'error');
    return;
  }

  closePruneModal();
  renderPruneCommitResult(data);

  const pruned = data.pruned_count || 0;
  const failed = (data.failed && data.failed.length) || 0;
  if (failed) toast(`Pruned ${pruned}, ${failed} failed.`, 'error');
  else        toast(`Pruned ${pruned} record${pruned === 1 ? '' : 's'}.`, 'success');
}

function renderPruneCommitResult(data) {
  const pruned = data.pruned_count || 0;
  const failed = data.failed || [];

  const children = [
    el('div', { class: 'result-panel-head' }, [
      el('span', { class: 'result-panel-icon', 'aria-hidden': 'true', text: failed.length ? '⚠' : '✓' }),
      el('span', { text: `Pruned ${pruned} record${pruned === 1 ? '' : 's'}.` }),
    ]),
  ];
  if (Array.isArray(data.pruned) && data.pruned.length) children.push(buildDanglingList(data.pruned));
  if (failed.length) {
    children.push(el('p', { text: `${failed.length} record${failed.length === 1 ? '' : 's'} could not be removed:` }));
    children.push(el('ul', { class: 'dangling-list' }, failed.map(entry =>
      el('li', {}, [
        el('span', { class: 'dangling-hash', title: entry.hash || '', text: shortHash(entry.hash) }),
        el('span', { class: 'dangling-name', text: entry.error || 'unknown error' }),
      ]))));
  }
  pruneResults.replaceChildren(el('div', { class: 'result-panel ' + (failed.length ? 'is-warning' : 'is-success') }, children));
}

// ── Boot ────────────────────────────────────────────────────────────────────
(async function boot() {
  await bootAdmin({ require: 'file.delete' });
})();
