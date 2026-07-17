// Shared per-track metadata editor — one modal editing a single file's tags.
//
// The regular (compact) modal edits the four base tags (title / artist / album
// artist / album) plus the track number. An "Extended edit" button opens a
// second, wider modal stacked above it that collects the rarely-touched tags
// (year, track total, disc number, genre, composer, comment) in a two-column
// layout. Both modals' Save persist the whole tag set in one PATCH.
//
// Self-contained: builds its own .modal-backdrop elements on document.body; the
// styling comes from app.css (.modal-backdrop / .modal / .modal-wide /
// .btn-close / .edit-form / .edit-grid-2), so any page that links app.css can use
// it without extra markup.
//
// Consumers (docs/architecture/moderation.md, "Code reuse"):
//   - the admin Files page  → PATCH /api/files/{hash}/metadata (metadata.edit)
//   - the upload page's "My uploads" tab → PATCH /api/my/uploads/{hash}/metadata
//     (owner-scoped; drafts and returned files only)
// The endpoints are caller-supplied, the component owns the form/modal mechanics.
//
// The track number + extended fields are shown only when `detailURL` is supplied:
// the modal GETs the file's current full tag set on open so it can edit those
// fields without clobbering the ones the user never sees. Without `detailURL` the
// modal is base-fields-only, exactly as before.

let nextEditorId = 1;

// The extended fields (shown in the wide modal) and their input flavour. A
// 'span' field spans both grid columns. Exported so bulk-edit.js builds the same
// extended set from one source of truth.
export const EXTENDED_FIELDS = [
  ['year', 'Year', 'number'],
  ['track_total', 'Track total', 'number'],
  ['disc_number', 'Disc number', 'number'],
  ['genre', 'Genre', 'text'],
  ['composer', 'Composer', 'text', 'span'],
  ['comment', 'Comment', 'textarea', 'span'],
];

// Every field the suggestions panel can diff/apply, in display order — the
// base tags plus the track number plus the extended set above.
const SUGGEST_FIELDS = [
  ['title', 'Title'],
  ['artist', 'Artist'],
  ['album_artist', 'Album artist'],
  ['album', 'Album'],
  ['track_number', 'Track number'],
  ...EXTENDED_FIELDS.map(([key, label]) => [key, label]),
];

/**
 * createTrackEditor builds the modal(s) (hidden) and returns its controls.
 *
 * @param {Object}   opts
 * @param {Function} opts.patchURL  (file) => string — the PATCH endpoint for this file.
 * @param {Function} [opts.detailURL] (file) => string — GET endpoint returning the
 *                                  full tag set. Enables the track-number field +
 *                                  the "Extended edit" wide modal; omit for
 *                                  base-fields-only editing.
 * @param {string}   [opts.note]    Explanatory text shown under the base fields.
 * @param {Function} [opts.suggestURL] (file) => string|null — GET endpoint returning
 *                                  candidate tagsets (/api/tagsets/{id}/suggestions).
 *                                  Enables the "Suggest tags…" panel: source chips
 *                                  (ID3v2 / ID3v1 / services) with a current-vs-
 *                                  suggested diff table, per-field apply and a
 *                                  charset override for the local sources. Return
 *                                  null for rows that carry no tagset id.
 * @param {Function} [opts.checkAuth] (Response) => bool — return true when the
 *                                  response was an auth failure the caller
 *                                  already handled (the operation is abandoned).
 * @param {Function} opts.onSaved   (file, data, ctx) => void — called with the
 *                                  server's authoritative values on success.
 * @param {Function} [opts.onError] (err, file) => void — load/save failure.
 * @param {Object}   [opts.access]  Optional access section (License + Guest),
 *                                  shown only when supplied (admin scopes):
 *                                  { licenses: string[],
 *                                    save: async (file, {guest, license}) => void }.
 *                                  The tag PATCH runs first; `save` is awaited
 *                                  after it, so access writes hit their own
 *                                  endpoints (POST …/guest, …/license).
 * @returns {{ open(file, ctx?): void, close(): void, destroy(): void }}
 *   open()'s `file` needs { hash, title, artist, album_artist, album }; with
 *   `access` it also reads { guest_playable, license }. `ctx` is passed through
 *   to onSaved untouched (e.g. "which view opened me").
 */
