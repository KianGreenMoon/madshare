// Admin · Library — Review scope (moderation queue). A bespoke, self-contained
// list built for the recording-tagsets P4 review flow: per-uploader collapsible
// groups of submission cards that a moderator can approve / return / discard
// right on the row, or expand to validate the three independent pieces of a
// submission — the file (bytes), the recording it lands on, and its appearance
// (tagset). It deliberately does NOT reuse the generic file-list.js component:
// the review workflow needs its own row anatomy and per-piece decision card.
//
// Endpoints are all tagset-addressed (the appearance is the row's identity):
//   GET  /api/admin/moderation                     — one page of the queue
//   GET  /api/admin/moderation/{tid}/classify      — case + ladder compare
//   POST /api/admin/moderation/{tid}/{approve,return,discard}
//   POST /api/admin/moderation/bulk                — batched over ids or a filter
//   GET/PATCH /api/admin/moderation/{tid}/metadata — moderator tag edit
//
// Mounted by library.js into the Library page's Review panel (#fileListReview);
// the shared preview player is injected as `play`. Design: the P4 UX draft and
// docs/architecture/recording-tagsets.md.
import { API, fmtBytes, toast, handleAuthError, el } from './shared.js';
import { createTrackEditor } from '../track-edit.js';

const PAGE_SIZE = 200;

const STATE_LABEL = { submitted: 'submitted', returned: 'returned', draft: 'draft' };

// Classification label + chip class (the chip styles live in file-view.css as
// .state-badge.cls-*). A collision (offered appearance already exists) trumps the
// case label — there is nothing new to publish.
const CLASS_LABEL = {
  new_recording:  { text: 'New recording',  cls: 'cls-new' },
  new_appearance: { text: 'New appearance', cls: 'cls-appear' },
  no_new_bytes:   { text: 'No new bytes',   cls: 'cls-bytes' },
};

const displayTitle = f => f.title || f.filename || 'this file';

// A colliding submission (its offered appearance is identical to one already in
// the library) has nothing new to publish at the review gate: it is excluded from
// approve (bulk + per-row) so a sweep can never manufacture a duplicate
// appearance. A better-master collision is adopted through the Duplicates page.
const isNoop = f => !!f.collides;
const canApprove = f => f.state === 'submitted' && !isNoop(f);
const classChip = f => {
  if (f.collides) return { text: 'Nothing new', cls: 'cls-warn', title: 'The offered appearance already exists on this recording' };
  return CLASS_LABEL[f.class] || null;
};

// techLabel renders a rendition's tech fields (from the classify compare) as a
// compact "CODEC · 24-bit / 96 kHz" or "MP3 · 320 kbps" string; a dash when
// ffprobe never filled them.
function techLabel(r) {
  if (!r) return '—';
  const parts = [];
  if (r.codec) parts.push(String(r.codec).toUpperCase());
  const q = [];
  if (r.bit_depth) q.push(`${r.bit_depth}-bit`);
  if (r.sample_rate) q.push(`${(r.sample_rate / 1000).toFixed(r.sample_rate % 1000 ? 1 : 0)} kHz`);
  if (!q.length && r.bitrate) q.push(`${Math.round(r.bitrate / 1000)} kbps`);
  if (q.length) parts.push(q.join(' / '));
  return parts.join(' · ') || '—';
}

// shortMeta is the collapsed row's one-line summary (list payload only — no
// ffprobe tech here; the expanded ladder carries that).
function shortMeta(f) {
  const bits = [];
  if (f.artist) bits.push(f.artist);
  if (f.album) bits.push(f.album);
  if (f.track_number) bits.push(`trk ${f.track_number}`);
  if (f.byte_size) bits.push(fmtBytes(f.byte_size));
  return bits;
}

