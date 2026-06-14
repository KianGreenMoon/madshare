// Admin · Duplicates — recordings with more than one rendition of the same audio
// (recordings P2, docs/architecture/recordings.md). Lists each recording with
// its renditions ranked by the quality ladder, a keep/variant suggestion, a
// page-local preview player, and two human-confirmed actions: Delete (soft
// delete → Trash) and Split off (detach into a new pinned recording).
//
// Moderator-accessible: bootAdmin additionally requires content.moderate.
import { API, el, fmtBytes, fmtTime, toast, handleAuthError, bootAdmin } from './shared.js';

const results = document.getElementById('dupResults');
const loading = document.getElementById('dupLoading');

// ── Preview player (page-local) ───────────────────────────────────────────────
const player      = document.getElementById('dupPlayer');
const playerTitle = document.getElementById('dupPlayerTitle');
const audio       = document.getElementById('dupAudio');

function preview(r) {
  audio.src = API + r.url;
  playerTitle.textContent = `${r.title || r.hash}${r.artist ? ' — ' + r.artist : ''}`;
  player.classList.remove('hidden');
  audio.play().catch(() => {/* autoplay may be blocked; controls still work */});
}

// ── Delete confirmation modal ─────────────────────────────────────────────────
const delModal   = document.getElementById('delModal');
const delBody     = document.getElementById('delModalBody');
const delConfirm = document.getElementById('delConfirm');
const delCancel  = document.getElementById('delCancel');
const delClose   = document.getElementById('delClose');

function confirmDelete(r) {
  return new Promise(resolve => {
    delBody.textContent = `Send "${r.title || r.hash}" to Trash? The blob is kept and can be restored.`;
    delModal.classList.remove('hidden');
    delConfirm.focus();
    const cleanup = () => {
      delConfirm.removeEventListener('click', onOk);
      delCancel.removeEventListener('click', onCancel);
      delClose.removeEventListener('click', onCancel);
      delModal.removeEventListener('click', onBackdrop);
      document.removeEventListener('keydown', onKey);
    };
    const onOk      = () => { delModal.classList.add('hidden'); cleanup(); resolve(true); };
    const onCancel  = () => { delModal.classList.add('hidden'); cleanup(); resolve(false); };
    const onBackdrop = e => { if (e.target === delModal) onCancel(); };
    const onKey      = e => { if (e.key === 'Escape' && !delModal.classList.contains('hidden')) onCancel(); };
    delConfirm.addEventListener('click', onOk);
    delCancel.addEventListener('click', onCancel);
    delClose.addEventListener('click', onCancel);
    delModal.addEventListener('click', onBackdrop);
    document.addEventListener('keydown', onKey);
  });
}

// ── Actions ───────────────────────────────────────────────────────────────────
async function deleteRendition(r) {
  if (!(await confirmDelete(r))) return;
  let res;
  try { res = await fetch(`${API}/api/admin/files/${encodeURIComponent(r.hash)}`, { method: 'DELETE' }); }
  catch { toast('Network error deleting rendition.', 'error'); return; }
  if (handleAuthError(res)) return;
  if (!res.ok) { toast(`Delete failed (HTTP ${res.status}).`, 'error'); return; }
  toast('Rendition sent to Trash.', 'success');
  load();
}

async function splitRendition(r) {
  let res;
  try { res = await fetch(`${API}/api/admin/duplicates/${r.file_id}/split`, { method: 'POST' }); }
  catch { toast('Network error splitting rendition.', 'error'); return; }
  if (handleAuthError(res)) return;
  if (!res.ok) { toast(`Split failed (HTTP ${res.status}).`, 'error'); return; }
  toast('Split into its own recording.', 'success');
  load();
}

// ── Rendering ─────────────────────────────────────────────────────────────────
function techCell(r) {
  const parts = [];
  if (r.bitrate)     parts.push(`${Math.round(r.bitrate / 1000)} kbps`);
  if (r.sample_rate) parts.push(`${(r.sample_rate / 1000).toFixed(1)} kHz`);
  if (r.bit_depth)   parts.push(`${r.bit_depth}-bit`);
  return parts.join(' · ') || '—';
}

function renditionRow(r) {
  const cells = [
    el('td', { class: 'dup-rank' }, [r.best ? el('span', { class: 'dup-best', title: 'Best by the quality ladder' }, ['★ best']) : `#${r.rank}`]),
    el('td', {}, [
      el('div', { class: 'dup-title' }, [r.title || '(untitled)']),
      r.artist ? el('div', { class: 'dup-artist muted' }, [r.artist]) : null,
    ]),
    el('td', {}, [r.format || '—']),
    el('td', { class: 'dup-tech' }, [techCell(r)]),
    el('td', {}, [fmtTime(r.duration)]),
    el('td', {}, [fmtBytes(r.size)]),
    el('td', { class: 'dup-actions' }, [
      el('button', { class: 'btn btn-sm btn-neutral', onclick: () => preview(r) }, ['Play']),
      el('button', { class: 'btn btn-sm btn-neutral', title: 'Detach into its own recording', onclick: () => splitRendition(r) }, ['Split off']),
      el('button', { class: 'btn btn-sm btn-destructive-outline', onclick: () => deleteRendition(r) }, ['Delete']),
    ]),
  ];
  return el('tr', {}, cells);
}

function recordingCard(group) {
  const table = el('table', { class: 'dup-table' }, [
    el('thead', {}, [el('tr', {}, [
      el('th', {}, ['Rank']), el('th', {}, ['Track']), el('th', {}, ['Format']),
      el('th', {}, ['Quality']), el('th', {}, ['Length']), el('th', {}, ['Size']),
      el('th', {}, ['']),
    ])]),
    el('tbody', {}, group.renditions.map(renditionRow)),
  ]);
  // Recording-level play: preview the best (top-ranked) rendition with one click.
  const best = group.renditions.find(r => r.best) || group.renditions[0];
  return el('section', { class: 'dup-card' }, [
    el('div', { class: 'dup-card-head' }, [
      el('button', { class: 'btn btn-sm btn-neutral dup-play-best', title: 'Play the best rendition', onclick: () => preview(best) }, ['▶ Play best']),
      el('span', { class: 'dup-count' }, [`${group.renditions.length} renditions`]),
      el('span', { class: 'dup-suggestion' }, [group.suggestion]),
    ]),
    table,
  ]);
}

async function load() {
  if (loading) loading.remove();
  let res;
  try { res = await fetch(`${API}/api/admin/duplicates`); }
  catch { results.replaceChildren(el('p', { class: 'error' }, ['Network error loading duplicates.'])); return; }
  if (handleAuthError(res)) return;
  if (!res.ok) { results.replaceChildren(el('p', { class: 'error' }, [`Failed to load (HTTP ${res.status}).`])); return; }
  const groups = await res.json();
  if (!groups.length) {
    results.replaceChildren(el('p', { class: 'muted' }, ['No duplicate renditions — every recording has a single copy.']));
    return;
  }
  results.replaceChildren(...groups.map(recordingCard));
}

(async function init() {
  if (!(await bootAdmin({ require: 'content.moderate' }))) return;
  load();
})();
