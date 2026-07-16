// The lean shared list backing the file/recording-grain lenses: the Trash
// Recordings and Files sub-modes (docs/architecture/gc-model.md).
// All the same shape: a paged {total, items} bin with per-row icon
// actions (+ optional Play), an optional checkbox column + bulk bar, and a
// "Load more" footer. Bespoke on purpose — the Appearances lenses keep the
// full file-list.js component; these only need a simple table, so this stays
// small and reuses the shared table/toolbar CSS (file-view.css).
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
//
// The restore/delete pair above is the Trash default. A lens with different
// verbs passes its own instead:
//   rowActions: [{icon, title, kind:'danger'|null, run(item)->Promise<bool>}]
//               (true = reload the list; label via title + rowLabel)
//   bulkActions: [{label(n), kind, run(ids, all)->Promise<number|null>}]
//               (count toasts are the action's job; null = error surfaced;
//               all=true targets the whole bin — ids is empty then, the
//               action sends {action, all:true} and the server resolves the
//               set)
//   itemNoun — the banner's noun ('file', 'recording'; default 'item')
// The checkbox column + bulk bar render only when bulk actions exist. Once
// every loaded row is ticked and more remain unfetched, a "Select all N"
// banner flips bulk actions onto the whole bin (parity with file-list.js's
// cross-page select-all).
export function createTrashList(cfg) {
  const pageSize = cfg.pageSize || 100;
  const noun = cfg.itemNoun || 'item';
  const sel = new Set();
  let items = [];
  let total = 0;
  let offset = 0;
  let loading = false;
  let mounted = false;
  let allMatching = false; // bulk acts on the whole bin, not just checked rows

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
      if (String(e.message) !== 'auth') toast(cfg.loadErrorText || 'Couldn’t load the list.', 'error');
    } finally {
      loading = false;
      render();
    }
  }

  // ── Actions ───────────────────────────────────────────────────────────────
  // The Trash default set; a lens's own cfg.rowActions / cfg.bulkActions win.
  const rowActions = cfg.rowActions || [
    { icon: RESTORE_ICON, title: 'Restore', run: it => cfg.restoreOne(it) },
    {
      icon: TRASH_ICON, title: 'Delete forever', kind: 'danger',
      run: async it => (await cfg.confirmDelete(1)) && cfg.deleteOne(it),
    },
  ];
  const bulkActions = cfg.bulkActions || [
    { label: n => `Restore selected (${n})`, run: (ids, all) => runDefaultBulk('restore', ids, all) },
    { label: n => `Delete selected (${n})`, kind: 'danger', run: (ids, all) => runDefaultBulk('delete', ids, all) },
  ];
  const selectable = bulkActions.length > 0;

  async function runDefaultBulk(kind, ids, all) {
    if (kind === 'delete' && !await cfg.confirmDelete(all ? total : ids.length)) return null;
    const n = kind === 'delete' ? await cfg.bulkDelete(ids, all) : await cfg.bulkRestore(ids, all);
    if (n == null) return null; // error already surfaced
    const verb = kind === 'delete' ? 'Permanently deleted' : 'Restored';
    if (n) toast(`${verb} ${n} item${n === 1 ? '' : 's'}.`, 'success');
    return n;
  }

  async function doRowAction(action, it) {
    if (await action.run(it)) { sel.delete(keyOf(it)); await loadPage(true); }
  }
  async function doBulk(action) {
    const all = allMatching;
    const ids = all ? [] : [...sel];
    if (!all && !ids.length) return;
    if (await action.run(ids, all) == null) return;
    sel.clear();
    allMatching = false;
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
      const n = allMatching ? total : sel.size;
      host.appendChild(el('div', { class: 'bulk-toolbar' }, [
        el('span', { class: 'bulk-selcount' }, [allMatching ? `All ${total} selected` : `${sel.size} selected`]),
        el('span', { class: 'bulk-spacer' }),
        ...bulkActions.map(a => el('button', {
          class: 'btn btn-sm ' + (a.kind === 'danger' ? 'btn-destructive-solid' : 'btn-neutral'),
          onclick: () => doBulk(a),
        }, [a.label(n)])),
        el('button', { class: 'btn btn-sm btn-neutral', onclick: () => { sel.clear(); allMatching = false; render(); } }, ['Clear']),
      ]));
    }

    // Cross-page select-all (parity with file-list.js): once every loaded row
    // is ticked and more remain unfetched, offer the whole bin.
    const allSelected = items.every(it => sel.has(keyOf(it)));
    if (allMatching) {
      host.appendChild(el('div', { class: 'select-all-banner is-active' }, [
        `All ${total} ${noun}${total === 1 ? '' : 's'} selected. `,
        el('button', { type: 'button', class: 'linklike', onclick: () => { sel.clear(); allMatching = false; render(); } }, ['Clear selection']),
      ]));
    } else if (sel.size && allSelected && items.length < total) {
      host.appendChild(el('div', { class: 'select-all-banner' }, [
        `All ${items.length} loaded ${noun}${items.length === 1 ? '' : 's'} selected. `,
        el('button', { type: 'button', class: 'linklike', onclick: () => { allMatching = true; render(); } }, [`Select all ${total}`]),
      ]));
    }

    const head = el('tr', {}, [
      selectable ? el('th', { class: 'cell-check' }, [el('input', {
        type: 'checkbox', 'aria-label': 'Select all', ...(allSelected ? { checked: '' } : {}),
        onchange: e => {
          allMatching = false;
          if (e.target.checked) items.forEach(it => sel.add(keyOf(it)));
          else sel.clear();
          render();
        },
      })]) : null,
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
      for (const a of rowActions) {
        actions.push(el('button', {
          class: 'icon-btn' + (a.kind === 'danger' ? ' icon-btn--danger' : ''),
          title: a.title, 'aria-label': `${a.title} ${name}`, html: a.icon,
          onclick: () => doRowAction(a, it),
        }));
      }
      return el('tr', { class: cfg.rowClass?.(it) || '', 'data-key': String(key) }, [
        selectable ? el('td', { class: 'cell-check' }, [el('input', {
          type: 'checkbox', 'aria-label': 'Select row', ...(sel.has(key) ? { checked: '' } : {}),
          onchange: e => { allMatching = false; if (e.target.checked) sel.add(key); else sel.delete(key); render(); },
        })]) : null,
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