export function createReviewScope({ play, perms }) {
  const canEdit = perms.includes('metadata.edit');
  const canDelete = perms.includes('file.delete');

  // ── State ─────────────────────────────────────────────────────────────────
  let host = null;                 // the mount container (#fileListReview)
  let rows = [];                   // accumulated queue items (paged, sort=uploader)
  let total = 0, selectableTotal = 0;
  let loading = false;
  const selected = new Set();      // selected tagset-id strings
  const collapsedGroups = new Set();
  const openCards = new Set();     // expanded tagset ids (strings)
  const classifyCache = new Map(); // tid -> classify payload (or {error})
  const decisions = new Map();     // tid -> { bytes:'keep'|'drop', forceNew:bool }
  let selectAllMatching = false;   // "select all N matching" whole-set mode
  let playingKey = null;

  const key = f => String(f.tagset_id);
  const byKey = k => rows.find(f => key(f) === k);

  // ── Fetch helpers ───────────────────────────────────────────────────────────
  async function loadPage(offset) {
    const params = new URLSearchParams({ limit: String(PAGE_SIZE), offset: String(offset), sort: 'uploader' });
    const res = await fetch(`${API}/api/admin/moderation?${params.toString()}`);
    if (handleAuthError(res)) throw new Error('Your session expired.');
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    return res.json();
  }

  async function classify(tid) {
    const cached = classifyCache.get(tid);
    if (cached) return cached;
    try {
      const res = await fetch(`${API}/api/admin/moderation/${tid}/classify`);
      if (handleAuthError(res)) throw new Error('auth');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      classifyCache.set(tid, data);
      return data;
    } catch (err) {
      const data = { error: String(err.message || err) };
      classifyCache.set(tid, data);
      return data;
    }
  }

  async function callOne(tid, action, body) {
    const res = await fetch(`${API}/api/admin/moderation/${tid}/${action}`, {
      method: 'POST', headers: body ? { 'Content-Type': 'application/json' } : {},
      body: body ? JSON.stringify(body) : undefined,
    });
    if (handleAuthError(res)) throw new Error('Your session expired.');
    const data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
    return data;
  }

  async function bulkCall(body) {
    const res = await fetch(`${API}/api/admin/moderation/bulk`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
    });
    if (handleAuthError(res)) throw new Error('Your session expired.');
    const data = await res.json().catch(() => ({}));
    if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
    return data.affected || 0;
  }

  // The bulk endpoint takes an explicit id list OR a filter; "select all matching"
  // has no search term here, so it always passes all:true over the whole queue.
  const idsBody = () => ({ tagset_ids: [...selected].map(Number) });
  const allBody = () => ({ filter: { q: '', field: '' }, all: true });
  const actionBody = () => (selectAllMatching ? allBody() : idsBody());

  // ── Actions ─────────────────────────────────────────────────────────────────
  async function approve(tid, opts) {
    try {
      await callOne(tid, 'approve', opts || {});
      toast(opts && opts.drop_bytes ? 'Published the appearance, dropped the blob.' : 'Approved into the library.', 'success');
      afterMutation([tid]);
    } catch (err) { toast(err.message, 'error'); }
  }
  async function discard(tid) {
    try {
      await callOne(tid, 'discard');
      toast('Discarded to Trash.', 'success');
      afterMutation([tid]);
    } catch (err) { toast(err.message, 'error'); }
  }
  async function returnOne(tid) {
    const note = await promptReturnNote('Send this submission back to its uploader to fix?');
    if (note == null) return;
    try {
      await callOne(tid, 'return', { note });
      toast('Returned to the uploader.', 'success');
      afterMutation([tid]);
    } catch (err) { toast(err.message, 'error'); }
  }

  async function bulkApprove() {
    try {
      const n = await bulkCall({ action: 'approve', ...actionBody() });
      toast(`Approved ${n} ${n === 1 ? 'appearance' : 'appearances'} into the library.`, 'success');
      reload();
    } catch (err) { toast(err.message, 'error'); }
  }
  async function bulkReturn() {
    const n = selectAllMatching ? selectableTotal : selected.size;
    const note = await promptReturnNote(`Send ${n} ${n === 1 ? 'submission' : 'submissions'} back with one note?`);
    if (note == null) return;
    try {
      const done = await bulkCall({ action: 'return', note, ...actionBody() });
      toast(`Returned ${done} ${done === 1 ? 'submission' : 'submissions'}.`, 'success');
      reload();
    } catch (err) { toast(err.message, 'error'); }
  }
  async function bulkDiscard() {
    const n = selectAllMatching ? selectableTotal : selected.size;
    if (!await confirmDiscard(`Discard ${n} ${n === 1 ? 'appearance' : 'appearances'} to Trash?`, `Discard ${n}`)) return;
    try {
      const done = await bulkCall({ action: 'discard', ...actionBody() });
      toast(`Discarded ${done} ${done === 1 ? 'appearance' : 'appearances'} to Trash.`, 'success');
      reload();
    } catch (err) { toast(err.message, 'error'); }
  }

  // afterMutation drops the acted-on rows locally and repaints (cheaper than a
  // full reload for a single-row action; the counts follow).
  function afterMutation(tids) {
    const gone = new Set(tids.map(String));
    rows = rows.filter(f => !gone.has(key(f)));
    for (const k of gone) { selected.delete(k); openCards.delete(k); }
    total = Math.max(0, total - gone.size);
    selectableTotal = Math.max(0, selectableTotal - gone.size);
    render();
  }

  // ── Shared edit modal (per-appearance tag edit) ──────────────────────────────
  const editor = createTrackEditor({
    patchURL: f => `${API}/api/admin/moderation/${f.tagset_id}/metadata`,
    detailURL: f => `${API}/api/admin/moderation/${f.tagset_id}/metadata`,
    suggestURL: f => `${API}/api/tagsets/${f.tagset_id}/suggestions`,
    note: 'Edits this appearance’s tags before approval.',
    checkAuth: handleAuthError,
    onSaved: (f, data) => {
      const row = byKey(key(f));
      if (row) {
        row.title = data.title ?? row.title;
        row.artist = data.artist ?? row.artist;
        row.album_artist = data.album_artist ?? row.album_artist;
        row.album = data.album ?? row.album;
        row.track_number = data.track_number ?? row.track_number;
      }
      toast('Saved the tags.', 'success');
      render();
    },
    onError: err => toast(err.message || 'Couldn’t save.', 'error'),
  });

  // ── Return-note + discard-confirm modals (markup in admin/library.html) ───────
  const returnModal   = document.getElementById('returnModal');
  const returnForm    = document.getElementById('returnForm');
  const returnBody    = document.getElementById('returnBody');
  const returnNote    = document.getElementById('returnNote');
  const returnError   = document.getElementById('returnError');
  const returnCancel  = document.getElementById('returnCancel');
  const returnClose   = document.getElementById('returnClose');

  function promptReturnNote(bodyText) {
    return new Promise(resolve => {
      returnBody.textContent = bodyText;
      returnNote.value = '';
      returnError.hidden = true; returnError.textContent = '';
      returnModal.classList.remove('hidden');
      returnNote.focus();
      const cleanup = () => {
        returnForm.removeEventListener('submit', onSubmit);
        returnCancel.removeEventListener('click', onCancel);
        returnClose.removeEventListener('click', onCancel);
        returnModal.removeEventListener('click', onBackdrop);
        document.removeEventListener('keydown', onKey);
      };
      const onSubmit = e => {
        e.preventDefault();
        const note = returnNote.value.trim();
        if (!note) { returnError.textContent = 'A note is required.'; returnError.hidden = false; return; }
        returnModal.classList.add('hidden'); cleanup(); resolve(note);
      };
      const onCancel   = () => { returnModal.classList.add('hidden'); cleanup(); resolve(null); };
      const onBackdrop = e => { if (e.target === returnModal) onCancel(); };
      const onKey      = e => { if (e.key === 'Escape' && !returnModal.classList.contains('hidden')) onCancel(); };
      returnForm.addEventListener('submit', onSubmit);
      returnCancel.addEventListener('click', onCancel);
      returnClose.addEventListener('click', onCancel);
      returnModal.addEventListener('click', onBackdrop);
      document.addEventListener('keydown', onKey);
    });
  }

  const discardModal   = document.getElementById('discardModal');
  const discardBody    = document.getElementById('discardBody');
  const discardConfirm = document.getElementById('discardConfirm');
  const discardCancel  = document.getElementById('discardCancel');
  const discardClose   = document.getElementById('discardClose');

  function confirmDiscard(bodyText, confirmLabel) {
    return new Promise(resolve => {
      discardBody.textContent = bodyText;
      discardConfirm.textContent = confirmLabel;
      discardModal.classList.remove('hidden');
      discardConfirm.focus();
      const cleanup = () => {
        discardConfirm.removeEventListener('click', onOk);
        discardCancel.removeEventListener('click', onCancel);
        discardClose.removeEventListener('click', onCancel);
        discardModal.removeEventListener('click', onBackdrop);
        document.removeEventListener('keydown', onKey);
      };
      const onOk       = () => { discardModal.classList.add('hidden'); cleanup(); resolve(true); };
      const onCancel   = () => { discardModal.classList.add('hidden'); cleanup(); resolve(false); };
      const onBackdrop = e => { if (e.target === discardModal) onCancel(); };
      const onKey      = e => { if (e.key === 'Escape' && !discardModal.classList.contains('hidden')) onCancel(); };
      discardConfirm.addEventListener('click', onOk);
      discardCancel.addEventListener('click', onCancel);
      discardClose.addEventListener('click', onCancel);
      discardModal.addEventListener('click', onBackdrop);
      document.addEventListener('keydown', onKey);
    });
  }

  // ── Preview (via the shared page player) ─────────────────────────────────────
  function previewList() { return rows.map(f => ({ url: f.url, title: displayTitle(f), artist: f.artist || '', key: key(f) })); }
  function playRow(f) {
    const items = previewList();
    const idx = items.findIndex(x => x.key === key(f));
    play(items, idx < 0 ? 0 : idx, k => setPlaying(k));
  }
  function setPlaying(k) {
    playingKey = k;
    if (!host) return;
    host.querySelectorAll('.rev-play.is-playing').forEach(b => b.classList.remove('is-playing'));
    host.querySelectorAll(`.rev-play[data-play-key="${cssEsc(k)}"]`).forEach(b => b.classList.add('is-playing'));
  }
  const cssEsc = s => (window.CSS && CSS.escape ? CSS.escape(s) : String(s).replace(/"/g, '\\"'));

  // ── Selection ─────────────────────────────────────────────────────────────
  const selectableRows = () => rows.filter(canApprove);
  function toggleRow(k, on) {
    if (on) selected.add(k); else selected.delete(k);
    selectAllMatching = false;
    render();
  }
  function toggleGroup(uid, on) {
    for (const f of rows) if ((f.uploader_id || 0) === uid && canApprove(f)) { on ? selected.add(key(f)) : selected.delete(key(f)); }
    selectAllMatching = false;
    render();
  }
  function toggleAll(on) {
    selected.clear();
    if (on) for (const f of selectableRows()) selected.add(key(f));
    else selectAllMatching = false;
    render();
  }

  // ── Rendering ───────────────────────────────────────────────────────────────
  function render() {
    if (!host) return;
    const parts = [intro(), legend(), bulkbar(), ...groupsDom(), loadMoreDom()].filter(Boolean);
    host.replaceChildren(...parts);
    if (playingKey) setPlaying(playingKey);
  }

  function intro() {
    return el('div', { class: 'rev-intro' }, [
      el('h2', { text: 'Review queue' }),
      el('p', { text: 'Uploads staged for review, grouped by uploader. Approve, return or discard right on a row — or expand one to validate the file, the recording it lands on, and its appearance independently.' }),
    ]);
  }

  function legend() {
    const item = (cls, label, note) => el('span', { class: 'lg' }, [
      el('span', { class: `state-badge ${cls}`, text: label }), ' ', el('span', { class: 'lg-note', text: note }),
    ]);
    return el('div', { class: 'rev-legend', 'aria-hidden': 'true' }, [
      el('span', { class: 'lg-title', text: 'Classification' }),
      item('cls-new', 'New recording', 'net-new audio'),
      item('cls-appear', 'New appearance', 'same audio, new release'),
      item('cls-bytes', 'No new bytes', 'byte-identical dup'),
      item('cls-warn', 'Nothing new', 'collides with an existing appearance'),
    ]);
  }

  function bulkbar() {
    const sel = selectableRows();
    const allOn = sel.length > 0 && sel.every(f => selected.has(key(f)));
    const master = el('input', { type: 'checkbox', 'aria-label': 'Select all awaiting review' });
    master.checked = selectAllMatching || allOn;
    master.addEventListener('change', () => toggleAll(master.checked));

    const count = selectAllMatching ? selectableTotal : selected.size;
    const kids = [
      master,
      el('span', { class: 'rev-selcount' }, [el('b', { text: String(count) }), ' selected']),
    ];

    // "Select all N matching" — offered once every loaded selectable row is ticked
    // but more matching rows remain unloaded (mirrors file-list's banner).
    if (!selectAllMatching && allOn && selectableTotal > sel.length) {
      kids.push(el('span', { class: 'rev-selall' }, [
        '· ', el('button', { type: 'button', class: 'linklike', text: `Select all ${selectableTotal} matching`,
          onclick: () => { selectAllMatching = true; render(); } }),
      ]));
    } else if (selectAllMatching) {
      kids.push(el('span', { class: 'rev-selall' }, [
        '· all ', String(selectableTotal), ' selected ',
        el('button', { type: 'button', class: 'linklike', text: 'clear', onclick: () => { selectAllMatching = false; selected.clear(); render(); } }),
      ]));
    }

    kids.push(el('span', { class: 'rev-spacer' }));
    const disabled = count === 0;
    const approveBtn = el('button', { class: 'btn btn-neutral btn-sm', text: 'Approve selected', onclick: bulkApprove });
    const returnBtn  = el('button', { class: 'btn btn-neutral btn-sm', text: 'Return…', onclick: bulkReturn });
    const discardBtn = el('button', { class: 'btn btn-destructive btn-sm', text: 'Discard', onclick: bulkDiscard });
    for (const b of [approveBtn, returnBtn, discardBtn]) b.disabled = disabled;
    if (!canDelete) discardBtn.disabled = true;
    kids.push(approveBtn, returnBtn, discardBtn);
    return el('div', { class: 'rev-bulkbar' }, kids);
  }

  // Group the loaded rows by uploader in server order (sort=uploader keeps a
  // uploader's rows contiguous, so a first-seen order is stable across pages).
  function groupsDom() {
    if (!rows.length && !loading) return [el('div', { class: 'rev-empty', text: 'Nothing awaiting review.' })];
    const order = [];
    const groups = new Map();
    for (const f of rows) {
      const uid = f.uploader_id || 0;
      if (!groups.has(uid)) { groups.set(uid, []); order.push(uid); }
      groups.get(uid).push(f);
    }
    return order.map(uid => groupDom(uid, groups.get(uid)));
  }

  function groupDom(uid, items) {
    const name = items[0].uploader || '(unknown uploader)';
    const collapsed = collapsedGroups.has(uid);
    const n = s => items.filter(f => f.state === s).length;
    const counts = [];
    if (n('submitted')) counts.push(`${n('submitted')} awaiting`);
    if (n('returned'))  counts.push(`${n('returned')} returned`);
    if (n('draft'))     counts.push(`${n('draft')} draft${n('draft') === 1 ? '' : 's'}`);

    const groupSel = items.filter(canApprove);
    const groupCheck = el('input', { type: 'checkbox', class: 'rev-group-check', 'aria-label': `Select ${name}'s batch` });
    groupCheck.disabled = groupSel.length === 0;
    groupCheck.checked = groupSel.length > 0 && groupSel.every(f => selected.has(key(f)));
    groupCheck.addEventListener('change', () => toggleGroup(uid, groupCheck.checked));

    const toggle = el('button', { class: 'rev-group-toggle', 'aria-expanded': String(!collapsed),
      onclick: () => { collapsed ? collapsedGroups.delete(uid) : collapsedGroups.add(uid); render(); } }, [
      el('span', { class: 'rev-group-chevron', text: '▾' }),
      el('span', { class: 'rev-group-name', text: name }),
      el('span', { class: 'rev-group-counts', text: counts.join(' · ') }),
    ]);

    const header = el('div', { class: 'rev-group-header' }, [groupCheck, toggle]);
    const body = el('div', { class: 'rev-group-body' }, items.map(cardDom));
    return el('div', { class: `rev-group${collapsed ? ' is-collapsed' : ''}` }, [header, body]);
  }

  // ── Submission card (collapsed head + lazy decision body) ────────────────────
  function cardDom(f) {
    const k = key(f);
    const open = openCards.has(k);
    const selectable = canApprove(f);

    const check = el('input', { type: 'checkbox', class: 'rev-check', 'aria-label': `Select ${displayTitle(f)}` });
    check.disabled = !selectable;
    check.checked = selected.has(k);
    check.addEventListener('change', () => toggleRow(k, check.checked));

    const chip = classChip(f);
    const chipEl = chip ? el('span', { class: `state-badge ${chip.cls}`, text: chip.text }) : null;
    if (chipEl && chip.title) chipEl.title = chip.title;
    const titleLine = el('div', { class: 'rev-t' }, [
      el('span', { class: 'rev-tt', text: displayTitle(f) }),
      chipEl,
      el('span', { class: `state-badge is-${f.state}`, text: STATE_LABEL[f.state] || f.state }),
      f.duplicate && !f.collides ? el('span', { class: 'state-badge cls-warn', text: 'possible duplicate' }) : null,
    ]);
    const meta = el('div', { class: 'rev-meta' });
    shortMeta(f).forEach((s, i) => { if (i) meta.append(el('span', { class: 'dot', text: '•' })); meta.append(document.createTextNode(s)); });

    const playBtn = el('button', { class: 'icon-btn rev-play', 'data-play-key': k, title: 'Preview submitted file', text: '▶',
      onclick: e => { e.stopPropagation(); playRow(f); } });

    const actions = [playBtn, el('span', { class: 'rev-act-sep' })];
    if (selectable) actions.push(el('button', { class: 'rev-act rev-act--approve', title: 'Approve', text: '✓',
      onclick: e => { e.stopPropagation(); approve(f.tagset_id, defaultApproveOpts(f)); } }));
    actions.push(el('button', { class: 'rev-act rev-act--return', title: 'Return to uploader…', text: '↩',
      onclick: e => { e.stopPropagation(); returnOne(f.tagset_id); } }));
    if (canDelete) actions.push(el('button', { class: 'rev-act rev-act--discard', title: 'Discard', text: '✕',
      onclick: e => { e.stopPropagation(); discard(f.tagset_id); } }));
    actions.push(el('span', { class: 'rev-act-sep' }));
    const expandBtn = el('button', { class: 'rev-expand', 'aria-expanded': String(open), title: 'Expand review' },
      [el('span', { class: 'chev', text: '▸' })]);
    expandBtn.addEventListener('click', e => { e.stopPropagation(); toggleCard(f); });
    actions.push(expandBtn);

    const head = el('div', { class: 'rev-head' }, [
      check,
      el('div', { class: 'rev-title' }, [titleLine, meta]),
      el('div', { class: 'rev-head-actions' }, actions),
    ]);
    head.addEventListener('click', e => { if (!e.target.closest('button, input')) toggleCard(f); });

    const card = el('article', { class: `rev-card${open ? ' is-open' : ''}${selected.has(k) ? ' is-selected' : ''}` }, [head]);
    card.dataset.tid = k;
    if (open) card.append(cardBody(f));
    return card;
  }

  function toggleCard(f) {
    const k = key(f);
    if (openCards.has(k)) { openCards.delete(k); render(); return; }
    openCards.add(k);
    render();
    // Case-B cards need the ladder compare; fetch, then repaint that card in place.
    if (f.class === 'new_appearance' && !classifyCache.has(f.tagset_id)) {
      classify(f.tagset_id).then(() => { if (openCards.has(k)) render(); });
    }
  }

  // defaultApproveOpts is the per-row (compact) approve decision: for a case-B
  // submission, drop the bytes unless they are the recording's new ladder-best.
  function defaultApproveOpts(f) {
    if (f.class !== 'new_appearance') return {};
    const c = classifyCache.get(f.tagset_id);
    const dec = decisions.get(String(f.tagset_id));
    if (dec) return { drop_bytes: dec.bytes === 'drop', force_new: dec.forceNew };
    const dropByDefault = c && c.submitted_is_new_best === false;
    return { drop_bytes: !!dropByDefault };
  }

  function decisionFor(f) {
    const k = key(f);
    let dec = decisions.get(k);
    if (dec) return dec;
    const c = classifyCache.get(f.tagset_id);
    // Recommend dropping bytes that rank below the current best; keep a new best.
    dec = { bytes: c && c.submitted_is_new_best === false ? 'drop' : 'keep', forceNew: false };
    // Persist the seed only once the ladder is known, so the default reflects it.
    if (c) decisions.set(k, dec);
    return dec;
  }

  function cardBody(f) {
    if (f.class === 'new_appearance') return caseBBody(f);
    if (f.class === 'no_new_bytes')   return caseCBody(f);
    return caseABody(f);
  }

  function landing(text) {
    const d = el('div', { class: 'rev-landing' }, [el('span', { class: 'i', text: '↳' }), el('div', { text })]);
    return d;
  }

  function pieceHead(num, title, hint) {
    return el('div', { class: 'piece-head' }, [
      el('span', { class: 'piece-num', text: String(num) }),
      el('span', { class: 'piece-title', text: title }),
      hint ? el('span', { class: 'piece-hint', text: hint }) : null,
    ]);
  }
  const piece = (head, body) => el('div', { class: 'piece' }, [head, el('div', { class: 'piece-body' }, body)]);

  function tagsGrid(f) {
    const dl = el('dl', { class: 'tags-grid' });
    const add = (k, v) => { dl.append(el('dt', { text: k }), el('dd', { text: v || '—' })); };
    add('Title', f.title);
    add('Artist', f.artist);
    add('Album', f.album);
    add('Album artist', f.album_artist);
    if (f.track_number) add('Track', String(f.track_number));
    if (f.year) add('Year', String(f.year));
    return dl;
  }

  function editButton(f) {
    const btn = el('button', { class: 'btn btn-neutral btn-sm', text: 'Edit tagset…',
      onclick: () => editor.open({ tagset_id: f.tagset_id, title: f.title, artist: f.artist, album_artist: f.album_artist, album: f.album }) });
    btn.disabled = !canEdit;
    return el('div', { class: 'override-row' }, [btn]);
  }

  function decideBar(f, summaryNode, primary) {
    const bar = el('div', { class: 'decide-bar' }, [summaryNode]);
    bar.append(primary);
    bar.append(el('button', { class: 'btn btn-neutral btn-sm', text: 'Return…', onclick: () => returnOne(f.tagset_id) }));
    if (canDelete) bar.append(el('button', { class: 'btn btn-destructive btn-sm', text: 'Discard', onclick: () => discard(f.tagset_id) }));
    return bar;
  }

  // Case A — new recording (no fingerprint match). No compare, no blob choice.
  function caseABody(f) {
    const p1 = piece(pieceHead(1, 'The file', 'preview plays the submitted blob'),
      el('div', { class: 'file-line' }, [
        el('div', { text: `${f.filename} · only rendition (new recording)` }),
        el('button', { class: 'icon-btn rev-play', 'data-play-key': key(f), title: 'Play submitted', text: '▶', onclick: () => playRow(f) }),
      ]));
    const p2 = piece(pieceHead(2, 'Recording assignment'),
      el('div', { class: 'assign' }, [el('span', { class: 'match', text: '✓ new' }), el('span', { text: 'Approving creates a new recording from this file.' })]));
    const p3 = piece(pieceHead(3, 'The appearance (tagset)', 'what gets published'), [tagsGrid(f), editButton(f)]);
    const summary = el('div', { class: 'summary' }, ['On approve: ', el('b', { text: 'new recording + 1 rendition + published appearance' }), '.']);
    const primary = el('button', { class: 'btn btn-primary btn-sm', text: 'Approve', onclick: () => approve(f.tagset_id, {}) });
    return el('div', { class: 'rev-body' }, [
      landing('Approving creates a new recording from this file and publishes its appearance.'),
      el('div', { class: 'pieces' }, [p1, p2, p3]),
      decideBar(f, summary, primary),
    ]);
  }

  // Case C — content-hash dup (no blob stored). Only the appearance is new.
  function caseCBody(f) {
    if (f.collides) {
      const summary = el('div', { class: 'summary is-noop', text: 'Nothing would change on approve.' });
      const bar = el('div', { class: 'decide-bar' }, [summary]);
      bar.append(el('button', { class: 'btn btn-neutral btn-sm', text: 'Return…', onclick: () => returnOne(f.tagset_id) }));
      if (canDelete) bar.append(el('button', { class: 'btn btn-destructive btn-sm', text: 'Deny (discard)', onclick: () => discard(f.tagset_id) }));
      return el('div', { class: 'rev-body' }, [
        el('div', { class: 'callout' }, [el('span', { class: 'i', text: '◆' }),
          el('div', {}, [el('b', { text: 'Nothing to add. ' }), `Same bytes and the offered appearance already exists on recording #${f.recording_id}.`])]),
        bar,
      ]);
    }
    const p1 = piece(pieceHead(1, 'Recording assignment'),
      el('div', { class: 'assign' }, [el('span', { class: 'match', text: '✓ exact bytes' }), el('span', { text: `Joins recording #${f.recording_id}. No file is stored.` })]));
    const p2 = piece(pieceHead(2, 'The appearance (tagset)', 'the only new thing'), [tagsGrid(f), editButton(f)]);
    const summary = el('div', { class: 'summary' }, ['On approve: ', el('b', { text: 'publish appearance' }), `. No blob stored.`]);
    const primary = el('button', { class: 'btn btn-primary btn-sm', text: 'Approve appearance', onclick: () => approve(f.tagset_id, {}) });
    return el('div', { class: 'rev-body' }, [
      landing(`Content-hash dup of recording #${f.recording_id} — no file is stored. Only the appearance is new.`),
      el('div', { class: 'pieces' }, [p1, p2]),
      decideBar(f, summary, primary),
    ]);
  }

  // Case B — new appearance backed by a new blob. Two decisions: the bytes
  // (keep / drop, against the ladder) and — via Approve vs Discard — the appearance.
  function caseBBody(f) {
    const c = classifyCache.get(f.tagset_id);
    const dec = decisionFor(f);

    // Piece 1 — the file, with the ladder compare (once classify loaded).
    let ladder;
    if (!c) ladder = el('div', { class: 'rev-loading', text: 'Comparing renditions…' });
    else if (c.error) ladder = el('div', { class: 'rev-loading', text: 'Couldn’t load the rendition compare.' });
    else ladder = ladderTable(c);

    const collides = f.collides;
    const p1kids = [ladder];
    // The keep/drop choice needs the ladder to recommend correctly — show it only
    // once classify has loaded (the card repaints when it arrives).
    if (!collides && c && !c.error) p1kids.push(bytesChoices(f, dec, !!c.submitted_is_new_best));
    const p1 = piece(pieceHead(1, 'The file (bytes)', 'A/B against the recording’s current best'), p1kids);

    // Piece 2 — recording assignment + force-new.
    const forceCb = el('input', { type: 'checkbox' });
    forceCb.checked = dec.forceNew;
    forceCb.addEventListener('change', () => { dec.forceNew = forceCb.checked; renderCardInline(f); });
    const p2 = piece(pieceHead(2, 'Recording assignment'), [
      el('div', { class: 'assign' }, [el('span', { class: 'match', text: '✓ fingerprint match' }), el('span', {}, [`Joins recording #${f.recording_id}.`])]),
      el('label', { class: 'force-new' }, [forceCb, el('span', { text: ' Force “this is actually new” (pins a new recording)' })]),
    ]);

    // Piece 3 — the appearance.
    let p3;
    if (collides) {
      p3 = piece(pieceHead(3, 'The appearance (tagset)', 'collides with an existing appearance'),
        el('div', { class: 'rev-note' }, [
          'The offered tags equal an appearance already on this recording — nothing new to publish. Adopt a better master through the Duplicates page, or edit the tags to make it a distinct release.',
          editButton(f),
        ]));
    } else {
      p3 = piece(pieceHead(3, 'The appearance (tagset)', 'published on approve'), [tagsGrid(f), editButton(f)]);
    }

    const pieces = [p1, p2, p3];
    const summary = el('div', { class: 'summary' });
    fillSummaryB(summary, f, dec);
    let primary;
    if (collides) {
      primary = el('button', { class: 'btn btn-neutral btn-sm', text: 'Approve (keep bytes)', onclick: () => approve(f.tagset_id, { drop_bytes: dec.bytes === 'drop', force_new: dec.forceNew }) });
    } else {
      primary = el('button', { class: 'btn btn-primary btn-sm', text: 'Approve',
        onclick: () => approve(f.tagset_id, { drop_bytes: dec.bytes === 'drop', force_new: dec.forceNew }) });
    }

    return el('div', { class: 'rev-body' }, [
      landing(`Same audio as recording #${f.recording_id} (we already hold it). Decide the bytes against the ladder, and approve to publish the appearance — or discard to skip it.`),
      el('div', { class: 'pieces' }, pieces),
      decideBar(f, summary, primary),
    ]);
  }

  function bytesChoices(f, dec, newBest) {
    const mk = (val, label, desc, recommended) => {
      const input = el('input', { type: 'radio', name: `bytes-${key(f)}`, value: val });
      input.checked = dec.bytes === val;
      input.addEventListener('change', () => { dec.bytes = val; renderCardInline(f); });
      const lbl = el('label', { class: `choice${dec.bytes === val ? ' is-picked' : ''}` }, [
        input,
        el('div', {}, [
          el('div', { class: 'c-label' }, [label, recommended ? el('span', { class: 'rec-tag', text: 'recommended' }) : null]),
          el('div', { class: 'c-desc', text: desc }),
        ]),
      ]);
      return lbl;
    };
    // Recommendation follows the ladder: keep a new best, drop a below-best dup.
    return el('div', { class: 'choices' }, [
      mk('keep', 'Keep as a rendition', newBest
        ? 'Becomes the recording’s new ladder-best — every appearance upgrades to this master.'
        : 'Store this file too (e.g. a lower-bandwidth tier); the recording still serves its ladder-best.', !!newBest),
      mk('drop', 'Drop the bytes', 'Publish only the appearance; store nothing new on disk.', !newBest),
    ]);
  }

  function ladderTable(c) {
    const best = c.current_best, sub = c.submitted;
    const newBest = !!c.submitted_is_new_best;
    const table = el('table', { class: 'ladder' });
    const thead = el('thead', {}, [el('tr', {}, [
      el('th', { text: 'Rendition' }), el('th', { text: 'Format' }), el('th', { text: 'Ladder' }), el('th', { text: '' }),
    ])]);
    const tb = el('tbody');
    const row = (label, r, opts) => el('tr', { class: opts.cls || '' }, [
      el('td', { class: opts.best ? 'best' : '', text: label }),
      el('td', { class: 'tech', text: techLabel(r) }),
      el('td', { class: 'rank', text: opts.rank }),
      el('td', {}, [opts.tag ? el('span', { class: opts.tagCls, text: opts.tag }) : null]),
    ]);
    if (newBest) {
      tb.append(row('Submitted', sub, { cls: 'is-new', best: true, rank: '#1 (new best)', tag: 'new file', tagCls: 'tag-new' }));
      if (best) tb.append(row('Current', best, { rank: '#2', tag: 'in library', tagCls: 'tag-cur' }));
    } else {
      if (best) tb.append(row('Current best', best, { best: true, rank: '#1', tag: 'in library', tagCls: 'tag-cur' }));
      tb.append(row('Submitted', sub, { cls: 'is-new', rank: 'below best', tag: 'new file', tagCls: 'tag-new' }));
    }
    table.append(thead, tb);
    return el('div', { class: 'ladder-wrap' }, [table]);
  }

  function fillSummaryB(node, f, dec) {
    node.replaceChildren();
    if (f.collides) {
      node.append('On approve: ', el('b', { text: dec.bytes === 'drop' ? 'nothing (bytes dropped, no new appearance)' : 'add rendition, no new appearance' }), '.');
      node.classList.toggle('is-noop', dec.bytes === 'drop');
      return;
    }
    const bytesPart = dec.bytes === 'drop' ? 'drop the submitted bytes' : 'keep the bytes as a rendition';
    const rec = dec.forceNew ? 'a new pinned recording' : `recording #${f.recording_id}`;
    node.append('On approve: ', el('b', { text: 'publish the appearance' }), ` on ${rec}, `, el('b', { text: bytesPart }), '.');
    node.classList.remove('is-noop');
  }

  // renderCardInline repaints a single open card in place (radio / checkbox flips)
  // without disturbing the rest of the queue or the scroll position.
  function renderCardInline(f) {
    if (!host) return;
    const art = host.querySelector(`.rev-card[data-tid="${cssEsc(key(f))}"]`);
    if (!art) { render(); return; }
    const fresh = cardDom(f);
    fresh.dataset.tid = key(f);
    art.replaceWith(fresh);
    if (playingKey) setPlaying(playingKey);
  }

  function loadMoreDom() {
    if (rows.length >= total) return null;
    const btn = el('button', { class: 'btn btn-neutral', text: loading ? 'Loading…' : `Load more (${rows.length} of ${total})`,
      onclick: () => loadMore() });
    btn.disabled = loading;
    return el('div', { class: 'rev-loadmore' }, [btn]);
  }

  async function loadMore() {
    if (loading || rows.length >= total) return;
    loading = true; render();
    try {
      const data = await loadPage(rows.length);
      rows = rows.concat(data.items || []);
      total = data.total || rows.length;
      selectableTotal = data.selectable_total ?? selectableTotal;
    } catch (err) { toast(err.message, 'error'); }
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
      classifyCache.clear(); decisions.clear();
      // keep openCards only for rows still present
      const present = new Set(rows.map(key));
      for (const k of [...openCards]) if (!present.has(k)) openCards.delete(k);
    } catch (err) { toast(err.message, 'error'); rows = []; }
    finally { loading = false; render(); }
  }

  function mount() {
    host = document.getElementById('fileListReview');
    if (!host) return;
    host.setAttribute('aria-busy', 'false');
    host.classList.add('rev-scope');
    reload();
  }

  return {
    id: 'review',
    label: 'Review',
    available: perms.includes('content.moderate'),
    mount,
    reload,
    destroy: () => { editor.destroy(); },
  };
}
