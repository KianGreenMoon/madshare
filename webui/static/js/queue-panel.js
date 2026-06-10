// queue-panel.js — the current-queue panel (Phase 5 step 2 of
// docs/plans/playlists.md). Opens from the player-bar queue button and shows
// the controller's queue as an editable list: click to play, × to remove,
// drag (or Ctrl/Alt+Arrow) to reorder, Clear, and "Save as playlist…" (POST
// /api/playlists with the queue's hashes). The panel is shell chrome — wired
// once by shell.js, it survives page swaps like the player bar itself.
import { fmtTime } from './player.js';
import { openLoginModal } from './auth.js';
import { loadDurCache } from './dur-cache.js';
import { trackHash } from './favorites.js';

const API = document.querySelector('meta[name="api-url"]')?.content || '';

export function initQueuePanel(controller, showToast) {
  const panel    = document.getElementById('queue-panel');
  const btn      = document.getElementById('btnQueue');
  if (!panel || !btn) return; // page without the queue UI (e.g. admin preview player)
  const list     = document.getElementById('queueList');
  const countEl  = document.getElementById('queueCount');
  const saveBtn  = document.getElementById('queueSaveBtn');
  const clearBtn = document.getElementById('queueClearBtn');
  const closeBtn = document.getElementById('queueCloseBtn');
  const saveForm = document.getElementById('queueSaveForm');
  const saveName = document.getElementById('queueSaveName');
  const saveCancel = document.getElementById('queueSaveCancel');

  let open = false;
  function setOpen(on) {
    open = on;
    panel.classList.toggle('hidden', !on);
    btn.classList.toggle('active', on);
    btn.setAttribute('aria-expanded', String(on));
    if (on) render();
  }

  btn.addEventListener('click', () => setOpen(!open));
  closeBtn.addEventListener('click', () => setOpen(false));
  document.addEventListener('keydown', e => {
    if (e.key === 'Escape' && open) setOpen(false);
  });
  // Auto-close on outside click. The player bar is exempt so play/pause/seek
  // (and the toggle button itself) don't snap the panel shut while you're
  // watching the queue. composedPath() — not contains() — because clicking a
  // queue row re-renders the list, detaching the clicked node before this
  // bubble-phase listener runs.
  document.addEventListener('click', e => {
    if (!open) return;
    const path = e.composedPath();
    if (path.includes(panel)) return;
    const bar = document.getElementById('player-bar');
    if (bar && path.includes(bar)) return;
    setOpen(false);
  });

  clearBtn.addEventListener('click', () => controller.clear());

  // ── Save as playlist ────────────────────────────────────────────────────────
  saveBtn.addEventListener('click', () => {
    saveForm.hidden = false;
    saveName.value = '';
    saveName.focus();
  });
  saveCancel.addEventListener('click', () => { saveForm.hidden = true; });
  saveForm.addEventListener('submit', async e => {
    e.preventDefault();
    const name = saveName.value.trim();
    const { tracks } = controller.getQueue();
    const hashes = tracks.map(trackHash).filter(Boolean);
    if (!name || !hashes.length) return;
    try {
      const res = await fetch(`${API}/api/playlists`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name, hashes }),
      });
      if (res.status === 401 || res.status === 403) { openLoginModal(); return; }
      if (!res.ok) {
        const msg = (await res.text().catch(() => '')).trim();
        showToast(`Couldn't save playlist: ${msg || `HTTP ${res.status}`}`, { type: 'error' });
        return;
      }
      saveForm.hidden = true;
      showToast(`Saved playlist "${name}".`, { type: 'success' });
    } catch (err) {
      showToast(`Couldn't save playlist: ${err.message}`, { type: 'error' });
    }
  });

  // ── Rendering ───────────────────────────────────────────────────────────────
  function render() {
    if (!open) return; // closed panel: skip the DOM work, render on open
    const { tracks, index } = controller.getQueue();
    countEl.textContent = tracks.length ? `(${tracks.length})` : '';
    saveBtn.disabled = clearBtn.disabled = tracks.length === 0;

    if (!tracks.length) {
      list.innerHTML = '<div class="queue-empty">Queue is empty — play something.</div>';
      return;
    }

    // Durations: prefer what the track already carries, else the shared cache
    // (filled by the library's background metadata fetch) — so known durations
    // show up front, not only after a track has played.
    const durCache = loadDurCache();
    const durText = t => {
      if (typeof t.dur === 'number') return fmtTime(t.dur);
      if (typeof t.dur === 'string' && t.dur && t.dur !== '—') return t.dur;
      return durCache[t.url] || '';
    };

    const frag = document.createDocumentFragment();
    tracks.forEach((t, i) => {
      const row = document.createElement('div');
      row.className = 'queue-row' + (i === index ? ' playing' : '');
      row.setAttribute('role', 'listitem');
      row.tabIndex = 0;
      row.draggable = true;
      row.dataset.i = i;
      row.setAttribute('aria-label',
        `${t.title || 'Unknown'}${i === index ? ' (now playing)' : ''}. ` +
        'Enter plays, Delete removes, Ctrl+Arrow moves.');

      const num = document.createElement('span');
      num.className = 'queue-num';
      num.textContent = i === index ? '▶' : String(i + 1);

      const body = document.createElement('div');
      body.className = 'queue-row-body';
      const title = document.createElement('div');
      title.className = 'queue-row-title';
      title.textContent = t.title || 'Unknown';
      const meta = document.createElement('div');
      meta.className = 'queue-row-meta';
      meta.textContent = t.artist || '';
      body.append(title, meta);

      const dur = document.createElement('span');
      dur.className = 'queue-dur';
      dur.textContent = durText(t);

      const rm = document.createElement('button');
      rm.className = 'queue-remove';
      rm.setAttribute('aria-label', `Remove ${t.title || 'track'} from queue`);
      rm.title = 'Remove from queue';
      rm.textContent = '×';
      rm.addEventListener('click', e => { e.stopPropagation(); controller.removeAt(i); });

      row.append(num, body, dur, rm);
      row.addEventListener('click', () => controller.playAt(i));
      row.addEventListener('keydown', e => {
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); controller.playAt(i); }
        else if (e.key === 'Delete' || e.key === 'Backspace') { e.preventDefault(); controller.removeAt(i); }
        else if ((e.ctrlKey || e.altKey) && e.key === 'ArrowUp' && i > 0) {
          e.preventDefault(); controller.move(i, i - 1); focusRow(i - 1);
        } else if ((e.ctrlKey || e.altKey) && e.key === 'ArrowDown' && i < tracks.length - 1) {
          e.preventDefault(); controller.move(i, i + 1); focusRow(i + 1);
        }
      });

      // HTML5 drag reorder. dragIndex travels module-scope (dataTransfer is
      // unreadable during dragover in some browsers).
      row.addEventListener('dragstart', e => {
        dragIndex = i;
        row.classList.add('dragging');
        e.dataTransfer.effectAllowed = 'move';
      });
      row.addEventListener('dragend', () => {
        dragIndex = -1;
        row.classList.remove('dragging');
        clearDropMarks();
      });
      row.addEventListener('dragover', e => {
        if (dragIndex < 0 || dragIndex === i) return;
        e.preventDefault();
        e.dataTransfer.dropEffect = 'move';
        clearDropMarks();
        row.classList.add(dragIndex < i ? 'drop-below' : 'drop-above');
      });
      row.addEventListener('drop', e => {
        e.preventDefault();
        if (dragIndex >= 0 && dragIndex !== i) controller.move(dragIndex, i);
        dragIndex = -1;
      });

      frag.appendChild(row);
    });
    list.replaceChildren(frag);
  }

  let dragIndex = -1;
  function clearDropMarks() {
    list.querySelectorAll('.drop-above, .drop-below')
      .forEach(r => r.classList.remove('drop-above', 'drop-below'));
  }
  function focusRow(i) {
    // render() rebuilds rows on queuechange; refocus the moved row after that.
    queueMicrotask(() => list.querySelector(`.queue-row[data-i="${i}"]`)?.focus());
  }

  controller.on('queuechange', render);
  controller.on('trackchange', render);
}
