// Trash — the lean shared list backing the Recordings and Files sub-modes
// (soft-delete.md). Both are the same shape: a paged {total, items} bin with a
// checkbox column, per-row Restore / Delete forever (+ optional Play), a bulk
// bar, and a "Load more" footer. Bespoke on purpose — the Appearances lens keeps
// the full file-list.js component; these two only need a simple table, so this
// stays small and reuses the shared table/toolbar CSS (file-view.css).
import { el, handleAuthError, toast } from './shared.js';
import { PLAY_ICON, RESTORE_ICON, TRASH_ICON } from '../icons.js';

// createTrashList(cfg) mounts one sub-list into cfg.host. cfg:
//   fetchPage(offset, limit) -> Promise<{total, items}>
//   columns:   [label, …]                     header labels (data columns)
//   renderCells(item) -> [<td>, …]            the data cells for one row
//   rowKey(item) -> number                    unique id (selection + de-dup)
//   rowLabel(item) -> string                  name for the icon buttons' aria-label
//   rowClass(item) -> string|null             optional row class
//   onPlay(item, visibleItems)                optional — adds a Play action
//   restoreOne(item) -> Promise<bool>         single restore (true = ok)
//   deleteOne(item)  -> Promise<bool>         single delete (already confirmed)
//   bulkRestore(ids) -> Promise<number>       restored count
//   bulkDelete(ids)  -> Promise<number>       deleted count
//   confirmDelete(count) -> Promise<bool>     shared confirm modal
//   emptyText, pageSize
export function createTrashList(cfg) {
  const pageSize = cfg.pageSize || 100;
  const sel = new Set();
  let items = [];
  let total = 0;
  let offset = 0;
  let loading = false;
  let mounted = false;

  const host = cfg.host;

  function keyOf(it) { return cfg.rowKey(it); }

  // ── Fetch ─────────────────────────────────────────────────────────────────
  async function loadPage(reset) {
    if (loading) return;
    loading = true;
    if (reset) { offset = 0; items = []; }
    try {
      const data = await cfg.fetchPage(offset, pageSize);
      total = data.total || 0;
      const batch = data.items || [];
      items = reset ? batch : items.concat(batch);
      offset = items.length;
    } catch (e) {
      if (String(e.message) !== 'auth') toast('Couldn’t load Trash.', 'error');
    } finally {
      loading = false;
      render();
    }
  }

  // ── Actions ───────────────────────────────────────────────────────────────
  async function doRestoreOne(it) {
    if (await cfg.restoreOne(it)) { sel.delete(keyOf(it)); await loadPage(true); }
  }
  async function doDeleteOne(it) {
    if (!await cfg.confirmDelete(1)) return;
    if (await cfg.deleteOne(it)) { sel.delete(keyOf(it)); await loadPage(true); }
  }
  async function doBulk(kind) {
    const ids = [...sel];
    if (!ids.length) return;
    if (kind === 'delete' && !await cfg.confirmDelete(ids.length)) return;
    const n = kind === 'delete' ? await cfg.bulkDelete(ids) : await cfg.bulkRestore(ids);
    if (n == null) return; // error already surfaced
    const verb = kind === 'delete' ? 'Permanently deleted' : 'Restored';
    if (n) toast(`${verb} ${n} item${n === 1 ? '' : 's'}.`, 'success');
    sel.clear();
    await loadPage(true);
  }

  // ── Render ────────────────────────────────────────────────────────────────
  function render() {
    host.replaceChildren();
    host.setAttribute('aria-busy', loading ? 'true' : 'false');

    if (!items.length) {
      host.appendChild(el('p', { class: loading ? 'trash-loading' : 'trash-empty' },
        [loading ? 'Loading…' : cfg.emptyText]));
      return;
    }

    // Bulk toolbar (only while something is selected).
    if (sel.size) {
      host.appendChild(el('div', { class: 'bulk-toolbar' }, [
        el('span', { class: 'bulk-selcount' }, [`${sel.size} selected`]),
        el('span', { class: 'bulk-spacer' }),
        el('button', { class: 'btn btn-sm btn-neutral', onclick: () => doBulk('restore') }, [`Restore selected (${sel.size})`]),
        el('button', { class: 'btn btn-sm btn-destructive-solid', onclick: () => doBulk('delete') }, [`Delete selected (${sel.size})`]),
        el('button', { class: 'btn btn-sm btn-neutral', onclick: () => { sel.clear(); render(); } }, ['Clear']),
      ]));
    }

    const allSelected = items.every(it => sel.has(keyOf(it)));
    const head = el('tr', {}, [
      el('th', { class: 'cell-check' }, [el('input', {
        type: 'checkbox', 'aria-label': 'Select all', ...(allSelected ? { checked: '' } : {}),
        onchange: e => {
          if (e.target.checked) items.forEach(it => sel.add(keyOf(it)));
          else sel.clear();
          render();
        },
      })]),
      ...cfg.columns.map(c => el('th', {}, [c])),
      el('th', { class: 'col-actions' }, ['']),
    ]);

    const rows = items.map(it => {
      const key = keyOf(it);
      const name = cfg.rowLabel?.(it) || 'this item';
      // Icon-only, like every other row-action set (file-view.css .icon-btn).
      const actions = [];
      if (cfg.onPlay) {
        actions.push(el('button', { class: 'play-btn', title: 'Preview', 'aria-label': `Preview ${name}`, html: PLAY_ICON,
          onclick: () => cfg.onPlay(it, items) }));
      }
      actions.push(el('button', { class: 'icon-btn', title: 'Restore', 'aria-label': `Restore ${name}`, html: RESTORE_ICON,
        onclick: () => doRestoreOne(it) }));
      actions.push(el('button', { class: 'icon-btn icon-btn--danger', title: 'Delete forever', 'aria-label': `Delete ${name} forever`, html: TRASH_ICON,
        onclick: () => doDeleteOne(it) }));
      return el('tr', { class: cfg.rowClass?.(it) || '', 'data-key': String(key) }, [
        el('td', { class: 'cell-check' }, [el('input', {
          type: 'checkbox', 'aria-label': 'Select row', ...(sel.has(key) ? { checked: '' } : {}),
          onchange: e => { if (e.target.checked) sel.add(key); else sel.delete(key); render(); },
        })]),
        ...cfg.renderCells(it),
        el('td', { class: 'cell-actions' }, [el('div', { class: 'trash-actions' }, actions)]),
      ]);
    });

    host.appendChild(el('div', { class: 'files-table-wrap trash-list-wrap' }, [
      el('table', { class: 'files-table' }, [el('thead', {}, [head]), el('tbody', {}, rows)]),
    ]));

    if (items.length < total) {
      host.appendChild(el('div', { class: 'trash-more' }, [
        el('button', { class: 'btn btn-neutral', onclick: () => loadPage(false) },
          [`Load more (${items.length} of ${total})`]),
      ]));
    }
  }

  // setPlaying repaints the active row highlight (parity with file-list.js so the
  // shared player can mark what's playing).
  function setPlaying(key) {
    host.querySelectorAll('tr[data-key]').forEach(tr =>
      tr.classList.toggle('playing-row', tr.getAttribute('data-key') === String(key)));
  }

  return {
    mount() { if (mounted) return; mounted = true; render(); loadPage(true); },
    reload() { if (mounted) loadPage(true); },
    setPlaying,
  };
}
