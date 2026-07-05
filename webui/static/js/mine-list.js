// My-uploads — the uploader's staging list on the upload page (/upload,
// /admin/upload). A bespoke, self-contained list (NOT the generic file-list.js):
// the caller's non-approved appearances grouped by review state — Returned (with
// the moderator's note), Drafts, and Awaiting review (locked) — so an uploader
// can see exactly what is where, fix a draft's tags, resend a returned one, and
// send drafts to approval. Rows are tagset-addressed (the appearance is the row).
//
// Endpoints (all owner-scoped; drafts + returned only for edits):
//   GET    /api/my/uploads                     — one page of the caller's staging
//   PATCH/GET /api/my/uploads/{tid}/metadata   — edit one appearance's tags
//   DELETE /api/my/uploads/{tid}               — remove (soft delete) one
//   POST   /api/my/uploads/bulk                — submit / remove over ids or a filter
//
// Instantiated by upload.js; the preview sink (`preview`) is the shell player by
// default, or a page-local one under the admin shell. Design: the P4 UX draft.
import { createTrackEditor } from './track-edit.js';
import { showToast } from './toast.js';

const PAGE_SIZE = 200;
const EDITABLE = new Set(['draft', 'returned']);
const STATE_BADGE = { returned: 'returned', draft: 'draft', submitted: 'submitted' };
const SECTIONS = [
  { key: 'returned',  label: 'Returned — needs your attention' },
  { key: 'draft',     label: 'Drafts' },
  { key: 'submitted', label: 'Awaiting review — locked' },
];

const title = f => f.title || f.filename || 'this file';

// el('button', {class, onclick, text}, [children]) — minimal local DOM builder.
function el(tag, props = {}, children = []) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (v == null) continue;
    if (k === 'class') node.className = v;
    else if (k === 'text') node.textContent = v;
    else if (k.startsWith('on') && typeof v === 'function') node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v);
  }
  (Array.isArray(children) ? children : [children]).forEach(c => {
    if (c != null) node.append(c.nodeType ? c : document.createTextNode(c));
  });
  return node;
}

