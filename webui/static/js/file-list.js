// file-list.js — the one file-management view. A single component renders every
// surface that lists files with title/artist/album + a metadata editor: the
// admin Library scopes (All / Review / Trash) and the uploader's My-uploads.
// Design + contract: docs/architecture/file-management-view.md.
//
// It is parameterised by a SCOPE descriptor and owns only presentation:
// rendering (flat list, grouped list, or artist/album browse), selection, the
// bulk toolbar, badges, inline two-step confirms, and wiring to the shared
// track-edit.js + bulk-edit.js modals. Everything domain-specific — what to
// load, which endpoints an action hits, how to play a row — is injected by the
// scope, so the component is reusable from both the admin pages and the
// (shell-native) upload page without importing either's helpers.

import { createTrackEditor } from './track-edit.js';
import { createBulkEditor } from './bulk-edit.js';
import { createCoverPicker } from './cover-edit.js';
import { discKey, discSort, discLabel, isMultiDisc } from './disc.js';
import { createVirtualList } from './virtual-list.js';
import { createGroupedStream } from './grouped-stream.js';

// Local DOM builder + formatter so this module has no page-specific imports.
// el('button', {class:'btn', onclick: fn}, ['Label'])
function el(tag, props = {}, children = []) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (v == null) continue;
    if (k === 'class') node.className = v;
    else if (k === 'text') node.textContent = v;
    else if (k === 'html') node.innerHTML = v;            // trusted markup only (icons)
    else if (k.startsWith('on') && typeof v === 'function') node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v);
  }
  (Array.isArray(children) ? children : [children]).forEach(c => {
    if (c != null) node.append(c.nodeType ? c : document.createTextNode(c));
  });
  return node;
}

function fmtBytes(n) {
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1024) return n + ' B';
  const kb = n / 1024;
  if (kb < 1024) return kb.toFixed(kb < 10 ? 1 : 0) + ' KB';
  const mb = kb / 1024;
  if (mb < 1024) return mb.toFixed(mb < 10 ? 1 : 0) + ' MB';
  return (mb / 1024).toFixed(1) + ' GB';
}

const PLAY_ICON = '<svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M8 5v14l11-7z"/></svg>';

/**
 * createFileList builds a file-management view from a scope descriptor.
 *
 * Scope (all optional unless noted):
 *   title            heading text (required)
 *   desc             one-line description under the heading
 *   emptyText        message when the scope has no files
 *   columns          ['check','title','artist','album','size','access','meta','actions'] (required)
 *   metaLabel        header for the 'meta' column
 *   metaValue(file)  → string for the 'meta' cell
 *   badge(file)      → { text, cls } | null  (the title-cell pill)
 *   accessEditable   show License+Guest in the editors (admin scopes)
 *   licenses         license vocabulary for the pickers
 *   load()           async → file[]   (required for list presentation)
 *   selectable(file) → bool — which rows enter bulk selection (default: none)
 *   autoSelect       pre-check every selectable row after each load
 *   editable(file)   → bool — gate the built-in Edit per row (default: all)
 *   rowActions       [{ id,label,kind:'neutral'|'danger',confirm?:'inline',
 *                       confirmPrompt?,confirmLabel?, show?(file)=>bool,
 *                       run:async(file)=>void|false }]
 *   bulkActions      [{ id,label,kind, run:async(hashes)=>void }]
 *   editPatchURL(file) → url        (enables the built-in Edit action)
 *   editDetailURL(file) → url       (GET full tags; enables track #/extended edit)
 *   editNote         note shown in the edit modal
 *   saveAccess(file,{guest,license})→Promise  (when accessEditable)
 *   bulkApply(hashes,patch)→Promise (enables the built-in "Edit tags…" bulk action)
 *   grouping         null | {kind:'collapsible', by, label, counts}
 *                         | {kind:'sections', sections:[{key,label,match}]}
 *   browse           null | { loaders:{artists(),albums(a),tracks(a,al)},
 *                             coverURL(level,item,artist)?, groupHashes(level,item,artist),
 *                             trackHash(track) }
 *   onPlay(file, files)  play a row within the visible set (page owns the player)
 *   toast(msg,type)      notifier
 *   handleAuthError(res) → bool
 *
 * @returns {{ mount(el), reload(), setPlaying(hash), getVisible(), destroy() }}
 */
