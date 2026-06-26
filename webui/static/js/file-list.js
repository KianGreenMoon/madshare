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
  let rows = [];                 // current list rows
  let loading = false, loadError = false;
  let filterText = '';
  let view = hasBrowse ? 'browse' : 'list';
  let playingHash = null;
  let contentEl = null;          // the rows container; re-rendered alone on filter

  // Default ⇄ artist/album sort (scope.artistAlbumSort). Persisted so the choice
  // sticks across visits.
  const SORT_KEY = 'madshare-files-sort';
  let sortMode = 'default';
  if (scope.artistAlbumSort) { try { if (localStorage.getItem(SORT_KEY) === 'grouped') sortMode = 'grouped'; } catch { /* ignore */ } }

  const selected = new Set();    // selected file hashes (shared list ⇄ browse)
  const collapsed = new Set();   // collapsed group keys (collapsible grouping)
  const br = { level: 'artists', artist: null, album: null, items: [] };

  // ── Server-paged mode (scope.paged) ─────────────────────────────────────────
  // The admin All-files scope is too large to hold in the DOM, so it loads one
  // server page at a time (file-list-scaling.md). filterText + sort + page round-
  // trip to scope.loadPage({limit,offset,q,sort}) → {total,items}; rows holds just
  // the current page. selectAllMatching flips bulk actions onto the whole filtered
  // set (via the scope's runAll) rather than the in-memory selection. In this mode
  // the client grouped sort, sections, and in-memory filter are all bypassed.
  const paged = !!scope.paged;
  const PAGE_SIZE = scope.pageSize || 100;
  let page = 0;                  // zero-based page index
  let total = 0;                 // total rows matching the current filter
  let serverSort = 'created_desc';
  let selectAllMatching = false; // bulk acts on every matching row, not just the page

  let _editor = null, _bulk = null, _cover = null;

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
          await scope.bulkApplyAll({ q: filterText.trim() }, patch);
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

  // ── Loading ─────────────────────────────────────────────────────────────────
  async function reload() {
    if (view === 'browse' && hasBrowse) return loadBrowse();
    if (paged) return loadPage();
    return loadList();
  }
  async function loadPage() {
    loading = true; loadError = false; render();
    try {
      const res = await scope.loadPage({ limit: PAGE_SIZE, offset: page * PAGE_SIZE, q: filterText.trim(), sort: serverSort }) || {};
      rows = res.items || [];
      total = res.total || 0;
    } catch (err) { loadError = true; console.error('file-list page load failed:', err); }
    loading = false;
    // A delete/filter can leave us past the last page; clamp once and refetch.
    const lastPage = Math.max(0, Math.ceil(total / PAGE_SIZE) - 1);
    if (!loadError && page > lastPage && rows.length === 0 && total > 0) {
      page = lastPage;
      return loadPage();
    }
    render();
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
  function visibleFiles() {
    if (paged) return rows;   // the server already filtered this page
    const q = filterText.trim().toLowerCase();
    if (!q) return rows;
    return rows.filter(f => [f.title, f.artist, f.album, f.filename].filter(Boolean).join(' ').toLowerCase().includes(q));
  }
  function getVisible() { return visibleFiles(); }

  // ── Cells ──────────────────────────────────────────────────────────────────
  function accessSummary(f) {
    const parts = [f.guest_playable ? 'Guest' : 'Private'];
    if (f.license) parts.push(f.license);
    return el('span', { class: 'cell-muted', text: parts.join(' · ') });
  }

  function titleCell(f) {
    const titleSpan = f.title
      ? el('span', { class: 'cell-title', text: f.title })
      : el('span', { class: 'cell-title is-fallback', text: f.filename || 'Untitled' });
    const kids = [titleSpan];
    // The badge fn gets whether the list is in grouped sort, so a scope can show
    // a state badge only when its native (sectioned) grouping is hidden.
    const b = scope.badge ? scope.badge(f, scope.artistAlbumSort && sortMode === 'grouped') : null;
    if (b) kids.push(document.createTextNode(' '), el('span', { class: `state-badge ${b.cls || ''}`, title: b.title || null, text: b.text }));
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
      case 'artist': return f.artist ? el('td', { 'data-label': 'Artist', text: f.artist }) : el('td', { class: 'cell-muted', 'data-label': 'Artist', text: '—' });
      case 'album':  return f.album ? el('td', { 'data-label': 'Album', text: f.album }) : el('td', { class: 'cell-muted', 'data-label': 'Album', text: '—' });
      case 'size':   return el('td', { class: 'cell-size', 'data-label': 'Size', text: fmtBytes(f.byte_size) });
      case 'access': return el('td', { class: 'cell-access', 'data-label': 'Access' }, [accessSummary(f)]);
      case 'meta': {
        const v = scope.metaValue ? scope.metaValue(f) : '';
        return v ? el('td', { 'data-label': scope.metaLabel || 'Meta', text: v }) : el('td', { class: 'cell-muted', 'data-label': scope.metaLabel || 'Meta', text: '—' });
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

    mountEl.querySelectorAll('.fl-group').forEach(g => {
      const gc = g.querySelector('.fl-groupcheck'); if (!gc) return;
      const cs = [...g.querySelectorAll('.fl-rowcheck')];
      const n = cs.filter(c => c.checked).length;
      gc.checked = cs.length > 0 && n === cs.length;
      gc.indeterminate = n > 0 && n < cs.length;
    });

    // Artist/album separator checkboxes (grouped sort): checked when all governed
    // hashes are selected, indeterminate when some.
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
    render();
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

  function table(files, withSelectAll = true) {
    const body = el('tbody');
    files.forEach(f => {
      const holder = {};
      const tr = el('tr', rowAttrs(f), scope.columns.map(c => bodyCell(c, f, holder)));
      body.appendChild(tr);
    });
    return el('div', { class: 'files-table-wrap' }, [
      el('table', { class: 'files-table' }, [el('thead', {}, [headRow(withSelectAll)]), body]),
    ]);
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

  function groupedTable(files) {
    const body = el('tbody', { class: 'is-grouped' });
    for (const art of buildArtistGroups(files)) {
      const artFiles = art.albumList.flatMap(al => al.files);
      const artHashes = artFiles.filter(isSelectable).map(f => f.hash);
      // All files in a group share one artist_id / album_id (resolved from the
      // same effectiveArtist/album), so the representative row's has_image flag
      // reflects the whole group's entity.
      body.appendChild(grpSepRow('artist', art.key || 'Unknown artist',
        `${art.albumList.length} album${art.albumList.length === 1 ? '' : 's'} · ${artFiles.length} track${artFiles.length === 1 ? '' : 's'}`,
        artHashes, !art.key, coverBtn('artist', { artist: art.key }, !art.key, artFiles[0]?.artist_has_image)));
      for (const al of art.albumList) {
        const y = albumYear(al.files);
        body.appendChild(grpSepRow('album', al.key || 'Other', y < 9999 ? String(y) : '',
          al.files.filter(isSelectable).map(f => f.hash), !al.key,
          coverBtn('album', { artist: art.key, album: al.key }, !al.key, al.files[0]?.album_has_image)));
        // Multi-disc album → a quiet "Disc N" separator before each disc (purely
        // visual; the files are already disc-then-track ordered above). disc.js is
        // the shared rule (docs/architecture/disc-numbering.md).
        const multiDisc = isMultiDisc(al.files);
        let shownDisc;   // undefined: no real disc key equals it
        al.files.forEach(f => {
          const disc = discKey(f.disc_number);
          if (multiDisc && disc !== shownDisc) {
            shownDisc = disc;
            body.appendChild(grpSepRow('disc', discLabel(disc), '', [], false, null));
          }
          body.appendChild(groupedTrack(f));
        });
      }
    }
    return el('div', { class: 'files-table-wrap' }, [
      el('table', { class: 'files-table' }, [el('thead', {}, [headRow(true)]), body]),
    ]);
  }

  function uploaderGroups(files) {
    const g = scope.grouping;
    const byKey = new Map();
    for (const f of files) {
      const k = String(g.by(f));
      if (!byKey.has(k)) byKey.set(k, { key: k, label: g.label(f), items: [] });
      byKey.get(k).items.push(f);
    }
    const frag = document.createDocumentFragment();
    for (const grp of byKey.values()) {
      const section = el('section', { class: 'mod-group fl-group' + (collapsed.has(grp.key) ? ' is-collapsed' : '') });
      const bodyId = `flGroup-${grp.key}`;

      const groupCheck = el('input', { type: 'checkbox', class: 'mod-group-check fl-groupcheck', 'aria-label': `Select all from ${grp.label}` });
      const selectableItems = grp.items.filter(isSelectable);
      if (!selectableItems.length) groupCheck.disabled = true;
      groupCheck.addEventListener('change', () => {
        selectableItems.forEach(f => groupCheck.checked ? selected.add(f.hash) : selected.delete(f.hash));
        render();
      });

      const toggle = el('button', { class: 'mod-group-toggle', 'aria-expanded': String(!collapsed.has(grp.key)), 'aria-controls': bodyId }, [
        el('span', { class: 'mod-group-chevron', 'aria-hidden': 'true', text: '▾' }),
        el('span', { text: grp.label }),
        el('span', { class: 'mod-group-counts', text: g.counts ? g.counts(grp.items) : `${grp.items.length}` }),
      ]);
      toggle.addEventListener('click', () => {
        const c = section.classList.toggle('is-collapsed');
        if (c) collapsed.add(grp.key); else collapsed.delete(grp.key);
        toggle.setAttribute('aria-expanded', String(!c));
      });

      section.append(
        el('div', { class: 'mod-group-header' }, [groupCheck, toggle]),
        el('div', { class: 'mod-group-body', id: bodyId }, [table(grp.items, false)]),
      );
      frag.appendChild(section);
    }
    return frag;
  }

  function sectionGroups(files) {
    const frag = document.createDocumentFragment();
    for (const sec of scope.grouping.sections) {
      const items = files.filter(sec.match);
      if (!items.length) continue;
      frag.append(
        el('h3', { class: 'section-title section-title--sub', text: `${sec.label} (${items.length})` }),
        table(items, false),
      );
    }
    return frag;
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
  let filterTimer;
  function headerBar() {
    const search = el('input', { type: 'search', placeholder: 'Filter…', autocomplete: 'off', 'aria-label': 'Filter files' });
    search.value = filterText;
    search.addEventListener('input', () => {
      clearTimeout(filterTimer);
      filterTimer = setTimeout(() => {
        filterText = search.value;
        // Paged: a new filter is a fresh server query from page 0; in-memory
        // scopes just re-filter the rows already loaded.
        if (paged) { page = 0; clearPageSelection(); reload(); }
        else renderContent();
      }, 200);
    });
    const heading = el('h2', { class: 'section-title section-title--files' });
    heading.append(scope.title);
    if (view === 'list') heading.append(` (${paged ? total : rows.length})`);
    const controls = [];
    if (paged && view === 'list') controls.push(serverSortControl());
    else if (scope.artistAlbumSort && view === 'list') controls.push(sortSwitch());
    controls.push(el('div', { class: 'files-search' }, [search]));
    return el('div', { class: 'files-bar' }, [heading, el('div', { class: 'files-bar-controls' }, controls)]);
  }

  // serverSortControl is the paged list's sort dropdown — the tokens mirror the
  // server's allow-list (fileSortOrder). Changing sort restarts at page 0.
  const SORT_OPTIONS = [
    ['created_desc', 'Newest first'], ['created_asc', 'Oldest first'],
    ['title_asc', 'Title A–Z'], ['title_desc', 'Title Z–A'],
    ['artist_asc', 'Artist A–Z'], ['artist_desc', 'Artist Z–A'],
    ['size_desc', 'Largest first'], ['size_asc', 'Smallest first'],
    ['untagged_first', 'Untagged first'],
  ];
  function serverSortControl() {
    const sel = el('select', { class: 'files-sort-select', 'aria-label': 'Sort' });
    for (const [val, label] of SORT_OPTIONS) {
      const o = el('option', { value: val, text: label });
      if (val === serverSort) o.selected = true;
      sel.appendChild(o);
    }
    sel.addEventListener('change', () => { serverSort = sel.value; page = 0; clearPageSelection(); reload(); });
    return el('div', { class: 'files-sort' }, [sel]);
  }

  // pager renders the Prev / "page N of M · T files" / Next control under the
  // paged table. Changing page clears the selection (it can't span pages — use
  // "Select all N matching" for that).
  function pager() {
    const pages = Math.max(1, Math.ceil(total / PAGE_SIZE));
    const cur = Math.min(page + 1, pages);
    const go = d => { page += d; clearPageSelection(); reload(); };
    const prev = el('button', { class: 'btn btn-neutral btn-sm', text: '‹ Prev', disabled: cur <= 1 ? 'true' : null, onclick: () => go(-1) });
    const next = el('button', { class: 'btn btn-neutral btn-sm', text: 'Next ›', disabled: cur >= pages ? 'true' : null, onclick: () => go(1) });
    const label = el('span', { class: 'pager-label', text: `Page ${cur} of ${pages} · ${total} file${total === 1 ? '' : 's'}` });
    return el('div', { class: 'files-pager' }, [prev, label, next]);
  }

  // selectAllBanner offers (and reflects) cross-page selection in paged mode:
  // once the whole page is ticked and more matches exist, "Select all N matching"
  // flips bulk actions onto the entire filtered set (via the scope's runAll).
  function selectAllBanner() {
    if (!paged || view !== 'list') return null;
    if (selectAllMatching) {
      return el('div', { class: 'select-all-banner is-active' }, [
        `All ${total} matching file${total === 1 ? '' : 's'} selected. `,
        el('button', { type: 'button', class: 'linklike', text: 'Clear selection', onclick: () => { clearPageSelection(); render(); } }),
      ]);
    }
    const pageSel = rows.filter(isSelectable);
    if (pageSel.length && pageSel.every(f => selected.has(f.hash)) && total > pageSel.length) {
      return el('div', { class: 'select-all-banner' }, [
        `All ${pageSel.length} on this page selected. `,
        el('button', { type: 'button', class: 'linklike', text: `Select all ${total} matching`, onclick: () => { selectAllMatching = true; render(); } }),
      ]);
    }
    return null;
  }

  // sortSwitch toggles the flat list between its default order and the
  // artist → album → track# grouped sort (scope.artistAlbumSort).
  function sortSwitch() {
    const mk = (m, label) => {
      const b = el('button', { type: 'button', class: 'vm-btn' + (sortMode === m ? ' is-active' : ''), 'aria-pressed': String(sortMode === m), text: label });
      b.addEventListener('click', () => {
        if (sortMode === m) return;
        sortMode = m;
        try { localStorage.setItem(SORT_KEY, m); } catch { /* ignore */ }
        render();
      });
      return b;
    };
    return el('div', { class: 'sort-switch', role: 'group', 'aria-label': 'Sort' }, [mk('default', 'Default'), mk('grouped', 'By artist / album')]);
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
        const changed = await a.runAll({ q: filterText.trim() });
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

  function content() {
    if (loading) return stateBlock('Loading…');
    if (loadError) return errorBlock();
    if (view === 'browse' && hasBrowse) return el('div', {}, [crumb(), browseTree()]);
    // Paged: a flat table of just this page (grouping/sections live elsewhere).
    if (paged) {
      if (!rows.length) return filterText.trim() ? stateBlock(`No files match “${filterText.trim()}”`) : emptyBlock();
      return table(rows, true);
    }
    if (!rows.length) return emptyBlock();
    const files = visibleFiles();
    if (!files.length) return stateBlock(`No files match “${filterText.trim()}”`);
    if (scope.artistAlbumSort && sortMode === 'grouped') return groupedTable(files);
    if (scope.grouping?.kind === 'collapsible') return uploaderGroups(files);
    if (scope.grouping?.kind === 'sections') return sectionGroups(files);
    return table(files, true);
  }

  function render() {
    if (!mountEl) return;
    const kids = [headerBar()];
    if (hasBrowse) kids.push(viewSwitch());
    if (scope.desc) kids.push(el('p', { class: 'scope-desc', text: scope.desc }));
    const tb = (view === 'list') ? bulkToolbar() : (hasBrowse ? bulkToolbar() : null);
    if (tb) kids.push(tb);
    const banner = selectAllBanner();
    if (banner) kids.push(banner);
    contentEl = el('div', { class: 'fl-content' });
    contentEl.appendChild(content());
    kids.push(contentEl);
    if (paged && view === 'list' && !loading && !loadError && total > 0) kids.push(pager());
    mountEl.replaceChildren(...kids);
    syncSelectionUI();
    applyPlayingHighlight();
  }

  // renderContent re-renders only the rows, leaving the header (search box + its
  // focus/caret) and the bulk toolbar in place — used by the filter input so
  // typing doesn't rebuild and blur the search field.
  function renderContent() {
    if (!mountEl || !contentEl) { render(); return; }
    contentEl.replaceChildren(content());
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
    _editor?.destroy(); _editor = null;
    _bulk?.destroy(); _bulk = null;
    _cover?.destroy(); _cover = null;
    mountEl = null;
  }

  return { mount, reload, setPlaying, getVisible, destroy };
}

function shortHash(h) {
  if (!h) return '';
  return h.length > 12 ? h.slice(0, 12) + '…' : h;
}
