// Shared demo core for the unified-file-management mockups. THROWAWAY review
// code — its only job is to prove the shape: one row renderer + one Edit modal +
// one bulk-edit modal + one selection model, parameterised by `scope`, plus a
// Browse (artist → album → track) presentation for finding files without search.
// The real build extracts this into webui/static/js/file-list.js, wired to the
// live endpoints + track-edit.js. Loaded as a classic script (file:// blocks
// ES-module fetches in some browsers); everything hangs off window.Demo.

(function () {
  const STATE_LABEL = { submitted: 'Awaiting review', returned: 'Returned', draft: 'Draft' };
  const PLAY = '<svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor"><path d="M8 5v14l11-7z"/></svg>';
  const LICENSES = ['', 'CC BY', 'CC BY-SA', 'CC0', 'All rights reserved'];

  // ── Per-scope config: columns, grouping, bulk + row actions, badge, copy ──
  const SCOPES = {
    all: {
      title: 'Files', count: 128, group: null,
      desc: 'Every published track. Browse by artist/album to find things, or switch to a flat list. Edit tags + access per file, or select several and edit them together.',
      columns: ['check', 'title', 'artist', 'album', 'size', 'access', 'actions'],
      bulk: [{ label: 'Edit tags…', bulkEdit: true }, { label: 'Move to Trash', danger: true }],
      rowActions: ['play', 'edit', 'trash'],
    },
    review: {
      title: 'Review queue', count: 3, group: 'uploader',
      desc: 'Uploads staged for review, grouped by uploader. Fix tags before approving, approve to publish, return with a note, or discard to Trash.',
      columns: ['check', 'title', 'artist', 'album', 'size', 'meta', 'actions'],
      metaLabel: 'Submitted',
      bulk: [{ label: 'Edit tags…', bulkEdit: true }, { label: 'Approve' }, { label: 'Return…' }, { label: 'Discard', danger: true }],
      rowActions: ['play', 'edit', 'approve', 'return', 'discard'],
      badge: true,
    },
    trash: {
      title: 'Trash', count: 2, group: null,
      desc: 'Files hidden from the library; their blobs remain on disk. Edit is available here too — fix a tag (or access) before restoring. Restore, or delete forever.',
      columns: ['check', 'title', 'artist', 'album', 'size', 'meta', 'actions'],
      metaLabel: 'Deleted',
      bulk: [{ label: 'Edit tags…', bulkEdit: true }, { label: 'Restore' }, { label: 'Delete forever', danger: true }],
      rowActions: ['play', 'edit', 'restore', 'delete'],
      badge: true,
    },
    mine: {
      title: 'My uploads', count: 3, group: 'state',
      desc: 'Your own staged files (owner view — same component, owner-scoped endpoints). Check the tags, then send to approval.',
      columns: ['check', 'title', 'artist', 'album', 'size', 'meta', 'actions'],
      metaLabel: 'State',
      bulk: [{ label: 'Edit tags…', bulkEdit: true }, { label: 'Send to approval' }, { label: 'Remove', danger: true }],
      rowActions: ['play', 'edit', 'send', 'remove'],
      badge: true,
    },
  };

  const DATA = {
    all: [
      { title: 'Cobalt 2.5', artist: 'Solar Fields', album: 'Movements', hash: '9f3a1c77', size: '8.4 MB', guest: true, license: 'CC BY' },
      { title: 'Leaving Home', artist: 'Carbon Based Lifeforms', album: 'World of Sleepers', hash: '2b7e0d41', size: '11.2 MB', guest: true, license: 'CC BY-SA' },
      { title: '', fallback: '04 - take.flac', artist: '', album: '', hash: '0aa4f9c2', size: '3.1 MB', guest: false, license: '' },
    ],
    review: [
      { title: 'Night Drive', artist: 'H.U.V.A. Network', album: 'Distances', hash: 'c41d9a08', size: '9.7 MB', state: 'submitted', meta: '2026-06-10 14:02', uploader: 'jdoe' },
      { title: 'Side B (rough)', artist: 'jdoe', album: 'demos', hash: 'e8820b1f', size: '6.0 MB', state: 'returned', meta: '2026-06-10 09:15', uploader: 'jdoe', note: 'Fix the artist tag — this is tagged with your username.' },
      { title: '', fallback: 'sketch.m4a', artist: '', album: '', hash: '771fab90', size: '2.4 MB', state: 'draft', meta: '—', uploader: 'mara' },
    ],
    trash: [
      { title: 'Old Master', artist: 'Aes Dana', album: 'Leylines', hash: '5d0c33ab', size: '10.1 MB', meta: '2026-06-09', pending: false },
      { title: 'Rejected take', artist: 'jdoe', album: 'demos', hash: 'a90b22ce', size: '5.5 MB', meta: '2026-06-08', pending: true },
    ],
    mine: [
      { title: 'Night Drive', artist: 'H.U.V.A. Network', album: 'Distances', hash: 'c41d9a08', size: '9.7 MB', state: 'submitted', meta: 'Awaiting review' },
      { title: 'Side B (rough)', artist: 'jdoe', album: 'demos', hash: 'e8820b1f', size: '6.0 MB', state: 'returned', meta: 'Returned', note: 'Fix the artist tag — this is tagged with your username.' },
      { title: 'New idea', artist: 'jdoe', album: 'demos', hash: '4417cd92', size: '4.2 MB', state: 'draft', meta: 'Draft' },
    ],
  };

  // Browse hierarchy for the All-files scope (artist → album → track).
  const BROWSE = [
    { artist: 'Solar Fields', tint: '#3a6df0', albums: [
      { title: 'Movements', year: 2009, tracks: [
        { n: 1, title: 'Cobalt 2.5', hash: '9f3a1c77', dur: '3:12', size: '8.4 MB', guest: true, license: 'CC BY' },
        { n: 2, title: 'Sing (a song)', hash: '1a2b3c4d', dur: '4:01', size: '9.1 MB', guest: true, license: 'CC BY' },
      ] },
      { title: 'Reflective Frequencies', year: 2001, tracks: [
        { n: 1, title: 'Pulse', hash: '88cc77aa', dur: '7:20', size: '14.0 MB', guest: false, license: '' },
      ] },
    ] },
    { artist: 'Carbon Based Lifeforms', tint: '#1f9d72', albums: [
      { title: 'World of Sleepers', year: 2006, tracks: [
        { n: 1, title: 'Proton/Electron', hash: '77aa11bb', dur: '5:33', size: '12.2 MB', guest: false, license: '' },
        { n: 2, title: 'Leaving Home', hash: '2b7e0d41', dur: '6:10', size: '11.2 MB', guest: true, license: 'CC BY-SA' },
      ] },
    ] },
    { artist: '', fallback: '(no artist)', tint: '#555', albums: [
      { title: '', fallback: '(no album)', tracks: [
        { n: null, title: '', fallback: '04 - take.flac', hash: '0aa4f9c2', dur: '2:55', size: '3.1 MB', guest: false, license: '' },
      ] },
    ] },
  ];

  // ── Selection (shared across list + browse; hashes) ──
  const selected = new Set();
  const allBrowseHashes = () => BROWSE.flatMap(a => a.albums.flatMap(al => al.tracks.map(t => t.hash)));

  // ── Cell + table rendering (flat list) ──
  function badge(scope, row) {
    if (scope === 'trash') return row.pending
      ? '<span class="state-badge is-returned" title="Restores into the review queue">pending review</span>' : '';
    if (row.state) return `<span class="state-badge is-${row.state}">${STATE_LABEL[row.state] || row.state}</span>`;
    return '';
  }

  function actionButtons(scope) {
    const map = {
      play:    `<button class="play-btn" title="Preview" aria-label="Preview">${PLAY}</button>`,
      edit:    `<button class="btn btn-neutral btn-sm btn-edit" data-edit>Edit</button>`,
      trash:   `<button class="btn btn-destructive-outline btn-sm">Move to Trash</button>`,
      approve: `<button class="btn btn-neutral btn-sm">Approve</button>`,
      return:  `<button class="btn btn-neutral btn-sm">Return…</button>`,
      discard: `<button class="btn btn-destructive-outline btn-sm">Discard</button>`,
      restore: `<button class="btn btn-neutral btn-sm">Restore</button>`,
      delete:  `<button class="btn btn-destructive-outline btn-sm">Delete forever</button>`,
      send:    `<button class="btn btn-neutral btn-sm">Send</button>`,
      remove:  `<button class="btn btn-destructive-outline btn-sm">Remove</button>`,
    };
    return `<div class="trash-actions">${SCOPES[scope].rowActions.map(a => map[a]).join('')}</div>`;
  }

  // Access is now a READ-ONLY summary column (editing moved into the modals).
  function accessSummary(row) {
    const parts = [row.guest ? 'Guest' : 'Private'];
    if (row.license) parts.push(row.license);
    return `<span class="cell-muted">${parts.join(' · ')}</span>`;
  }

  function headCell(col, cfg) {
    return ({
      check: '<th class="col-check"><input type="checkbox" aria-label="Select all"></th>',
      title: '<th>Title</th>', artist: '<th>Artist</th>', album: '<th>Album</th>',
      size: '<th class="col-size">Size</th>', access: '<th class="col-access">Access</th>',
      meta: `<th>${cfg.metaLabel || 'Meta'}</th>`, actions: '<th class="col-actions">Actions</th>',
    })[col];
  }

  function bodyCell(scope, col, row, cfg) {
    switch (col) {
      case 'check': return '<td class="cell-check"><input type="checkbox" class="row-check"></td>';
      case 'title': {
        const t = row.title
          ? `<span class="cell-title">${row.title}</span>`
          : `<span class="cell-title is-fallback">${row.fallback || 'Untitled'}</span>`;
        const b = badge(scope, row);
        const note = row.note ? `<span class="mod-note">Note: ${row.note}</span>` : '';
        return `<td class="cell-title-td" data-label="Title">${t}${b ? ' ' + b : ''}<span class="cell-hash">${row.hash}</span>${note}</td>`;
      }
      case 'artist': return row.artist ? `<td data-label="Artist">${row.artist}</td>` : '<td class="cell-muted" data-label="Artist">—</td>';
      case 'album':  return row.album  ? `<td data-label="Album">${row.album}</td>`  : '<td class="cell-muted" data-label="Album">—</td>';
      case 'size':   return `<td class="cell-size" data-label="Size">${row.size}</td>`;
      case 'access': return `<td class="cell-access" data-label="Access">${accessSummary(row)}</td>`;
      case 'meta':   return `<td data-label="${cfg.metaLabel}">${row.meta || '—'}</td>`;
      case 'actions':return `<td class="cell-actions" data-label="Actions">${actionButtons(scope)}</td>`;
    }
  }

  function rowHTML(scope, cfg, row) {
    const payload = encodeURIComponent(JSON.stringify({ ...row, scope }));
    return `<tr data-row="${payload}">${cfg.columns.map(c => bodyCell(scope, c, row, cfg)).join('')}</tr>`;
  }

  function tableHTML(scope, rows) {
    const cfg = SCOPES[scope];
    const head = `<thead><tr>${cfg.columns.map(c => headCell(c, cfg)).join('')}</tr></thead>`;
    const body = `<tbody>${rows.map(r => rowHTML(scope, cfg, r)).join('')}</tbody>`;
    return `<div class="files-table-wrap"><table class="files-table">${head}${body}</table></div>`;
  }

  function grouped(rows, field) {
    const order = [], map = new Map();
    for (const r of rows) { const k = r[field] || ''; if (!map.has(k)) { map.set(k, []); order.push(k); } map.get(k).push(r); }
    return order.map(k => ({ key: k, rows: map.get(k) }));
  }
  function counts(rows) {
    const n = s => rows.filter(r => r.state === s).length;
    const p = [];
    if (n('submitted')) p.push(`${n('submitted')} awaiting`);
    if (n('returned')) p.push(`${n('returned')} returned`);
    if (n('draft')) p.push(`${n('draft')} draft${n('draft') === 1 ? '' : 's'}`);
    return p.join(' · ');
  }

  function bulkBar(cfg) {
    return `<div class="bulk-toolbar">
      <span class="bulk-selcount">0 selected</span><span class="bulk-spacer"></span>
      ${cfg.bulk.map(b => `<button class="btn ${b.danger ? 'btn-destructive-outline' : 'btn-neutral'} btn-sm"${b.bulkEdit ? ' data-bulk-edit' : ''}>${b.label}</button>`).join(' ')}
    </div>`;
  }

  function filesBar(cfg) {
    return `<div class="files-bar">
      <h2 class="section-title section-title--files">${cfg.title} (<span>${cfg.count}</span>)</h2>
      <div class="files-search"><label class="visually-hidden">Filter</label>
        <input type="search" placeholder="Filter…" autocomplete="off"></div>
    </div>`;
  }

  // Full scope body (list presentation) — used by review/trash/mine + v1/v3.
  function contentHTML(scope) {
    const cfg = SCOPES[scope], rows = DATA[scope];
    let tables;
    if (cfg.group === 'uploader') {
      tables = grouped(rows, 'uploader').map(g => `
        <section class="mod-group">
          <div class="mod-group-header">
            <input type="checkbox" class="mod-group-check" aria-label="Select batch">
            <button class="mod-group-toggle" aria-expanded="true">
              <span class="mod-group-chevron" aria-hidden="true">▾</span>
              <span>${g.key || '(unknown uploader)'}</span>
              <span class="mod-group-counts">${counts(g.rows)}</span>
            </button>
          </div>
          <div class="mod-group-body">${tableHTML(scope, g.rows)}</div>
        </section>`).join('');
    } else if (cfg.group === 'state') {
      const lab = { returned: 'Returned by a moderator', draft: 'Drafts', submitted: 'Awaiting review' };
      tables = ['returned', 'draft', 'submitted'].map(st => {
        const rs = rows.filter(r => r.state === st);
        return rs.length ? `<h3 class="section-title section-title--sub">${lab[st]} (${rs.length})</h3>${tableHTML(scope, rs)}` : '';
      }).join('');
    } else {
      tables = tableHTML(scope, rows);
    }
    return `${filesBar(cfg)}<p class="scope-desc">${cfg.desc}</p>${bulkBar(cfg)}${tables}`;
  }

  // ── Browse presentation (artist → album → track) ──
  const brState = { level: 'artists', artist: null, album: null };

  function cover(tint, label, big) {
    const size = big ? 56 : 40;
    return `<div class="entity-cover" aria-hidden="true" style="width:${size}px;height:${size}px;background:
      linear-gradient(135deg, ${tint}, transparent 140%);display:flex;align-items:center;justify-content:center;
      color:rgba(255,255,255,.85);font-size:${big ? 20 : 15}px">${label}</div>`;
  }
  function trackCount(n) { return `${n} track${n === 1 ? '' : 's'}`; }

  function crumb() {
    const parts = [`<button class="crumb-link" data-br="artists">Artists</button>`];
    if (brState.artist != null) {
      const a = BROWSE[brState.artist];
      parts.push(`<span class="crumb-sep">›</span>`);
      parts.push(brState.level === 'albums'
        ? `<span class="crumb-current">${a.artist || a.fallback}</span>`
        : `<button class="crumb-link" data-br="albums">${a.artist || a.fallback}</button>`);
    }
    if (brState.album != null && brState.level === 'tracks') {
      const al = BROWSE[brState.artist].albums[brState.album];
      parts.push(`<span class="crumb-sep">›</span>`);
      parts.push(`<span class="crumb-current">${al.title || al.fallback}</span>`);
    }
    return `<nav class="entity-breadcrumb">${parts.join(' ')}</nav>`;
  }

  function groupChecked(hashes) {
    return hashes.length && hashes.every(h => selected.has(h));
  }

  function selBar() {
    if (!selected.size) return '';
    return `<div class="bulk-toolbar" style="margin:0 0 var(--space-3)">
      <span class="bulk-selcount">${selected.size} selected</span><span class="bulk-spacer"></span>
      <button class="btn btn-neutral btn-sm" data-bulk-edit>Edit tags…</button>
      <button class="btn btn-destructive-outline btn-sm">Move to Trash</button>
      <button class="btn btn-neutral btn-sm" data-br-clear>Clear</button>
    </div>`;
  }

  function browseHTML() {
    let list = '';
    if (brState.level === 'artists') {
      list = BROWSE.map((a, i) => {
        const hashes = a.albums.flatMap(al => al.tracks.map(t => t.hash));
        return `<div class="entity-row">
          <input type="checkbox" class="brsel" data-hashes="${hashes.join(',')}" ${groupChecked(hashes) ? 'checked' : ''} aria-label="Select all by ${a.artist || a.fallback}">
          ${cover(a.tint, '♪')}
          <button class="entity-main" data-drill-artist="${i}">
            <span class="entity-name${a.artist ? '' : ' is-fallback'}">${a.artist || a.fallback}</span>
            <span class="entity-meta">${a.albums.length} album${a.albums.length === 1 ? '' : 's'} · ${trackCount(hashes.length)}</span>
          </button>
          <div class="entity-actions">
            <button class="btn btn-neutral btn-sm" data-bulk-edit data-group="${hashes.join(',')}">Edit tags…</button>
          </div>
        </div>`;
      }).join('');
    } else if (brState.level === 'albums') {
      const a = BROWSE[brState.artist];
      list = a.albums.map((al, i) => {
        const hashes = al.tracks.map(t => t.hash);
        const meta = [al.year, trackCount(hashes.length)].filter(v => v != null).join(' · ');
        return `<div class="entity-row">
          <input type="checkbox" class="brsel" data-hashes="${hashes.join(',')}" ${groupChecked(hashes) ? 'checked' : ''} aria-label="Select album ${al.title || al.fallback}">
          ${cover(a.tint, '♫')}
          <button class="entity-main" data-drill-album="${i}">
            <span class="entity-name${al.title ? '' : ' is-fallback'}">${al.title || al.fallback}</span>
            <span class="entity-meta">${meta}</span>
          </button>
          <div class="entity-actions">
            <button class="btn btn-neutral btn-sm" data-bulk-edit data-group="${hashes.join(',')}">Edit tags…</button>
          </div>
        </div>`;
      }).join('');
    } else {
      const a = BROWSE[brState.artist], al = a.albums[brState.album];
      list = al.tracks.map(t => `<div class="entity-row entity-row--track">
        <input type="checkbox" class="brsel" data-hashes="${t.hash}" ${selected.has(t.hash) ? 'checked' : ''} aria-label="Select ${t.title || t.fallback}">
        <button class="play-btn" title="Preview" aria-label="Preview">${PLAY}</button>
        <span class="entity-tracknum">${t.n != null ? t.n : ''}</span>
        <span class="entity-name${t.title ? '' : ' is-fallback'}">${t.title || t.fallback}</span>
        <span class="entity-meta">${t.dur} · ${t.guest ? 'Guest' : 'Private'}${t.license ? ' · ' + t.license : ''}</span>
        <div class="entity-actions">
          <button class="btn btn-neutral btn-sm btn-edit" data-edit-track="${t.hash}">Edit</button>
        </div>
      </div>`).join('');
    }
    return `${crumb()}${selBar()}<div class="entity-panel">${list}</div>`;
  }

  function findTrack(hash) {
    for (const a of BROWSE) for (const al of a.albums) for (const t of al.tracks)
      if (t.hash === hash) return { ...t, artist: a.artist, album: al.title, scope: 'all' };
    return null;
  }

  function mountBrowse(el) {
    function render() {
      el.innerHTML = browseHTML();
      el.querySelectorAll('[data-drill-artist]').forEach(b => b.addEventListener('click', () => {
        brState.artist = Number(b.dataset.drillArtist); brState.level = 'albums'; brState.album = null; render();
      }));
      el.querySelectorAll('[data-drill-album]').forEach(b => b.addEventListener('click', () => {
        brState.album = Number(b.dataset.drillAlbum); brState.level = 'tracks'; render();
      }));
      el.querySelectorAll('[data-br]').forEach(b => b.addEventListener('click', () => {
        const to = b.dataset.br;
        if (to === 'artists') { brState.level = 'artists'; brState.artist = null; brState.album = null; }
        else if (to === 'albums') { brState.level = 'albums'; brState.album = null; }
        render();
      }));
      el.querySelectorAll('.brsel').forEach(cb => cb.addEventListener('change', () => {
        cb.dataset.hashes.split(',').forEach(h => cb.checked ? selected.add(h) : selected.delete(h));
        render();
      }));
      el.querySelector('[data-br-clear]')?.addEventListener('click', () => { selected.clear(); render(); });
      el.querySelectorAll('[data-bulk-edit]').forEach(b => b.addEventListener('click', () => {
        const g = b.dataset.group ? b.dataset.group.split(',').length : selected.size;
        window.Demo.openBulkEdit?.(g);
      }));
      el.querySelectorAll('[data-edit-track]').forEach(b => b.addEventListener('click', () =>
        window.Demo.openEdit?.(findTrack(b.dataset.editTrack))));
    }
    render();
  }

  // wireContent attaches list-mode demo interactions inside a rendered body.
  function wireContent(root) {
    root.querySelectorAll('[data-edit]').forEach(btn => btn.addEventListener('click', () => {
      const tr = btn.closest('tr');
      const data = tr?.dataset.row ? JSON.parse(decodeURIComponent(tr.dataset.row)) : {};
      window.Demo.openEdit?.(data);
    }));
    root.querySelectorAll('[data-bulk-edit]').forEach(btn =>
      btn.addEventListener('click', () => window.Demo.openBulkEdit?.(selected.size || 2)));
    root.querySelectorAll('.mod-group-toggle').forEach(btn => btn.addEventListener('click', () => {
      const sec = btn.closest('.mod-group');
      const collapsed = sec.classList.toggle('is-collapsed');
      btn.setAttribute('aria-expanded', String(!collapsed));
    }));
  }

  window.Demo = {
    SCOPES, LICENSES, contentHTML, wireContent, mountBrowse,
    listTableHTML: (scope) => tableHTML(scope, DATA[scope]),
    filesBar: (scope) => filesBar(SCOPES[scope]),
    bulkBar: (scope) => bulkBar(SCOPES[scope]),
    selectedCount: () => selected.size,
  };
})();