export function createFileList(scope) {
  const toast = scope.toast || (() => {});
  const hasBrowse = !!scope.browse;

  let mountEl = null;
  let rows = [];                 // current list rows (paged: every row loaded so far)
  let loading = false, loadError = false;
  let filterText = '';
  let view = hasBrowse ? 'browse' : 'list';
  let playingHash = null;
  let bodyHost = null;           // persistent body container (survives chrome rebuilds)

  const selected = new Set();    // selected file hashes (shared list ⇄ browse)
  const collapsed = new Set();   // collapsed group keys (collapsible grouping)
  const br = { level: 'artists', artist: null, album: null, items: [] };

  // ── Windowed list (virtual-list.js) ──────────────────────────────────────────
  // The flat and the "By artist / album" single-table presentations render through
  // a persistent virtual scroller (only on-screen rows in the DOM), so they never
  // freeze and the grouped view scales. The classic collapsible (Review) and
  // sections (My uploads) groupings stay non-windowed for now. Design:
  // docs/architecture/infinite-scroll-virtualization.md ("This pass").
  let vlist = null, vWrap = null, vTbody = null, vGrouped = null;

  // ── Grouped streaming (paged "By artist / album" view) ───────────────────────
  // The paged grouped view streams pages in server-sorted (sort=grouped) order and
  // inserts the artist/album/disc separators on the client as the keys change — so
  // it scales like the flat infinite-scroll instead of loading every row first. The
  // pure grouping state machine lives in createGroupedStream (unit-tested); this
  // closure only drives its I/O (fetchMoreGrouped) and reads gstream.items.
  const gstream = createGroupedStream(isSelectable);

  // ── Server-paged mode (scope.paged) ─────────────────────────────────────────
  // The admin All-files scope is too large to hold in the DOM, so it loads one
  // server page at a time (file-list-scaling.md). filterText + sort + page round-
  // trip to scope.loadPage({limit,offset,q,sort}) → {total,items}; rows holds just
  // the current page. selectAllMatching flips bulk actions onto the whole filtered
  // set (via the scope's runAll) rather than the in-memory selection. In this mode
  // the client grouped sort, sections, and in-memory filter are all bypassed.
  const paged = !!scope.paged;
  const PAGE_SIZE = scope.pageSize || 100;
  let total = 0;                 // total rows matching the current filter
  let loadedCount = 0;           // rows fetched so far (paged infinite scroll)
  let selectAllMatching = false; // bulk acts on every matching row, not just the page

  // Flat sort selection (the dropdown), shared by every list view. Paged scopes
  // pass it to the server as the sort token; non-paged scopes sort in memory and
  // additionally allow 'default' (as loaded). The "By artist / album" GROUPED view
  // is a SEPARATE toggle (groupToggle) — it's a view mode, not a sort order, so
  // keeping it out of the dropdown declutters the orders (owner ask). Both are
  // persisted (non-paged) so the choice sticks across visits.
  const SORT_KEY = 'madshare-files-sort';
  const GROUP_KEY = 'madshare-files-grouped';
  let sortToken = paged ? 'created_desc' : 'default';
  let grouped = false;           // the "By artist / album" view is on
  if (!paged) {
    try {
      let s = localStorage.getItem(SORT_KEY);
      if (s === 'grouped') {     // migrate the legacy combined token, once
        grouped = true; s = 'default';
        localStorage.setItem(SORT_KEY, 'default');
      }
      if (s) sortToken = s;
      if (localStorage.getItem(GROUP_KEY) === '1') grouped = true;
    } catch { /* ignore */ }
  }

  // Filter-field scope (the filter-type dropdown): '' = General (every field),
  // else one of artist / album / title. It narrows what the search box matches —
  // paged scopes pass it to the server (mirrors fileFilterWhere); non-paged scopes
  // apply it in visibleFiles(). Persisted globally like the sort/grouping choice.
  const FIELD_KEY = 'madshare-files-field';
  let qField = '';
  try {
    const fv = localStorage.getItem(FIELD_KEY);
    if (fv === 'artist' || fv === 'album' || fv === 'title') qField = fv;
  } catch { /* ignore */ }

  let _editor = null, _bulk = null, _cover = null;

  // The filter box is a PERSISTENT node, created once. A paged reload rebuilds the
  // whole header bar, so a freshly-built <input> would blur after a single
  // keystroke (the All-files search dropped focus on every server round-trip);
  // reusing the same node lets render() refocus it and keep the caret. See
  // headerBar() (sync) and render() (focus restore).
  let filterTimer = null;
  const searchInput = el('input', { type: 'search', placeholder: 'Filter…', autocomplete: 'off', 'aria-label': 'Filter files' });
  searchInput.addEventListener('input', () => {
    clearTimeout(filterTimer);
    filterTimer = setTimeout(() => {
      filterText = searchInput.value;
      // Paged: a new filter is a fresh server query (from offset 0); in-memory
      // scopes just re-filter the rows already loaded.
      if (paged) { clearPageSelection(); reload(); }
      else renderContent();
    }, 200);
  });

  // The filter box's placeholder echoes the active field scope, so it's obvious
  // the term will only match (say) artists. Kept in sync with qField.
  const FIELD_PLACEHOLDERS = { '': 'Filter…', artist: 'Filter by artist…', album: 'Filter by album…', title: 'Filter by track name…' };
  function updateSearchPlaceholder() { searchInput.placeholder = FIELD_PLACEHOLDERS[qField] || 'Filter…'; }
  updateSearchPlaceholder();

  const displayTitle = f => f.title || f.filename || 'this file';

  // ── Shared modals (lazy) ───────────────────────────────────────────────────
  function editor() {
    if (_editor) return _editor;
    _editor = createTrackEditor({
      patchURL: scope.editPatchURL,
      detailURL: scope.editDetailURL,
      note: scope.editNote || '',
      checkAuth: scope.handleAuthError,
      access: scope.accessEditable
        ? { licenses: scope.licenses || [], save: scope.saveAccess }
        : null,
      onSaved: (file) => { toast(`Metadata saved for “${displayTitle(file)}”.`, 'success'); reload(); },
      onError: (err) => toast(`Couldn’t save metadata: ${err.message}`, 'error'),
    });
    return _editor;
  }
  function bulkEditor() {
    if (_bulk) return _bulk;
    _bulk = createBulkEditor({
      access: scope.accessEditable ? { licenses: scope.licenses || [] } : null,
      // When the scope can read a file's full tags, let the bulk editor fetch them
      // for the selection so the Extended modal can pre-fill its shared values too.
      loadDetails: scope.editDetailURL ? loadSelectionDetails : null,
      onApply: async (hashes, patch) => {
        // Filter mode: apply to the whole matching set via the scope's runAll
        // equivalent (it owns its own success toast); else the explicit page set.
        if (paged && selectAllMatching && scope.bulkApplyAll) {
          await scope.bulkApplyAll({ q: filterText.trim(), field: qField }, patch);
          clearPageSelection();
          await reload();
          return;
        }
        await scope.bulkApply(hashes, patch);
        selected.clear();
        toast(`Updated ${hashes.length} file${hashes.length === 1 ? '' : 's'}.`, 'success');
        await reload();
      },
    });
    return _bulk;
  }
  function coverPicker() {
    if (_cover) return _cover;
    _cover = createCoverPicker({
      apiBase: scope.apiBase || '',
      toast,
      checkAuth: scope.handleAuthError,
      onUploaded: () => reload(),   // refresh so the now-covered group drops the button
    });
    return _cover;
  }

  // groupedActive: the "By artist / album" view is selected and the scope allows
  // it. On a paged scope the server supplies the grouping order (sort=grouped) so
  // the view streams page-by-page like the flat list; non-paged scopes group the
  // already-loaded set in the browser.
  function groupedActive() { return grouped && !!scope.artistAlbumSort; }

  // ── Loading ─────────────────────────────────────────────────────────────────
  async function reload() {
    if (view === 'browse' && hasBrowse) return loadBrowse();
    if (paged) return loadPage();
    return loadList();
  }
  // loadPage fetches the first page; further pages stream in via fetchMorePage (flat)
  // or fetchMoreGrouped (grouped) as the windowed list scrolls (file-list-scaling.md
  // backend, infinite-scroll UI). The grouped view asks the server for its order
  // (sort=grouped) and folds the page into the streamed item array as it arrives.
  async function loadPage() {
    const grouped = groupedActive();
    loading = true; loadError = false; rows = []; loadedCount = 0;
    if (grouped) gstream.reset();
    render();
    try {
      const res = await scope.loadPage({ limit: PAGE_SIZE, offset: 0, q: filterText.trim(), field: qField, sort: grouped ? 'grouped' : sortToken }) || {};
      rows = res.items || [];
      total = res.total || 0;
      loadedCount = rows.length;
      // Grouped: fold the first page into the streamed items now; a page wholly
      // absorbed into one still-open album yields nothing yet — the windowed list's
      // near-bottom fetch then pulls more until an album flushes.
      if (grouped) gstream.ingest(rows, loadedCount >= total || !rows.length);
    } catch (err) { loadError = true; console.error('file-list page load failed:', err); }
    loading = false; render();
  }
  // fetchMorePage backs the windowed flat list's infinite scroll: the next offset
  // page, appended to rows. done once the whole filtered total has been fetched.
  async function fetchMorePage() {
    if (loadedCount >= total) return { items: [], done: true };
    const res = await scope.loadPage({ limit: PAGE_SIZE, offset: loadedCount, q: filterText.trim(), field: qField, sort: sortToken }) || {};
    const items = res.items || [];
    if (typeof res.total === 'number') total = res.total;
    rows = rows.concat(items);
    loadedCount = rows.length;
    return { items: items.map(flatItem), done: loadedCount >= total || items.length === 0 };
  }
  // fetchMoreGrouped backs the grouped view's infinite scroll: pull the next page
  // (server-sorted) and hand it to the stream. A page wholly inside one still-open
  // album flushes nothing, so it keeps pulling until an album closes (or the listing
  // ends) — returning [] without `done` would make the scroller stop short.
  async function fetchMoreGrouped() {
    for (;;) {
      if (loadedCount >= total) return { items: gstream.ingest([], true), done: true };
      const res = await scope.loadPage({ limit: PAGE_SIZE, offset: loadedCount, q: filterText.trim(), field: qField, sort: 'grouped' }) || {};
      const items = res.items || [];
      if (typeof res.total === 'number') total = res.total;
      if (!items.length) return { items: gstream.ingest([], true), done: true };
      rows = rows.concat(items); loadedCount = rows.length;
      const isFinal = loadedCount >= total;
      const delta = gstream.ingest(items, isFinal);
      if (isFinal) return { items: delta, done: true };
      if (delta.length) return { items: delta, done: false };
    }
  }
  async function loadList() {
    loading = true; loadError = false; render();
    try { rows = (await scope.load()) || []; }
    catch (err) { loadError = true; console.error('file-list load failed:', err); }
    loading = false;
    // autoSelect pre-checks every selectable row after a load (the My-uploads
    // convention: "send the lot unless you untick").
    if (scope.autoSelect) { selected.clear(); rows.filter(isSelectable).forEach(f => selected.add(f.hash)); }
    render();
  }
  async function loadBrowse() {
    loading = true; loadError = false; render();
    try {
      const L = scope.browse.loaders;
      if (br.level === 'albums') br.items = await L.albums(br.artist);
      else if (br.level === 'tracks') br.items = await L.tracks(br.artist, br.album);
      else br.items = await L.artists();
    } catch (err) { loadError = true; console.error('file-list browse failed:', err); }
    loading = false; render();
  }

  // ── Filtering ────────────────────────────────────────────────────────────────
  function matches(s) {
    const q = filterText.trim().toLowerCase();
    return !q || (s || '').toLowerCase().includes(q);
  }
  // fieldStrings returns the haystacks the filter searches for a row, scoped by
  // the active field (qField). The General set mirrors the server's fileFilterWhere
  // (title / artist / album-artist / album / filename) so paged and in-memory
  // scopes match identically.
  function fieldStrings(f) {
    switch (qField) {
      case 'artist': return [f.album_artist, f.artist];
      case 'album':  return [f.album];
      case 'title':  return [f.title, f.filename];
      default:       return [f.title, f.artist, f.album_artist, f.album, f.filename];
    }
  }
  function visibleFiles() {
    if (paged) return rows;   // the server already filtered this page
    const q = filterText.trim().toLowerCase();
    if (!q) return rows;
    return rows.filter(f => fieldStrings(f).filter(Boolean).join(' ').toLowerCase().includes(q));
  }
  function getVisible() { return visibleFiles(); }

  // ── Cells ──────────────────────────────────────────────────────────────────
  function accessSummary(f) {
    const parts = [f.guest_playable ? 'Guest' : 'Private'];
    if (f.license) parts.push(f.license);
    return el('span', { class: 'cell-muted', text: parts.join(' · ') });
  }

  function titleCell(f) {
    const titleText = f.title || f.filename || 'Untitled';
    // The title can be overlong; it ellipsises (see .cell-title) so the columns
    // stay put — the full text rides along in a hover tooltip and the Edit modal.
    const titleSpan = el('span', { class: f.title ? 'cell-title' : 'cell-title is-fallback', title: titleText, text: titleText });
    // Title + badge share one flex line so the title ellipsises but the badge,
    // which must stay readable, never gets clipped.
    const line = [titleSpan];
    // The badge fn gets whether the list is in the grouped view, so a scope can
    // show a state badge only when its native (sectioned) grouping is hidden.
    const b = scope.badge ? scope.badge(f, groupedActive()) : null;
    if (b) line.push(el('span', { class: `state-badge ${b.cls || ''}`, title: b.title || null, text: b.text }));
    const kids = [el('span', { class: 'cell-title-line' }, line)];
    kids.push(el('span', { class: 'cell-hash', title: f.hash || '', text: shortHash(f.hash) }));
    if (f.note) kids.push(el('span', { class: 'mod-note', text: `Note: ${f.note}` }));
    return el('td', { class: 'cell-title-td', 'data-label': 'Title' }, kids);
  }

  function bodyCell(col, f, actionsHolder) {
    switch (col) {
      case 'check':
        if (!isSelectable(f)) return el('td', { class: 'cell-check' });
        return el('td', { class: 'cell-check' }, [rowCheckbox(f)]);
      case 'title':  return titleCell(f);
      case 'artist': return f.artist ? el('td', { class: 'cell-text', title: f.artist, 'data-label': 'Artist', text: f.artist }) : el('td', { class: 'cell-text cell-muted', 'data-label': 'Artist', text: '—' });
      case 'album':  return f.album ? el('td', { class: 'cell-text', title: f.album, 'data-label': 'Album', text: f.album }) : el('td', { class: 'cell-text cell-muted', 'data-label': 'Album', text: '—' });
      case 'size':   return el('td', { class: 'cell-size', 'data-label': 'Size', text: fmtBytes(f.byte_size) });
      case 'access': return el('td', { class: 'cell-access', 'data-label': 'Access' }, [accessSummary(f)]);
      case 'meta': {
        const v = scope.metaValue ? scope.metaValue(f) : '';
        return v ? el('td', { class: 'cell-text', title: v, 'data-label': scope.metaLabel || 'Meta', text: v }) : el('td', { class: 'cell-text cell-muted', 'data-label': scope.metaLabel || 'Meta', text: '—' });
      }
      case 'actions': {
        const wrap = el('div', { class: 'trash-actions' });
        wrap.append(...actionButtons(f, wrap));
        actionsHolder.wrap = wrap;
        return el('td', { class: 'cell-actions', 'data-label': 'Actions' }, [wrap]);
      }
    }
  }

  // ── Row actions (built-in play/edit + scope actions, with inline confirm) ───
  function actionButtons(f, wrap) {
    const out = [];
    if (scope.onPlay) {
      out.push(el('button', { class: 'play-btn', title: 'Preview', 'aria-label': `Preview ${displayTitle(f)}`, html: PLAY_ICON,
        onclick: () => scope.onPlay(f, visibleFiles()) }));
    }
    if (scope.editPatchURL && (!scope.editable || scope.editable(f))) {
      out.push(el('button', { class: 'btn btn-neutral btn-sm btn-edit', text: 'Edit', onclick: () => editor().open(f) }));
    }
    for (const a of scope.rowActions || []) {
      if (a.show && !a.show(f)) continue;
      const cls = a.kind === 'danger' ? 'btn btn-destructive-outline btn-sm' : 'btn btn-neutral btn-sm';
      out.push(el('button', { class: cls, text: a.label, onclick: () => a.confirm === 'inline' ? enterInlineConfirm(a, f, wrap) : runRowAction(a, f) }));
    }
    return out;
  }

  function enterInlineConfirm(action, f, wrap) {
    const restore = () => { wrap.replaceChildren(...actionButtons(f, wrap)); wrap.querySelector('button')?.focus(); };
    const cancel = el('button', { class: 'btn btn-neutral btn-sm', text: 'Cancel', onclick: restore });
    const confirm = el('button', {
      class: action.kind === 'danger' ? 'btn btn-destructive-solid btn-sm' : 'btn btn-neutral btn-sm',
      text: action.confirmLabel || action.label, onclick: () => runRowAction(action, f),
    });
    wrap.replaceChildren(el('span', { class: 'delete-confirm-label', text: action.confirmPrompt || `${action.label}?` }), cancel, confirm);
    wrap.addEventListener('keydown', e => { if (e.key === 'Escape') { e.stopPropagation(); restore(); } });
    cancel.focus();
  }

  async function runRowAction(action, f) {
    try {
      const changed = await action.run(f);
      if (changed !== false) await reload();
    } catch (err) {
      toast(`${action.label} failed: ${err.message}`, 'error');
    }
  }

  // ── Selection ───────────────────────────────────────────────────────────────
  function isSelectable(f) { return scope.selectable ? scope.selectable(f) : false; }

  // selectionTags inspects the already-loaded rows for a set of hashes and reports
  // which tags every selected file agrees on, so the bulk editor can pre-fill the
  // shared value and flag the rest as "multiple values". Only fields the list
  // payload actually carries (artist/album_artist/album + access) are considered;
  // a field absent from the data is simply not reported. Returns
  // { common: {field: value}, mixed: Set<field> }.
  function selectionTags(hashes) {
    const set = new Set(hashes);
    const files = rows.filter(f => set.has(f.hash));
    const common = {}, mixed = new Set();
    // Only pre-fill when every selected file's data is in hand; a partial subset
    // (e.g. a browse group whose hashes aren't all loaded here) could otherwise
    // report a "shared" value that isn't actually shared across the selection.
    if (!files.length || files.length !== set.size) return { common, mixed };
    const agree = (field, present, read) => {
      if (!present) return;                       // not in this scope's payload
      const v = read(files[0]);
      if (files.every(f => read(f) === v)) common[field] = v; else mixed.add(field);
    };
    for (const k of ['artist', 'album_artist', 'album', 'license']) agree(k, k in files[0], f => f[k] ?? '');
    agree('guest', 'guest_playable' in files[0], f => !!f.guest_playable);
    return { common, mixed };
  }

  // loadSelectionDetails fetches the full tag set for each selected hash via the
  // scope's detail endpoint, so the bulk editor's Extended modal can compute the
  // shared values for fields the list payload doesn't carry (genre, composer, …).
  async function loadSelectionDetails(hashes) {
    return Promise.all(hashes.map(async hash => {
      const res = await fetch(scope.editDetailURL({ hash }));
      if (scope.handleAuthError && scope.handleAuthError(res)) throw new Error('Your session expired.');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      return res.json();
    }));
  }
  function rowCheckbox(f) {
    const cb = el('input', { type: 'checkbox', class: 'fl-rowcheck', 'aria-label': `Select ${displayTitle(f)}` });
    cb.dataset.hash = f.hash;
    cb.checked = selected.has(f.hash);
    cb.addEventListener('change', () => {
      cb.checked ? selected.add(f.hash) : selected.delete(f.hash);
      // A manual tick narrows the selection back to explicit rows — drop the
      // "all matching" mode and re-render so the banner/count reflect it.
      if (selectAllMatching) { selectAllMatching = false; render(); return; }
      syncSelectionUI();
    });
    return cb;
  }

  function syncSelectionUI() {
    if (!mountEl) return;
    const sel = selected.size;
    const allMatch = paged && selectAllMatching;
    mountEl.querySelectorAll('.fl-selcount').forEach(n => (n.textContent = allMatch ? `All ${total} selected` : `${sel} selected`));
    const active = allMatch || sel > 0;
    mountEl.querySelectorAll('.fl-bulk-btn').forEach(b => (b.disabled = !active));
    // Bulk "Edit tags…" over the whole filtered set needs the scope's bulkApplyAll;
    // without it, it can only target the current page (disable in select-all mode).
    if (allMatch && !scope.bulkApplyAll) {
      mountEl.querySelectorAll('.fl-bulk-edit').forEach(b => {
        b.disabled = true;
        b.title = 'Editing every matching file isn’t available here — clear “select all” to edit this page.';
      });
    }

    const checks = [...mountEl.querySelectorAll('.fl-rowcheck')];
    // Reconcile each row checkbox from the selection Set so a group-checkbox
    // (artist/album separator) cascade is reflected on the track rows it governs.
    checks.forEach(c => { if (c.dataset.hash) c.checked = selected.has(c.dataset.hash); });
    const visSel = checks.filter(c => c.checked).length;
    const selectAll = mountEl.querySelector('.fl-selectall');
    if (selectAll) { selectAll.checked = checks.length > 0 && visSel === checks.length; selectAll.indeterminate = visSel > 0 && visSel < checks.length; }

    // Group-select checkboxes on the separator / collapsible-header rows (artist,
    // album, uploader): checked when all governed hashes are selected,
    // indeterminate when some — works for off-screen rows too (hash-set based).
    mountEl.querySelectorAll('.grp-check').forEach(cb => {
      const hs = cb.dataset.hashes ? cb.dataset.hashes.split(',').filter(Boolean) : [];
      const n = hs.filter(h => selected.has(h)).length;
      cb.checked = hs.length > 0 && n === hs.length;
      cb.indeterminate = n > 0 && n < hs.length;
    });
  }

  function selectAllVisible(on) {
    if (!on) selectAllMatching = false;   // unchecking select-all clears the whole-set mode too
    visibleFiles().filter(isSelectable).forEach(f => on ? selected.add(f.hash) : selected.delete(f.hash));
    afterSelectionChange();
  }

  // windowedMode: every list presentation (flat, grouped, collapsible, sections)
  // now renders through the virtual scroller; only the browse view is not windowed.
  function windowedMode() { return view === 'list'; }

  // afterSelectionChange repaints checkbox state without a data reload: windowed
  // mode re-paints the on-screen slice (keeping scroll); browse re-renders.
  function afterSelectionChange() {
    if (windowedMode() && vlist) { vlist.refresh(); syncSelectionUI(); }
    else render();
  }

  // ── Table + grouping ─────────────────────────────────────────────────────────
  // needsMeta flags a file with neither an artist nor an album-artist tag, so
  // editors can see at a glance which rows want metadata first.
  function needsMeta(f) { return !(f.artist || '').trim() && !(f.album_artist || '').trim(); }
  function rowAttrs(f) { return { 'data-hash': f.hash, class: needsMeta(f) ? 'fl-needs-meta' : null }; }

  function headRow(withSelectAll) {
    const ths = scope.columns.map(c => {
      if (c === 'check') {
        if (!withSelectAll) return el('th', { class: 'col-check' });
        const sa = el('input', { type: 'checkbox', class: 'fl-selectall', 'aria-label': 'Select all' });
        sa.addEventListener('change', () => selectAllVisible(sa.checked));
        return el('th', { class: 'col-check' }, [sa]);
      }
      const label = { title: 'Title', artist: 'Artist', album: 'Album', size: 'Size', access: 'Access', meta: scope.metaLabel || 'Meta', actions: 'Actions' }[c] || '';
      const cls = c === 'size' ? 'col-size' : c === 'access' ? 'col-access' : c === 'actions' ? 'col-actions' : null;
      return el('th', { scope: 'col', class: cls, text: label });
    });
    return el('tr', {}, ths);
  }

  // rowTr builds one flat data row for the windowed renderer. groupedTrack() is the
  // grouped-mode variant (it prefixes a track #).
  function rowTr(f) {
    const holder = {};
    return el('tr', rowAttrs(f), scope.columns.map(c => bodyCell(c, f, holder)));
  }

  // ── Artist → album → track grouped sort (scope.artistAlbumSort, grouped mode) ──
  // Still one flat .files-table; artist/album separator rows carry a group-select
  // checkbox, and tracks sort by track# (then title). Group by the ALBUM artist
  // (album_artist ?? artist) so a compilation stays under one band. Empty
  // artist/album fall into the Unknown / Other buckets, sorted last.
  const lc = s => (s || '').toLowerCase();
  function albumYear(files) { for (const f of files) if (f.year) return f.year; return 9999; }

  function buildArtistGroups(files) {
    const arts = new Map();
    for (const f of files) {
      const aKey = (f.album_artist || f.artist || '').trim();
      const alKey = (f.album || '').trim();
      if (!arts.has(aKey)) arts.set(aKey, { key: aKey, albums: new Map() });
      const art = arts.get(aKey);
      if (!art.albums.has(alKey)) art.albums.set(alKey, { key: alKey, files: [] });
      art.albums.get(alKey).files.push(f);
    }
    const artList = [...arts.values()].sort((a, b) => (!a.key - !b.key) || lc(a.key).localeCompare(lc(b.key)));
    for (const art of artList) {
      art.albumList = [...art.albums.values()].sort((a, b) =>
        (!a.key - !b.key) || (albumYear(a.files) - albumYear(b.files)) || lc(a.key).localeCompare(lc(b.key)));
      for (const al of art.albumList) {
        al.files.sort((a, b) =>
          (discSort(a.disc_number) - discSort(b.disc_number)) ||
          ((a.track_number == null) - (b.track_number == null)) ||
          ((a.track_number ?? 0) - (b.track_number ?? 0)) ||
          lc(a.title || a.filename).localeCompare(lc(b.title || b.filename)));
      }
    }
    return artList;
  }

  function grpSepRow(kind, label, meta, hashes, fallback, extra) {
    const kids = [];
    if (hashes.length) {
      const cb = el('input', { type: 'checkbox', class: 'grp-check', 'aria-label': `Select all in ${label}` });
      cb.dataset.hashes = hashes.join(',');
      cb.addEventListener('change', () => { hashes.forEach(h => cb.checked ? selected.add(h) : selected.delete(h)); syncSelectionUI(); });
      kids.push(cb);
    }
    // The label/meta/cover button live in an inner flex row; the <td> stays a
    // plain table cell so it still spans the full colspan width (a flex <td>
    // collapses to its content, cutting the separator line short).
    const labelRow = el('div', { class: 'grp-label-row' },
      [label, meta ? el('span', { class: 'grp-meta', text: meta }) : null, extra || null]);
    return el('tr', { class: 'grp grp-' + kind }, [
      el('td', { class: 'cell-check' }, kids),
      el('td', { colspan: String(scope.columns.length - 1), class: 'grp-label' + (fallback ? ' is-fallback' : '') },
        [labelRow]),
    ]);
  }

  // coverBtn returns the grouped-separator cover affordance for an artist/album:
  //   - "Add cover"  when the entity has none yet      (gated by allowCoverAdd)
  //   - "Edit cover" when it already has one           (gated by allowCoverEdit)
  // Replacing an existing cover needs metadata.edit (the server enforces it too),
  // so only scopes that grant it set allowCoverEdit. Never shown on the
  // Unknown/Other fallback bucket (no real entity to attach a cover to).
  function coverBtn(kind, target, fallback, hasImage) {
    if (fallback) return null;
    const add = !hasImage && scope.allowCoverAdd;
    const edit = hasImage && scope.allowCoverEdit;
    if (!add && !edit) return null;
    return el('button', {
      type: 'button', class: 'btn btn-neutral btn-sm grp-cover-btn', text: edit ? 'Edit cover' : 'Add cover',
      onclick: () => coverPicker().pick({ kind, ...target }),
    });
  }

  function groupedTrack(f) {
    const holder = {};
    const tr = el('tr', rowAttrs(f), scope.columns.map(c => bodyCell(c, f, holder)));
    const titleTd = tr.querySelector('.cell-title-td');
    if (titleTd) titleTd.insertBefore(el('span', { class: 'tracknum', text: f.track_number != null ? String(f.track_number) : '' }), titleTd.firstChild);
    return tr;
  }

  // groupedItems flattens the artist → album → disc → track tree into a single
  // ordered item array (separator + track entries) that the windowed table renders
  // one slice at a time. All files in a group share one artist_id / album_id, so
  // the representative row's *_has_image flag governs the whole group's cover.
  function groupedItems(files) {
    const items = [];
    for (const art of buildArtistGroups(files)) {
      const artFiles = art.albumList.flatMap(al => al.files);
      items.push({
        kind: 'sep', sep: 'artist', label: art.key || 'Unknown artist',
        meta: `${art.albumList.length} album${art.albumList.length === 1 ? '' : 's'} · ${artFiles.length} track${artFiles.length === 1 ? '' : 's'}`,
        hashes: artFiles.filter(isSelectable).map(f => f.hash), fallback: !art.key,
        cover: { kind: 'artist', target: { artist: art.key }, hasImage: artFiles[0]?.artist_has_image },
      });
      for (const al of art.albumList) {
        const y = albumYear(al.files);
        items.push({
          kind: 'sep', sep: 'album', label: al.key || 'Other', meta: y < 9999 ? String(y) : '',
          hashes: al.files.filter(isSelectable).map(f => f.hash), fallback: !al.key,
          cover: { kind: 'album', target: { artist: art.key, album: al.key }, hasImage: al.files[0]?.album_has_image },
        });
        // Multi-disc album → a quiet "Disc N" separator before each disc (purely
        // visual; files are already disc-then-track ordered). disc.js is the shared
        // rule (docs/architecture/disc-numbering.md).
        const multiDisc = isMultiDisc(al.files);
        let shownDisc;   // undefined: no real disc key equals it
        al.files.forEach(f => {
          const disc = discKey(f.disc_number);
          if (multiDisc && disc !== shownDisc) {
            shownDisc = disc;
            items.push({ kind: 'sep', sep: 'disc', label: discLabel(disc), meta: '', hashes: [], fallback: false, cover: null });
          }
          items.push({ kind: 'grow', file: f });
        });
      }
    }
    return items;
  }

  // flatItem wraps a file as a windowed-list row entry.
  const flatItem = f => ({ kind: 'row', file: f });

  // ── Windowed rendering (virtual-list.js) ─────────────────────────────────────
  // renderWindowItem builds one item element on demand as it scrolls into view.
  function renderWindowItem(item) {
    if (!item) return null;
    switch (item.kind) {
      case 'row':   return rowTr(item.file);
      case 'grow':  return groupedTrack(item.file);
      case 'ghead': return groupHeadRow(item);
      case 'shead': return sectionHeadRow(item);
      default: {    // 'sep' — artist / album / disc separator
        const extra = item.cover ? coverBtn(item.cover.kind, item.cover.target, item.fallback, item.cover.hasImage) : null;
        return grpSepRow(item.sep, item.label, item.meta, item.hashes, item.fallback, extra);
      }
    }
  }

  // estimateItemHeight is the starting height before a row is measured; the
  // scroller corrects it to the real offsetHeight once rendered (separators,
  // headers, and the responsive card mode vary, so heights aren't assumed fixed).
  function estimateItemHeight(item) {
    if (item) {
      if (item.kind === 'sep')   return item.sep === 'artist' ? 40 : item.sep === 'disc' ? 28 : 34;
      if (item.kind === 'ghead') return 44;
      if (item.kind === 'shead') return 40;
    }
    return 46;
  }

  // makeSpacerRow is a table spacer <tr> of a given pixel height (one full-colspan
  // cell), so windowing keeps the sticky <thead> and all the table/card CSS intact.
  function makeSpacerRow(px) {
    const td = el('td', { class: 'fl-spacer-cell', colspan: String(scope.columns.length) });
    td.style.height = `${Math.max(0, px)}px`;
    return el('tr', { class: 'fl-spacer', 'aria-hidden': 'true' }, [td]);
  }

  function buildWindowedShell(grouped) {
    vTbody = el('tbody', { class: grouped ? 'is-grouped' : null });
    const tbl = el('table', { class: 'files-table' }, [el('thead', {}, [headRow(true)]), vTbody]);
    vWrap = el('div', { class: 'files-table-wrap fl-virtual' }, [tbl]);
    vGrouped = grouped;
    return vWrap;
  }
  function destroyVList() { vlist?.destroy(); vlist = null; vWrap = null; vTbody = null; vGrouped = null; }

  // renderWindowed mounts (or reuses) the virtual scroller in the body host and
  // feeds it the current item array (flat / grouped / collapsible / sections). Both
  // paged views (flat and grouped) get infinite-scroll fetchMore — flat appends
  // rows, grouped appends streamed separators+rows; the non-paged scopes hold their
  // whole set already. The is-grouped tbody class (track-# indent) is for the
  // artist/album view only.
  function renderWindowed() {
    const grouped = groupedActive();
    if (!vWrap || vGrouped !== grouped) {
      destroyVList();
      bodyHost.replaceChildren(buildWindowedShell(grouped));
      vlist = createVirtualList({
        scrollEl: vWrap, sizerEl: vTbody,
        makeSpacer: makeSpacerRow,
        renderRow: renderWindowItem,
        estimateHeight: estimateItemHeight,
        fetchMore: paged ? (grouped ? fetchMoreGrouped : fetchMorePage) : null,
        onAfterRender: () => { syncSelectionUI(); applyPlayingHighlight(); },
      });
    }
    vlist.setItems(buildItems());
  }

  // ── Native groupings as windowed item arrays ─────────────────────────────────
  // The collapsible (Review, by uploader) and sections (My uploads, by state)
  // groupings fold into the SAME windowed table as a flat item array: a header
  // entry followed by its (sorted) rows. A collapsed group omits its rows. This is
  // what lets these scopes scale like the flat/grouped views — every list view is
  // now windowed (docs/architecture/infinite-scroll-virtualization.md "This pass").
  function collapsibleItems(files) {
    const g = scope.grouping;
    const byKey = new Map();
    for (const f of files) {
      const k = String(g.by(f));
      if (!byKey.has(k)) byKey.set(k, { key: k, label: g.label(f), items: [] });
      byKey.get(k).items.push(f);
    }
    const items = [];
    for (const grp of byKey.values()) {
      const isCollapsed = collapsed.has(grp.key);
      items.push({
        kind: 'ghead', key: grp.key, label: grp.label, collapsed: isCollapsed,
        counts: g.counts ? g.counts(grp.items) : String(grp.items.length),
        hashes: grp.items.filter(isSelectable).map(f => f.hash),
      });
      if (!isCollapsed) for (const f of grp.items) items.push({ kind: 'row', file: f });
    }
    return items;
  }

  function sectionItems(files) {
    const items = [];
    for (const sec of scope.grouping.sections) {
      const secFiles = files.filter(sec.match);
      if (!secFiles.length) continue;
      items.push({ kind: 'shead', label: `${sec.label} (${secFiles.length})` });
      for (const f of secFiles) items.push({ kind: 'row', file: f });
    }
    return items;
  }

  // groupHeadRow is the collapsible (uploader) group header as a table row: a
  // select-all-in-group checkbox (reusing the .grp-check selection cascade) + a
  // collapse toggle (chevron + label + counts).
  function groupHeadRow(item) {
    const checkKids = [];
    if (item.hashes.length) {
      const cb = el('input', { type: 'checkbox', class: 'grp-check', 'aria-label': `Select all in ${item.label}` });
      cb.dataset.hashes = item.hashes.join(',');
      cb.addEventListener('change', () => { item.hashes.forEach(h => cb.checked ? selected.add(h) : selected.delete(h)); afterSelectionChange(); });
      checkKids.push(cb);
    }
    const toggle = el('button', { type: 'button', class: 'grp-collapse-btn', 'aria-expanded': String(!item.collapsed) }, [
      el('span', { class: 'grp-chevron' + (item.collapsed ? ' is-collapsed' : ''), 'aria-hidden': 'true', text: '▾' }),
      el('span', { class: 'grp-head-label', text: item.label }),
      el('span', { class: 'grp-meta', text: item.counts }),
    ]);
    toggle.addEventListener('click', () => {
      if (collapsed.has(item.key)) collapsed.delete(item.key); else collapsed.add(item.key);
      vlist?.setItems(buildItems(), { keepScroll: true });   // hide/show the group's rows in place
    });
    return el('tr', { class: 'grp grp-group' }, [
      el('td', { class: 'cell-check' }, checkKids),
      el('td', { colspan: String(scope.columns.length - 1), class: 'grp-label' }, [toggle]),
    ]);
  }

  // sectionHeadRow is a My-uploads state section header (label only) as a table row.
  function sectionHeadRow(item) {
    return el('tr', { class: 'grp grp-section' }, [
      el('td', { class: 'cell-check' }),
      el('td', { colspan: String(scope.columns.length - 1), class: 'grp-label grp-section-label', text: item.label }),
    ]);
  }

  // buildItems is the single source for the windowed item array, by current view:
  // grouped (artist/album), collapsible (uploader), sections (state), or flat.
  function buildItems() {
    const files = visibleFiles();
    // Grouped: paged scopes stream their items (built incrementally as pages
    // arrive); non-paged scopes hold the whole set and group it in one pass.
    if (groupedActive()) return paged ? gstream.items.slice() : groupedItems(files);
    if (scope.grouping?.kind === 'collapsible') return collapsibleItems(sortFilesBy(files, sortToken));
    if (scope.grouping?.kind === 'sections')   return sectionItems(sortFilesBy(files, sortToken));
    // Flat: paged scopes are already server-sorted (and stream more on scroll);
    // non-paged flat scopes (Trash) sort the loaded rows client-side.
    return (paged ? files : sortFilesBy(files, sortToken)).map(flatItem);
  }

  // ── Browse (artist → album → track) ──────────────────────────────────────────
  function trackCount(n) { return `${n} track${n === 1 ? '' : 's'}`; }
  function cover(level, item) {
    const url = scope.browse.coverURL ? scope.browse.coverURL(level, item, br.artist) : null;
    if (!url) return el('div', { class: 'entity-cover entity-cover--empty', 'aria-hidden': 'true', text: '♪' });
    return el('img', { class: 'entity-cover', alt: '', loading: 'lazy', src: url });
  }
  function crumb() {
    const parts = [crumbNode('Artists', br.level === 'artists' ? null : () => { br.level = 'artists'; br.artist = null; br.album = null; loadBrowse(); })];
    if (br.artist != null) {
      parts.push(el('span', { class: 'crumb-sep', 'aria-hidden': 'true', text: '›' }));
      parts.push(crumbNode(br.artist || '(no artist)', br.level === 'albums' ? null : () => { br.level = 'albums'; br.album = null; loadBrowse(); }));
    }
    if (br.album != null && br.level === 'tracks') {
      parts.push(el('span', { class: 'crumb-sep', 'aria-hidden': 'true', text: '›' }));
      parts.push(crumbNode(br.album || '(no album)', null));
    }
    return el('nav', { class: 'entity-breadcrumb', 'aria-label': 'Library navigation' }, parts);
  }
  function crumbNode(label, onClick) {
    return onClick ? el('button', { class: 'crumb-link', text: label, onclick: onClick }) : el('span', { class: 'crumb-current', text: label });
  }

  function browseTree() {
    const panel = el('div', { class: 'entity-panel' });
    const items = (br.items || []).filter(it => matches(it.name ?? it.title));
    if (!items.length) { panel.appendChild(el('div', { class: 'table-state-row', text: filterText ? 'No matches.' : 'Nothing here.' })); return panel; }

    if (br.level === 'artists') {
      items.forEach(a => panel.appendChild(entityRow({
        name: a.name, fallback: '(no artist)', item: a, level: 'artist',
        meta: `${a.album_count != null ? a.album_count + ' albums · ' : ''}${trackCount(a.track_count ?? 0)}`,
        onOpen: () => { br.level = 'albums'; br.artist = a.name; br.album = null; loadBrowse(); },
      })));
    } else if (br.level === 'albums') {
      items.forEach(a => {
        const meta = [a.year, trackCount(a.track_count ?? 0)].filter(v => v != null).join(' · ');
        panel.appendChild(entityRow({
          name: a.title, fallback: '(no album)', item: a, level: 'album', meta,
          onOpen: () => { br.level = 'tracks'; br.album = a.title; loadBrowse(); },
        }));
      });
    } else {
      items.forEach(t => panel.appendChild(trackRow(t)));
    }
    return panel;
  }

  function entityRow({ name, fallback, item, level, meta, onOpen }) {
    const main = el('button', { class: 'entity-main', onclick: onOpen, 'aria-label': `Open ${name || fallback}` }, [
      el('span', { class: 'entity-name' + (name ? '' : ' is-fallback'), text: name || fallback }),
      el('span', { class: 'entity-meta', text: meta }),
    ]);
    const row = el('div', { class: 'entity-row' }, [cover(level, item), main]);
    if (scope.bulkApply && name) {
      const actions = el('div', { class: 'entity-actions' }, [
        el('button', { class: 'btn btn-neutral btn-sm', text: 'Edit tags…', onclick: () => editGroupTags(level, item) }),
      ]);
      row.appendChild(actions);
    }
    return row;
  }

  function trackRow(t) {
    const hash = scope.browse.trackHash ? scope.browse.trackHash(t) : t.hash;
    const kids = [];
    if (hash && isSelectableTrack()) {
      const cb = el('input', { type: 'checkbox', class: 'fl-rowcheck', 'aria-label': `Select ${t.title || 'track'}` });
      cb.dataset.hash = hash;
      cb.checked = selected.has(hash);
      cb.addEventListener('change', () => { cb.checked ? selected.add(hash) : selected.delete(hash); syncSelectionUI(); });
      kids.push(cb);
    }
    if (scope.onPlay) kids.push(el('button', { class: 'play-btn', title: 'Preview', 'aria-label': `Preview ${t.title || 'track'}`, html: PLAY_ICON, onclick: () => scope.onPlay({ ...t, hash }, []) }));
    kids.push(el('span', { class: 'entity-tracknum', text: t.track_number != null ? String(t.track_number) : '' }));
    kids.push(el('span', { class: 'entity-name' + (t.title ? '' : ' is-fallback'), text: t.title || 'Untitled' }));
    if (scope.editPatchURL) {
      kids.push(el('div', { class: 'entity-actions' }, [
        el('button', { class: 'btn btn-neutral btn-sm btn-edit', text: 'Edit', onclick: () => editBrowseTrack(t) }),
      ]));
    }
    return el('div', { class: 'entity-row entity-row--track', 'data-hash': hash || '' }, kids);
  }
  function isSelectableTrack() { return !!scope.bulkApply; }

  async function editBrowseTrack(t) {
    const f = scope.browse.resolveTrackFile ? await scope.browse.resolveTrackFile(t) : t;
    if (!f) { toast('Couldn’t find this file’s details.', 'error'); return; }
    editor().open(f);
  }
  async function editGroupTags(level, item) {
    try {
      const hashes = await scope.browse.groupHashes(level, item, br.artist);
      if (!hashes.length) { toast('No files found for this group.', 'error'); return; }
      bulkEditor().open(hashes, selectionTags(hashes));
    } catch (err) { toast(`Couldn’t gather the group: ${err.message}`, 'error'); }
  }

  // clearPageSelection drops the current selection when the page/filter/sort
  // changes (paged mode), so a checkbox can never act on a row no longer shown.
  function clearPageSelection() { selected.clear(); selectAllMatching = false; }

  // ── Chrome ──────────────────────────────────────────────────────────────────
  function headerBar() {
    // Keep the persistent search box's value in sync with filterText when it's not
    // being typed into (so a programmatic reset shows), but never clobber live
    // typing — the input event owns the value while the field is focused.
    if (document.activeElement !== searchInput) searchInput.value = filterText;
    const heading = el('h2', { class: 'section-title section-title--files' });
    heading.append(scope.title);
    if (view === 'list') heading.append(` (${paged ? total : rows.length})`);
    const controls = [];
    if (view === 'list') {
      controls.push(sortControl());
      if (scope.artistAlbumSort) controls.push(groupToggle());
      controls.push(filterFieldControl());
    }
    controls.push(el('div', { class: 'files-search' }, [searchInput]));
    return el('div', { class: 'files-bar' }, [heading, el('div', { class: 'files-bar-controls' }, controls)]);
  }

  // SORT_OPTIONS are the flat orders; the tokens mirror the server's allow-list
  // (fileSortOrder) so the same dropdown drives the paged (server) and the
  // in-memory (client) lists identically.
  const SORT_OPTIONS = [
    ['created_desc', 'Newest first'], ['created_asc', 'Oldest first'],
    ['title_asc', 'Title A–Z'], ['title_desc', 'Title Z–A'],
    ['artist_asc', 'Artist A–Z'], ['artist_desc', 'Artist Z–A'],
    ['size_desc', 'Largest first'], ['size_asc', 'Smallest first'],
    ['untagged_first', 'Untagged first'],
  ];

  // sortControl is the single sort dropdown for every list view. Non-paged scopes
  // also get a "Default order" (as loaded) entry. The grouped view is a separate
  // toggle (groupToggle), so it isn't in this list. Changing the sort re-queries
  // the server (paged) or re-renders in place (non-paged). The dropdown is disabled
  // while grouping is on, since the grouped view imposes its own order.
  function sortControl() {
    const opts = [];
    if (!paged) opts.push(['default', 'Default order']);
    opts.push(...SORT_OPTIONS);

    const on = groupedActive();
    const sel = el('select', {
      class: 'files-sort-select', 'aria-label': 'Sort',
      disabled: on ? 'true' : null,
      title: on ? 'Grouped by artist / album — turn off grouping to sort' : null,
    });
    for (const [val, label] of opts) {
      const o = el('option', { value: val, text: label });
      if (val === sortToken) o.selected = true;
      sel.appendChild(o);
    }
    sel.addEventListener('change', () => {
      sortToken = sel.value;
      if (paged) { clearPageSelection(); reload(); return; }
      try { localStorage.setItem(SORT_KEY, sortToken); } catch { /* ignore */ }
      render();
    });
    return el('div', { class: 'files-sort' }, [sel]);
  }

  // groupToggle is the independent "By artist / album" view switch — a pill that's
  // separate from the flat sort dropdown so the grouped view isn't lost among the
  // sort orders (owner ask). On a paged scope, turning it on re-queries with the
  // server grouping order and streams it; turning it off returns to the flat list.
  // Both stay lazy (infinite scroll). Reuses the .sort-switch / .vm-btn styling.
  function groupToggle() {
    const on = groupedActive();
    const b = el('button', {
      type: 'button', class: 'vm-btn' + (on ? ' is-active' : ''),
      'aria-pressed': String(on), title: 'Group by artist / album', text: 'By artist / album',
    });
    b.addEventListener('click', () => {
      grouped = !grouped;
      // Paged: the grouped view streams its own server order, so re-query (and drop
      // the page/filter-mode selection). Non-paged: just re-render — the selected
      // hashes stay valid across the flat ⇄ grouped views.
      if (paged) { clearPageSelection(); reload(); return; }
      try { localStorage.setItem(GROUP_KEY, grouped ? '1' : '0'); } catch { /* ignore */ }
      render();
    });
    return el('div', { class: 'sort-switch', role: 'group', 'aria-label': 'Grouping' }, [b]);
  }

  // FIELD_OPTIONS are the filter-type scopes (the dropdown). The tokens match the
  // server's normalizeQField allow-list and the client fieldStrings() switch, so
  // the same choice drives the paged (server) and in-memory (client) filters.
  const FIELD_OPTIONS = [['', 'General'], ['artist', 'Artist'], ['album', 'Album'], ['title', 'Track name']];

  // filterFieldControl is the filter-type dropdown beside the search box: it scopes
  // what the filter term matches (General = every field, or just artist / album /
  // track name). Changing it re-queries (paged) or re-filters in place (non-paged)
  // and updates the search placeholder. Persisted across visits like the sort.
  function filterFieldControl() {
    const sel = el('select', { class: 'files-sort-select files-field-select', 'aria-label': 'Filter type' });
    for (const [val, label] of FIELD_OPTIONS) {
      const o = el('option', { value: val, text: label });
      if (val === qField) o.selected = true;
      sel.appendChild(o);
    }
    sel.addEventListener('change', () => {
      qField = sel.value;
      try { localStorage.setItem(FIELD_KEY, qField); } catch { /* ignore */ }
      updateSearchPlaceholder();
      // An empty filter box means the field choice changes nothing yet — but a
      // re-render is harmless and keeps the control state consistent.
      if (paged) { clearPageSelection(); reload(); }
      else renderContent();
    });
    return el('div', { class: 'files-filter-field' }, [sel]);
  }

  // sortFilesBy returns a sorted COPY of an in-memory row set for a flat token
  // (non-paged scopes; the paged list is ordered server-side). 'default' / unknown
  // leaves the order as loaded. Recency falls back across the scopes' timestamps.
  function sortFilesBy(files, token) {
    const arr = files.slice();
    const lc = s => (s || '').toLowerCase();
    const rec = f => Number(f.created_at ?? f.deleted_at ?? f.id ?? 0);
    const title = f => lc(f.title || f.filename);
    const artist = f => lc(f.album_artist || f.artist);
    const size = f => Number(f.byte_size ?? 0);
    const cmp = {
      created_desc: (a, b) => rec(b) - rec(a),
      created_asc: (a, b) => rec(a) - rec(b),
      title_asc: (a, b) => title(a).localeCompare(title(b)),
      title_desc: (a, b) => title(b).localeCompare(title(a)),
      artist_asc: (a, b) => artist(a).localeCompare(artist(b)),
      artist_desc: (a, b) => artist(b).localeCompare(artist(a)),
      size_desc: (a, b) => size(b) - size(a),
      size_asc: (a, b) => size(a) - size(b),
      untagged_first: (a, b) => (needsMeta(b) - needsMeta(a)) || (rec(b) - rec(a)),
    }[token];
    return cmp ? arr.sort(cmp) : arr;
  }

  // selectAllBanner offers (and reflects) cross-page selection in paged mode: once
  // every loaded row is ticked and more matches remain unfetched, "Select all N
  // matching" flips bulk actions onto the entire filtered set (via the scope's
  // runAll), so a bulk action need never materialise the unscrolled rows.
  function selectAllBanner() {
    if (!paged || view !== 'list') return null;
    if (selectAllMatching) {
      return el('div', { class: 'select-all-banner is-active' }, [
        `All ${total} matching file${total === 1 ? '' : 's'} selected. `,
        el('button', { type: 'button', class: 'linklike', text: 'Clear selection', onclick: () => { clearPageSelection(); render(); } }),
      ]);
    }
    const loadedSel = rows.filter(isSelectable);
    if (loadedSel.length && loadedSel.every(f => selected.has(f.hash)) && total > loadedSel.length) {
      return el('div', { class: 'select-all-banner' }, [
        `All ${loadedSel.length} loaded file${loadedSel.length === 1 ? '' : 's'} selected. `,
        el('button', { type: 'button', class: 'linklike', text: `Select all ${total} matching`, onclick: () => { selectAllMatching = true; render(); } }),
      ]);
    }
    return null;
  }

  function viewSwitch() {
    const mk = (id, label) => {
      const b = el('button', { class: 'vm-btn' + (view === id ? ' is-active' : ''), 'aria-pressed': String(view === id), text: label });
      b.addEventListener('click', () => { if (view !== id) { view = id; reload(); } });
      return b;
    };
    return el('div', { class: 'vm-switch', role: 'group', 'aria-label': 'View' }, [mk('browse', 'Browse'), mk('list', 'List')]);
  }

  function bulkToolbar() {
    const buttons = [];
    if (scope.bulkApply) buttons.push(el('button', { class: 'btn btn-neutral btn-sm fl-bulk-btn fl-bulk-edit', text: 'Edit tags…', disabled: 'true', onclick: () => openBulkEditor() }));
    for (const a of scope.bulkActions || []) {
      const cls = (a.kind === 'danger' ? 'btn btn-destructive-outline btn-sm' : 'btn btn-neutral btn-sm') + ' fl-bulk-btn';
      buttons.push(el('button', { class: cls, text: a.label, disabled: 'true', onclick: () => runBulkAction(a) }));
    }
    if (!buttons.length) return null;
    return el('div', { class: 'bulk-toolbar' }, [
      el('span', { class: 'bulk-selcount fl-selcount', text: '0 selected' }),
      el('span', { class: 'bulk-spacer' }),
      ...buttons,
    ]);
  }
  // openBulkEditor opens the shared bulk tag/access editor. In filter mode it
  // can't compute shared values from the page alone, so it opens set-only (empty
  // prefill) with an "all N matching" headline; onApply routes to bulkApplyAll.
  function openBulkEditor() {
    if (paged && selectAllMatching && scope.bulkApplyAll) {
      bulkEditor().open([...selected], { common: {}, mixed: new Set() }, `Applies to all ${total} matching files.`);
    } else {
      bulkEditor().open([...selected], selectionTags([...selected]));
    }
  }

  async function runBulkAction(a) {
    // Filter-mode (select-all-matching): act on the whole filtered set server-
    // side via the scope's runAll, instead of the in-memory hash selection.
    if (paged && selectAllMatching) {
      if (!a.runAll) { toast('That action can’t target all matching files yet.', 'error'); return; }
      try {
        const changed = await a.runAll({ q: filterText.trim(), field: qField });
        if (changed === false) return;
        clearPageSelection();
        await reload();
      } catch (err) { toast(`${a.label} failed: ${err.message}`, 'error'); }
      return;
    }
    const hashes = [...selected];
    if (!hashes.length) return;
    try {
      const changed = await a.run(hashes);
      if (changed === false) return;     // e.g. a cancelled confirm — keep selection
      selected.clear();
      await reload();
    } catch (err) { toast(`${a.label} failed: ${err.message}`, 'error'); }
  }

  function stateBlock(text) { return el('div', { class: 'table-state-row', text }); }
  function emptyBlock() {
    return el('div', { class: 'empty-state' }, [
      el('div', { class: 'drop-icon', 'aria-hidden': 'true', text: '♪' }),
      el('p', { text: scope.emptyText || 'Nothing here.' }),
    ]);
  }
  function errorBlock() {
    const retry = el('button', { class: 'btn btn-neutral btn-sm', text: 'Retry', onclick: reload });
    return el('div', { role: 'alert' }, [el('p', { class: 'section-copy', text: 'Failed to load.' }), retry]);
  }

  function ensureBodyHost() { if (!bodyHost) bodyHost = el('div', { class: 'fl-body' }); return bodyHost; }
  function emptyOrNoMatch() {
    const q = filterText.trim();
    return (rows.length && q) ? stateBlock(`No files match “${q}”`) : emptyBlock();
  }

  // renderBody fills the persistent body host: the loading/error/browse states, or
  // the windowed list (flat / grouped / collapsible / sections — every list view is
  // windowed). The header bar (search box) is owned by render() and untouched here.
  function renderBody() {
    const host = ensureBodyHost();
    if (loading)   { destroyVList(); host.replaceChildren(stateBlock('Loading…')); return; }
    if (loadError) { destroyVList(); host.replaceChildren(errorBlock()); return; }
    if (view === 'browse' && hasBrowse) { destroyVList(); host.replaceChildren(crumb(), browseTree()); return; }
    if (!visibleFiles().length) { destroyVList(); host.replaceChildren(emptyOrNoMatch()); return; }
    renderWindowed();
  }

  function render() {
    if (!mountEl) return;
    // If the persistent search box had focus, restore it after the rebuild so the
    // caret survives a reload (the same node is re-mounted by headerBar()).
    const keepSearchFocus = document.activeElement === searchInput;
    const kids = [headerBar()];
    if (hasBrowse) kids.push(viewSwitch());
    if (scope.desc) kids.push(el('p', { class: 'scope-desc', text: scope.desc }));
    const tb = (view === 'list') ? bulkToolbar() : (hasBrowse ? bulkToolbar() : null);
    if (tb) kids.push(tb);
    const banner = selectAllBanner();
    if (banner) kids.push(banner);
    kids.push(ensureBodyHost());
    mountEl.replaceChildren(...kids);
    if (keepSearchFocus) searchInput.focus();
    renderBody();
    syncSelectionUI();
    applyPlayingHighlight();
  }

  // renderContent re-fills only the body, leaving the header (search box + its
  // focus/caret) and the bulk toolbar in place — used by the non-paged filter so
  // typing doesn't rebuild and blur the search field.
  function renderContent() {
    if (!mountEl || !bodyHost) { render(); return; }
    renderBody();
    syncSelectionUI();
    applyPlayingHighlight();
  }

  // ── Playing-row highlight (page drives it via setPlaying) ───────────────────
  function applyPlayingHighlight() {
    if (!mountEl) return;
    mountEl.querySelectorAll('.playing-row').forEach(r => r.classList.remove('playing-row'));
    if (!playingHash) return;
    mountEl.querySelector(`[data-hash="${CSS.escape(playingHash)}"]`)?.classList.add('playing-row');
  }
  function setPlaying(hash) { playingHash = hash || null; applyPlayingHighlight(); }

  // ── Public surface ────────────────────────────────────────────────────────────
  function mount(node) { mountEl = node; reload(); }
  function destroy() {
    destroyVList();
    _editor?.destroy(); _editor = null;
    _bulk?.destroy(); _bulk = null;
    _cover?.destroy(); _cover = null;
    bodyHost = null;
    mountEl = null;
  }

  return { mount, reload, setPlaying, getVisible, destroy };
}

function shortHash(h) {
  if (!h) return '';
  return h.length > 12 ? h.slice(0, 12) + '…' : h;
}