function fmtBytes(n) {
  if (!n) return '';
  const u = ['B', 'KB', 'MB', 'GB'];
  let i = 0, v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(v < 10 && i ? 1 : 0)} ${u[i]}`;
}

function metaLine(f) {
  const bits = [];
  if (f.artist) bits.push(f.artist);
  if (f.album) bits.push(f.album);
  if (f.track_number) bits.push(`trk ${f.track_number}`);
  if (f.byte_size) bits.push(fmtBytes(f.byte_size));
  return bits.join(' · ');
}

/**
 * createMineList builds the staging list controller.
 * @param {Object}   o
 * @param {string}   o.API          API base ('' = same-origin).
 * @param {Function} o.preview      (tracks, idx) => void — the preview sink.
 * @param {boolean}  o.canEditMeta  whether to offer cover/extended edits (server enforces).
 * @param {Function} [o.onCount]    (total) => void — refresh the tab badge.
 * @returns {{ mount(host):void, reload():Promise, destroy():void }}
 */
export function createMineList({ API = '', preview, canEditMeta = false, onCount }) {
  let host = null;
  let rows = [];
  let total = 0, selectableTotal = 0;
  let loading = false;
  const selected = new Set();      // selected tagset-id strings (editable rows only)
  let selectAllMatching = false;
  let playingKey = null;

  const key = f => String(f.tagset_id);
  const byKey = k => rows.find(f => key(f) === k);
  const editableRows = () => rows.filter(f => EDITABLE.has(f.state));

  // ── Shared edit modal ─────────────────────────────────────────────────────
  const editor = createTrackEditor({
    patchURL: f => `${API}/api/my/uploads/${f.tagset_id}/metadata`,
    detailURL: f => `${API}/api/my/uploads/${f.tagset_id}/metadata`,
    note: 'Fix the tags before sending to approval — title, artist and album decide where the track lands.',
    onSaved: (f, data) => {
      const row = byKey(key(f));
      if (row) {
        row.title = data.title ?? row.title;
        row.artist = data.artist ?? row.artist;
        row.album_artist = data.album_artist ?? row.album_artist;
        row.album = data.album ?? row.album;
        row.track_number = data.track_number ?? row.track_number;
      }
      showToast('Saved the tags.', { type: 'success' });
      render();
    },
    onError: err => showToast(err.message || 'Couldn’t save.', { type: 'error' }),
  });

  // ── Fetch helpers ─────────────────────────────────────────────────────────
  async function loadPage(offset) {
    const params = new URLSearchParams({ limit: String(PAGE_SIZE), offset: String(offset), sort: 'state' });
    const res = await fetch(`${API}/api/my/uploads?${params.toString()}`);
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    return res.json();
  }

  async function bulkCall(body) {
    const res = await fetch(`${API}/api/my/uploads/bulk`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
    return data;
  }

  async function removeOne(tid) {
    const res = await fetch(`${API}/api/my/uploads/${tid}`, { method: 'DELETE' });
    const data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
  }

  const idsBody = keys => ({ tagset_ids: keys.map(Number) });
  const allBody = () => ({ filter: { q: '', field: '' }, all: true });
  const actionBody = () => (selectAllMatching ? allBody() : idsBody([...selected]));

  // ── Actions ───────────────────────────────────────────────────────────────
  function sendToast(data) {
    const n = data.submitted ?? 0;
    showToast(data.approved
      ? `Published ${n} file${n === 1 ? '' : 's'} to the library.`
      : `Sent ${n} file${n === 1 ? '' : 's'} for review.`, { type: 'success' });
    if (data.warning) showToast(data.warning, { type: 'info' });
  }

  async function sendSelected() {
    try { sendToast(await bulkCall({ action: 'submit', ...actionBody() })); reload(); }
    catch (err) { showToast(err.message, { type: 'error' }); }
  }
  async function removeSelected() {
    const n = selectAllMatching ? selectableTotal : selected.size;
    if (!window.confirm(`Remove ${n} staged file${n === 1 ? '' : 's'}? They move to Trash.`)) return;
    try {
      const data = await bulkCall({ action: 'remove', ...actionBody() });
      const done = data.removed ?? 0;
      showToast(`Removed ${done} file${done === 1 ? '' : 's'}.`, { type: 'success' });
      reload();
    } catch (err) { showToast(err.message, { type: 'error' }); }
  }
  async function resendRow(f) {
    try { sendToast(await bulkCall({ action: 'submit', tagset_ids: [f.tagset_id] })); reload(); }
    catch (err) { showToast(err.message, { type: 'error' }); }
  }
  async function removeRow(f) {
    if (!window.confirm(`Remove “${title(f)}”? It moves to Trash.`)) return;
    try {
      await removeOne(f.tagset_id);
      showToast(`Removed “${title(f)}”.`, { type: 'success' });
      afterRemoval([f.tagset_id]);
    } catch (err) { showToast(err.message, { type: 'error' }); }
  }
  function afterRemoval(tids) {
    const gone = new Set(tids.map(String));
    rows = rows.filter(f => !gone.has(key(f)));
    for (const k of gone) selected.delete(k);
    total = Math.max(0, total - gone.size);
    selectableTotal = Math.max(0, selectableTotal - gone.size);
    onCount?.(total);
    render();
  }

  // ── Preview ───────────────────────────────────────────────────────────────
  function playRow(f) {
    const list = rows.filter(x => x.url);
    const tracks = list.map(e => ({ url: `${API}${e.url}`, hash: e.hash, title: title(e), artist: e.artist || '', dur: e.duration || undefined, key: key(e) }));
    const idx = list.findIndex(e => key(e) === key(f));
    playingKey = key(f);
    preview(tracks, idx < 0 ? 0 : idx);
    paintPlaying();
  }
  function paintPlaying() {
    if (!host) return;
    host.querySelectorAll('.mu-play.is-playing').forEach(b => b.classList.remove('is-playing'));
    if (playingKey != null) host.querySelectorAll(`.mu-play[data-k="${cssEsc(playingKey)}"]`).forEach(b => b.classList.add('is-playing'));
  }
  const cssEsc = s => (window.CSS && CSS.escape ? CSS.escape(s) : String(s).replace(/"/g, '\\"'));

  // ── Selection ─────────────────────────────────────────────────────────────
  function toggleRow(k, on) { on ? selected.add(k) : selected.delete(k); selectAllMatching = false; render(); }
  function toggleAll(on) {
    selected.clear();
    if (on) for (const f of editableRows()) selected.add(key(f));
    else selectAllMatching = false;
    render();
  }

  // ── Rendering ─────────────────────────────────────────────────────────────
  function render() {
    if (!host) return;
    const parts = [intro()];
    if (!rows.length && !loading) {
      parts.push(el('div', { class: 'mu-empty', text: 'Nothing staged. Files you upload appear here for a metadata check before they reach the library.' }));
    } else {
      parts.push(bulkbar());   // above the list, so the selection actions are always in view
      for (const sec of SECTIONS) {
        const items = rows.filter(f => f.state === sec.key);
        if (!items.length) continue;
        parts.push(sectionDom(sec, items));
      }
      const more = loadMoreDom();
      if (more) parts.push(more);
    }
    host.replaceChildren(...parts.filter(Boolean));
    paintPlaying();
  }

  function intro() {
    return el('div', { class: 'mu-intro' }, [
      el('p', { text: 'Files you uploaded that aren’t in the library yet. Check their tags, then send them to approval — a moderator reviews them (or, with moderation rights, they publish immediately). A returned file shows the moderator’s note.' }),
    ]);
  }

  function sectionDom(sec, items) {
    return el('div', { class: 'mu-section' }, [
      el('h2', { text: sec.label }),
      ...items.map(rowDom),
    ]);
  }

  function rowDom(f) {
    const k = key(f);
    const editable = EDITABLE.has(f.state);

    const check = el('input', { type: 'checkbox', class: 'mu-check', 'aria-label': `Select ${title(f)}` });
    check.disabled = !editable;
    check.checked = selected.has(k);
    check.addEventListener('change', () => toggleRow(k, check.checked));

    const t = el('div', { class: 'mu-t' }, [
      el('span', { class: 'mu-tt', text: title(f) }),
      el('span', { class: `state-badge is-${f.state}`, text: STATE_BADGE[f.state] || f.state }),
    ]);
    const main = el('div', { class: 'mu-main' }, [t, el('div', { class: 'mu-m', text: metaLine(f) })]);
    if (f.state === 'returned' && f.note) {
      main.append(el('div', { class: 'mu-note', text: `✎ ${f.note}` }));
    }

    const playBtn = el('button', { class: 'icon-btn mu-play', 'data-k': k, title: 'Preview', text: '▶', onclick: () => playRow(f) });
    const actions = [playBtn];
    if (editable) {
      const editBtn = el('button', { class: 'btn btn-neutral btn-sm', text: 'Edit…',
        onclick: () => editor.open({ tagset_id: f.tagset_id, title: f.title, artist: f.artist, album_artist: f.album_artist, album: f.album }) });
      actions.push(editBtn);
      if (f.state === 'returned') {
        actions.push(el('button', { class: 'btn btn-neutral btn-sm mu-resend', text: 'Resend', onclick: () => resendRow(f) }));
      }
      actions.push(el('button', { class: 'btn btn-destructive btn-sm', text: 'Remove', onclick: () => removeRow(f) }));
    }

    return el('div', { class: `mu-row${f.state === 'submitted' ? ' is-locked' : ''}` }, [
      check, main, el('div', { class: 'mu-actions' }, actions),
    ]);
  }

  function bulkbar() {
    const editable = editableRows();
    const allOn = editable.length > 0 && editable.every(f => selected.has(key(f)));
    const count = selectAllMatching ? selectableTotal : selected.size;

    const master = el('input', { type: 'checkbox', 'aria-label': 'Select all editable' });
    master.checked = selectAllMatching || allOn;
    master.disabled = editable.length === 0;
    master.addEventListener('change', () => toggleAll(master.checked));

    const kids = [master, el('span', { class: 'mu-selcount' }, [el('b', { text: String(count) }), ' selected'])];
    if (!selectAllMatching && allOn && selectableTotal > editable.length) {
      kids.push(el('span', { class: 'mu-selall' }, [
        '· ', el('button', { type: 'button', class: 'linklike', text: `Select all ${selectableTotal} matching`, onclick: () => { selectAllMatching = true; render(); } }),
      ]));
    } else if (selectAllMatching) {
      kids.push(el('span', { class: 'mu-selall' }, [
        '· all ', String(selectableTotal), ' selected ',
        el('button', { type: 'button', class: 'linklike', text: 'clear', onclick: () => { selectAllMatching = false; selected.clear(); render(); } }),
      ]));
    }
    kids.push(el('span', { class: 'mu-spacer' }));
    const sendBtn = el('button', { class: 'btn btn-neutral btn-sm', text: 'Send to approval', onclick: sendSelected });
    const rmBtn = el('button', { class: 'btn btn-destructive btn-sm', text: 'Remove selected', onclick: removeSelected });
    sendBtn.disabled = rmBtn.disabled = count === 0;
    kids.push(sendBtn, rmBtn);
    return el('div', { class: 'mu-bulkbar' }, kids);
  }

  function loadMoreDom() {
    if (rows.length >= total) return null;
    const btn = el('button', { class: 'btn btn-neutral', text: loading ? 'Loading…' : `Load more (${rows.length} of ${total})`, onclick: loadMore });
    btn.disabled = loading;
    return el('div', { class: 'mu-loadmore' }, [btn]);
  }

  async function loadMore() {
    if (loading || rows.length >= total) return;
    loading = true; render();
    try {
      const data = await loadPage(rows.length);
      rows = rows.concat(data.items || []);
      total = data.total || rows.length;
      selectableTotal = data.selectable_total ?? selectableTotal;
    } catch (err) { showToast(err.message, { type: 'error' }); }
    finally { loading = false; render(); }
  }

  async function reload() {
    if (!host) return;
    loading = true;
    selected.clear(); selectAllMatching = false;
    render();
    try {
      const data = await loadPage(0);
      rows = data.items || [];
      total = data.total || rows.length;
      selectableTotal = data.selectable_total ?? 0;
      onCount?.(total);
    } catch (err) { showToast(err.message, { type: 'error' }); rows = []; }
    finally { loading = false; render(); }
  }

  function mount(hostEl) {
    host = hostEl;
    if (!host) return;
    host.setAttribute('aria-busy', 'false');
    host.classList.add('mu-scope');
    reload();
  }

  function destroy() { editor.destroy(); host = null; }

  return { mount, reload, destroy };
}