export function createTrackEditor({ patchURL, createURL = null, detailURL = null, note = '',
  create = false, createTitle = 'Add appearance', createNote = '', onCreated = null,
  checkAuth = null, onSaved, onError, access = null, suggestURL = null }) {
  const titleId = `trackEditTitle${nextEditorId++}`;
  // Create mode: submit POSTs a brand-new record (no detail fetch — the extended
  // fields start blank and editable), and server refusals show inline so the
  // user can fix and retry rather than the modal vanishing. A create-mode editor
  // shows the track-number + extended fields even without a detailURL.
  const wantsExtended = !!detailURL || create;

  // labelled builds a <label>text + control</label> and returns the <label>.
  function labelled(parent, text, control, cls = '') {
    const wrap = document.createElement('label');
    if (cls) wrap.className = cls;
    wrap.append(document.createTextNode(text), control);
    parent.appendChild(wrap);
    return wrap;
  }

  function makeBackdrop(extraClass = '') {
    const b = document.createElement('div');
    b.className = `modal-backdrop hidden${extraClass ? ' ' + extraClass : ''}`;
    b.setAttribute('role', 'dialog');
    b.setAttribute('aria-modal', 'true');
    return b;
  }
  function makeHeader(text, onClose) {
    const header = document.createElement('div');
    header.className = 'modal-header';
    const h = document.createElement('h2');
    h.textContent = text;
    const x = document.createElement('button');
    x.type = 'button';
    x.className = 'btn-close';
    x.setAttribute('aria-label', 'Close');
    x.textContent = '×';
    x.addEventListener('click', onClose);
    header.append(h, x);
    return { header, heading: h };
  }
  function makeActions(buttons) {
    const actions = document.createElement('div');
    actions.className = 'modal-actions';
    actions.append(...buttons);
    return actions;
  }

  // ── Regular (compact) modal: base tags + track number + access ──────────────
  const backdrop = makeBackdrop();
  const modal = document.createElement('div');
  modal.className = 'modal';
  const { header: mainHeader, heading } = makeHeader(create ? createTitle : 'Edit metadata', () => closeAll());
  heading.id = titleId;
  backdrop.setAttribute('aria-labelledby', titleId);

  const form = document.createElement('form');
  form.className = 'edit-form';

  const inputs = {};
  for (const [key, label] of [
    ['title', 'Title'],
    ['artist', 'Artist'],
    ['album_artist', 'Album artist'],
    ['album', 'Album'],
  ]) {
    const input = document.createElement('input');
    input.type = 'text';
    input.autocomplete = 'off';
    inputs[key] = input;
    labelled(form, label, input);
  }

  // Track number — main form, but only meaningful (and shown) in detail mode.
  const trackNumberInput = document.createElement('input');
  trackNumberInput.type = 'number';
  trackNumberInput.min = '0';
  trackNumberInput.step = '1';
  trackNumberInput.inputMode = 'numeric';
  const trackNumberWrap = labelled(form, 'Track number', trackNumberInput);

  // Optional access section (admin scopes only). Saved via access.save() after
  // the tag PATCH, since guest/license have their own endpoints.
  let licenseSel = null, guestCb = null;
  if (access) {
    const split = document.createElement('div');
    split.className = 'field-split';

    const licWrap = document.createElement('label');
    licWrap.append(document.createTextNode('License'));
    licenseSel = document.createElement('select');
    for (const lic of access.licenses || []) {
      const opt = document.createElement('option');
      opt.value = lic;
      opt.textContent = lic || '— none —';
      licenseSel.appendChild(opt);
    }
    licWrap.appendChild(licenseSel);

    const guestWrap = document.createElement('label');
    guestWrap.className = 'field-check';
    guestCb = document.createElement('input');
    guestCb.type = 'checkbox';
    const guestText = document.createElement('span');
    guestText.textContent = 'Guest-playable';
    guestWrap.append(guestCb, guestText);

    split.append(licWrap, guestWrap);
    form.appendChild(split);
  }

  // Explanatory note — its text is set per open (create/edit can differ).
  const noteEl = document.createElement('p');
  noteEl.className = 'modal-body';
  noteEl.textContent = create ? (createNote || note) : note;
  if (noteEl.textContent) form.appendChild(noteEl);

  // Inline error slot — populated on a create-mode refusal (nameless / identical
  // / bad number), so the modal stays open with the reason shown.
  const errorEl = document.createElement('p');
  errorEl.className = 'edit-error hidden';
  errorEl.setAttribute('role', 'alert');
  form.appendChild(errorEl);

  // "Extended edit" — opens the wide modal (detail edit, or any create).
  let extendedBtn = null;
  if (wantsExtended) {
    extendedBtn = document.createElement('button');
    extendedBtn.type = 'button';
    extendedBtn.className = 'edit-extended-toggle';
    extendedBtn.textContent = '+ Extended edit…';
    extendedBtn.addEventListener('click', () => openExtended());
    form.appendChild(extendedBtn);
  }

  // "Suggest tags" — opens the suggestions panel (edit mode only; hidden when
  // the row carries no suggestions endpoint, e.g. create mode).
  let suggestBtn = null;
  if (suggestURL) {
    suggestBtn = document.createElement('button');
    suggestBtn.type = 'button';
    suggestBtn.className = 'edit-extended-toggle';
    suggestBtn.textContent = '✦ Suggest tags…';
    suggestBtn.addEventListener('click', () => openSuggest());
    form.appendChild(suggestBtn);
  }

  const submitBtn = document.createElement('button');
  submitBtn.type = 'submit';
  submitBtn.className = 'btn btn-neutral';
  submitBtn.textContent = 'Save';
  const cancelBtn = document.createElement('button');
  cancelBtn.type = 'button';
  cancelBtn.className = 'btn btn-neutral';
  cancelBtn.textContent = 'Cancel';
  cancelBtn.addEventListener('click', () => closeAll());
  form.appendChild(makeActions([cancelBtn, submitBtn]));

  modal.append(mainHeader, form);
  backdrop.appendChild(modal);
  document.body.appendChild(backdrop);

  // ── Wide secondary modal: the extended tags (built in detail-edit or create) ─
  let extBackdrop = null, extForm = null, extSubmitBtn = null;
  const extInputs = {};
  if (wantsExtended) {
    extBackdrop = makeBackdrop('is-stacked');
    const extModal = document.createElement('div');
    extModal.className = 'modal modal-wide';
    const { header: extHeader } = makeHeader('Extended tags', () => closeExtended());
    extModal.appendChild(extHeader);

    extForm = document.createElement('form');
    extForm.className = 'edit-form';
    const grid = document.createElement('div');
    grid.className = 'edit-grid-2';
    for (const [key, label, kind, span] of EXTENDED_FIELDS) {
      let input;
      if (kind === 'textarea') {
        input = document.createElement('textarea');
        input.rows = 2;
      } else {
        input = document.createElement('input');
        input.type = kind;
        if (kind === 'number') { input.min = '0'; input.step = '1'; input.inputMode = 'numeric'; }
        else input.autocomplete = 'off';
      }
      extInputs[key] = input;
      labelled(grid, label, input, span === 'span' ? 'span-2' : '');
    }
    extForm.appendChild(grid);

    extSubmitBtn = document.createElement('button');
    extSubmitBtn.type = 'submit';
    extSubmitBtn.className = 'btn btn-neutral';
    extSubmitBtn.textContent = 'Save';
    const extBack = document.createElement('button');
    extBack.type = 'button';
    extBack.className = 'btn btn-neutral';
    extBack.textContent = 'Back';
    extBack.addEventListener('click', () => closeExtended());
    extForm.appendChild(makeActions([extBack, extSubmitBtn]));

    extModal.appendChild(extForm);
    extBackdrop.appendChild(extModal);
    extBackdrop.addEventListener('click', e => { if (e.target === extBackdrop) closeExtended(); });
    document.body.appendChild(extBackdrop);
  }

  // ── Suggestions panel: source chips + current-vs-suggested diff table ──────
  // (docs/architecture/tag-suggestions.md). Read-only lookups; "Use" only fills
  // the inputs above — Save stays the one write path.
  let sugBackdrop = null, sugChipsRow = null, sugCharsetWrap = null, sugCharsetSel = null,
      sugBody = null, sugUseAllBtn = null, sugSearchWrap = null, sugSearchInput = null,
      sugSearchBtn = null;
  let sugData = null;       // last /suggestions response while the panel is open
  let sugActive = -1;       // index into sugData.suggestions
  let sugExtLoading = null; // external source name while its lookup is in flight
  if (suggestURL) {
    sugBackdrop = makeBackdrop('is-stacked');
    const sugModal = document.createElement('div');
    sugModal.className = 'modal modal-wide';
    const { header: sugHeader } = makeHeader('Suggested tags', () => closeSuggest());
    sugModal.appendChild(sugHeader);

    sugChipsRow = document.createElement('div');
    sugChipsRow.className = 'suggest-chips';
    sugCharsetWrap = document.createElement('label');
    sugCharsetWrap.className = 'suggest-charset hidden';
    sugCharsetWrap.append(document.createTextNode('Charset'));
    sugCharsetSel = document.createElement('select');
    sugCharsetWrap.appendChild(sugCharsetSel);
    sugCharsetSel.addEventListener('change', () => refetchCharset());
    // Text-search fallback row (P2): revealed when the service lookup finds
    // nothing to match on (no fingerprint / no hit).
    sugSearchWrap = document.createElement('div');
    sugSearchWrap.className = 'suggest-search hidden';
    sugSearchInput = document.createElement('input');
    sugSearchInput.type = 'text';
    sugSearchInput.placeholder = 'artist and title…';
    sugSearchInput.setAttribute('aria-label', 'MusicBrainz search terms');
    sugSearchInput.addEventListener('keydown', e => {
      if (e.key === 'Enter') { e.preventDefault(); searchExternal('musicbrainz'); }
    });
    sugSearchBtn = document.createElement('button');
    sugSearchBtn.type = 'button';
    sugSearchBtn.className = 'btn btn-neutral';
    sugSearchBtn.textContent = 'Search MusicBrainz';
    sugSearchBtn.addEventListener('click', () => searchExternal('musicbrainz'));
    sugSearchWrap.append(sugSearchInput, sugSearchBtn);

    sugBody = document.createElement('div');
    sugBody.className = 'suggest-body';

    sugUseAllBtn = document.createElement('button');
    sugUseAllBtn.type = 'button';
    sugUseAllBtn.className = 'btn btn-neutral';
    sugUseAllBtn.textContent = 'Use all';
    sugUseAllBtn.addEventListener('click', () => {
      const s = sugData && sugData.suggestions[sugActive];
      if (!s) return;
      for (const [key, , , val] of suggestRows(s)) applyField(key, val);
      closeSuggest();
    });
    const sugCloseBtn = document.createElement('button');
    sugCloseBtn.type = 'button';
    sugCloseBtn.className = 'btn btn-neutral';
    sugCloseBtn.textContent = 'Close';
    sugCloseBtn.addEventListener('click', () => closeSuggest());

    sugModal.append(sugChipsRow, sugCharsetWrap, sugSearchWrap, sugBody, makeActions([sugCloseBtn, sugUseAllBtn]));
    sugBackdrop.appendChild(sugModal);
    sugBackdrop.addEventListener('click', e => { if (e.target === sugBackdrop) closeSuggest(); });
    document.body.appendChild(sugBackdrop);
  }

  let editing = null;       // { file, ctx, mode } while open ('edit' | 'create')
  let detailLoaded = false; // true once the extended/track fields carry values
  const mainOpen = () => !backdrop.classList.contains('hidden');
  const extOpen = () => extBackdrop && !extBackdrop.classList.contains('hidden');

  const numStr = v => (v === null || v === undefined ? '' : String(v));

  // setExtendedFrom populates the track-number + extended inputs from a server
  // metadata payload (numbers may be null) and reveals the controls.
  function setExtendedFrom(data) {
    trackNumberInput.value = numStr(data.track_number);
    trackNumberWrap.classList.remove('hidden');
    for (const [key, , kind] of EXTENDED_FIELDS) {
      extInputs[key].value = kind === 'number' ? numStr(data[key]) : (data[key] || '');
    }
    if (extendedBtn) extendedBtn.classList.remove('hidden');
    detailLoaded = true;
  }

  // seedExtendedEmpty reveals the track-number + extended controls with blank
  // values — the create-mode counterpart of setExtendedFrom (no server payload).
  function seedExtendedEmpty() {
    trackNumberInput.value = '';
    trackNumberWrap.classList.remove('hidden');
    for (const [key] of EXTENDED_FIELDS) extInputs[key].value = '';
    if (extendedBtn) extendedBtn.classList.remove('hidden');
    detailLoaded = true;
  }

  function showError(msg) { errorEl.textContent = msg; errorEl.classList.remove('hidden'); }
  function clearError() { errorEl.textContent = ''; errorEl.classList.add('hidden'); }

  // hideExtended keeps the modal base-fields-only (no detailURL, or a failed load).
  function hideExtended() {
    detailLoaded = false;
    trackNumberWrap.classList.add('hidden');
    if (extendedBtn) extendedBtn.classList.add('hidden');
    if (extBackdrop) extBackdrop.classList.add('hidden');
  }

  function openExtended() {
    if (extBackdrop) { extBackdrop.classList.remove('hidden'); extInputs.year.focus(); }
  }
  function closeExtended() {
    if (extBackdrop) extBackdrop.classList.add('hidden');
  }

  // ── Suggestions panel behavior ──────────────────────────────────────────────
  const suggestOpenNow = () => sugBackdrop && !sugBackdrop.classList.contains('hidden');

  // fieldInput maps a suggestion field key to its form control (undefined when
  // this editor doesn't carry the control).
  function fieldInput(key) {
    if (key in inputs) return inputs[key];
    if (key === 'track_number') return trackNumberInput;
    return extInputs[key];
  }
  function applyField(key, val) {
    const input = fieldInput(key);
    if (input) input.value = val;
  }

  // suggestRows filters the active suggestion down to fields this modal can
  // edit right now — track-number/extended only once the detail fetch seeded
  // them, so applying never clobbers unseen stored values.
  // → [key, label, current, suggested]
  function suggestRows(s) {
    const rows = [];
    for (const [key, label] of SUGGEST_FIELDS) {
      if (!s.tags || !(key in s.tags)) continue;
      const input = fieldInput(key);
      if (!input) continue;
      if (!(key in inputs) && !detailLoaded) continue;
      rows.push([key, label, input.value || '', String(s.tags[key])]);
    }
    return rows;
  }

  function setSuggestMsg(text) {
    const p = document.createElement('p');
    p.className = 'suggest-empty';
    p.textContent = text;
    sugBody.replaceChildren(p);
  }

  async function openSuggest() {
    if (!editing || editing.mode !== 'edit') return;
    const url = suggestURL(editing.file);
    if (!url) return;
    sugData = null;
    sugActive = -1;
    sugExtLoading = null;
    sugChipsRow.replaceChildren();
    sugCharsetWrap.classList.add('hidden');
    sugSearchWrap.classList.add('hidden');
    sugSearchInput.value = '';
    sugUseAllBtn.disabled = true;
    setSuggestMsg('Reading tag blocks…');
    sugBackdrop.classList.remove('hidden');
    try {
      const res = await fetch(url);
      if (checkAuth && checkAuth(res)) { closeSuggest(); return; }
      const data = await res.json().catch(() => ({}));
      if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
      sugData = data;
      renderChips();
      const first = (data.suggestions || []).findIndex(s => !s.error);
      if (first >= 0) selectChip(first);
      else if ((data.external_sources || []).length) setSuggestMsg('No readable tag blocks — try a service lookup above.');
      else setSuggestMsg('No readable tag blocks in this file.');
    } catch (err) {
      setSuggestMsg('Couldn’t load suggestions.');
      if (onError) onError(err, editing && editing.file);
    }
  }

  // extSourceLabel maps an external source name to its display label.
  function extSourceLabel(src) {
    return src === 'musicbrainz' ? 'MusicBrainz' : src;
  }

  function renderChips() {
    sugChipsRow.replaceChildren();
    (sugData.suggestions || []).forEach((s, i) => {
      const chip = document.createElement('button');
      chip.type = 'button';
      chip.className = 'suggest-chip' + (i === sugActive ? ' is-active' : '');
      chip.textContent = s.label || s.source;
      // Service matches carry a confidence score — show it on the chip.
      if (!s.error && s.confidence > 0 && s.confidence < 1) {
        chip.textContent += ` · ${Math.round(s.confidence * 100)}%`;
      }
      if (s.error) { chip.disabled = true; chip.title = s.error; }
      else chip.addEventListener('click', () => selectChip(i));
      sugChipsRow.appendChild(chip);
    });
    // Enabled-but-not-yet-queried external sources: on-demand chips whose
    // click issues the explicit service lookup (user-triggered only).
    (sugData.external_sources || []).forEach(src => {
      const chip = document.createElement('button');
      chip.type = 'button';
      chip.className = 'suggest-chip is-external';
      chip.textContent = sugExtLoading === src ? `${extSourceLabel(src)}…` : `⌕ ${extSourceLabel(src)}`;
      chip.disabled = sugExtLoading === src;
      chip.addEventListener('click', () => loadExternal(src));
      sugChipsRow.appendChild(chip);
    });
  }

  // mergeExternalResults replaces src's previous entries (a stale "no match"
  // placeholder, or an earlier search's candidates) with the fresh ones and
  // activates the best new candidate. Returns false when nothing usable came
  // back — the caller picks the message and offers the search fallback.
  function mergeExternalResults(src, found) {
    const active = sugActive >= 0 ? sugData.suggestions[sugActive] : null;
    sugData.suggestions = sugData.suggestions.filter(x => x.source !== src);
    const firstNew = sugData.suggestions.length;
    sugData.suggestions.push(...found);
    sugActive = active ? sugData.suggestions.indexOf(active) : -1;
    const firstOk = sugData.suggestions.findIndex((x, i) => i >= firstNew && !x.error);
    if (firstOk >= 0) { selectChip(firstOk); return true; }
    renderChips();
    return false;
  }

  // showSearchRow reveals the text-search fallback, seeding the field from
  // the modal's current inputs on first reveal.
  function showSearchRow() {
    if (sugSearchWrap.classList.contains('hidden') && !sugSearchInput.value) {
      sugSearchInput.value = [inputs.artist.value, inputs.title.value]
        .map(v => v.trim()).filter(Boolean).join(' ');
    }
    sugSearchWrap.classList.remove('hidden');
  }

  // runExternal is the shared fetch/merge cycle of the two external paths:
  // the fingerprint lookup (loadExternal) and the text search (searchExternal).
  // A 429 (shared rate limit) keeps the chip clickable with a friendly retry
  // message.
  async function runExternal(src, params, progressMsg, emptyMsg) {
    if (!editing || sugExtLoading) return;
    sugExtLoading = src;
    renderChips();
    sugUseAllBtn.disabled = true;
    sugSearchBtn.disabled = true;
    setSuggestMsg(progressMsg);
    const base = suggestURL(editing.file);
    const sep = base.includes('?') ? '&' : '?';
    try {
      const res = await fetch(`${base}${sep}sources=${encodeURIComponent(src)}${params}`);
      if (checkAuth && checkAuth(res)) { closeSuggest(); return; }
      if (res.status === 429) {
        setSuggestMsg('Lookup service busy — try again in a moment.');
        return;
      }
      const data = await res.json().catch(() => ({}));
      if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
      const found = (data.suggestions || []).filter(x => x.source === src);
      sugData.external_sources = (sugData.external_sources || []).filter(x => x !== src);
      if (!mergeExternalResults(src, found)) {
        const errText = found.length ? found[0].error : '';
        setSuggestMsg(`${extSourceLabel(src)}: ${errText || emptyMsg}.`);
        showSearchRow(); // no usable result → offer/keep the text search (P2)
      }
    } catch (err) {
      setSuggestMsg('Couldn’t reach the lookup service.');
      if (onError) onError(err, editing && editing.file);
    } finally {
      sugExtLoading = null;
      sugSearchBtn.disabled = false;
      renderChips();
    }
  }

  // loadExternal runs the source's default lookup (fingerprint-keyed).
  function loadExternal(src) {
    return runExternal(src, '', `Looking up ${extSourceLabel(src)}…`,
      'no match for this file');
  }

  // searchExternal runs an explicit text search with the field's query (the
  // server seeds an empty one from the stored tags).
  function searchExternal(src) {
    return runExternal(src, `&query=${encodeURIComponent(sugSearchInput.value.trim())}`,
      `Searching ${extSourceLabel(src)}…`, 'no results — try different terms');
  }

  function selectChip(i) {
    sugActive = i;
    renderChips();
    renderSuggestion();
  }

  function renderSuggestion() {
    const s = sugData && sugData.suggestions[sugActive];
    if (!s) return;
    // Charset override (local sources only): re-fetches this source re-decoded.
    if (s.charsets && s.charsets.length) {
      sugCharsetSel.replaceChildren();
      for (const name of s.charsets) {
        const opt = document.createElement('option');
        opt.value = name;
        opt.textContent = name;
        sugCharsetSel.appendChild(opt);
      }
      sugCharsetSel.value = s.charset || s.charsets[0];
      sugCharsetWrap.classList.remove('hidden');
    } else {
      sugCharsetWrap.classList.add('hidden');
    }

    const rows = suggestRows(s);
    if (!rows.length) {
      setSuggestMsg('This source carries no usable fields.');
      sugUseAllBtn.disabled = true;
      return;
    }
    const table = document.createElement('table');
    table.className = 'suggest-table';
    const thead = table.createTHead().insertRow();
    for (const h of ['Field', 'Current', 'Suggested', '']) {
      const th = document.createElement('th');
      th.textContent = h;
      thead.appendChild(th);
    }
    const tbody = table.createTBody();
    for (const [key, label, cur, val] of rows) {
      const tr = tbody.insertRow();
      if (cur.trim() !== val.trim()) tr.className = 'is-diff';
      tr.insertCell().textContent = label;
      const curCell = tr.insertCell();
      curCell.className = 'suggest-cur';
      curCell.textContent = cur;
      const valCell = tr.insertCell();
      valCell.className = 'suggest-val';
      valCell.textContent = val;
      const useBtn = document.createElement('button');
      useBtn.type = 'button';
      useBtn.className = 'suggest-use';
      useBtn.textContent = '←';
      useBtn.title = 'Use this value';
      useBtn.setAttribute('aria-label', `Use suggested ${label}`);
      useBtn.addEventListener('click', () => { applyField(key, val); renderSuggestion(); });
      tr.insertCell().appendChild(useBtn);
    }
    sugBody.replaceChildren(table);
    sugUseAllBtn.disabled = false;
  }

  // refetchCharset re-queries only the active source with the chosen charset
  // (?sources=<one>&charset=<cs>) and swaps the result in place — the live
  // preview of the override dropdown.
  async function refetchCharset() {
    const s = sugData && sugData.suggestions[sugActive];
    if (!s || !editing) return;
    const base = suggestURL(editing.file);
    const sep = base.includes('?') ? '&' : '?';
    try {
      const res = await fetch(`${base}${sep}sources=${encodeURIComponent(s.source)}&charset=${encodeURIComponent(sugCharsetSel.value)}`);
      if (checkAuth && checkAuth(res)) { closeSuggest(); return; }
      const data = await res.json().catch(() => ({}));
      const repl = res.ok && data.ok && (data.suggestions || []).find(x => x.source === s.source);
      if (!repl) throw new Error(data.error || `HTTP ${res.status}`);
      sugData.suggestions[sugActive] = repl;
      renderSuggestion();
    } catch (err) {
      if (onError) onError(err, editing && editing.file);
    }
  }

  function closeSuggest() {
    if (sugBackdrop) sugBackdrop.classList.add('hidden');
  }

  async function open(file, ctx = null) {
    editing = { file, ctx, mode: 'edit' };
    clearError();
    inputs.title.value        = file.title || '';
    inputs.artist.value       = file.artist || '';
    inputs.album_artist.value = file.album_artist || '';
    inputs.album.value        = file.album || '';
    if (access) {
      licenseSel.value = file.license || '';
      guestCb.checked  = !!file.guest_playable;
    }
    if (suggestBtn) suggestBtn.classList.toggle('hidden', !suggestURL(file));
    hideExtended();
    backdrop.classList.remove('hidden');
    inputs.title.focus();

    if (!detailURL) return;
    // Fetch the full tag set so the track-number + extended fields reflect the
    // stored values (and can be saved without clobbering unseen fields).
    try {
      const res = await fetch(detailURL(file));
      if (checkAuth && checkAuth(res)) { closeAll(); return; }
      if (editing && editing.file === file) {
        if (res.ok) {
          const data = await res.json().catch(() => ({}));
          setExtendedFrom(data);
        } else if (onError) {
          onError(new Error(`HTTP ${res.status}`), file);
        }
      }
    } catch (err) {
      if (onError) onError(err, file);
    }
  }

  // openCreate opens the modal blank for a brand-new record; `ctx` is passed
  // through to onCreated (e.g. the recording the appearance is added to). All
  // fields — base + track-number + extended — start empty and editable.
  function openCreate(ctx = null) {
    editing = { file: null, ctx, mode: 'create' };
    clearError();
    if (suggestBtn) suggestBtn.classList.add('hidden'); // no subject file yet
    for (const k of ['title', 'artist', 'album_artist', 'album']) inputs[k].value = '';
    seedExtendedEmpty();
    backdrop.classList.remove('hidden');
    inputs.title.focus();
  }

  function closeAll() {
    backdrop.classList.add('hidden');
    if (extBackdrop) extBackdrop.classList.add('hidden');
    closeSuggest();
    sugData = null;
    editing = null;
  }

  backdrop.addEventListener('click', e => { if (e.target === backdrop) closeAll(); });
  const onKey = e => {
    if (e.key !== 'Escape') return;
    if (suggestOpenNow()) closeSuggest();
    else if (extOpen()) closeExtended();
    else if (mainOpen()) closeAll();
  };
  document.addEventListener('keydown', onKey);

  async function submit(e) {
    if (e) e.preventDefault();
    if (!editing) return;
    const { file, ctx, mode } = editing;
    const isCreate = mode === 'create';
    const body = {
      title:        inputs.title.value,
      artist:       inputs.artist.value,
      album_artist: inputs.album_artist.value,
      album:        inputs.album.value,
    };
    // Only send the track-number + extended fields once they were loaded, so a
    // base-only edit (or a failed detail fetch) never blanks them. Create mode
    // always seeds them, so they always ride along.
    if (detailLoaded) {
      body.track_number = trackNumberInput.value;
      for (const [key] of EXTENDED_FIELDS) body[key] = extInputs[key].value;
    }
    clearError();
    submitBtn.disabled = true;
    if (extSubmitBtn) extSubmitBtn.disabled = true;
    try {
      const url = isCreate ? (createURL ? createURL(ctx) : patchURL(ctx)) : patchURL(file);
      const res = await fetch(url, {
        method: isCreate ? 'POST' : 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (checkAuth && checkAuth(res)) return;
      const data = await res.json().catch(() => ({}));
      if (!res.ok || !data.ok) {
        // A create refusal (nameless / identical / bad number) stays on the form
        // with its reason shown; an edit failure defers to onError, as before.
        if (isCreate) { showError(data.error || `Couldn’t add the appearance (HTTP ${res.status}).`); return; }
        throw new Error(data.error || `HTTP ${res.status}`);
      }
      // Access writes hit their own endpoints; run them after the tag PATCH.
      if (!isCreate && access) await access.save(file, { guest: guestCb.checked, license: licenseSel.value });
      closeAll();
      if (isCreate) (onCreated || onSaved)(data, ctx);
      else onSaved(file, data, ctx);
    } catch (err) {
      if (isCreate) showError(err.message || 'Network error.');
      else if (onError) onError(err, file);
      else console.error('track-edit save failed:', err);
    } finally {
      submitBtn.disabled = false;
      if (extSubmitBtn) extSubmitBtn.disabled = false;
    }
  }

  form.addEventListener('submit', submit);
  if (extForm) extForm.addEventListener('submit', submit);

  // destroy removes the modals and the document-level listener — required on
  // shell pages, whose modules are torn down when <main> is swapped.
  function destroy() {
    document.removeEventListener('keydown', onKey);
    backdrop.remove();
    if (extBackdrop) extBackdrop.remove();
    if (sugBackdrop) sugBackdrop.remove();
    editing = null;
  }

  return { open, openCreate, close: closeAll, destroy };
}
